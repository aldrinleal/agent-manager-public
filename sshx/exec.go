package sshx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"

	"golang.org/x/crypto/ssh"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
)

// Client is one host's transport.Transport. It is cheap — the expensive
// *ssh.Client underneath is owned and reused by the Pool.
type Client struct {
	pool   *Pool
	host   *config.Host
	client *ssh.Client
}

var _ transport.Transport = (*Client)(nil)

// Host returns the inventory entry this client talks to.
func (c *Client) Host() *config.Host { return c.host }

// Run executes cmd through the remote /bin/sh.
//
// A non-zero exit lands in Result.ExitCode rather than in error: callers care
// about the difference between "tmux says no such session" (exit 1, expected)
// and "the connection dropped" (error).
func (c *Client) Run(ctx context.Context, cmd string) (transport.Result, error) {
	return c.RunStdin(ctx, cmd, nil)
}

// RunStdin is Run with bytes piped to the command's stdin.
func (c *Client) RunStdin(ctx context.Context, cmd string, stdin io.Reader) (transport.Result, error) {
	var result transport.Result

	session, err := c.client.NewSession()
	if err != nil {
		return result, ErrExec.Wrap(err, "opening session on %s", c.host.Name)
	}
	defer func() { _ = session.Close() }()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if stdin != nil {
		session.Stdin = stdin
	}

	// ssh.Session predates context; closing it is what unblocks Wait.
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-done:
		}
	}()

	runErr := session.Run(transport.WrapEnv(c.host.Env, cmd))

	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ErrExec.Wrap(ctxErr, "running on %s: %s", c.host.Name, transport.FirstLine(cmd))
	}

	if runErr == nil {
		return result, nil
	}

	var exitErr *ssh.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitStatus()
		return result, nil
	}

	var signalErr *ssh.ExitMissingError
	if errors.As(runErr, &signalErr) {
		// Remote died without reporting status — treat as a failure, not a
		// transport error, so callers can still read the output it produced.
		result.ExitCode = -1
		return result, nil
	}

	return result, ErrExec.Wrap(runErr, "running on %s: %s", c.host.Name, transport.FirstLine(cmd))
}

// WriteFile writes r to path with the given mode, the content going down the
// session's stdin (transport.WriteFileCommand).
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

// Close is a no-op: the connection belongs to the Pool, which reuses it.
func (c *Client) Close() error { return nil }
