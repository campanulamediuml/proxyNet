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
	ssh        *sshclient.Client
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
	log.Printf("Config loaded: server=%s:%d user=%s", cfg.Server, cfg.Port, cfg.User)

	if cfg.Password == "" && cfg.PrivateKeyPath == "" {
		log.Println("No authentication method configured")
		fmt.Fprintln(os.Stderr, "No authentication method configured.")
		writeExampleConfig(exeDir)
		fmt.Println("A config.json.example has been created. Please edit it and rename to config.json.")
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}

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

// startTunnel establishes the SSH tunnel and starts sing-box, waiting until
// the TUN interface is actually up. Caller must hold a.mu. On failure it
// cleans up whatever it started.
func (a *App) startTunnel() error {
	localAddr := fmt.Sprintf("127.0.0.1:%d", a.cfg.ListenPort)
	remoteAddr := fmt.Sprintf("127.0.0.1:%d", a.cfg.ListenPort)
	log.Printf("SSH tunnel: %s -> %s:%s", localAddr, a.cfg.Server, remoteAddr)

	sshClient, err := sshclient.New(
		a.cfg.Server,
		a.cfg.Port,
		a.cfg.User,
		a.cfg.Password,
		a.cfg.PrivateKeyPath,
		localAddr,
		remoteAddr,
	)
	if err != nil {
		return fmt.Errorf("ssh init: %w", err)
	}

	if err := sshClient.Start(); err != nil {
		return fmt.Errorf("ssh connect: %w", err)
	}
	log.Println("SSH tunnel established")
	a.ssh = sshClient

	if err := a.sb.WriteConfig(
		a.cfg.TUNInterface,
		a.cfg.TUNAddress,
		a.cfg.TUNMTU,
		a.cfg.ListenPort,
		a.cfg.Server,
		a.cfg.DNSServer,
		a.cfg.ExcludeInterfaces,
		a.cfg.LocalSubnets,
	); err != nil {
		a.ssh.Stop()
		a.ssh = nil
		return fmt.Errorf("write sing-box config: %w", err)
	}

	if err := a.sb.Start(); err != nil {
		a.ssh.Stop()
		a.ssh = nil
		return fmt.Errorf("start sing-box: %w", err)
	}
	log.Println("sing-box client started")

	// Wait until the TUN interface is actually up; bail out if sing-box dies.
	for i := 0; i < 12; i++ {
		if err := a.sb.Err(); err != nil {
			_ = a.sb.Stop()
			a.ssh.Stop()
			a.ssh = nil
			return fmt.Errorf("sing-box failed: %w", err)
		}
		if tunInterfaceExists(a.cfg.TUNInterface) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	_ = a.sb.Stop()
	a.ssh.Stop()
	a.ssh = nil
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

		sshDead := a.ssh == nil || !a.ssh.Alive(8*time.Second)
		sbDead := a.sb.Err() != nil
		if !sshDead && !sbDead {
			failures = 0
			a.mu.Unlock()
			continue
		}

		log.Printf("watchdog: tunnel down (sshDead=%v sbDead=%v), reconnecting", sshDead, sbDead)
		a.systray.SetStatus("reconnecting...")
		_ = a.sb.Stop()
		if a.ssh != nil {
			a.ssh.Stop()
			a.ssh = nil
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
	if a.ssh != nil {
		a.ssh.Stop()
		a.ssh = nil
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
