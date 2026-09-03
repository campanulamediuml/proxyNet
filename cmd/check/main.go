// check reads the servers list from config.json and health-checks every
// server in parallel: SSH reachability, sing-box/dnsmasq services, listener
// ports and outbound internet access. Prints a per-server status table.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type serverEntry struct {
	Server         string `json:"server"`
	Port           int    `json:"port,omitempty"`
	User           string `json:"user,omitempty"`
	Password       string `json:"password,omitempty"`
	PrivateKeyPath string `json:"private_key_path,omitempty"`
}

type config struct {
	Server         string        `json:"server"`
	Port           int           `json:"port"`
	User           string        `json:"user"`
	Password       string        `json:"password"`
	PrivateKeyPath string        `json:"private_key_path"`
	Servers        []serverEntry `json:"servers,omitempty"`
}

func (c *config) serverList() []serverEntry {
	if len(c.Servers) == 0 {
		return []serverEntry{{
			Server:         c.Server,
			Port:           c.Port,
			User:           c.User,
			Password:       c.Password,
			PrivateKeyPath: c.PrivateKeyPath,
		}}
	}
	out := make([]serverEntry, len(c.Servers))
	for i, s := range c.Servers {
		if s.Port == 0 {
			s.Port = c.Port
		}
		if s.User == "" {
			s.User = c.User
		}
		if s.Password == "" {
			s.Password = c.Password
		}
		if s.PrivateKeyPath == "" {
			s.PrivateKeyPath = c.PrivateKeyPath
		}
		out[i] = s
	}
	return out
}

type report struct {
	server string
	online bool
	detail string
}

const probeScript = `
SB=$(systemctl is-active sing-box 2>/dev/null || echo missing)
DM=$(systemctl is-active dnsmasq 2>/dev/null || echo missing)
S10010=$(ss -tln 2>/dev/null | grep -c '127.0.0.1:10010' || true)
S53=$(ss -tln 2>/dev/null | grep -c '127.0.0.1:53' || true)
PING=$(ping -c 1 -W 3 baidu.com >/dev/null 2>&1 && echo ok || echo fail)
HTTP=$(curl -m 6 -s -o /dev/null -w '%{http_code}' https://www.baidu.com 2>/dev/null || echo 000)
echo "SB=$SB DM=$DM SOCKS=$S10010 DNS53=$S53 PING=$PING HTTP=$HTTP"
`

func main() {
	cfgPath := flag.String("config", "", "path to config.json")
	flag.Parse()

	cfg := loadConfig(*cfgPath)
	servers := cfg.serverList()
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "no servers in config")
		os.Exit(1)
	}

	fmt.Printf("checking %d server(s)...\n\n", len(servers))

	results := make(chan report, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- checkOne(s)
		}()
	}
	wg.Wait()
	close(results)

	var reports []report
	for r := range results {
		reports = append(reports, r)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].server < reports[j].server })

	failed := 0
	for _, r := range reports {
		if r.online {
			fmt.Printf("[ UP ] %-16s %s\n", r.server, r.detail)
		} else {
			failed++
			fmt.Printf("[DOWN] %-16s %s\n", r.server, r.detail)
		}
	}
	fmt.Printf("\n%d up / %d down\n", len(reports)-failed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func checkOne(s serverEntry) report {
	addr := fmt.Sprintf("%s:%d", s.Server, s.Port)

	sshCfg, err := buildSSHConfig(s)
	if err != nil {
		return report{server: s.Server, detail: err.Error()}
	}

	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return report{server: s.Server, detail: fmt.Sprintf("ssh unreachable: %v", err)}
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return report{server: s.Server, detail: fmt.Sprintf("session: %v", err)}
	}
	defer session.Close()

	out, err := session.Output(probeScript)
	if err != nil {
		return report{server: s.Server, detail: fmt.Sprintf("probe failed: %v", err)}
	}

	kv := map[string]string{}
	for _, field := range strings.Fields(strings.TrimSpace(string(out))) {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) == 2 {
			kv[parts[0]] = parts[1]
		}
	}

	// ICMP may be blocked on some networks; only services and real outbound
	// HTTPS matter for the proxy to work.
	ok := kv["SB"] == "active" && kv["DM"] == "active" && kv["SOCKS"] != "0" && kv["DNS53"] != "0" && kv["HTTP"] == "200"
	detail := fmt.Sprintf("sing-box=%s dnsmasq=%s socks:%s dns53:%s ping=%s http=%s",
		kv["SB"], kv["DM"], kv["SOCKS"], kv["DNS53"], kv["PING"], kv["HTTP"])
	return report{server: s.Server, online: ok, detail: detail}
}

func buildSSHConfig(s serverEntry) (*ssh.ClientConfig, error) {
	var auth []ssh.AuthMethod
	if s.PrivateKeyPath != "" {
		key, err := os.ReadFile(s.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if s.Password != "" {
		auth = append(auth, ssh.Password(s.Password))
	}
	if len(auth) == 0 {
		return nil, fmt.Errorf("no authentication method for %s", s.Server)
	}
	return &ssh.ClientConfig{
		User:            s.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}, nil
}

func loadConfig(path string) *config {
	cfg := &config{Server: "192.168.3.33", Port: 10022, User: "root"}

	candidates := []string{path}
	if path == "" {
		exe, _ := os.Executable()
		candidates = []string{
			"config.json",
			filepath.Join("dist", "config.json"),
			filepath.Join(filepath.Dir(exe), "config.json"),
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		data, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, cfg); err == nil {
			fmt.Printf("using config: %s\n", c)
			return cfg
		}
	}
	return cfg
}
