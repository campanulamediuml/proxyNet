// deploy reads the servers list from config.json and deploys the proxyNet
// server-side environment (sing-box + dnsmasq + systemd units) to every
// server over SSH/SFTP. It uploads the local server-kit files first so the
// remote side needs no internet access at all.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
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

type result struct {
	server string
	err    error
}

func main() {
	cfgPath := flag.String("config", "", "path to config.json")
	kitPath := flag.String("kit", "", "path to server-kit directory")
	flag.Parse()

	cfg := loadConfig(*cfgPath)
	servers := cfg.serverList()
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "no servers in config")
		os.Exit(1)
	}

	kit := findKit(*kitPath)
	fmt.Printf("server-kit: %s\n", kit)
	fmt.Printf("deploying to %d server(s)...\n\n", len(servers))

	results := make(chan result, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- result{server: s.Server, err: deployOne(s, kit)}
		}()
	}
	wg.Wait()
	close(results)

	fmt.Println("\n===== summary =====")
	failed := 0
	for r := range results {
		if r.err != nil {
			failed++
			fmt.Printf("[FAIL] %s: %v\n", r.server, r.err)
		} else {
			fmt.Printf("[ OK ] %s\n", r.server)
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// deployOne uploads the server-kit and runs deploy-server.sh on one server.
func deployOne(s serverEntry, kit string) error {
	prefix := fmt.Sprintf("[%s] ", s.Server)
	logf := func(format string, args ...interface{}) {
		fmt.Printf(prefix+format+"\n", args...)
	}

	sshCfg, err := buildSSHConfig(s)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", s.Server, s.Port)
	logf("connecting ...")
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	// Upload server-kit via SFTP (best effort; missing local files are skipped
	// and the remote script falls back to downloading).
	ft, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("sftp: %w", err)
	}
	defer ft.Close()

	remoteDir := "/root/server-kit"
	_ = ft.MkdirAll(remoteDir + "/debs")

	uploads := map[string]string{
		filepath.Join(kit, "deploy-server.sh"): remoteDir + "/deploy-server.sh",
	}
	// sing-box tarball and debs are optional but enable fully offline deploys.
	if entries, err := os.ReadDir(kit); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "sing-box-") && strings.HasSuffix(e.Name(), ".tar.gz") {
				uploads[filepath.Join(kit, e.Name())] = remoteDir + "/" + e.Name()
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(kit, "debs")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".deb") {
				uploads[filepath.Join(kit, "debs", e.Name())] = remoteDir + "/debs/" + e.Name()
			}
		}
	}

	for local, remote := range uploads {
		if _, err := os.Stat(local); err != nil {
			continue
		}
		if err := uploadFile(ft, local, remote); err != nil {
			return fmt.Errorf("upload %s: %w", filepath.Base(local), err)
		}
		logf("uploaded %s", filepath.Base(local))
	}
	_ = ft.Chmod(remoteDir+"/deploy-server.sh", 0755)

	// Run the deploy script, streaming its output.
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	session.Stdout = &prefixWriter{prefix: prefix, w: os.Stdout}
	session.Stderr = &prefixWriter{prefix: prefix, w: os.Stderr}

	logf("running deploy-server.sh ...")
	if err := session.Run("bash " + remoteDir + "/deploy-server.sh"); err != nil {
		return fmt.Errorf("deploy script: %w", err)
	}
	logf("done")
	return nil
}

func uploadFile(ft *sftp.Client, localPath, remotePath string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := ft.Create(remotePath)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
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
		Timeout:         15 * time.Second,
	}, nil
}

// prefixWriter prefixes every line with the server tag so parallel output
// stays readable.
type prefixWriter struct {
	prefix string
	w      io.Writer
	buf    string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf += string(b)
	for {
		i := strings.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i]
		p.buf = p.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			fmt.Fprintf(p.w, "%s%s\n", p.prefix, line)
		}
	}
	return len(b), nil
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

func findKit(flagValue string) string {
	exe, _ := os.Executable()
	candidates := []string{flagValue}
	if flagValue == "" {
		candidates = []string{
			"server-kit",
			filepath.Join("dist", "server-kit"),
			filepath.Join(filepath.Dir(exe), "server-kit"),
		}
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	fmt.Fprintln(os.Stderr, "server-kit directory not found (deploy-server.sh + tarball + debs)")
	os.Exit(1)
	return ""
}
