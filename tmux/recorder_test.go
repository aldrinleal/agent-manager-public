package tmux

import (
	"context"
	"io"
	"io/fs"

	"github.com/aldrinleal/agent-manager-public/transport"
)

// recorder captures the command string a tmux helper builds.
type recorderTransport struct{ into *string }

func recorder(into *string) transport.Transport { return recorderTransport{into} }

func (r recorderTransport) Run(_ context.Context, cmd string) (transport.Result, error) {
	*r.into = cmd

	return transport.Result{}, nil
}

func (r recorderTransport) RunStdin(ctx context.Context, cmd string, _ io.Reader) (transport.Result, error) {
	return r.Run(ctx, cmd)
}

func (recorderTransport) WriteFile(context.Context, string, fs.FileMode, io.Reader) error { return nil }
func (recorderTransport) Close() error                                                    { return nil }
