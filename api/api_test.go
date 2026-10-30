package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

func newEngine(token string) *gin.Engine {
	engine := gin.New()

	g := engine.Group("/api/fleet/v1")
	g.Use(RequireToken(token))
	g.GET("/ping", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	engine.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	return engine
}

func get(engine *gin.Engine, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	return w
}

func TestRequireTokenAcceptsTheRightBearer(t *testing.T) {
	engine := newEngine("s3cret")

	if code := get(engine, "/api/fleet/v1/ping", "Bearer s3cret").Code; code != http.StatusOK {
		t.Errorf("valid token got %d, want 200", code)
	}
}

func TestRequireTokenRejects(t *testing.T) {
	engine := newEngine("s3cret")

	cases := map[string]string{
		"no header":      "",
		"wrong token":    "Bearer wrong",
		"no scheme":      "s3cret",
		"wrong scheme":   "Basic s3cret",
		"case mismatch":  "bearer s3cret",
		"prefix only":    "Bearer s3cre",
		"trailing space": "Bearer s3cret ",
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			if code := get(engine, "/api/fleet/v1/ping", header).Code; code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", code)
			}
		})
	}
}

// An unset token must never mean "no auth required". This process holds a
// private key for every host in the inventory.
func TestRequireTokenFailsClosedWhenUnset(t *testing.T) {
	engine := newEngine("")

	w := get(engine, "/api/fleet/v1/ping", "")

	if w.Code == http.StatusOK {
		t.Fatal("an empty token must not disable auth")
	}

	// Even presenting "Bearer " (which would equal "Bearer "+"") must fail.
	if code := get(engine, "/api/fleet/v1/ping", "Bearer ").Code; code == http.StatusOK {
		t.Fatal("an empty token must not be satisfiable by an empty bearer")
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(body["error"], "AGENTMGR_API_TOKEN") {
		t.Errorf("error should name the missing setting, got %q", body["error"])
	}
}

// /healthz stays open — it is mounted on the root engine, outside the group.
func TestHealthzIsUnauthenticated(t *testing.T) {
	if code := get(newEngine("s3cret"), "/healthz", "").Code; code != http.StatusOK {
		t.Errorf("healthz got %d, want 200", code)
	}
}
