# scripts

Standalone shell helpers for the machine you're sitting at. They don't use the
`fleet` package, the registry or SSH.

`observer/` is the exception: it is installed on *remote* observer hosts by a
person (see `observer/install.sh`), and `make install` does not touch it.

## `cc-grep` / `cc-kill`

These list the Claude Code sessions running on this machine and send signals to
them. `cc-kill` is a symlink to `cc-grep`; the script checks `$0` to tell which
one was called.

```
$ cc-grep session_01Exa
PID      TTY    TEMPO       STATUS  REPO                     BRANCH  NOME                          SESSÃO
1354842  pts/6  2-02:45:14  idle    github.com/acme-x/specs  master  Fix tax rounding, add a test  session_01Example0Session0Id0001
```

```bash
cc-grep [-a] [-l] [pattern]                                  # list
cc-kill [-y] [-s SIG|-9|--stop|--cont] [--self] pattern      # signal the matches (asks first unless -y)
```

| Flag | Effect |
|---|---|
| `-a` | Also lists helper processes, such as `claude --chrome-native-host` |
| `-l` | Adds the working directory and the full command line |
| `-y` | Skips the confirmation prompt |
| `-s SIG`, `-9` | Signal to send. The default is `TERM` |
| `--stop` / `--cont` | `SIGSTOP` / `SIGCONT`: freezes a session for debugging and resumes it later. A frozen session shows `PARADO` in the STATUS column |
| `--self` | Lets `cc-kill` hit the session it was called from. Without it, that session is skipped |

The pattern is a case-insensitive `grep -E` regex. It is matched against the
repo's `origin` (`github.com/owner/repo`), branch, working directory, session
name, `sessionId`, `bridgeSessionId` (`session_…`) and command line. `cc-kill`
requires a pattern; `.` matches every session.

### Where the data comes from

- **Processes**: `pgrep -x claude` finds the native binary, and
  `node …/@anthropic-ai/claude-code/cli.js` finds npm installs. The working
  directory, TTY and elapsed time come from `/proc` and `ps`.
- **Session metadata**: Claude Code writes `~/.claude/sessions/<pid>.json` for
  each live process. The file holds `name`, `status` (`idle`/`busy`),
  `sessionId` and, when Remote Control is on, `bridgeSessionId`. The SESSÃO
  column shows `bridgeSessionId` when there is one and `sessionId` otherwise.
  This file format is undocumented, so a Claude Code update can change it.
- **The title shown in claude.ai is not stored locally.** If a session is
  renamed remotely (for example, "Release notes for v2"), the JSON file and
  the transcript still have only the local auto title (`ai-title`). Search by the
  `session_…` id, the repo or the PID instead.
- The script walks up from its own PID to the nearest `claude` process. The
  match is shown with `*` and is protected from `cc-kill` unless `--self` is
  passed.

### Install

```bash
make install                      # cc-grep + cc-kill into ~/.local/bin
make install PREFIX=/usr/local    # or anywhere else (BINDIR overrides PREFIX/bin)
make uninstall
```

`cc-grep` is copied and `cc-kill` is a symlink to it next to it, since the
script picks its mode from `$0`. Rerun `make install` after pulling changes.

Requires `bash`, `jq`, `git`, `procps` (`pgrep`, `ps`) and `column`. The script
reads `/proc`, so it runs on Linux only.
