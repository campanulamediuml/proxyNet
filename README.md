# proxyNet

一个小工具：双击 exe 后，本机所有流量通过 SSH 加密隧道转发到内网工控机（192.168.3.33），对外只看到 `172.16.13.16 ↔ 192.168.3.33:22` 的 SSH 流量。

下班关 exe 即恢复普通网络。

## 原理

```
本机应用
   ↓
Windows TUN 网卡（sing-box 客户端）
   ↓
127.0.0.1:1080 SOCKS5
   ↓
SSH 本地端口转发（proxyNet 内置）
   ↓
192.168.3.33:22 加密 SSH 流量
   ↓
3.33 上的 sing-box 服务端 SOCKS5
   ↓
外网
```

## 前置条件

- 本机 Windows 10/11，能 SSH 到 `192.168.3.33`
- 3.33 是一台 Linux 工控机，能访问外网
- 本机已安装 Go（仅编译时需要）

## 1. 在 3.33 上部署 sing-box 服务端

把 `deploy-server.sh` 上传到 3.33，然后执行：

```bash
chmod +x deploy-server.sh
./deploy-server.sh
```

脚本会自动下载 sing-box、写入服务端配置并后台启动。

如果下载失败，手动下载对应版本的 `sing-box-1.13.19-linux-amd64.tar.gz`，解压后把 `sing-box` 放到 `/usr/local/bin/`，然后运行：

```bash
nohup sing-box run -c /etc/sing-box-server.json > /var/log/sing-box.log 2>&1 &
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
  "server": "192.168.3.33",
  "port": 10022,
  "user": "root",
  "password": "你的root密码",
  "private_key_path": "",
  "listen_port": 1080,
  "tun_interface": "tun0",
  "tun_address": "172.19.0.1/30",
  "tun_mtu": 1400,
  "dns_server": "127.0.0.1",
  "exclude_interfaces": [],
  "local_subnets": [
    "192.168.0.0/16",
    "10.0.0.0/8",
    "172.16.0.0/12",
    "127.0.0.0/8"
  ]
}
```

**建议用私钥登录**，把 `private_key_path` 写成私钥路径，`password` 留空。

## 4. 运行

右键 `dist\proxyNet.exe` → **以管理员身份运行**。

系统托盘会出现一个 `PN` 图标，右键点击 **Connect** 即可建立隧道。

访问 `https://ip.sb` 验证，显示的应该是 3.33 的出口 IP。

下班时右键托盘图标 → **Exit**，网络自动恢复。

## 注意事项

- 必须以管理员运行（TUN 网卡和路由表修改需要）。
- 第一次运行时如果没有 `config.json`，程序会生成 `config.json.example`，你改好密码后重命名为 `config.json`。
- `dns_server` 默认指向 3.33 上的 `dnsmasq` 转发器（`127.0.0.1:53`），DNS 查询会加密在 SSH 隧道里，**不会泄漏到公司 DNS**。
- `servers` 数组可填多台工控机（缺省字段继承顶层 `port/user/password`）。填多台时每个新连接随机挑一台走，某台挂了自动跳过并重连；填一台或不填则退化为原来的单服务器模式。
- `local_subnets` 里的网段会走物理网卡直连（公司内网、WSL/Hyper-V 内部网段等），公网流量（包括 WSL）默认也走隧道。如果 WSL 出现异常，可把对应接口名加入 `exclude_interfaces` 排查。
- 如果程序崩溃或被强杀导致断网：再次启动程序会自动恢复 DNS；或双击 `dist\恢复网络.bat`（等同 `proxyNet.exe -restore`）。`dns_backup.json` 会常驻，启动时按备份恢复仍在的网卡。
- 审计侧看不到具体域名，但能看到你的机器和 3.33 之间有大量持续 SSH 流量。

## 文件说明

```
proxyNet/
├── main.go                 # 程序入口
├── config.go               # 配置读取
├── admin_windows.go        # UAC 提权
├── sshclient/              # SSH 本地端口转发
├── singbox/                # sing-box 配置生成和进程管理
├── tray/                   # Windows 系统托盘
├── setup.ps1               # 下载 Windows sing-box
├── deploy-server.sh        # 在 3.33 部署 sing-box 服务端
├── build.bat               # 编译脚本
└── README.md               # 本文件
```
