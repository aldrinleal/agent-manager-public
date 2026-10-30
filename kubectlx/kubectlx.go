// Package kubectlx is the transport for workspace pods: commands go through
// the API server's pods/exec, the way `kubectl exec -i` does it, but natively
// via client-go — the same principle as sshx using x/crypto/ssh instead of an
// ssh binary, so the image stays FROM scratch (SPEC.md §5.5).
//
// It is the only package that imports client-go. Nothing above the
// transport.Transport seam knows it exists.
package kubectlx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/joomcode/errorx"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/utils/exec"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
)

var Errors = errorx.NewNamespace("kubectlx")

var (
	ErrConfig = Errors.NewType("config")
	ErrExec   = Errors.NewType("exec")
)

// Dialer is the transport.Dialer for `transport: kubectl` hosts.
//
// The cluster config is resolved on the first Dial, not at construction, so a
// CLI with no kubeconfig and an inventory of ssh hosts behaves exactly as it
// did before this package existed.
type Dialer struct {
	cfg *config.Config
	inv *config.Inventory

	once sync.Once
	rest *rest.Config
	core *rest.RESTClient
	err  error
}

// coreScheme knows core/v1 and nothing else. client-go's typed Clientset
// would register every API group and roughly triple the binary; exec needs
// exactly one type, PodExecOptions.
var coreScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		panic(err)
	}

	return s
}()

var _ transport.Dialer = (*Dialer)(nil)

// NewDialer builds the dialer. It does not touch the cluster.
func NewDialer(cfg *config.Config, inv *config.Inventory) *Dialer {
	return &Dialer{cfg: cfg, inv: inv}
}

// Dial returns a Transport for a kubectl host. There is no connection to open:
// every command is its own exec stream.
func (d *Dialer) Dial(hostName string) (transport.Transport, error) {
	host, err := d.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	if !host.IsKubectl() {
		return nil, ErrConfig.New("host %q is not a kubectl host", hostName)
	}

	d.once.Do(d.init)

	if d.err != nil {
		return nil, d.err
	}

	return &Client{dialer: d, host: host}, nil
}

// Close is a no-op: nothing is pooled.
func (d *Dialer) Close() error { return nil }

func (d *Dialer) init() {
	rc, err := restConfig(d.cfg.Kubeconfig, rest.InClusterConfig, defaultKubeconfig())
	if err != nil {
		d.err = err
		return
	}

	// Long-running streams are bounded by the caller's context, never by a
	// client-wide timeout that would cut off a slow git fetch.
	rc.Timeout = 0

	coreConfig := rest.CopyConfig(rc)
	coreConfig.APIPath = "/api"
	coreConfig.GroupVersion = &corev1.SchemeGroupVersion
	coreConfig.NegotiatedSerializer = serializer.NewCodecFactory(coreScheme).WithoutConversion()

	core, err := rest.RESTClientFor(coreConfig)
	if err != nil {
		d.err = ErrConfig.Wrap(err, "building the kubernetes client")
		return
	}

	d.rest, d.core = rc, core
}

// restConfig resolves the cluster config: an explicit kubeconfig, then the
// in-cluster service account, then ~/.kube/config. A kubeconfig that is
// named but broken is an error, never a silent fall-through to another
// cluster.
func restConfig(explicit string, inCluster func() (*rest.Config, error), fallback string) (*rest.Config, error) {
	if explicit != "" {
		rc, err := clientcmd.BuildConfigFromFlags("", explicit)
		if err != nil {
			return nil, ErrConfig.Wrap(err, "loading kubeconfig %s", explicit)
		}

		return rc, nil
	}

	rc, err := inCluster()
	if err == nil {
		return rc, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, ErrConfig.Wrap(err, "loading the in-cluster config")
	}

	if fallback == "" {
		return nil, ErrConfig.New("not in a cluster and no kubeconfig (set AGENTMGR_KUBECONFIG)")
	}

	rc, err = clientcmd.BuildConfigFromFlags("", fallback)
	if err != nil {
		return nil, ErrConfig.Wrap(err, "loading kubeconfig %s (set AGENTMGR_KUBECONFIG)", fallback)
	}

	return rc, nil
}

func defaultKubeconfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".kube", "config")
}

// Client is one pod's transport.Transport.
type Client struct {
	dialer *Dialer
	host   *config.Host
}

var _ transport.Transport = (*Client)(nil)

// Run executes cmd through the pod's /bin/sh.
//
// A non-zero exit lands in Result.ExitCode rather than in error, exactly as in
// sshx: callers tell "tmux says no such session" (exit 1) from "the API
// server is unreachable" (error).
func (c *Client) Run(ctx context.Context, cmd string) (transport.Result, error) {
	return c.RunStdin(ctx, cmd, nil)
}

// RunStdin is Run with bytes piped to the command's stdin.
//
// The command is the agent-manager command, never `claude` itself: the agent
// lives under the pod's tmux server, so this stream ending — or the manager
// being rolled — kills nothing (SPEC.md §5.5).
func (c *Client) RunStdin(ctx context.Context, cmd string, stdin io.Reader) (transport.Result, error) {
	var result transport.Result

	exec, err := c.executor(transport.WrapEnv(c.host.Env, cmd), stdin != nil)
	if err != nil {
		return result, err
	}

	var stdout, stderr bytes.Buffer

	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ErrExec.Wrap(ctxErr, "running on %s: %s", c.host.Name, transport.FirstLine(cmd))
	}

	if streamErr == nil {
		return result, nil
	}

	var exitErr utilexec.ExitError
	if errors.As(streamErr, &exitErr) {
		result.ExitCode = exitErr.ExitStatus()
		return result, nil
	}

	return result, ErrExec.Wrap(streamErr, "running on %s: %s", c.host.Name, transport.FirstLine(cmd))
}

// executor builds the exec stream: WebSocket first (the server default since
// 1.30), SPDY when the upgrade is refused — what kubectl itself does.
func (c *Client) executor(cmd string, withStdin bool) (remotecommand.Executor, error) {
	req := c.dialer.core.Post().
		Resource("pods").
		Namespace(c.host.Namespace).
		Name(c.host.Pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: c.host.Container,
			Command:   []string{"/bin/sh", "-c", cmd},
			Stdin:     withStdin,
			Stdout:    true,
			Stderr:    true,
		}, runtime.NewParameterCodec(coreScheme))

	ws, err := remotecommand.NewWebSocketExecutor(c.dialer.rest, "GET", req.URL().String())
	if err != nil {
		return nil, ErrExec.Wrap(err, "websocket executor for %s", c.host.Name)
	}

	spdy, err := remotecommand.NewSPDYExecutor(c.dialer.rest, "POST", req.URL())
	if err != nil {
		return nil, ErrExec.Wrap(err, "spdy executor for %s", c.host.Name)
	}

	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, ErrExec.Wrap(err, "exec for %s", c.host.Name)
	}

	return exec, nil
}

// WriteFile writes r to path with the given mode, the content going down the
// stream's stdin (transport.WriteFileCommand) — same bytes as over SSH.
func (c *Client) WriteFile(ctx context.Context, filePath string, mode fs.FileMode, r io.Reader) error {
	result, err := c.RunStdin(ctx, transport.WriteFileCommand(filePath, mode), r)
	if err != nil {
		return err
	}

	if !result.OK() {
		return ErrExec.New("writing %s on %s failed (exit %d): %s",
			filePath, c.host.Name, result.ExitCode, result.Err())
	}

	return nil
}

// Close is a no-op: there is no connection behind a Client.
func (c *Client) Close() error { return nil }
