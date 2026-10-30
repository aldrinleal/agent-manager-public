// Package console serves the fleet view: one embedded HTML page, vanilla JS,
// no build step and no npm.
package console

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed index.html
var indexHTML []byte

// Mount serves the console at parentPath.
//
// Unauthenticated on purpose: the page itself carries no data. Every fetch it
// makes goes to /api/fleet/v1 with the bearer token the operator pastes in,
// which is the thing actually being protected.
func Mount(parentPath string, engine *gin.Engine) {
	engine.GET(parentPath, func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
}
