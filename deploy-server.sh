#!/bin/bash

set -e

VERSION="1.13.19"

if [ "$(id -u)" != "0" ]; then
    echo "请用 root 运行"
    exit 1
fi

echo "=== [1/5] sing-box 二进制 ==="
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) echo "不支持的架构: $ARCH"; exit 1 ;;
esac
F="sing-box-${VERSION}-linux-${ARCH}.tar.gz"

# 优先：脚本同目录下的安装包 > /root/sing-box > GitHub 下载
if ! command -v sing-box >/dev/null 2>&1; then
    if [ -f "$SCRIPT_DIR/$F" ]; then
        echo "使用脚本目录下的安装包: $SCRIPT_DIR/$F"
        tar xzf "$SCRIPT_DIR/$F" -C /tmp
        install -m 0755 "/tmp/sing-box-${VERSION}-linux-${ARCH}/sing-box" /usr/local/bin/sing-box
        rm -rf "/tmp/sing-box-${VERSION}-linux-${ARCH}"
    elif [ -x /root/sing-box ]; then
        echo "发现 /root/sing-box，直接安装"
        install -m 0755 /root/sing-box /usr/local/bin/sing-box
        rm -f /root/sing-box
    else
        cd /tmp
        ok=0
        for u in \
            "https://ghproxy.net/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
            "https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
            "https://mirror.ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
            "https://ghproxy.com/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}" \
            "https://github.moeyy.xyz/https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${F}"; do
            echo "尝试 $u"
            if curl -fL -k --max-time 300 -o "$F" "$u"; then ok=1; break; fi
        done
        [ "$ok" = "1" ] || { echo "sing-box 下载失败"; exit 1; }
        tar xzf "$F"
        install -m 0755 "sing-box-${VERSION}-linux-${ARCH}/sing-box" /usr/local/bin/sing-box
        rm -rf "$F" "sing-box-${VERSION}-linux-${ARCH}"
    fi
fi
sing-box version | head -1

echo "=== [2/5] 服务端配置 ==="
cat > /etc/sing-box-server.json <<'EOF'
{
  "log": {
    "level": "warn"
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

echo "=== [3/5] systemd 服务 ==="
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

# 清掉以前 nohup 手动起的残留，以及可能存在的旧版 sing-box-server.service
pkill -f "sing-box run" 2>/dev/null || true
if systemctl list-unit-files | grep -q "sing-box-server.service"; then
    systemctl stop sing-box-server.service 2>/dev/null || true
    systemctl disable sing-box-server.service 2>/dev/null || true
    rm -f /etc/systemd/system/sing-box-server.service
fi

systemctl daemon-reload
systemctl enable sing-box
systemctl restart sing-box
echo "sing-box service: $(systemctl is-active sing-box)"

echo "=== [4/5] dnsmasq DNS 转发器 ==="

# 公司网络访问不了官方 Ubuntu 源时，换阿里云镜像（只换一次，原文件自动备份）
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
    echo "apt 源已切换为 mirrors.aliyun.com (${CODENAME})"
}

# 优先：脚本同目录 debs/ 下的离线 deb 包 > apt 安装（新机器可能完全出不了网）
if ! command -v dnsmasq >/dev/null 2>&1; then
    if ls "$SCRIPT_DIR"/debs/*.deb >/dev/null 2>&1; then
        echo "使用本地 deb 包离线安装 dnsmasq"
        dpkg -i "$SCRIPT_DIR"/debs/dns-root-data_*.deb \
                "$SCRIPT_DIR"/debs/dnsmasq-base_*.deb \
                "$SCRIPT_DIR"/debs/dnsmasq_*.deb || true
    fi
fi
if ! command -v dnsmasq >/dev/null 2>&1; then
    setup_apt_mirror
    apt-get update -qq || true
    apt-get install -y -qq dnsmasq || echo "WARN: dnsmasq 安装失败"
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
    systemctl restart dnsmasq || echo "WARN: dnsmasq 重启失败"
    echo "dnsmasq service: $(systemctl is-active dnsmasq)"
fi

echo "=== [5/5] 验证 ==="
sleep 1
ss -tlnp | grep -E ":(53|10010)\s" || echo "WARN: 53/10010 没有监听"
echo "=== 恢复完成 ==="

# 成功后自毁：删掉安装包、deb 包和脚本自己，下次直接往里拷新文件即可。
# （脚本若中途失败则不会执行到这里，文件保留以便重跑）
echo "=== 清理安装文件 ==="
rm -f "$SCRIPT_DIR/$F"
rm -rf "$SCRIPT_DIR/debs"
rm -f "$0"
[ "$SCRIPT_DIR" != "/root" ] && rmdir "$SCRIPT_DIR" 2>/dev/null || true
