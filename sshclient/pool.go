package sshclient

import (
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// ServerConfig describes one SSH endpoint of the pool.
type ServerConfig struct {
	Server  string
	Port    int
	User    string
	Password string
	KeyPath string
}

// Pool maintains SSH connections to multiple servers and assigns each
// incoming SOCKS connection to a random healthy server. Existing connections
// stay on their server until they close naturally, so hopping between servers
// never breaks in-flight traffic.
type Pool struct {
	localAddr  string
	remoteAddr string
	members    []*member

	listener net.Listener
	quit     chan struct{}
	wg       sync.WaitGroup
	running  bool
	mu       sync.Mutex
}

type member struct {
	addr   string
	cfg    *ssh.ClientConfig
	mu     sync.Mutex
	client *ssh.Client
	down   bool // true once marked offline; used to log state changes only once
}

// NewPool creates a pool for the given servers, listening on localAddr and
// dialing remoteAddr through the chosen server for each connection.
func NewPool(localAddr, remoteAddr string, servers []ServerConfig) (*Pool, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers configured")
	}

	p := &Pool{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		quit:       make(chan struct{}),
	}

	for _, s := range servers {
		cfg, err := buildSSHConfig(s)
		if err != nil {
			return nil, err
		}
		p.members = append(p.members, &member{
			addr: fmt.Sprintf("%s:%d", s.Server, s.Port),
			cfg:  cfg,
		})
	}
	return p, nil
}

func buildSSHConfig(s ServerConfig) (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	if s.KeyPath != "" {
		key, err := os.ReadFile(s.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key %s: %w", s.KeyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}
	if s.Password != "" {
		authMethods = append(authMethods, ssh.Password(s.Password))
	}
	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication method for %s", s.Server)
	}

	return &ssh.ClientConfig{
		User:            s.User,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

// Start dials all servers in parallel, then starts the listener, accept loop
// and per-member health loops. At least one server must be reachable.
func (p *Pool) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return fmt.Errorf("pool already running")
	}

	var okCount int32
	var dials sync.WaitGroup
	for _, m := range p.members {
		m := m
		dials.Add(1)
		go func() {
			defer dials.Done()
			if err := m.dial(); err != nil {
				// Offline hosts are simply excluded from the pool; the health
				// loop keeps retrying them silently in the background.
				m.markDown()
				log.Printf("pool: %s offline, skipped (background retry): %v", m.addr, err)
			} else {
				atomic.AddInt32(&okCount, 1)
				log.Printf("pool: connected to %s", m.addr)
			}
		}()
	}
	dials.Wait()

	if atomic.LoadInt32(&okCount) == 0 {
		return fmt.Errorf("no server reachable")
	}
	log.Printf("pool: %d/%d servers online", okCount, len(p.members))

	listener, err := net.Listen("tcp", p.localAddr)
	if err != nil {
		for _, m := range p.members {
			m.close()
		}
		return fmt.Errorf("listen on %s: %w", p.localAddr, err)
	}
	p.listener = listener
	p.running = true

	p.wg.Add(1)
	go p.acceptLoop()
	for _, m := range p.members {
		p.wg.Add(1)
		go p.healthLoop(m)
	}
	return nil
}

// Stop closes the listener and all server connections.
func (p *Pool) Stop() {
	p.mu.Lock()
	running := p.running
	p.running = false
	p.mu.Unlock()

	if !running {
		return
	}

	close(p.quit)
	if p.listener != nil {
		_ = p.listener.Close()
	}
	for _, m := range p.members {
		m.close()
	}
	p.wg.Wait()
}

// AliveCount reports how many server connections are currently established.
func (p *Pool) AliveCount() int {
	n := 0
	for _, m := range p.members {
		m.mu.Lock()
		if m.client != nil {
			n++
		}
		m.mu.Unlock()
	}
	return n
}

func (p *Pool) acceptLoop() {
	defer p.wg.Done()

	for {
		select {
		case <-p.quit:
			return
		default:
		}

		conn, err := p.listener.Accept()
		if err != nil {
			select {
			case <-p.quit:
				return
			default:
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		p.wg.Add(1)
		go p.handleConn(conn)
	}
}

// handleConn assigns the connection to a random healthy server.
func (p *Pool) handleConn(local net.Conn) {
	defer p.wg.Done()
	defer local.Close()

	var remote net.Conn
	for attempt := 0; attempt < 3 && remote == nil; attempt++ {
		c := p.pick()
		if c == nil {
			break
		}
		r, err := c.Dial("tcp", p.remoteAddr)
		if err == nil {
			remote = r
		}
	}
	if remote == nil {
		return
	}
	defer remote.Close()

	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(remote, local)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(local, remote)
		errChan <- err
	}()

	select {
	case <-p.quit:
	case <-errChan:
	}
}

// pick returns the ssh.Client of a random connected member, or nil if none.
func (p *Pool) pick() *ssh.Client {
	var alive []*ssh.Client
	for _, m := range p.members {
		m.mu.Lock()
		if m.client != nil {
			alive = append(alive, m.client)
		}
		m.mu.Unlock()
	}
	if len(alive) == 0 {
		return nil
	}
	return alive[rand.Intn(len(alive))]
}

// healthLoop periodically checks the member and re-dials it with backoff.
// State changes (online -> offline, offline -> online) are logged once;
// repeated failures of an already-offline member stay silent.
func (p *Pool) healthLoop(m *member) {
	defer p.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	backoff := 5 * time.Second

	for {
		select {
		case <-p.quit:
			return
		case <-ticker.C:
		}

		if m.alive(8 * time.Second) {
			backoff = 5 * time.Second
			continue
		}
		m.close()

		for {
			select {
			case <-p.quit:
				return
			case <-time.After(backoff):
			}

			if err := m.dial(); err != nil {
				if m.markDown() {
					log.Printf("pool: %s went offline, skipping it (background retry)", m.addr)
				}
				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
				continue
			}
			if m.markUp() {
				log.Printf("pool: %s back online", m.addr)
			}
			backoff = 5 * time.Second
			break
		}
	}
}

// markDown transitions the member to the offline state, returning true only
// on the transition (so callers can log state changes once).
func (m *member) markDown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false
	}
	m.down = true
	return true
}

// markUp transitions the member back to the online state, returning true
// only on the transition.
func (m *member) markUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.down {
		return false
	}
	m.down = false
	return true
}

func (m *member) dial() error {
	client, err := ssh.Dial("tcp", m.addr, m.cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.client = client
	m.mu.Unlock()
	return nil
}

func (m *member) close() {
	m.mu.Lock()
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	m.mu.Unlock()
}

// alive reports whether the member's connection responds to a keepalive
// request within timeout.
func (m *member) alive(timeout time.Duration) bool {
	m.mu.Lock()
	client := m.client
	m.mu.Unlock()
	if client == nil {
		return false
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@proxynet", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(timeout):
		return false
	}
}
