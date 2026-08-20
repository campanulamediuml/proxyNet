package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"proxyNet/route"
	"proxyNet/singbox"
	"proxyNet/sshclient"
	"proxyNet/tray"
)

// App orchestrates the SSH tunnel, sing-box client and tray UI.
type App struct {
	cfg        *Config
	pool       *sshclient.Pool
	sb         *singbox.Manager
	dnsManager *route.DNSManager
	systray    *tray.Tray
	mu         sync.Mutex
	connected  bool
	dnsApplied bool
	watchStop  chan struct{}
}

func main() {
	exeDir, err := getExeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to locate executable directory: %v\n", err)
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}

	logFile, err := initLogger(exeDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to init logger: %v\n", err)
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}
	defer logFile.Close()

	log.Println("proxyNet starting")

	if !isAdmin() {
		log.Println("Not running as admin, requesting elevation")
		fmt.Println("Administrator privileges required. Requesting elevation...")
		if err := runAsAdmin(); err != nil {
			log.Printf("Failed to elevate: %v", err)
			fmt.Fprintf(os.Stderr, "Failed to elevate: %v\n", err)
			fmt.Println("Please right-click proxyNet.exe and select 'Run as administrator'.")
			time.Sleep(5 * time.Second)
			os.Exit(1)
		}
		os.Exit(0)
	}
	log.Println("Running with admin privileges")

	flag.Parse()

	backupPath := filepath.Join(exeDir, "dns_backup.json")

	// Manual rescue: proxyNet.exe -restore
	if *RestoreDNS {
		dm := route.NewDNSManager(backupPath)
		if err := dm.Restore(); err != nil {
			log.Printf("Manual DNS restore failed: %v", err)
			fmt.Fprintf(os.Stderr, "DNS restore failed: %v\n", err)
		} else {
			log.Println("Manual DNS restore completed")
			fmt.Println("DNS settings restored.")
		}
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}

	// Startup safety net: if a backup exists (e.g. after an unclean exit),
	// restore the original DNS values for the adapters that still exist.
	{
		dm := route.NewDNSManager(backupPath)
		if recovered, err := dm.RecoverIfNeeded(); err != nil {
			log.Printf("DNS recovery from previous session failed: %v", err)
		} else if recovered {
			log.Println("Restored DNS settings from backup")
			fmt.Println("Restored DNS settings from backup.")
		}
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Printf("Failed to load config: %v", err)
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		writeExampleConfig(exeDir)
		fmt.Println("A config.json.example has been created. Please edit it and rename to config.json.")
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}
	serverAddrs := make([]string, 0, len(cfg.ServerList()))
	for _, s := range cfg.ServerList() {
		serverAddrs = append(serverAddrs, fmt.Sprintf("%s:%d", s.Server, s.Port))
	}
	log.Printf("Config loaded: %d server(s): %s", len(serverAddrs), strings.Join(serverAddrs, ", "))

	sbExe := singbox.FindExecutable(exeDir)
	if sbExe == "" {
		log.Println("sing-box.exe not found")
		fmt.Fprintf(os.Stderr, "sing-box.exe not found in %s or %s\\dist\n", exeDir, exeDir)
		fmt.Println("Please place sing-box.exe next to proxyNet.exe.")
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}
	log.Printf("sing-box executable: %s", sbExe)

	app := &App{
		cfg:        cfg,
		sb:         singbox.New(sbExe, filepath.Join(exeDir, "sing-box-client.json")),
		dnsManager: route.NewDNSManager(filepath.Join(exeDir, "dns_backup.json")),
	}

	app.systray = tray.New(
		app.connect,
		app.disconnect,
		app.exit,
	)

	log.Println("Starting tray UI")
	app.systray.Run()
	log.Println("Tray UI stopped")
}

func initLogger(exeDir string) (*os.File, error) {
	logPath := filepath.Join(exeDir, "proxyNet.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(f, os.Stdout))
	log.SetFlags(log.LstdFlags)
	return f, nil
}

func (a *App) connect() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.connected {
		return
	}

	log.Println("Connecting...")
	a.systray.SetStatus("connecting...")

	if err := a.startTunnel(); err != nil {
		log.Printf("Connect failed: %v", err)
		a.systray.SetStatus(fmt.Sprintf("connect failed: %v", err))
		return
	}

	if !a.dnsApplied {
		if err := a.dnsManager.BackupAndClear(); err != nil {
			log.Printf("Backup and clear DNS failed: %v", err)
		} else {
			a.dnsApplied = true
		}
	}

	a.connected = true
	a.watchStop = make(chan struct{})
	go a.watchdog(a.watchStop)
	a.systray.SetConnected(true)
	a.systray.SetStatus("connected")
	log.Println("Connected")
}

// startTunnel establishes the SSH connection pool and starts sing-box,
// waiting until the TUN interface is actually up. Caller must hold a.mu.
// On failure it cleans up whatever it started.
func (a *App) startTunnel() error {
	localAddr := fmt.Sprintf("127.0.0.1:%d", a.cfg.ListenPort)
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", a.cfg.ListenPort)

	servers := a.cfg.ServerList()
	serverAddrs := make([]string, len(servers))
	poolConfigs := make([]sshclient.ServerConfig, len(servers))
	for i, s := range servers {
		serverAddrs[i] = s.Server
		poolConfigs[i] = sshclient.ServerConfig{
			Server:   s.Server,
			Port:     s.Port,
			User:     s.User,
			Password: s.Password,
			KeyPath:  s.PrivateKeyPath,
		}
	}
	log.Printf("SSH pool: %s -> %v:%s", localAddr, serverAddrs, remoteAddr)

	pool, err := sshclient.NewPool(localAddr, remoteAddr, poolConfigs, a.cfg.ConnsPerServer)
	if err != nil {
		return fmt.Errorf("pool init: %w", err)
	}
	if err := pool.Start(); err != nil {
		return fmt.Errorf("pool start: %w", err)
	}
	a.pool = pool

	if err := a.sb.WriteConfig(
		a.cfg.TUNInterface,
		a.cfg.TUNAddress,
		a.cfg.TUNMTU,
		a.cfg.ListenPort,
		serverAddrs,
		a.cfg.DNSServer,
		a.cfg.ExcludeInterfaces,
		a.cfg.LocalSubnets,
	); err != nil {
		a.pool.Stop()
		a.pool = nil
		return fmt.Errorf("write sing-box config: %w", err)
	}

	if err := a.sb.Start(); err != nil {
		a.pool.Stop()
		a.pool = nil
		return fmt.Errorf("start sing-box: %w", err)
	}
	log.Println("sing-box client started")

	// Wait until the TUN interface is actually up; bail out if sing-box dies.
	for i := 0; i < 12; i++ {
		if err := a.sb.Err(); err != nil {
			_ = a.sb.Stop()
			a.pool.Stop()
			a.pool = nil
			return fmt.Errorf("sing-box failed: %w", err)
		}
		if tunInterfaceExists(a.cfg.TUNInterface) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	_ = a.sb.Stop()
	a.pool.Stop()
	a.pool = nil
	return fmt.Errorf("tun interface %s did not come up", a.cfg.TUNInterface)
}

// watchdog periodically checks the tunnel and reconnects automatically
// (e.g. after the machine wakes from sleep). DNS blackhole settings are left
// untouched during reconnects.
func (a *App) watchdog(stop chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	failures := 0

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		a.mu.Lock()
		if !a.connected {
			a.mu.Unlock()
			return
		}

		sshDead := a.pool == nil || a.pool.AliveCount() == 0
		sbDead := a.sb.Err() != nil
		if !sshDead && !sbDead {
			failures = 0
			a.mu.Unlock()
			continue
		}

		log.Printf("watchdog: tunnel down (sshDead=%v sbDead=%v), reconnecting", sshDead, sbDead)
		a.systray.SetStatus("reconnecting...")
		_ = a.sb.Stop()
		if a.pool != nil {
			a.pool.Stop()
			a.pool = nil
		}
		a.mu.Unlock()

		failures++
		backoff := time.Duration(failures) * 5 * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}

		a.mu.Lock()
		if !a.connected {
			a.mu.Unlock()
			return
		}
		if err := a.startTunnel(); err != nil {
			log.Printf("watchdog: reconnect failed: %v", err)
			a.systray.SetStatus(fmt.Sprintf("reconnect failed: %v", err))
			a.mu.Unlock()
			continue
		}
		failures = 0
		a.systray.SetStatus("connected")
		log.Println("watchdog: reconnected")
		a.mu.Unlock()
	}
}

func (a *App) disconnect() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.connected {
		return
	}

	log.Println("Disconnecting...")
	a.systray.SetStatus("disconnecting...")

	if a.watchStop != nil {
		close(a.watchStop)
		a.watchStop = nil
	}

	_ = a.sb.Stop()
	if a.pool != nil {
		a.pool.Stop()
		a.pool = nil
	}

	if a.dnsApplied {
		if err := a.dnsManager.Restore(); err != nil {
			log.Printf("Restore DNS failed: %v", err)
		}
		a.dnsApplied = false
	}

	a.connected = false
	a.systray.SetConnected(false)
	a.systray.SetStatus("disconnected")
	log.Println("Disconnected")
}

func (a *App) exit() {
	log.Println("Exiting...")
	a.disconnect()
	time.Sleep(200 * time.Millisecond)
	a.systray.Exit()
	os.Exit(0)
}

func getExeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func writeExampleConfig(dir string) {
	cfg := DefaultConfig()
	cfg.Password = "your_password_here"
	cfg.Servers = []ServerEntry{
		{Server: "192.168.3.33"},
		{Server: "192.168.3.100"},
		{Server: "192.168.3.101", Port: 22, User: "root", Password: "这台密码不一样就单独写"},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "config.json.example"), data, 0644)
}

// tunInterfaceExists reports whether a network adapter with the given name exists.
func tunInterfaceExists(name string) bool {
	escaped := strings.ReplaceAll(name, "'", "''")
	ps := fmt.Sprintf(
		"Get-NetAdapter -Name '%s' -ErrorAction SilentlyContinue | Select-Object -First 1 | ConvertTo-Json",
		escaped)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}
