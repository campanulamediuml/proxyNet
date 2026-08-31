package singbox

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Manager controls a local sing-box client process.
type Manager struct {
	exePath    string
	configPath string
	cmd        *exec.Cmd
	done       chan error

	outMu   sync.Mutex
	outTail []byte
}

// New creates a manager for the given sing-box executable and config file.
func New(exePath, configPath string) *Manager {
	return &Manager{
		exePath:    exePath,
		configPath: configPath,
	}
}

// GenerateClientConfig returns a sing-box client JSON config that routes all
// traffic through a local SOCKS5 proxy (the SSH tunnel endpoint).
// serverAddrs are the SSH server endpoints; all of them are excluded from the
// tunnel to avoid routing loops. dnsServer is the TCP DNS endpoint on the
// remote side (reachable through the proxy). localSubnets are routed directly
// instead of through the tunnel.
func GenerateClientConfig(tunName, tunAddr string, mtu int, socksPort int, serverAddrs []string, dnsServer string, excludeInterfaces, localSubnets []string) string {
	excludeJSON, _ := json.Marshal(excludeInterfaces)

	excludedAddrs := append([]string{}, localSubnets...)
	for _, s := range serverAddrs {
		excludedAddrs = append(excludedAddrs, s+"/32")
	}
	excludeAddrJSON, _ := json.Marshal(excludedAddrs)

	return fmt.Sprintf(`{
  "log": {
    "level": "info",
    "output": "sing-box.log"
  },
  "dns": {
    "servers": [
      {
        "tag": "remote",
        "type": "tcp",
        "server": %q,
        "server_port": 53,
        "detour": "proxy"
      }
    ]
  },
  "inbounds": [
    {
      "type": "tun",
      "tag": "tun-in",
      "interface_name": %q,
      "address": [%q],
      "mtu": %d,
      "auto_route": true,
      "strict_route": false,
      "stack": "system",
      "endpoint_independent_nat": true,
      "route_exclude_address": %s,
      "exclude_interface": %s
    }
  ],
  "outbounds": [
    {
      "type": "socks",
      "tag": "proxy",
      "server": "127.0.0.1",
      "server_port": %d
    },
    {
      "type": "direct",
      "tag": "direct"
    }
  ],
  "route": {
    "auto_detect_interface": true,
    "rules": [
      {
        "action": "sniff"
      },
      {
        "protocol": "dns",
        "action": "hijack-dns"
      },
      {
        "port": 53,
        "action": "hijack-dns"
      }
    ],
    "final": "proxy"
  }
}`, dnsServer, tunName, tunAddr, mtu, excludeAddrJSON, excludeJSON, socksPort)
}

// WriteConfig writes the generated config to disk.
func (m *Manager) WriteConfig(tunName, tunAddr string, mtu int, socksPort int, serverAddrs []string, dnsServer string, excludeInterfaces, localSubnets []string) error {
	cfg := GenerateClientConfig(tunName, tunAddr, mtu, socksPort, serverAddrs, dnsServer, excludeInterfaces, localSubnets)
	return os.WriteFile(m.configPath, []byte(cfg), 0644)
}

// Start launches sing-box with the config file.
func (m *Manager) Start() error {
	if m.cmd != nil {
		return fmt.Errorf("sing-box already running")
	}

	killStale(m.exePath)

	cmd := exec.Command(m.exePath, "run", "-c", m.configPath)
	tw := &tailWriter{m: m}
	cmd.Stdout = io.MultiWriter(os.Stdout, tw)
	cmd.Stderr = io.MultiWriter(os.Stderr, tw)
	cmd.Dir = filepath.Dir(m.configPath)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}
	m.cmd = cmd
	m.done = make(chan error, 1)
	go func() { m.done <- cmd.Wait() }()
	return nil
}

// Err reports whether the sing-box process has already exited, including the
// tail of its output for diagnostics. Returns nil while it is still running.
func (m *Manager) Err() error {
	if m.cmd == nil || m.done == nil {
		return nil
	}
	select {
	case err := <-m.done:
		m.cmd = nil
		return fmt.Errorf("sing-box exited: %v: %s", err, m.outputTail())
	default:
		return nil
	}
}

// Stop terminates the sing-box process.
func (m *Manager) Stop() error {
	if m.cmd == nil || m.cmd.Process == nil {
		m.cmd = nil
		return nil
	}

	if err := m.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("kill sing-box: %w", err)
	}
	if m.done != nil {
		<-m.done
	}
	m.cmd = nil
	m.done = nil
	return nil
}

func (m *Manager) outputTail() string {
	m.outMu.Lock()
	defer m.outMu.Unlock()
	return strings.TrimSpace(string(m.outTail))
}

// tailWriter keeps the last few KB of sing-box output for error reporting.
type tailWriter struct{ m *Manager }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.m.outMu.Lock()
	t.m.outTail = append(t.m.outTail, p...)
	if len(t.m.outTail) > 8192 {
		t.m.outTail = t.m.outTail[len(t.m.outTail)-8192:]
	}
	t.m.outMu.Unlock()
	return len(p), nil
}

// killStale terminates leftover sing-box processes of the same executable path
// from previous crashed runs, which otherwise hold the wintun adapter.
func killStale(exePath string) {
	escaped := strings.ReplaceAll(exePath, "'", "''")
	ps := fmt.Sprintf(
		"Get-Process sing-box -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq '%s' } | Stop-Process -Force",
		escaped)
	_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
}

// FindExecutable searches for the sing-box executable in common locations.
func FindExecutable(workDir string) string {
	name := "sing-box"
	if runtime.GOOS == "windows" {
		name = "sing-box.exe"
	}

	candidates := []string{
		filepath.Join(workDir, name),
		filepath.Join(workDir, "dist", name),
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	return ""
}
