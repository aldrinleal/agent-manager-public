// Package sshx is the SSH transport: it holds the private key, verifies host
// keys, dials (through a jump host when the inventory says so), and runs
// commands. It implements transport.Transport.
package sshx

import (
	"errors"
	"net"
	"os"
	"sort"
	"sync"

	"github.com/joomcode/errorx"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/aldrinleal/agent-manager-public/config"
)

var Errors = errorx.NewNamespace("sshx")

var (
	ErrKey       = Errors.NewType("key")
	ErrDial      = Errors.NewType("dial")
	ErrHostKey   = Errors.NewType("host_key")
	ErrExec      = Errors.NewType("exec")
	ErrPassphras = Errors.NewType("passphrase")
)

// PassphraseFunc is asked for a passphrase when an encrypted key is found and
// AGENTMGR_SSH_KEY_PASSPHRASE is unset. The CLI wires this to a terminal
// prompt; the server leaves it nil and fails instead (SPEC.md §5.1).
type PassphraseFunc func(keyDescription string) (string, error)

// KeyRing holds every private key agent-manager can authenticate with.
//
// The parsed signers live here for process lifetime. They are never logged,
// never persisted, and never returned by any API route — Fingerprints() is
// the only thing that leaves this struct.
type KeyRing struct {
	cfg        *config.Config
	passphrase PassphraseFunc

	mu      sync.Mutex
	inline  []ssh.Signer            // from AGENTMGR_SSH_KEY
	files   map[string][]ssh.Signer // identity path -> signers
	agentSg []ssh.Signer
	agentOK bool
}

// NewKeyRing loads the inline key and the default identity file eagerly.
//
// Eagerly, for two reasons: a bad key or a wrong passphrase then fails at
// startup rather than on the first dial, and Fingerprints() has something to
// report before anything has been connected to — otherwise `agentmgr hosts`
// and GET /hosts show no key at all until the first connection happens to
// occur. Per-host identity overrides still load on demand.
func NewKeyRing(cfg *config.Config, passphrase PassphraseFunc) (*KeyRing, error) {
	kr := &KeyRing{
		cfg:        cfg,
		passphrase: passphrase,
		files:      map[string][]ssh.Signer{},
	}

	if cfg.SSHKey != "" {
		signer, err := kr.parse([]byte(cfg.SSHKey), "AGENTMGR_SSH_KEY")
		if err != nil {
			return nil, err
		}

		kr.inline = []ssh.Signer{signer}
	}

	if cfg.SSHKeyFile != "" && fileExists(cfg.SSHKeyFile) {
		if _, err := kr.loadFileLocked(cfg.SSHKeyFile); err != nil {
			// Only fatal when it is the sole source; an agent or an inline key
			// can still carry the connection.
			if len(kr.inline) == 0 && !cfg.SSHUseAgent {
				return nil, err
			}

			log.Warnf("sshx: %s could not be loaded (%v); falling back to the agent", cfg.SSHKeyFile, err)
		}
	}

	return kr, nil
}

// AuthMethods returns the authentication methods to offer a host, preferring
// the host's own identity file when it names one.
//
// Order: AGENTMGR_SSH_KEY (the k8s Secret path) > the identity file >
// ssh-agent. At least one must produce a usable signer.
func (kr *KeyRing) AuthMethods(identityFile string) ([]ssh.AuthMethod, error) {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	var signers []ssh.Signer

	signers = append(signers, kr.inline...)

	path := identityFile
	if path == "" {
		path = kr.cfg.SSHKeyFile
	}

	// A missing identity file is only fatal if nothing else works, so it is
	// checked rather than opened — the agent or the inline key may carry it.
	if path != "" && fileExists(path) {
		fileSigners, err := kr.loadFileLocked(path)
		if err != nil {
			return nil, err
		}
		signers = append(signers, fileSigners...)
	}

	if kr.cfg.SSHUseAgent {
		signers = append(signers, kr.loadAgentLocked()...)
	}

	if len(signers) == 0 {
		return nil, ErrKey.New(
			"no usable private key: set AGENTMGR_SSH_KEY, point AGENTMGR_SSH_KEY_FILE at a key (tried %q), or start an ssh-agent",
			path)
	}

	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

// Fingerprints returns the SHA256 fingerprint of every loaded key, sorted and
// deduplicated. This is the only key material that ever leaves the process —
// never the key itself.
func (kr *KeyRing) Fingerprints() []string {
	kr.mu.Lock()
	defer kr.mu.Unlock()

	seen := map[string]bool{}

	out := []string{}

	add := func(signers []ssh.Signer) {
		for _, s := range signers {
			fp := ssh.FingerprintSHA256(s.PublicKey())
			if !seen[fp] {
				seen[fp] = true
				out = append(out, fp)
			}
		}
	}

	add(kr.inline)

	for _, signers := range kr.files {
		add(signers)
	}

	add(kr.agentSg)

	sort.Strings(out)

	return out
}

func (kr *KeyRing) loadFileLocked(path string) ([]ssh.Signer, error) {
	if cached, ok := kr.files[path]; ok {
		return cached, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrKey.Wrap(err, "reading identity %s", path)
	}

	signer, err := kr.parse(raw, path)
	if err != nil {
		return nil, err
	}

	kr.files[path] = []ssh.Signer{signer}

	return kr.files[path], nil
}

// parse handles both plain and passphrase-protected keys.
func (kr *KeyRing) parse(pem []byte, description string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return signer, nil
	}

	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, ErrKey.Wrap(err, "parsing private key %s", description)
	}

	passphrase := kr.cfg.SSHKeyPassphrase

	if passphrase == "" {
		if kr.passphrase == nil {
			return nil, ErrPassphras.New(
				"private key %s is encrypted; set AGENTMGR_SSH_KEY_PASSPHRASE (the server never prompts)",
				description)
		}

		passphrase, err = kr.passphrase(description)
		if err != nil {
			return nil, ErrPassphras.Wrap(err, "reading passphrase for %s", description)
		}
	}

	signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase))
	if err != nil {
		return nil, ErrKey.Wrap(err, "decrypting private key %s", description)
	}

	return signer, nil
}

// loadAgentLocked connects to $SSH_AUTH_SOCK once and caches its signers.
// A missing or broken agent is not an error — it is just one fewer source.
func (kr *KeyRing) loadAgentLocked() []ssh.Signer {
	if kr.agentOK {
		return kr.agentSg
	}
	kr.agentOK = true

	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil
	}

	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		_ = conn.Close()
		return nil
	}

	kr.agentSg = signers

	return kr.agentSg
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
