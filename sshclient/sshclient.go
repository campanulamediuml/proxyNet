package sshclient

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client manages an SSH connection with a local-to-remote port forwarding tunnel.
type Client struct {
	server     string
	config     *ssh.ClientConfig
	localAddr  string
	remoteAddr string

	client   *ssh.Client
	listener net.Listener
	quit     chan struct{}
	wg       sync.WaitGroup
	running  bool
	mu       sync.Mutex
}

// New creates a new SSH client that will forward localAddr to remoteAddr through server.
func New(server string, port int, user, password, keyPath, localAddr, remoteAddr string) (*Client, error) {
	var authMethods []ssh.AuthMethod

	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key %s: %w", keyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if password != "" {
		authMethods = append(authMethods, ssh.Password(password))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication method provided")
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	return &Client{
		server:     fmt.Sprintf("%s:%d", server, port),
		config:     config,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		quit:       make(chan struct{}),
	}, nil
}

// Start dials the SSH server and begins listening on the local address.
func (c *Client) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return fmt.Errorf("ssh client already running")
	}

	client, err := ssh.Dial("tcp", c.server, c.config)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", c.server, err)
	}
	c.client = client

	listener, err := net.Listen("tcp", c.localAddr)
	if err != nil {
		client.Close()
		return fmt.Errorf("listen on %s: %w", c.localAddr, err)
	}
	c.listener = listener
	c.running = true

	c.wg.Add(1)
	go c.acceptLoop()

	return nil
}

// Stop closes the SSH connection and local listener.
func (c *Client) Stop() {
	c.mu.Lock()
	running := c.running
	c.running = false
	c.mu.Unlock()

	if !running {
		return
	}

	close(c.quit)
	if c.listener != nil {
		_ = c.listener.Close()
	}
	if c.client != nil {
		_ = c.client.Close()
	}
	c.wg.Wait()
}

func (c *Client) acceptLoop() {
	defer c.wg.Done()

	for {
		select {
		case <-c.quit:
			return
		default:
		}

		conn, err := c.listener.Accept()
		if err != nil {
			select {
			case <-c.quit:
				return
			default:
				// Small backoff to avoid busy loop on transient errors.
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		c.wg.Add(1)
		go c.handleConn(conn)
	}
}

func (c *Client) handleConn(local net.Conn) {
	defer c.wg.Done()
	defer local.Close()

	remote, err := c.client.Dial("tcp", c.remoteAddr)
	if err != nil {
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
	case <-c.quit:
	case <-errChan:
	}
}

// Alive reports whether the SSH connection is still usable, by sending a
// global keepalive request and waiting for any reply within timeout.
func (c *Client) Alive(timeout time.Duration) bool {
	c.mu.Lock()
	if !c.running || c.client == nil {
		c.mu.Unlock()
		return false
	}
	client := c.client
	c.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		// Any global request works: sshd replies even to unknown names
		// (with failure), which still proves the connection is alive.
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

// Running reports whether the client is currently active.
func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}
