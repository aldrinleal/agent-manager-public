package transport

import (
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/aldrinleal/agent-manager-public/util"
)

// Dialer hands out a Transport per host. sshx.Pool is one, kubectlx.Dialer
// another; fleet routes each host to one of them by its `transport:` field.
type Dialer interface {
	Dial(host string) (Transport, error)
	Close() error
}

// WrapEnv prefixes a command with the host's configured environment.
//
// It is part of the command string, not of any one transport, which is why it
// lives here. This is where the PATH gotcha is handled: `claude` is typically
// a per-user npm or ~/.local/bin install, and a non-interactive remote command
// gets a shell that reads no rc file. Probes and launches both go through
// here, so `agentmgr check` tests the same PATH a real launch will see
// (SPEC.md §16.4).
func WrapEnv(env map[string]string, cmd string) string {
	if len(env) == 0 {
		return cmd
	}

	var b strings.Builder

	// Sorted for a stable, diffable command string.
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(&b, "export %s=%s; ", k, util.ShellQuote(env[k]))
	}

	b.WriteString(cmd)

	return b.String()
}

// WriteFileCommand is the command a WriteFile runs, with the content arriving
// on its stdin. The content is never part of the command, so it is never
// quoted and never shell-interpreted — that is what lets prompts contain
// quotes, newlines and backticks safely (SPEC.md §5.4, §7.1). The write goes
// to a temp file and is renamed, so a reader never sees a half-written script.
func WriteFileCommand(filePath string, mode fs.FileMode) string {
	dir := path.Dir(filePath)
	tmp := filePath + ".agentmgr.tmp"

	return fmt.Sprintf("mkdir -p %s && cat > %s && chmod %#o %s && mv -f %s %s",
		util.ShellQuote(dir),
		util.ShellQuote(tmp),
		mode.Perm(), util.ShellQuote(tmp),
		util.ShellQuote(tmp), util.ShellQuote(filePath))
}

// FirstLine shortens a command for an error message.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " ..."
	}

	if len(s) > 120 {
		s = s[:120] + "..."
	}

	return s
}
