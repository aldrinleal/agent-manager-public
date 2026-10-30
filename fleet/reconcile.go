package fleet

import (
	"context"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/runner"
	"github.com/aldrinleal/agent-manager-public/tmux"
	"github.com/aldrinleal/agent-manager-public/util"
)

// sweepSep splits the pane listing from the log stat output inside the one
// compound command a sweep sends.
const (
	sweepSep    = "@@AGENTMGR-LOGS@@"
	screensSep  = "@@AGENTMGR-SCREENS@@"
	screenMark  = "@@SCR "
	screenLines = 40
)

// StartReconciler runs Reconcile on a ticker until ctx or Close.
func (f *Fleet) StartReconciler(ctx context.Context) {
	if f.cfg.ReconcileInterval <= 0 {
		log.Info("fleet: reconciler disabled (AGENTMGR_RECONCILE_INTERVAL_S=0)")

		return
	}

	go func() {
		ticker := time.NewTicker(f.cfg.ReconcileInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stop:
				return
			case <-ticker.C:
				if err := f.Reconcile(ctx); err != nil {
					log.Warnf("fleet: reconcile: %v", err)
				}
			}
		}
	}()

	log.Infof("fleet: reconciling every %s", f.cfg.ReconcileInterval)
}

// Reconcile updates the state of every active session.
//
// Liveness is established independently of the control URL — a session whose
// URL never appeared, or whose Remote Control connection dropped, is still
// correctly reported as running (need.md).
func (f *Fleet) Reconcile(ctx context.Context) error {
	active, err := f.reg.List(ctx, registry.Filter{Active: true})
	if err != nil {
		return err
	}

	if len(active) == 0 {
		return nil
	}

	byHost := map[string][]*registry.Session{}
	for _, s := range active {
		byHost[s.Host] = append(byHost[s.Host], s)
	}

	var firstErr error

	for host, sessions := range byHost {
		if err := f.reconcileHost(ctx, host, sessions); err != nil {
			log.Warnf("fleet: reconciling %s: %v", host, err)

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// reconcileHost sweeps one host in a single round trip.
//
// One `tmux list-panes -a` plus one `stat` over every log file, rather than a
// has-session per session. With thirty sessions that is the difference
// between one command and sixty.
func (f *Fleet) reconcileHost(ctx context.Context, host string, sessions []*registry.Session) error {
	t, err := f.transportFor(host)
	if err != nil {
		return err
	}

	logs := make([]string, 0, len(sessions))
	for _, s := range sessions {
		logs = append(logs, s.LogPath)
	}

	// One command, one round trip per host: the pane list, the log stats, and
	// the visible screen of each session (to tell "waiting for you" from
	// "thinking"). The visible region only — no scrollback — because a prompt
	// that has been answered must stop counting the moment it leaves the screen.
	var capture strings.Builder

	for _, s := range sessions {
		capture.WriteString("echo " + util.ShellQuote(screenMark+s.TmuxSession) + "; ")
		capture.WriteString("tmux capture-pane -p -J -t " + util.ShellQuote("="+s.TmuxSession+":") +
			" 2>/dev/null | tail -n " + strconv.Itoa(screenLines) + "; ")
	}

	cmd := tmux.ListPanesCmd() +
		"; echo " + util.ShellQuote(sweepSep) +
		"; stat -c '%n|%Y|%s' " + util.JoinArgs(logs...) + " 2>/dev/null || true" +
		"; echo " + util.ShellQuote(screensSep) + "; " + capture.String()

	execCtx, cancel := context.WithTimeout(ctx, f.cfg.ExecTimeout)
	defer cancel()

	res, err := t.Run(execCtx, cmd)
	if err != nil {
		return err
	}

	paneOut, rest := splitSweep(res.Stdout)
	logOut, screenOut := splitScreens(rest)

	panes := tmux.ParsePanes(paneOut)
	stats := parseLogStats(logOut)
	screens := parseScreens(screenOut)

	now := time.Now().UTC()

	for _, s := range sessions {
		before := *s

		applyPaneState(s, panes, stats, screens, now, f.cfg.StaleAfter, f.cfg.URLScrapeTimeout)

		if before.State == s.State && before.PanePID == s.PanePID && s.LastSeenAt != nil {
			// Still touch last_seen_at so an operator can tell a quiet session
			// from an unreachable host.
			s.LastSeenAt = &now
		}

		if err := f.reg.Update(ctx, s); err != nil {
			return err
		}

		if before.State != s.State {
			log.Infof("fleet: %s/%s %s -> %s", s.Host, s.TaskID, before.State, s.State)
		}
	}

	return nil
}

// logStat is a log file's last-modified time and size.
type logStat struct {
	ModTime time.Time
	Size    int64
}

// applyPaneState decides a session's new state from the sweep output.
func applyPaneState(
	s *registry.Session,
	panes map[string]tmux.Pane,
	stats map[string]logStat,
	screens map[string]string,
	now time.Time,
	staleAfter time.Duration,
	scrapeWindow time.Duration,
) {
	s.LastSeenAt = &now

	pane, found := panes[s.TmuxSession]

	if !found {
		// The session is gone. remain-on-exit means a merely finished agent
		// would still be listed, so this is a kill, a reboot, or someone
		// tidying up by hand.
		s.State = registry.StateStopped
		if s.EndedAt == nil {
			s.EndedAt = &now
		}
		if s.Note == "" {
			s.Note = "tmux session no longer exists"
		}

		return
	}

	s.PanePID = pane.PID

	if pane.Dead {
		s.State = registry.StateStopped
		if s.EndedAt == nil {
			s.EndedAt = &now
		}

		if pane.ExitStatus >= 0 {
			status := pane.ExitStatus
			s.ExitStatus = &status

			if s.Note == "" {
				s.Note = "agent exited with status " + strconv.Itoa(status)
			}
		} else if s.Note == "" {
			s.Note = "agent exited"
		}

		return
	}

	// Alive. A prompt waiting for a human beats everything below it — including
	// "starting": a session stopped at the folder-trust prompt never prints a
	// URL, and is exactly the one that needs someone to notice.
	if screen, ok := screens[s.TmuxSession]; ok {
		if b, err := runner.Get(s.Runner); err == nil {
			if blocker, can := b.(runner.Blocker); can {
				if blocked, why := blocker.Blocked(screen); blocked {
					s.State = registry.StateBlocked
					s.Note = "waiting for you: " + why

					return
				}
			}
		}
	}

	// No longer blocked: drop the state and its note and judge from scratch.
	if s.State == registry.StateBlocked {
		s.State, s.Note = registry.StateRunning, ""
	}

	// Hold "starting" while the URL scrape still has time to run, so a
	// freshly launched session does not flicker to running and back.
	if s.State == registry.StateStarting && s.RemoteURL == "" && now.Sub(s.CreatedAt) < scrapeWindow {
		return
	}

	if stat, ok := stats[s.LogPath]; ok && staleAfter > 0 && now.Sub(stat.ModTime) > staleAfter {
		s.State = registry.StateStale

		return
	}

	s.State = registry.StateRunning
	s.Note = ""
}

// splitSweep divides the compound command's output at the marker.
func splitSweep(out string) (panes, logs string) {
	idx := strings.Index(out, sweepSep)
	if idx < 0 {
		return out, ""
	}

	return out[:idx], out[idx+len(sweepSep):]
}

// splitScreens divides the log-stat half of a sweep from the screens half.
func splitScreens(out string) (stats, screens string) {
	idx := strings.Index(out, screensSep)
	if idx < 0 {
		return out, ""
	}

	return out[:idx], out[idx+len(screensSep):]
}

// parseScreens reads the per-session screen blocks, keyed by tmux session.
func parseScreens(out string) map[string]string {
	screens := map[string]string{}

	var (
		name string
		buf  []string
	)

	flush := func() {
		if name != "" {
			screens[name] = strings.Join(buf, "\n")
		}
	}

	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, screenMark); ok {
			flush()

			name, buf = strings.TrimSpace(rest), nil

			continue
		}

		buf = append(buf, util.StripANSI(line))
	}

	flush()

	return screens
}

// parseLogStats reads `stat -c '%n|%Y|%s'` output.
func parseLogStats(out string) map[string]logStat {
	stats := map[string]logStat{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			continue
		}

		epoch, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}

		size, _ := strconv.ParseInt(parts[2], 10, 64)

		stats[parts[0]] = logStat{ModTime: time.Unix(epoch, 0).UTC(), Size: size}
	}

	return stats
}
