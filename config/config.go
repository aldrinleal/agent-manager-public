// Package config holds the two kinds of configuration agent-manager has:
// settings (env vars, secrets, deployment knobs — this file) and the
// inventory of hosts and paths (inventory.go).
//
// Settings follow ../notes-svc's pattern: one struct, one package-level
// instance, loaded once, read everywhere as config.Settings.
package config

import (
	"os"
	"sync"
	"time"

	"github.com/aldrinleal/agent-manager-public/util"
)

type Config struct {
	// InventoryPath is the hosts.yaml describing hosts and their allowed paths.
	InventoryPath string

	// DBPath is the sqlite file backing registry/ (sessions + host_checks).
	DBPath string

	// APIToken is required on every /api/fleet/v1 request. Empty means the
	// server refuses to start — an unset token must never mean "no auth".
	APIToken string

	// Port is the gin bind port for cmd/agentmgrd.
	Port string

	// --- SSH ---------------------------------------------------------------

	// SSHKey is the private key's PEM *contents*. Wins over SSHKeyFile; this
	// is the path a k8s Secret takes, so the key never touches disk.
	SSHKey string

	// SSHKeyFile is the default identity file, used when a host does not name
	// its own and SSHKey is empty.
	SSHKeyFile string

	// SSHKeyPassphrase decrypts an encrypted private key. The CLI may prompt
	// for this interactively; the server never does.
	SSHKeyPassphrase string

	// SSHUseAgent also offers identities from $SSH_AUTH_SOCK.
	SSHUseAgent bool

	// KnownHostsPath backs strict host key verification.
	KnownHostsPath string

	// InsecureHostKey disables host key verification entirely. Logs a warning
	// on every dial. Exists for a first run, not for production.
	InsecureHostKey bool

	SSHTimeout  time.Duration
	ExecTimeout time.Duration

	// Kubeconfig is the kubeconfig for transport: kubectl hosts. Empty means
	// the in-cluster service account (the pod), then ~/.kube/config (the CLI).
	Kubeconfig string

	// --- runner / reconcile -------------------------------------------------

	URLScrapeTimeout  time.Duration
	ReconcileInterval time.Duration
	StaleAfter        time.Duration
	MaxConcurrent     int

	// AuthLoginTimeout is how long a started `claude auth login` session is
	// kept waiting for its code before being killed.
	AuthLoginTimeout time.Duration

	// --- history (history/, fleet/history.go) -------------------------------

	// HistoryPullInterval is how often every host's ~/.claude/history.jsonl is
	// read; 0 disables the background collector (pull on demand still works).
	HistoryPullInterval time.Duration

	// HistoryStorePrompts keeps the prompt text (clipped) in the registry. It is
	// the user's own words, held on a PVC behind the API token, and the work log
	// needs it. false stores only project/session/time and the work log degrades
	// to counts.
	HistoryStorePrompts bool

	// HistoryExclude are substrings of project paths left out of every history
	// report.
	HistoryExclude string

	// TZ is the IANA zone the work log's day boundaries use.
	TZ string
}

var (
	once     sync.Once
	settings Config
)

// Settings returns the process-wide configuration, loading it from the
// environment on first call.
func Settings() *Config {
	once.Do(func() { settings = load() })
	return &settings
}

func load() Config {
	return Config{
		InventoryPath: util.ExpandHome(util.EnvIf("AGENTMGR_INVENTORY", "hosts.yaml")),
		DBPath:        util.ExpandHome(util.EnvIf("AGENTMGR_DB_PATH", "agentmgr.db")),
		APIToken:      os.Getenv("AGENTMGR_API_TOKEN"),
		Port:          util.EnvIf("AGENTMGR_PORT", "8000"),

		SSHKey:           os.Getenv("AGENTMGR_SSH_KEY"),
		SSHKeyFile:       util.ExpandHome(util.EnvIf("AGENTMGR_SSH_KEY_FILE", "~/.ssh/id_ed25519")),
		SSHKeyPassphrase: os.Getenv("AGENTMGR_SSH_KEY_PASSPHRASE"),
		SSHUseAgent:      util.EnvBool("AGENTMGR_SSH_USE_AGENT", true),
		KnownHostsPath:   util.ExpandHome(util.EnvIf("AGENTMGR_KNOWN_HOSTS", "~/.ssh/known_hosts")),
		InsecureHostKey:  util.EnvBool("AGENTMGR_SSH_INSECURE_HOST_KEY", false),
		SSHTimeout:       util.EnvSeconds("AGENTMGR_SSH_TIMEOUT_S", 20),
		ExecTimeout:      util.EnvSeconds("AGENTMGR_EXEC_TIMEOUT_S", 60),
		Kubeconfig:       util.ExpandHome(os.Getenv("AGENTMGR_KUBECONFIG")),

		URLScrapeTimeout:  util.EnvSeconds("AGENTMGR_URL_SCRAPE_TIMEOUT_S", 120),
		ReconcileInterval: util.EnvSeconds("AGENTMGR_RECONCILE_INTERVAL_S", 30),
		StaleAfter:        util.EnvSeconds("AGENTMGR_STALE_AFTER_S", 1800),
		MaxConcurrent:     util.EnvInt("AGENTMGR_MAX_CONCURRENT_LAUNCHES", 4),
		AuthLoginTimeout:  util.EnvSeconds("AGENTMGR_AUTH_LOGIN_TIMEOUT_S", 600),

		HistoryPullInterval: util.EnvSeconds("AGENTMGR_HISTORY_PULL_INTERVAL_S", 43200),
		HistoryStorePrompts: util.EnvBool("AGENTMGR_HISTORY_STORE_PROMPTS", true),
		HistoryExclude:      util.EnvIf("AGENTMGR_HISTORY_EXCLUDE", "scratch,/.openclaw,/.claude"),
		TZ:                  util.EnvIf("AGENTMGR_TZ", "UTC"),
	}
}
