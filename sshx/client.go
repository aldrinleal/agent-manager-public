package sshx

import (
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"

	"github.com/aldrinleal/agent-manager-public/config"
	"github.com/aldrinleal/agent-manager-public/transport"
)

const (
	keepaliveInterval = 30 * time.Second
	maxJumpDepth      = 8
)

// Pool hands out SSH clients, one per host, reused across commands.
//
// This matters more than it looks: the reconcile sweep and every multi-step
// git operation would otherwise open a connection per command and walk
// straight into sshd's MaxStartups (SPEC.md §16.7).
type Pool struct {
	cfg      *config.Config
	inv      *config.Inventory
	keys     *KeyRing
	hostKeys *hostKeys

	mu      sync.Mutex
	clients map[string]*ssh.Client
	stop    chan struct{}
	stopped bool
}

// NewPool builds the pool. The key ring is loaded here, so a bad key or a
// missing known_hosts fails at startup rather than on first use.
func NewPool(cfg *config.Config, inv *config.Inventory, passphrase PassphraseFunc) (*Pool, error) {
	keys, err := NewKeyRing(cfg, passphrase)
	if err != nil {
		return nil, err
	}

	hk, err := newHostKeys(cfg)
	if err != nil {
		return nil, err
	}

	p := &Pool{
		cfg:      cfg,
		inv:      inv,
		keys:     keys,
		hostKeys: hk,
		clients:  map[string]*ssh.Client{},
		stop:     make(chan struct{}),
	}

	go p.keepalive()

	return p, nil
}

// Keys exposes the key ring, for reporting fingerprints.
func (p *Pool) Keys() *KeyRing { return p.keys }

// Inventory returns the inventory this pool dials against.
func (p *Pool) Inventory() *config.Inventory { return p.inv }

// Client returns a live client for hostName, dialing or redialing as needed.
func (p *Pool) Client(hostName string) (*ssh.Client, error) {
	host, err := p.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	existing, ok := p.clients[hostName]
	p.mu.Unlock()

	if ok {
		if alive(existing) {
			return existing, nil
		}

		log.Debugf("sshx: connection to %s went away, redialing", hostName)

		p.mu.Lock()
		if p.clients[hostName] == existing {
			delete(p.clients, hostName)
		}
		p.mu.Unlock()

		_ = existing.Close()
	}

	client, err := p.dial(host, 0)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	// Another goroutine may have dialed the same host meanwhile; keep one.
	if winner, raced := p.clients[hostName]; raced && alive(winner) {
		p.mu.Unlock()
		_ = client.Close()
		return winner, nil
	}
	p.clients[hostName] = client
	p.mu.Unlock()

	return client, nil
}

// Dial returns a transport.Transport for hostName; it is what makes the pool a
// transport.Dialer.
func (p *Pool) Dial(hostName string) (transport.Transport, error) {
	return p.Transport(hostName)
}

// Transport returns the SSH client for hostName.
func (p *Pool) Transport(hostName string) (*Client, error) {
	client, err := p.Client(hostName)
	if err != nil {
		return nil, err
	}

	host, err := p.inv.Host(hostName)
	if err != nil {
		return nil, err
	}

	return &Client{pool: p, host: host, client: client}, nil
}

// dial opens a connection to host, recursing through its jump host first.
//
// The recursion is the whole jump implementation: dial the bastion, open a
// tunnelled TCP connection from there to the target, and run a fresh SSH
// handshake over it. That is exactly `ssh -J bastion -p 2200 user@target`.
func (p *Pool) dial(host *config.Host, depth int) (*ssh.Client, error) {
	if depth > maxJumpDepth {
		return nil, ErrDial.New("jump chain for %q is deeper than %d hops", host.Name, maxJumpDepth)
	}

	auth, err := p.keys.AuthMethods(host.Identity)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            host.User,
		Auth:            auth,
		HostKeyCallback: p.hostKeys.callback(host.Name),
		Timeout:         p.cfg.SSHTimeout,
		// Negotiate only host key algorithms we have pinned, the way OpenSSH
		// does — otherwise a host that `ssh` connects to happily can read as
		// a key mismatch here. See hostKeys.algorithms.
		HostKeyAlgorithms: p.hostKeys.algorithms(host.Addr),
	}

	conn, err := p.dialTCPDepth(host, depth)
	if err != nil {
		return nil, err
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, host.Addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, ErrDial.Wrap(err, "handshaking with %s (%s@%s)", host.Name, host.User, host.Addr)
	}

	return ssh.NewClient(c, chans, reqs), nil
}

func (p *Pool) dialTCP(host *config.Host) (net.Conn, error) {
	return p.dialTCPDepth(host, 0)
}

// dialTCPDepth produces the raw connection to host: straight TCP, or a
// channel opened by the jump host's client.
func (p *Pool) dialTCPDepth(host *config.Host, depth int) (net.Conn, error) {
	if host.Jump == "" {
		conn, err := net.DialTimeout("tcp", host.Addr, p.cfg.SSHTimeout)
		if err != nil {
			return nil, ErrDial.Wrap(err, "connecting to %s (%s)", host.Name, host.Addr)
		}
		return conn, nil
	}

	jumpHost, err := p.inv.Host(host.Jump)
	if err != nil {
		return nil, ErrDial.Wrap(err, "host %q jump", host.Name)
	}

	// Jump hosts are pooled too, so a bastion shared by five targets is dialed
	// once. Reuse the cached client when it is alive.
	p.mu.Lock()
	jumpClient, ok := p.clients[jumpHost.Name]
	p.mu.Unlock()

	if !ok || !alive(jumpClient) {
		if ok {
			_ = jumpClient.Close()
		}

		jumpClient, err = p.dial(jumpHost, depth+1)
		if err != nil {
			return nil, ErrDial.Wrap(err, "reaching jump host %q for %q", jumpHost.Name, host.Name)
		}

		p.mu.Lock()
		p.clients[jumpHost.Name] = jumpClient
		p.mu.Unlock()
	}

	conn, err := jumpClient.Dial("tcp", host.Addr)
	if err != nil {
		return nil, ErrDial.Wrap(err, "tunnelling to %s (%s) through %s", host.Name, host.Addr, jumpHost.Name)
	}

	return conn, nil
}

// alive probes a client with an OpenSSH keepalive request. A dead TCP
// connection only surfaces when something is actually sent, so this is what
// distinguishes "cached and usable" from "cached and stale".
func alive(c *ssh.Client) bool {
	if c == nil {
		return false
	}

	_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)

	return err == nil
}

func (p *Pool) keepalive() {
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.mu.Lock()
			clients := make(map[string]*ssh.Client, len(p.clients))
			for name, c := range p.clients {
				clients[name] = c
			}
			p.mu.Unlock()

			for name, c := range clients {
				if alive(c) {
					continue
				}

				log.Debugf("sshx: dropping dead connection to %s", name)

				p.mu.Lock()
				if p.clients[name] == c {
					delete(p.clients, name)
				}
				p.mu.Unlock()

				_ = c.Close()
			}
		}
	}
}

// Close tears down every pooled connection.
func (p *Pool) Close() error {
	p.mu.Lock()
	if !p.stopped {
		p.stopped = true
		close(p.stop)
	}
	clients := p.clients
	p.clients = map[string]*ssh.Client{}
	p.mu.Unlock()

	for _, c := range clients {
		_ = c.Close()
	}

	return nil
}
