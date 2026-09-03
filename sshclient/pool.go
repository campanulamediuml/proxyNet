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
	Server   string
	Port     int
	User     string
	Password string
	KeyPath  string
}

// Pool maintains SSH connections to multiple servers (several parallel
// connections per server) and assigns each incoming SOCKS connection to a
// random healthy connection. In-flight connections stay on their stream
// until they close naturally.
type Pool struct {
	localAddr  string
	remoteAddr string
	connsPer   int
	members    []*member

	listener net.Listener
	quit     chan struct{}
	wg       sync.WaitGroup
	running  bool
	mu       sync.Mutex
}

type member struct {
	addr    string
	cfg     *ssh.ClientConfig
	target  int // number of parallel streams to maintain
	mu      sync.Mutex
	clients []*ssh.Client
	down    bool // true once marked offline; used to log state changes only once

	activeConns int64
	totalConns  int64
	bytesUp     int64
	bytesDown   int64
}

// NewPool creates a pool for the given servers, listening on localAddr and
// dialing remoteAddr through the chosen server for each connection.
// connsPer is the number of parallel SSH streams kept per server.
func NewPool(localAddr, remoteAddr string, servers []ServerConfig, connsPer int) (*Pool, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers configured")
	}
	if connsPer < 1 {
		connsPer = 1
	}
	if connsPer > 32 {
		connsPer = 32
	}

	p := &Pool{
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		connsPer:   connsPer,
		quit:       make(chan struct{}),
	}

	for _, s := range servers {
		cfg, err := buildSSHConfig(s)
		if err != nil {
			return nil, err
		}
		p.members = append(p.members, &member{
			addr:   fmt.Sprintf("%s:%d", s.Server, s.Port),
			cfg:    cfg,
			target: connsPer,
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
		// Present as an ordinary OpenSSH client instead of "SSH-2.0-Go".
		ClientVersion: "SSH-2.0-OpenSSH_8.9p1 Ubuntu-3ubuntu0.13",
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
			m.topUp()
			if m.count() == 0 {
				m.markDown()
				log.Printf("pool: %s offline, skipped (background retry)", m.addr)
			} else {
				atomic.AddInt32(&okCount, 1)
				log.Printf("pool: connected to %s (%d streams)", m.addr, m.count())
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

// AliveCount reports how many servers currently have at least one stream.
func (p *Pool) AliveCount() int {
	n := 0
	for _, m := range p.members {
		if m.count() > 0 {
			n++
		}
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

// handleConn assigns the connection to a random healthy stream.
func (p *Pool) handleConn(local net.Conn) {
	defer p.wg.Done()
	defer local.Close()

	var remote net.Conn
	var chosen *member
	for attempt := 0; attempt < 3 && remote == nil; attempt++ {
		m, c := p.pick()
		if c == nil {
			break
		}
		r, err := c.Dial("tcp", p.remoteAddr)
		if err == nil {
			remote = r
			chosen = m
		}
	}
	if remote == nil {
		return
	}
	defer remote.Close()

	atomic.AddInt64(&chosen.activeConns, 1)
	atomic.AddInt64(&chosen.totalConns, 1)
	defer atomic.AddInt64(&chosen.activeConns, -1)

	errChan := make(chan error, 2)
	go func() {
		_, err := io.Copy(&countingWriter{w: remote, n: &chosen.bytesUp}, local)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(&countingWriter{w: local, n: &chosen.bytesDown}, remote)
		errChan <- err
	}()

	select {
	case <-p.quit:
	case <-errChan:
	}
}

// countingWriter increments an atomic counter as bytes are written.
type countingWriter struct {
	w io.Writer
	n *int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		atomic.AddInt64(cw.n, int64(n))
	}
	return n, err
}

// pick returns a random member with an established stream, or nil.
func (p *Pool) pick() (*member, *ssh.Client) {
	var aliveM []*member
	var aliveC []*ssh.Client
	for _, m := range p.members {
		m.mu.Lock()
		for _, c := range m.clients {
			aliveM = append(aliveM, m)
			aliveC = append(aliveC, c)
		}
		m.mu.Unlock()
	}
	if len(aliveC) == 0 {
		return nil, nil
	}
	i := rand.Intn(len(aliveC))
	return aliveM[i], aliveC[i]
}

// MemberStats is a per-server traffic snapshot.
type MemberStats struct {
	Addr        string
	Streams     int
	ActiveConns int64
	TotalConns  int64
	BytesUp     int64
	BytesDown   int64
	Online      bool
}

// Stats returns a traffic snapshot for every server in the pool.
func (p *Pool) Stats() []MemberStats {
	out := make([]MemberStats, 0, len(p.members))
	for _, m := range p.members {
		m.mu.Lock()
		streams := len(m.clients)
		m.mu.Unlock()
		out = append(out, MemberStats{
			Addr:        m.addr,
			Streams:     streams,
			ActiveConns: atomic.LoadInt64(&m.activeConns),
			TotalConns:  atomic.LoadInt64(&m.totalConns),
			BytesUp:     atomic.LoadInt64(&m.bytesUp),
			BytesDown:   atomic.LoadInt64(&m.bytesDown),
			Online:      streams > 0,
		})
	}
	return out
}

// healthLoop prunes dead streams, tops the member back up to its target
// stream count, and logs online/offline transitions once.
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

		m.pruneDead(8 * time.Second)

		if m.count() == 0 {
			if m.markDown() {
				log.Printf("pool: %s went offline, skipping it (background retry)", m.addr)
			}
			m.topUp()
			if m.count() > 0 {
				backoff = 5 * time.Second
				if m.markUp() {
					log.Printf("pool: %s back online", m.addr)
				}
				continue
			}
			backoff *= 2
			if backoff > 60*time.Second {
				backoff = 60 * time.Second
			}
			select {
			case <-p.quit:
				return
			case <-time.After(backoff):
			}
			continue
		}

		m.topUp()
		backoff = 5 * time.Second
	}
}

// count returns the number of established streams.
func (m *member) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.clients)
}

// topUp dials new streams in parallel until the member reaches its target.
func (m *member) topUp() {
	m.mu.Lock()
	missing := m.target - len(m.clients)
	m.mu.Unlock()
	if missing <= 0 {
		return
	}

	results := make(chan *ssh.Client, missing)
	var wg sync.WaitGroup
	for i := 0; i < missing; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, err := ssh.Dial("tcp", m.addr, m.cfg); err == nil {
				results <- c
			}
		}()
	}
	wg.Wait()
	close(results)

	m.mu.Lock()
	for c := range results {
		m.clients = append(m.clients, c)
	}
	m.mu.Unlock()
}

// pruneDead probes every stream and removes the ones that do not respond.
func (m *member) pruneDead(timeout time.Duration) {
	m.mu.Lock()
	clients := append([]*ssh.Client(nil), m.clients...)
	m.mu.Unlock()

	dead := make(chan *ssh.Client, len(clients))
	var wg sync.WaitGroup
	for _, c := range clients {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !probe(c, timeout) {
				dead <- c
			}
		}()
	}
	wg.Wait()
	close(dead)

	m.mu.Lock()
	for d := range dead {
		_ = d.Close()
		for i, c := range m.clients {
			if c == d {
				m.clients = append(m.clients[:i], m.clients[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
}

func (m *member) close() {
	m.mu.Lock()
	for _, c := range m.clients {
		_ = c.Close()
	}
	m.clients = nil
	m.mu.Unlock()
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

// probe reports whether the connection responds to a keepalive request
// within timeout. sshd replies even to unknown request names (with failure),
// which still proves the connection is alive.
func probe(client *ssh.Client, timeout time.Duration) bool {
	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(timeout):
		return false
	}
}
