// Package transport defines how agent-manager runs a command on a remote
// machine, without saying how it gets there.
//
// sshx implements it today. A kubectl-exec implementation drops in later with
// no change to tmux/, gitx/, runner/ or fleet/ — none of which import sshx
// directly. That is the whole point of this package existing (SPEC.md §5.4).
package transport

import (
	"context"
	"io"
	"io/fs"
	"strings"
)

// Result is the outcome of one remote command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// OK reports whether the command exited zero.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Out returns trimmed stdout — the common case for probes.
func (r Result) Out() string { return strings.TrimSpace(r.Stdout) }

// Err returns trimmed stderr, falling back to stdout when stderr is empty
// (plenty of tools report failures on stdout).
func (r Result) Err() string {
	if e := strings.TrimSpace(r.Stderr); e != "" {
		return e
	}
	return strings.TrimSpace(r.Stdout)
}

// Transport runs commands somewhere that is not here.
type Transport interface {
	// Run executes cmd through the remote /bin/sh and waits for it. A
	// non-zero exit is reported in Result.ExitCode, not as an error; error is
	// reserved for the transport itself failing.
	Run(ctx context.Context, cmd string) (Result, error)

	// RunStdin is Run with bytes piped to the command's stdin.
	RunStdin(ctx context.Context, cmd string, stdin io.Reader) (Result, error)

	// WriteFile writes r to path with the given mode, creating parent
	// directories. Content goes down the session's stdin, so it is never
	// quoted, never shell-interpreted, and needs no SFTP subsystem.
	WriteFile(ctx context.Context, path string, mode fs.FileMode, r io.Reader) error

	// Close releases the underlying connection.
	Close() error
}
