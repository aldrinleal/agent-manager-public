package kubectlx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/aldrinleal/agent-manager-public/config"
)

const kubeconfigYAML = `
apiVersion: v1
kind: Config
clusters:
  - name: home
    cluster: {server: "https://%s:6443"}
contexts:
  - name: home
    context: {cluster: home, user: me}
current-context: home
users:
  - name: me
    user: {token: not-a-real-token}
`

func kubeconfig(t *testing.T, server string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := []byte(fmt.Sprintf(kubeconfigYAML, server))
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func inCluster(host string) func() (*rest.Config, error) {
	return func() (*rest.Config, error) { return &rest.Config{Host: host}, nil }
}

func notInCluster() (*rest.Config, error) { return nil, rest.ErrNotInCluster }

func TestRestConfigOrder(t *testing.T) {
	explicit, fallback := kubeconfig(t, "explicit"), kubeconfig(t, "fallback")

	cases := []struct {
		name      string
		explicit  string
		inCluster func() (*rest.Config, error)
		fallback  string
		wantHost  string
	}{
		{"AGENTMGR_KUBECONFIG wins, even in a pod", explicit, inCluster("https://pod"), fallback, "https://explicit:6443"},
		{"in the pod", "", inCluster("https://pod"), fallback, "https://pod"},
		{"on a workstation", "", notInCluster, fallback, "https://fallback:6443"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, err := restConfig(c.explicit, c.inCluster, c.fallback)
			if err != nil {
				t.Fatal(err)
			}
			if rc.Host != c.wantHost {
				t.Errorf("host = %q, want %q", rc.Host, c.wantHost)
			}
		})
	}
}

// A named kubeconfig that is broken must fail, not quietly reach another
// cluster through the in-cluster config.
func TestRestConfigNeverFallsThroughAnExplicitKubeconfig(t *testing.T) {
	if _, err := restConfig("/nonexistent/kubeconfig", inCluster("https://pod"), ""); err == nil {
		t.Fatal("expected an error for a missing explicit kubeconfig")
	}
}

func TestRestConfigBrokenInClusterIsAnError(t *testing.T) {
	broken := func() (*rest.Config, error) { return nil, errors.New("token unreadable") }

	if _, err := restConfig("", broken, kubeconfig(t, "fallback")); err == nil {
		t.Fatal("a pod whose service account is broken must not fall back to ~/.kube/config")
	}
}

func TestDialRefusesAnSSHHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte("hosts:\n  - name: a\n    address: u@a\n    paths: [{name: p, dir: /p}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := config.LoadInventory(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := NewDialer(&config.Config{}, inv).Dial("a"); err == nil {
		t.Fatal("an ssh host must not be dialed through pods/exec")
	}
}
