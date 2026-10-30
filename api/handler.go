package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/joomcode/errorx"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/fleet"
	"github.com/aldrinleal/agent-manager-public/gitx"
	"github.com/aldrinleal/agent-manager-public/registry"
)

type handler struct {
	fleet *fleet.Fleet
}

func (h *handler) listHosts(c *gin.Context) {
	hosts, err := h.fleet.Hosts(c.Request.Context())
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{
		"hosts": hosts,
		// Fingerprints, never the key itself (SPEC.md §5.1).
		"key_fingerprints": h.fleet.KeyFingerprints(),
		"runners":          []string{"claude-code"},
	})
}

func (h *handler) checkHost(c *gin.Context) {
	check, err := h.fleet.Check(c.Request.Context(), c.Param("host"))
	if err != nil {
		// A failed preflight is a real answer about the host, not a server
		// error — return the detail alongside the reason.
		c.JSON(http.StatusOK, gin.H{"check": check, "error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, gin.H{"check": check})
}

func (h *handler) listSessions(c *gin.Context) {
	filter := registry.Filter{
		Host:     c.Query("host"),
		RepoName: c.Query("repo"),
		TaskID:   c.Query("task_id"),
		State:    registry.State(c.Query("state")),
		Active:   c.Query("active") == "true",
	}

	if limit, err := strconv.Atoi(c.Query("limit")); err == nil {
		filter.Limit = limit
	}

	// Reconciling on every poll would put the console's 5s refresh straight
	// onto every host. Opt in with ?reconcile=true; the background sweep
	// keeps the rows fresh otherwise.
	sessions, err := h.fleet.List(c.Request.Context(), filter, c.Query("reconcile") == "true")
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"sessions": sessions, "count": len(sessions)})
}

func (h *handler) launch(c *gin.Context) {
	var req fleet.LaunchRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	session, err := h.fleet.Launch(c.Request.Context(), req)
	if err != nil {
		if session != nil {
			// A conflict hands back the session that is already running, so the
			// caller can use it instead of retrying blind.
			c.JSON(statusFor(err), gin.H{"error": err.Error(), "session": session})

			return
		}

		fail(c, err)

		return
	}

	// 202: tmux is up, but the control URL is still being scraped.
	c.JSON(http.StatusAccepted, gin.H{"session": session})
}

func (h *handler) getSession(c *gin.Context) {
	session, err := h.fleet.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"session": session})
}

func (h *handler) getLog(c *gin.Context) {
	lines, err := strconv.Atoi(c.DefaultQuery("tail", "200"))
	if err != nil {
		lines = 200
	}

	out, err := h.fleet.Log(c.Request.Context(), c.Param("id"), lines)
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"log": out})
}

func (h *handler) getAttach(c *gin.Context) {
	info, err := h.fleet.Attach(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"attach": info})
}

func (h *handler) send(c *gin.Context) {
	var body struct {
		Text string `json:"text" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if err := h.fleet.Send(c.Request.Context(), c.Param("id"), body.Text); err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"sent": true})
}

func (h *handler) kill(c *gin.Context) {
	session, err := h.fleet.Kill(c.Request.Context(), c.Param("id"), c.Query("remove_worktree") == "true")
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"session": session})
}

func (h *handler) refresh(c *gin.Context) {
	var body struct {
		Host      string          `json:"host" binding:"required"`
		Path      string          `json:"path"`
		Strategy  config.Strategy `json:"strategy"`
		Force     bool            `json:"force"`
		Autostash bool            `json:"autostash"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	opts := gitx.RefreshOpts{Strategy: body.Strategy, Force: body.Force, Autostash: body.Autostash}
	ctx := c.Request.Context()

	// No path means every path on the host.
	if body.Path == "" {
		results, err := h.fleet.RefreshHost(ctx, body.Host, opts)
		if err != nil {
			c.JSON(statusFor(err), gin.H{"results": results, "error": err.Error()})

			return
		}

		c.JSON(http.StatusOK, gin.H{"results": results})

		return
	}

	result, err := h.fleet.Refresh(ctx, body.Host, body.Path, opts)
	if err != nil {
		c.JSON(statusFor(err), gin.H{"result": result, "error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, gin.H{"result": result})
}

// fail maps an error to a status code and a JSON body.
func fail(c *gin.Context, err error) {
	c.JSON(statusFor(err), gin.H{"error": err.Error()})
}

// statusFor translates errorx types into HTTP status codes, so a caller can
// tell "you asked for something that does not exist" from "the host is down".
func statusFor(err error) int {
	switch {
	case errorx.IsOfType(err, registry.ErrNotFound),
		errorx.IsOfType(err, fleet.ErrNotFound),
		errorx.IsOfType(err, config.ErrUnknownHost),
		errorx.IsOfType(err, config.ErrUnknownPath):
		return http.StatusNotFound

	case errorx.IsOfType(err, registry.ErrConflict),
		errorx.IsOfType(err, fleet.ErrConflict),
		errorx.IsOfType(err, gitx.ErrConflict),
		errorx.IsOfType(err, gitx.ErrDirty),
		errorx.IsOfType(err, gitx.ErrInProgress):
		return http.StatusConflict

	case errorx.IsOfType(err, fleet.ErrBadTarget),
		errorx.IsOfType(err, fleet.ErrLaunch):
		return http.StatusBadRequest

	case errorx.IsOfType(err, fleet.ErrPreflight):
		return http.StatusFailedDependency

	default:
		return http.StatusInternalServerError
	}
}

// planLaunch answers "what would POST /sessions do?" without doing any of it.
// Same body as /sessions; always 200 for a well-formed request, with
// `blocked` saying whether the launch itself would be refused.
func (h *handler) planLaunch(c *gin.Context) {
	var req fleet.LaunchRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	plan, err := h.fleet.Plan(c.Request.Context(), req)
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"plan": plan})
}

// planRefresh answers "what would POST /refresh do?" without doing any of it.
func (h *handler) planRefresh(c *gin.Context) {
	var body struct {
		Host      string          `json:"host" binding:"required"`
		Path      string          `json:"path"`
		Strategy  config.Strategy `json:"strategy"`
		Force     bool            `json:"force"`
		Autostash bool            `json:"autostash"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	items, err := h.fleet.PlanRefresh(c.Request.Context(), body.Host, body.Path,
		gitx.RefreshOpts{Strategy: body.Strategy, Force: body.Force, Autostash: body.Autostash})
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"plan": items})
}

// listPaths reports every allowlisted checkout with its git state. One SSH
// round trip per host, cached for 60s; ?fresh=true bypasses the cache.
func (h *handler) listPaths(c *gin.Context) {
	paths, err := h.fleet.PathsStatus(c.Request.Context(), c.Query("host"), c.Query("path"), c.Query("fresh") == "true")
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"paths": paths, "count": len(paths)})
}

// getDiff reports what a session's agent has done: worktree state, commits
// ahead of the base, and a diffstat against the fork point.
func (h *handler) getDiff(c *gin.Context) {
	d, err := h.fleet.Diff(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"diff": d})
}

// planPush answers "what would pushing this session's branch do?".
func (h *handler) planPush(c *gin.Context) {
	plan, err := h.fleet.PlanPush(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"plan": plan})
}

// push sends a session's agent/<task> branch. There is no force option.
func (h *handler) push(c *gin.Context) {
	res, err := h.fleet.Push(c.Request.Context(), c.Param("id"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"push": res})
}

// getAuth reports a host's last known `claude auth status`.
func (h *handler) getAuth(c *gin.Context) {
	check, err := h.fleet.AuthStatus(c.Request.Context(), c.Param("host"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"auth": check})
}

// authLogin starts `claude auth login` on a host and returns the OAuth URL.
// The URL carries client parameters: it is returned here and nowhere else, and
// is not logged.
func (h *handler) authLogin(c *gin.Context) {
	login, err := h.fleet.AuthLoginStart(c.Request.Context(), c.Param("host"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"login": login})
}

// authCode submits the code the browser showed after authorizing.
func (h *handler) authCode(c *gin.Context) {
	var body struct {
		Code string `json:"code" binding:"required"`
	}

	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a code is required"})

		return
	}

	res, err := h.fleet.AuthLoginCode(c.Request.Context(), c.Param("host"), body.Code)
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"result": res})
}

// authCancel abandons a waiting login. Idempotent: nothing waiting is fine.
func (h *handler) authCancel(c *gin.Context) {
	was, err := h.fleet.AuthLoginCancel(c.Request.Context(), c.Param("host"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"cancelled": was})
}

func daysParam(c *gin.Context, def int) int {
	if n, err := strconv.Atoi(c.Query("days")); err == nil && n > 0 {
		return n
	}

	return def
}

// historyProjects ranks every worked-on project by distinct sessions.
func (h *handler) historyProjects(c *gin.Context) {
	projects, err := h.fleet.HistoryProjects(c.Request.Context(), daysParam(c, 90))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"projects": projects, "count": len(projects)})
}

// historyProposals returns allowlist verdicts and the YAML a person can paste.
// It never changes the allowlist.
func (h *handler) historyProposals(c *gin.Context) {
	res, err := h.fleet.HistoryProposals(c.Request.Context(), daysParam(c, 90))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, res)
}

// historyWorklog answers "what did I work on" by day, project and session.
func (h *handler) historyWorklog(c *gin.Context) {
	days, err := h.fleet.HistoryWorklog(c.Request.Context(), c.Query("host"), daysParam(c, 7), c.Query("tz"))
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"days": days})
}

func (h *handler) historyPulls(c *gin.Context) {
	pulls, err := h.fleet.HistoryPulls(c.Request.Context())
	if err != nil {
		fail(c, err)

		return
	}

	c.JSON(http.StatusOK, gin.H{"pulls": pulls})
}

// historyPull reads history now: one host, or every host when none is named.
func (h *handler) historyPull(c *gin.Context) {
	var body struct {
		Host string `json:"host"`
	}

	// An empty body means "every host".
	_ = c.ShouldBindJSON(&body)

	if body.Host != "" {
		p, err := h.fleet.PullHistory(c.Request.Context(), body.Host)
		if err != nil {
			fail(c, err)

			return
		}

		c.JSON(http.StatusOK, gin.H{"pulls": []any{p}})

		return
	}

	c.JSON(http.StatusOK, gin.H{"pulls": h.fleet.PullHistoryAll(c.Request.Context())})
}
