#!/bin/bash
# Deploy sing-box SOCKS5 server on 192.168.3.33 using Docker
set -e

VERSION="1.13.19"

if ! command -v docker &> /dev/null; then
    echo "ERROR: docker command not found. Please install Docker first."
    exit 1
fi

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
      "listen_port": 1080
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

# Stop and remove existing container
docker stop sing-box-server 2>/dev/null || true
docker rm sing-box-server 2>/dev/null || true

# Pull and run
docker run -d --name sing-box-server \
  --network host \
  --restart unless-stopped \
  -v /etc/sing-box-server.json:/etc/sing-box/config.json:ro \
  ghcr.io/sagernet/sing-box:v${VERSION} \
  -c /etc/sing-box/config.json run

echo "sing-box server started via Docker on 127.0.0.1:1080"
echo "Check logs: docker logs -f sing-box-server"
