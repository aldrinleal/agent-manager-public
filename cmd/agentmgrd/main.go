// Command agentmgrd serves the fleet API and the web console.
//
// Structure follows ../api-kit/cmd/api-kit/main.go: everything wired in
// init(), one gin engine, xff-unwrapped, ListenAndServe in main().
package main

import (
	"context"
	"net/http"
	"os"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	xff "github.com/sebest/xff"
	log "github.com/sirupsen/logrus"

	"github.com/aldrinleal/agent-manager-public/api"
	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/console"
	"github.com/aldrinleal/agent-manager-public/fleet"
	"github.com/aldrinleal/agent-manager-public/kubectlx"
	"github.com/aldrinleal/agent-manager-public/registry"
	"github.com/aldrinleal/agent-manager-public/sshx"
)

var (
	engine *gin.Engine
	mgr    *fleet.Fleet
	cfg    *config.Config
)

func init() {
	log.Infof("agentmgrd: initializing")

	cfg = config.Settings()

	// Fail closed. An unset token must never mean "no auth required", and it
	// is worse here than in an ordinary service: this process holds a private
	// key for every host in the inventory.
	if cfg.APIToken == "" {
		log.Fatal("AGENTMGR_API_TOKEN is not set — refusing to start. " +
			"Generate one with: openssl rand -base64 32")
	}

	inv, err := config.LoadInventory(cfg.InventoryPath)
	if err != nil {
		log.Fatalf("loading inventory: %v", err)
	}

	log.Infof("agentmgrd: %d host(s) from %s", len(inv.Hosts), cfg.InventoryPath)

	// No PassphraseFunc: the server never prompts. An encrypted key needs
	// AGENTMGR_SSH_KEY_PASSPHRASE, or startup fails here rather than at the
	// first launch (SPEC.md §5.1).
	pool, err := sshx.NewPool(cfg, inv, nil)
	if err != nil {
		log.Fatalf("ssh: %v", err)
	}

	if cfg.InsecureHostKey {
		log.Warn("AGENTMGR_SSH_INSECURE_HOST_KEY is set — host keys are NOT verified")
	}

	for _, fp := range pool.Keys().Fingerprints() {
		log.Infof("agentmgrd: ssh key %s", fp)
	}

	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("registry: %v", err)
	}

	mgr = fleet.New(cfg, inv, fleet.Dialers{
		SSH:             pool,
		Kubectl:         kubectlx.NewDialer(cfg, inv),
		KeyFingerprints: pool.Keys().Fingerprints(),
	}, reg)
	mgr.StartReconciler(context.Background())
	mgr.StartHistoryCollector(context.Background())

	engine = gin.Default()
	engine.RedirectTrailingSlash = false

	corsConfig := cors.DefaultConfig()
	corsConfig.AllowAllOrigins = true
	corsConfig.AddAllowHeaders("Authorization")
	engine.Use(cors.New(corsConfig))

	okFunc := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }

	engine.GET("/healthz", okFunc)

	console.Mount("/", engine)

	api.NewFleetGroup("/api/fleet/v1", engine, mgr, api.RequireToken(cfg.APIToken))
}

func main() {
	defer func() { _ = mgr.Close() }()

	endpoint := ":" + cfg.Port

	xffmw, err := xff.Default()
	if err != nil {
		log.Fatalf("xff: %v", err)
	}

	log.Infof("agentmgrd: listening on %s", endpoint)

	if err := http.ListenAndServe(endpoint, xffmw.Handler(engine)); err != nil {
		log.Errorf("agentmgrd: %v", err)

		os.Exit(1)
	}
}
