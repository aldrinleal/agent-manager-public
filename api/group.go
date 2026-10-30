// Package api exposes fleet over HTTP, so OpenClaw / Open WebUI / anything
// else can dispatch without importing Go.
//
// Route registration follows ../api-kit: one NewXGroup(prefix, engine, mw...)
// per feature package.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/aldrinleal/agent-manager-public/fleet"
)

// NewFleetGroup mounts the fleet API under parentPath.
func NewFleetGroup(parentPath string, engine *gin.Engine, f *fleet.Fleet, middleware ...gin.HandlerFunc) {
	h := &handler{fleet: f}

	g := engine.Group(parentPath)
	g.Use(middleware...)

	g.GET("/hosts", h.listHosts)
	g.POST("/hosts/:host/check", h.checkHost)
	g.GET("/hosts/:host/auth", h.getAuth)
	g.POST("/hosts/:host/auth/login", h.authLogin)
	g.POST("/hosts/:host/auth/code", h.authCode)
	g.DELETE("/hosts/:host/auth/login", h.authCancel)

	g.GET("/paths", h.listPaths)
	g.GET("/sessions", h.listSessions)
	g.POST("/sessions", h.launch)
	g.POST("/sessions/plan", h.planLaunch)
	g.GET("/sessions/:id", h.getSession)
	g.GET("/sessions/:id/log", h.getLog)
	g.GET("/sessions/:id/attach", h.getAttach)
	g.GET("/sessions/:id/diff", h.getDiff)
	g.POST("/sessions/:id/send", h.send)
	g.POST("/sessions/:id/push/plan", h.planPush)
	g.POST("/sessions/:id/push", h.push)
	g.DELETE("/sessions/:id", h.kill)

	g.GET("/history/projects", h.historyProjects)
	g.GET("/history/proposals", h.historyProposals)
	g.GET("/history/worklog", h.historyWorklog)
	g.GET("/history/pulls", h.historyPulls)
	g.POST("/history/pull", h.historyPull)

	g.POST("/refresh", h.refresh)
	g.POST("/refresh/plan", h.planRefresh)
}

// RequireToken is the bearer-token middleware.
//
// It fails closed: an empty token is a configuration error, never "no auth
// required". cmd/agentmgrd refuses to start without one, and this is the
// second line of that same defence.
func RequireToken(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusInternalServerError,
				gin.H{"error": "AGENTMGR_API_TOKEN is not set; refusing every request"})

			return
		}

		if c.GetHeader("Authorization") != "Bearer "+token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})

			return
		}

		c.Next()
	}
}
