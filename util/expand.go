package util

import (
	"os"
	"path/filepath"
	"strings"
)

// ExpandHome replaces a leading "~/" with the current user's home directory.
// Paths in hosts.yaml and the AGENTMGR_* env vars are written the way a human
// would write them; this is where that becomes a real path.
func ExpandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}

	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p // ~user/... is not supported; leave it alone
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}

	if p == "~" {
		return home
	}

	return filepath.Join(home, p[2:])
}
