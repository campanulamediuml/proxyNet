# proxyNet

一个小工具：双击 exe 后，本机所有公网流量通过 SSH 加密隧道转发到内网工控机（可多机负载），对外只看到你和几台工控机之间的 SSH 流量。

下班关 exe 即恢复普通网络；休眠、断网、服务器宕机都会自动重连/切换。

## 原理

```
本机应用
   ↓
Windows TUN 网卡（sing-box 客户端，劫持全部流量 + DNS）
   ↓
127.0.0.1:10010 SOCKS5
   ↓
SSH 连接池（proxyNet 内置，N 台工控机 × 每台 M 条流，逐连接随机分配）
   ↓
工控机上的 sing-box 服务端（systemd 托管）
   ↓
外网
```

- 每条新连接随机挑一台在线工控机、随机挑一条 SSH 流，单连接内稳定不中断；
- DNS 查询被 sing-box 劫持后同样走隧道，由工控机上的 dnsmasq 转发到公司 DNS，审计 DNS 日志里只会看到工控机在解析；
- `local_subnets` 里的内网网段直连，不进隧道。

## 前置条件

- 本机 Windows 10/11，能 SSH 到各工控机；
- 工控机为 Linux，能访问外网；
- 本机已安装 Go（仅编译时需要）。

## 1. 服务端部署（批量，一键）

确保 `dist\server-kit\` 完整（`deploy-server.sh` + `sing-box-*.tar.gz` + `debs\`），在 `config.json` 里填好 `servers` 列表后，直接运行：

```powershell
.\dist\deploy-server.exe
```

它会读取 `config.json` 的 `servers`，并行给所有机器 SFTP 上传安装包并远程执行部署：安装 sing-box、dnsmasq，注册 systemd 服务（开机自启 + 崩溃自动拉起），装完自动清理安装文件。整个过程不需要目标机器能访问外网。

也可以在某台机器上手动跑（把 `server-kit` 整个目录传上去后）：

```bash
bash /root/server-kit/deploy-server.sh
```

服务端日常管理：

```bash
systemctl status sing-box        # 服务状态
journalctl -u sing-box -f        # 实时日志
```

## 2. 本机编译 proxyNet.exe

在项目目录打开 PowerShell：

```powershell
# 下载 sing-box Windows 核心
.\setup.ps1

# 编译 exe
.\build.bat
```

如果 `setup.ps1` 下载失败，手动下载 `sing-box-1.13.19-windows-amd64.zip`，解压出 `sing-box.exe` 放到 `dist\` 目录，再运行 `build.bat`。

## 3. 配置

在 `dist\` 目录创建 `config.json`：

```json
{
  "listen_port": 10010,
  "tun_interface": "tun0",
  "tun_address": "172.19.0.1/30",
  "tun_mtu": 1400,
  "dns_server": "127.0.0.1",
  "conns_per_server": 4,
  "exclude_interfaces": [],
  "local_subnets": [
    "192.168.0.0/16",
    "10.0.0.0/8",
    "172.16.0.0/12",
    "127.0.0.0/8"
  ],
  "servers": [
    { "server": "192.168.3.33", "port": 10022, "user": "root", "password": "密码1" },
    { "server": "192.168.3.100" },
    { "server": "192.168.3.101", "port": 22, "user": "root", "password": "这台密码不一样就单独写" }
  ]
}
```

- `servers`：工控机列表。每个连接随机挑一台在线的走；离线的自动跳过（只提示一次，后台静默重试，恢复了自动入池）。缺省字段从顶层继承。
- `conns_per_server`：每台工控机维持的并行 SSH 流数量（默认 4，范围 1~32）。多流分摊流量，避免单条 TCP 队头阻塞导致卡顿。
- 顶层 `server/port/user/password/private_key_path` 只是 `servers` 的继承默认值，填了 `servers` 后可以不写。
- 不填 `servers` 则退化为单服务器模式（用顶层字段）。

## 4. 运行

右键 `dist\proxyNet.exe` → **以管理员身份运行**（无窗口，托盘出现 `PN` 图标），或跑 `dist\proxyNet_console.exe` 看实时日志。

右键托盘图标 → **Connect** 建立隧道；**Exit** 退出并恢复网络。

特性：

- **自动重连**：看门狗每 5 秒检查隧道和 sing-box，休眠唤醒/断网/服务器全挂恢复后自动重建；
- **DNS 防泄漏**：连接时物理网卡 DNS 改为黑洞 `127.0.0.1`（IPv6 为 `::1`），断开时从 `dns_backup.json` 恢复；
- **崩溃自愈**：程序被强杀/断电后，下次启动自动恢复 DNS；也可双击 `dist\恢复网络.bat`（等同 `proxyNet.exe -restore`）手动恢复；
- **启动自检**：sing-box 起不来（如 TUN 残留）会直接报错，不会假连接。

验证：连上后 `nslookup zhihu.com` 能出结果，`Get-DnsClientServerAddress` 只有 `tun0` 有 DNS（物理网卡均为 `127.0.0.1`）。

## 注意事项

- 必须以管理员运行（TUN 网卡和路由表修改需要）。
- `dns_server` 指向工控机上的 `dnsmasq` 转发器（`127.0.0.1:53`），DNS 查询加密在 SSH 隧道里，**不会泄漏到公司 DNS**。
- `local_subnets` 里的网段走物理网卡直连（公司内网、WSL/Hyper-V 内部网段等），公网流量（包括 WSL）默认走隧道。
- ping 结果不代表真实连通性（ICMP 过不了 SOCKS，TUN 会本地应答）；验证出口用 `curl https://api.ipify.org`。
- `dns_backup.json` 常驻，记录了各网卡原始 DNS；工控机静态 IP/MAC 绑定的环境下它永远是正确值。

## 文件说明

```
proxyNet/
├── main.go                 # 程序入口、连接编排、看门狗自动重连
├── config.go               # 配置读取（servers 列表 + 字段继承）
├── admin_windows.go        # UAC 提权
├── sshclient/pool.go       # SSH 连接池（多机 × 多流，逐连接随机分配，健康检查）
├── singbox/                # sing-box 配置生成和进程管理
├── route/dns_windows.go    # DNS 黑洞/备份/恢复（含注册表兜底）
├── tray/                   # Windows 系统托盘
├── cmd/deploy/             # 批量部署工具（读 servers 列表，SFTP + 远程执行）
├── cmd/remoterun/          # 远程执行小工具（调试用）
├── setup.ps1               # 下载 Windows sing-box
├── deploy-server.sh        # 服务端一键恢复脚本（离线可用，systemd 托管）
├── build.bat               # 编译脚本
├── dist/server-kit/        # 服务端离线部署包（脚本 + 安装包 + deb）
├── dist/恢复网络.bat        # DNS 手动恢复
└── README.md               # 本文件
```
