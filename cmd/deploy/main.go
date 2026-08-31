// deploy is a one-click tool that (re)installs the proxyNet server-side
// environment on the Linux box over SSH: sing-box binary, server config,
// a systemd unit with auto-restart, and the dnsmasq DNS forwarder.
// It is idempotent — safe to run again after messing up the server.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type config struct {
	Server   string `json:"server"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
}

const remoteScript = `set -e
VERSION="1.13.19"

echo "=== [1/5] sing-box binary ==="
if ! command -v sing-box >/dev/null 2>&1; then
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64) ARCH=amd64 ;;
        aarch64|arm64) ARCH=arm64 ;;
        *) echo "unsupported arch: $ARCH"; exit 1 ;;
    esac
    F="sing-box-${VERSION}-linux-${ARCH}.tar.gz"
    cd /tmp
    ok=0
    for u in \
        "https://ghproxy.net/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
        "https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
        "https://mirror.ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
        "https://ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}"; do
        echo "trying $u"
        if curl -fL -k --max-time 300 -o "$F" "$u"; then ok=1; break; fi
    done
    [ "$ok" = "1" ] || { echo "download sing-box failed"; exit 1; }
    tar xzf "$F"
    install -m 0755 "sing-box-${VERSION}-linux-${ARCH}/sing-box" /usr/local/bin/sing-box
    rm -rf "$F" "sing-box-${VERSION}-linux-${ARCH}"
fi
sing-box version | head -1

echo "=== [2/5] server config ==="
cat > /etc/sing-box-server.json <<'EOF'
{
  "log": {
    "level": "info"
  },
  "inbounds": [
    {
      "type": "socks",
      "tag": "socks-in",
      "listen": "127.0.0.1",
      "listen_port": 10010
    }
  ],
  "outbounds": [
    {
      "type": "direct",
      "tag": "direct"
    }
  ]
}
EOF
echo written

echo "=== [3/5] systemd unit ==="
cat > /etc/systemd/system/sing-box.service <<'EOF'
[Unit]
Description=sing-box SOCKS5 server (proxyNet)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/sing-box run -c /etc/sing-box-server.json
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

pkill -f "sing-box run" 2>/dev/null || true
systemctl daemon-reload
systemctl enable sing-box
systemctl restart sing-box
echo "sing-box service: $(systemctl is-active sing-box)"

echo "=== [4/5] dnsmasq DNS forwarder ==="

setup_apt_mirror() {
    if grep -q "mirrors.aliyun.com" /etc/apt/sources.list 2>/dev/null; then
        return
    fi
    CODENAME="jammy"
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        CODENAME="${VERSION_CODENAME:-jammy}"
    fi
    cp /etc/apt/sources.list "/etc/apt/sources.list.bak.$(date +%Y%m%d%H%M%S)" 2>/dev/null || true
    cat > /etc/apt/sources.list <<EOF
deb http://mirrors.aliyun.com/ubuntu/ ${CODENAME} main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ ${CODENAME}-updates main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ ${CODENAME}-backports main restricted universe multiverse
deb http://mirrors.aliyun.com/ubuntu/ ${CODENAME}-security main restricted universe multiverse
EOF
    echo "apt mirror switched to mirrors.aliyun.com (${CODENAME})"
}

if ! command -v dnsmasq >/dev/null 2>&1; then
    if ls /root/debs/*.deb >/dev/null 2>&1; then
        echo "offline install dnsmasq from /root/debs"
        dpkg -i /root/debs/dns-root-data_*.deb \
                /root/debs/dnsmasq-base_*.deb \
                /root/debs/dnsmasq_*.deb || true
    fi
fi
if ! command -v dnsmasq >/dev/null 2>&1; then
    setup_apt_mirror
    apt-get update -qq || true
    apt-get install -y -qq dnsmasq || echo "WARN: dnsmasq install failed"
fi
if command -v dnsmasq >/dev/null 2>&1; then
    cat > /etc/dnsmasq.d/proxynet.conf <<'EOF'
# proxyNet DNS forwarder: TCP/UDP on 127.0.0.1:53, forward to company DNS
listen-address=127.0.0.1
bind-interfaces
no-dhcp-interface=lo
no-resolv
server=192.168.3.250
EOF
    systemctl enable dnsmasq 2>/dev/null || true
    systemctl restart dnsmasq || echo "WARN: dnsmasq restart failed"
    echo "dnsmasq service: $(systemctl is-active dnsmasq)"
fi

echo "=== [5/5] verify ==="
sleep 1
ss -tlnp | grep -E ":(53|10010)\s" || echo "WARN: nothing listening on 53/10010"
echo "=== done ==="
`

func main() {
	cfgPath := flag.String("config", "", "path to config.json (default: ./config.json or ./dist/config.json)")
	server := flag.String("server", "", "override server address")
	port := flag.Int("port", 0, "override ssh port")
	user := flag.String("user", "", "override ssh user")
	password := flag.String("password", "", "override ssh password")
	flag.Parse()

	cfg := loadConfig(*cfgPath)
	if *server != "" {
		cfg.Server = *server
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *user != "" {
		cfg.User = *user
	}
	if *password != "" {
		cfg.Password = *password
	}
	if cfg.Server == "" || cfg.User == "" || cfg.Password == "" {
		fmt.Fprintln(os.Stderr, "server/user/password required (put them in config.json)")
		os.Exit(1)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server, cfg.Port)
	fmt.Printf("Deploying to %s as %s ...\n", addr, cfg.User)

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh dial failed: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "new session failed: %v\n", err)
		os.Exit(1)
	}
	defer session.Close()

	session.Stdin = strings.NewReader(remoteScript)
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	if err := session.Run("bash -s"); err != nil {
		fmt.Fprintf(os.Stderr, "\ndeploy failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nDeploy finished.")
}

func loadConfig(path string) *config {
	cfg := &config{Server: "192.168.3.33", Port: 10022, User: "root"}

	candidates := []string{path}
	if path == "" {
		exe, _ := os.Executable()
		exeDir := filepath.Dir(exe)
		candidates = []string{
			"config.json",
			filepath.Join("dist", "config.json"),
			filepath.Join(exeDir, "config.json"),
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
			fmt.Printf("Using config: %s\n", c)
			return cfg
		}
	}
	return cfg
}
