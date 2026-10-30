package sshx

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/aldrinleal/agent-manager-public/config"
)

// hostKeys wraps a known_hosts file. Verification is strict by default;
// InsecureHostKey is an explicit, loudly-logged opt-in, never the fallback
// when something goes wrong (SPEC.md §16.8).
type hostKeys struct {
	path     string
	insecure bool

	mu     sync.Mutex
	verify ssh.HostKeyCallback
}

func newHostKeys(cfg *config.Config) (*hostKeys, error) {
	hk := &hostKeys{path: cfg.KnownHostsPath, insecure: cfg.InsecureHostKey}

	if hk.insecure {
		return hk, nil
	}

	if err := hk.reload(); err != nil {
		return nil, err
	}

	return hk, nil
}

// reload re-reads known_hosts, creating it empty if absent. Called again
// after Trust appends a line, so a freshly trusted host verifies immediately.
func (hk *hostKeys) reload() error {
	hk.mu.Lock()
	defer hk.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(hk.path), 0o700); err != nil {
		return ErrHostKey.Wrap(err, "creating directory for %s", hk.path)
	}

	if _, err := os.Stat(hk.path); errors.Is(err, os.ErrNotExist) {
		f, cErr := os.OpenFile(hk.path, os.O_CREATE|os.O_WRONLY, 0o600)
		if cErr != nil {
			return ErrHostKey.Wrap(cErr, "creating %s", hk.path)
		}
		_ = f.Close()
	}

	verify, err := knownhosts.New(hk.path)
	if err != nil {
		return ErrHostKey.Wrap(err, "reading %s", hk.path)
	}

	hk.verify = verify

	return nil
}

// callback returns the ssh.HostKeyCallback for a named host, turning
// knownhosts' terse errors into ones that say what to do next.
func (hk *hostKeys) callback(hostName string) ssh.HostKeyCallback {
	if hk.insecure {
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			log.Warnf("sshx: AGENTMGR_SSH_INSECURE_HOST_KEY is set — accepting %s (%s) unverified, fingerprint %s",
				hostName, hostname, ssh.FingerprintSHA256(key))
			return nil
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		hk.mu.Lock()
		verify := hk.verify
		hk.mu.Unlock()

		err := verify(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) == 0 {
				return ErrHostKey.New(
					"host %q (%s) is not in %s — fingerprint is %s; run `agentmgr hosts trust %s` to accept it",
					hostName, hostname, hk.path, ssh.FingerprintSHA256(key), hostName)
			}

			return ErrHostKey.New(
				"HOST KEY MISMATCH for %q (%s): offered %s, but %s has a different key on line %d. "+
					"Someone may be intercepting this connection, or the host was rebuilt — resolve it by hand",
				hostName, hostname, ssh.FingerprintSHA256(key), hk.path, keyErr.Want[0].Line)
		}

		return ErrHostKey.Wrap(err, "verifying host key for %q (%s)", hostName, hostname)
	}
}

// ScanResult is what Scan learned about a host's key, for the operator to
// confirm before it is trusted.
type ScanResult struct {
	HostName    string `json:"host"`
	Address     string `json:"address"`
	KeyType     string `json:"key_type"`
	Fingerprint string `json:"fingerprint"`
	Line        string `json:"known_hosts_line"`

	key ssh.PublicKey
}

// Scan dials a host purely to collect its public key, without verifying it
// and without authenticating. The handshake is aborted as soon as the key is
// in hand, so this never runs anything on the far side.
func (p *Pool) Scan(hostName string) (*ScanResult, error) {
	host, err := p.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	var captured ssh.PublicKey

	// Abort the handshake once we have the key: authentication is irrelevant
	// here, and refusing it keeps Scan side-effect free.
	sentinel := errors.New("host key captured")

	cfg := &ssh.ClientConfig{
		User:    host.User,
		Auth:    []ssh.AuthMethod{},
		Timeout: p.cfg.SSHTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return sentinel
		},
	}

	conn, err := p.dialTCP(host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	c, chans, reqs, hErr := ssh.NewClientConn(conn, host.Addr, cfg)
	if hErr == nil {
		client := ssh.NewClient(c, chans, reqs)
		_ = client.Close()
	}

	if captured == nil {
		return nil, ErrHostKey.Wrap(hErr, "host %q did not offer a host key", hostName)
	}

	addr := host.Addr
	hostLine := knownhosts.Normalize(addr)

	return &ScanResult{
		HostName:    hostName,
		Address:     addr,
		KeyType:     captured.Type(),
		Fingerprint: ssh.FingerprintSHA256(captured),
		Line:        knownhosts.Line([]string{hostLine}, captured),
		key:         captured,
	}, nil
}

// Trust appends a scanned host key to known_hosts and reloads verification.
func (p *Pool) Trust(scan *ScanResult) error {
	if scan == nil || scan.key == nil {
		return ErrHostKey.New("nothing to trust: scan produced no key")
	}

	f, err := os.OpenFile(p.hostKeys.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrHostKey.Wrap(err, "opening %s for append", p.hostKeys.path)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, scan.Line); err != nil {
		return ErrHostKey.Wrap(err, "appending to %s", p.hostKeys.path)
	}

	if err := f.Close(); err != nil {
		return ErrHostKey.Wrap(err, "closing %s", p.hostKeys.path)
	}

	return p.hostKeys.reload()
}

// probeKey is a public key that can never match anything. Offering it to the
// known_hosts callback makes it report, via KeyError.Want, exactly which keys
// are pinned for a host — see algorithms.
type probeKey struct{}

func (probeKey) Type() string                        { return "agentmgr-probe-v1" }
func (probeKey) Marshal() []byte                     { return []byte("agentmgr-probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe key cannot verify") }

type probeAddr struct{}

func (probeAddr) Network() string { return "tcp" }
func (probeAddr) String() string  { return "0.0.0.0:0" }

// algorithms returns the host key algorithms pinned for addr in known_hosts.
//
// Without this, x/crypto/ssh negotiates by its own preference order, which is
// not OpenSSH's: a host whose known_hosts entry is ed25519 can end up offering
// ecdsa, and a stale ecdsa line then reads as a key mismatch even though
// `ssh` to the same host works. Constraining negotiation to what is actually
// pinned is what OpenSSH does, and it makes the two agree.
//
// An empty result means "nothing pinned" — the caller leaves the default
// order alone, and the callback reports the host as unknown, which is the
// right message for a host that has never been trusted.
func (hk *hostKeys) algorithms(addr string) []string {
	if hk.insecure {
		return nil
	}

	hk.mu.Lock()
	verify := hk.verify
	hk.mu.Unlock()

	if verify == nil {
		return nil
	}

	var keyErr *knownhosts.KeyError

	err := verify(addr, probeAddr{}, probeKey{})
	if !errors.As(err, &keyErr) || len(keyErr.Want) == 0 {
		return nil
	}

	seen := map[string]bool{}

	var algos []string

	add := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			algos = append(algos, a)
		}
	}

	for _, want := range keyErr.Want {
		keyType := want.Key.Type()

		// A pinned ssh-rsa key is equally valid under the SHA-2 signature
		// algorithms, and those are what a modern sshd prefers. Offering only
		// "ssh-rsa" would push the connection onto SHA-1, which many servers
		// now refuse outright.
		if keyType == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
		}

		add(keyType)
	}

	return algos
}
