package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
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
	mu          sync.Mutex
	connected   bool
	dnsApplied  bool
	watchStop   chan struct{}
	connectedAt time.Time

	ratesMu sync.Mutex
	rates   map[string][2]int64 // server addr -> [up bytes/s, down bytes/s]
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
		app.showStats,
		app.exit,
	)

	app.startStatsServer()

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
	a.connectedAt = time.Now()
	a.watchStop = make(chan struct{})
	go a.watchdog(a.watchStop)
	a.systray.SetConnected(true)
	a.systray.SetStatus("connected")
	log.Println("Connected")
}

// uptimeText returns how long the tunnel has been connected, like "1h23m".
func (a *App) uptimeText() string {
	if !a.connected || a.connectedAt.IsZero() {
		return ""
	}
	d := time.Since(a.connectedAt).Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
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
	ticks := 0

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
			ticks++
			if ticks >= 12 { // roughly every minute
				ticks = 0
				for _, line := range strings.Split(a.statsText(), "\n") {
					log.Println("stats: " + line)
				}
				a.systray.SetStatus(fmt.Sprintf("connected (%s)", a.uptimeText()))
			}
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
	a.connectedAt = time.Time{}
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

// showStats opens the live stats page in the default browser.
func (a *App) showStats() {
	openURL("http://127.0.0.1:10011/")
}

// startStatsServer serves a self-refreshing traffic stats page on
// 127.0.0.1:10011. It runs for the lifetime of the process.
func (a *App) startStatsServer() {
	a.startRateSampler()
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.statsHandler)
	srv := &http.Server{Addr: "127.0.0.1:10011", Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("stats server: %v", err)
		}
	}()
	log.Println("stats page: http://127.0.0.1:10011/")
}

// startRateSampler samples pool counters every 2s and derives per-server
// throughput rates (bytes/sec).
func (a *App) startRateSampler() {
	type prevSample struct {
		up, down int64
		at       time.Time
	}
	last := map[string]prevSample{}

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			a.mu.Lock()
			pool := a.pool
			a.mu.Unlock()

			rates := map[string][2]int64{}
			if pool != nil {
				now := time.Now()
				for _, s := range pool.Stats() {
					if p, ok := last[s.Addr]; ok {
						dt := now.Sub(p.at).Seconds()
						if dt > 0 {
							up := int64(float64(s.BytesUp-p.up) / dt)
							down := int64(float64(s.BytesDown-p.down) / dt)
							if up < 0 {
								up = 0
							}
							if down < 0 {
								down = 0
							}
							rates[s.Addr] = [2]int64{up, down}
						}
					}
					last[s.Addr] = prevSample{up: s.BytesUp, down: s.BytesDown, at: now}
				}
			}
			a.ratesMu.Lock()
			a.rates = rates
			a.ratesMu.Unlock()
		}
	}()
}

// rateOf returns the current [up, down] bytes/sec for a server.
func (a *App) rateOf(addr string) (int64, int64) {
	a.ratesMu.Lock()
	defer a.ratesMu.Unlock()
	r := a.rates[addr]
	return r[0], r[1]
}

func (a *App) statsHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	pool := a.pool
	connected := a.connected
	a.mu.Unlock()

	var rows strings.Builder
	var totalUp, totalDown, totalConns, totalActive int64
	var totalUpRate, totalDownRate int64
	if pool != nil {
		for _, s := range pool.Stats() {
			state := `<span style="color:#4ec9b0">在线</span>`
			if !s.Online {
				state = `<span style="color:#f44747">离线</span>`
			}
			upRate, downRate := a.rateOf(s.Addr)
			fmt.Fprintf(&rows, "<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s/s</td><td>%s/s</td></tr>\n",
				s.Addr, state, s.Streams, s.ActiveConns, s.TotalConns,
				humanBytes(s.BytesUp), humanBytes(s.BytesDown),
				humanBytes(upRate), humanBytes(downRate))
			totalUp += s.BytesUp
			totalDown += s.BytesDown
			totalConns += s.TotalConns
			totalActive += s.ActiveConns
			totalUpRate += upRate
			totalDownRate += downRate
		}
	}

	status := "未连接"
	if connected {
		status = "已连接"
		if up := a.uptimeText(); up != "" {
			status = "已连接 · 运行 " + up
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8">
<meta http-equiv="refresh" content="3">
<title>proxyNet 流量统计</title>
<style>
body{font-family:Consolas,Menlo,monospace;background:#1e1e1e;color:#d4d4d4;padding:20px}
h3{margin:0 0 12px}
table{border-collapse:collapse}
th,td{padding:6px 14px;border-bottom:1px solid #333;text-align:right}
th{color:#9cdcfe}
td:first-child,th:first-child{text-align:left}
.total{margin-top:12px;color:#ce9178}
.meta{margin-top:8px;color:#666;font-size:12px}
</style></head><body>
<h3>proxyNet 流量统计 <small style="color:#666">%s</small></h3>
<table><tr><th>服务器</th><th>状态</th><th>流数</th><th>活跃</th><th>累计连接</th><th>上行</th><th>下行</th><th>上行速度</th><th>下行速度</th></tr>
%s</table>
<div class="total">总计: 活跃 %d / 累计 %d  ↑%s ↓%s（↑%s/s ↓%s/s）</div>
<div class="meta">每 3 秒自动刷新 · %s</div>
</body></html>`, status, rows.String(), totalActive, totalConns,
		humanBytes(totalUp), humanBytes(totalDown),
		humanBytes(totalUpRate), humanBytes(totalDownRate),
		time.Now().Format("15:04:05"))
}

// statsText formats pool statistics as a multi-line block with a total line.
func (a *App) statsText() string {
	stats := a.pool.Stats()

	var totalUp, totalDown, totalConns, totalActive int64
	var totalUpRate, totalDownRate int64
	for _, s := range stats {
		totalUp += s.BytesUp
		totalDown += s.BytesDown
		totalConns += s.TotalConns
		totalActive += s.ActiveConns
		up, down := a.rateOf(s.Addr)
		totalUpRate += up
		totalDownRate += down
	}

	var b strings.Builder
	fmt.Fprintf(&b, "总计: 活跃 %d / 累计 %d  ↑%s ↓%s（↑%s/s ↓%s/s）", totalActive, totalConns,
		humanBytes(totalUp), humanBytes(totalDown),
		humanBytes(totalUpRate), humanBytes(totalDownRate))
	for _, s := range stats {
		state := "在线"
		if !s.Online {
			state = "离线"
		}
		upRate, downRate := a.rateOf(s.Addr)
		fmt.Fprintf(&b, "\n%s [%s] 流:%d 活跃:%d 累计:%d ↑%s ↓%s（↑%s/s ↓%s/s）",
			s.Addr, state, s.Streams, s.ActiveConns, s.TotalConns,
			humanBytes(s.BytesUp), humanBytes(s.BytesDown),
			humanBytes(upRate), humanBytes(downRate))
	}
	return b.String()
}

// humanBytes renders a byte count in a human-readable form.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
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
