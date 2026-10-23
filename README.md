# OpenCode Relay Server

[中文](README.md) | [English](README.en.md)

<div align="center">

**通过加密隧道，从任何地方安全访问你的 OpenCode 实例**

为 [OpenCode Mobile](https://github.com/thntime/opencode-mobile) 提供 Relay 基础设施：把你的 AI 编码助手安全地带到任何网络。

[![GitHub Stars](https://img.shields.io/github/stars/thntime/opencode-relay-server?style=for-the-badge)](https://github.com/thntime/opencode-relay-server)
[![Docker](https://img.shields.io/badge/docker-thntime%2Fopencode--relay--server-blue?style=for-the-badge&logo=docker)](https://hub.docker.com/r/thntime/opencode-relay-server)
[![npm](https://img.shields.io/badge/npm-%40thntime%2Fopencode--tunnel-cb3837?style=for-the-badge&logo=npm)](https://www.npmjs.com/package/@thntime/opencode-tunnel)
[![License](https://img.shields.io/badge/license-MIT-blue?style=for-the-badge)](https://opensource.org/licenses/MIT)

</div>

---

## 项目简介 / Overview

本仓库包含两个组件：

1. **Relay Server** — 基于 Go 的 WebSocket 中继服务器，可部署到 Azure、Fly.io、Docker 或自托管服务器。
2. **Tunnel Client** — 运行在你电脑上的 CLI 工具，负责建立并维护安全隧道。

Relay 只负责转发加密数据，不持有你的端到端密钥，也不读取你的 OpenCode 内容。

## 架构 / Architecture

```text
┌─────────────────┐         E2E         ┌─────────────────┐         E2E         ┌─────────────────┐
│                 │◄───────────────────►│                 │◄───────────────────►│  tunnel-client  │
│  OpenCode App   │       加密传输       │  Relay Server   │       加密传输       │                 │
│                 │                     │ (Azure/Fly/自托管)│                     │                 │
└─────────────────┘                     └─────────────────┘                     └────────┬────────┘
                                                                                         │
                                                                                         │ HTTP
                                                                                         ▼
                                                                                ┌─────────────────┐
                                                                                │ OpenCode Server │
                                                                                │   (localhost)   │
                                                                                └─────────────────┘
```

### 工作流程

1. `tunnel-client` 通过 WebSocket 连接到 Relay Server。
2. App 把加密后的请求发送给 Relay Server。
3. Relay Server 按主机标识把加密数据转发给对应的 `tunnel-client`。
4. `tunnel-client` 解密请求并转发到本机 OpenCode。
5. 响应沿相同路径返回，全程保持端到端加密。

### 安全模型

| 层次 | 保护方式 |
|------|----------|
| **传输** | 所有组件之间使用 TLS 1.3（WSS/HTTPS） |
| **载荷** | AES-256-GCM 端到端加密 |
| **密钥交换** | 主机生成 256 位密钥，用口令派生密钥包裹后上报；Relay 只保存包裹后的材料 |
| **认证** | JWT 令牌 + 每台设备独立凭据 |
| **零知识** | Relay 只看到加密后的数据块，无法解密 |

### 端到端加密如何工作

1. `tunnel-client` 为主机生成一个随机的 256 位 AES 密钥。
2. 密钥使用口令派生的 KEK（scrypt，N=2^15、r=8、p=1）包裹，`device.json` 只保存 `key_wrapped`、`key_salt` 和 `key_kdf`。
3. 客户端通过 `PUT /api/hosts/{hostId}/key` 上报包裹后的密钥材料。
4. App 通过 `GET /api/hosts/{hostId}/key` 获取材料，并在本地用口令解包。
5. Relay 永远不接收明文密钥；没有提供口令时，客户端会跳过密钥上报并打印警告，此时隧道可用但端到端加密未启用。

## 安装与发行 / Installation & Distribution

三种获取方式按用途区分：

| 用途 | 渠道 | 命令 / 入口 |
|------|------|-------------|
| 安装 tunnel client | npm | `npm install -g @thntime/opencode-tunnel` |
| 手动下载客户端 | GitHub Releases | [Releases](https://github.com/thntime/opencode-relay-server/releases) |
| 运行 Relay Server | Docker Hub | `docker pull thntime/opencode-relay-server:latest` |

### 通过 npm 安装客户端

需要 [Node.js](https://nodejs.org/) 14+，支持 macOS、Linux 和 Windows：

```bash
npm install -g @thntime/opencode-tunnel
opencode-tunnel
```

`postinstall` 会根据当前平台和包版本，从本仓库对应的 GitHub Release 下载 `tunnel-client` 二进制。

> 如果 npm scope 不是 `@thntime`，请把 `npm/opencode-tunnel/package.json` 的 `name` 和本文档中的安装命令一起替换。

### 从 GitHub Releases 下载

每次推送 `v*` 标签都会生成以下附件：

- `tunnel-client-linux-amd64`
- `tunnel-client-linux-arm64`
- `tunnel-client-darwin-amd64`
- `tunnel-client-darwin-arm64`
- `tunnel-client-windows-amd64.exe`
- `SHA256SUMS`

下载后按平台重命名并赋予执行权限即可：

```bash
chmod +x tunnel-client-darwin-arm64
./tunnel-client-darwin-arm64
```

### 通过 Docker Hub 运行服务端

```bash
docker pull thntime/opencode-relay-server:latest

docker run -d --name opencode-relay \
  -p 8080:8080 \
  -e PORT=8080 \
  -e JWT_SECRET='replace-with-a-long-random-secret' \
  -e DATABASE_PATH=/home/data/relay.db \
  -v relay-data:/home/data \
  thntime/opencode-relay-server:latest
```

镜像支持 `linux/amd64` 与 `linux/arm64`，标签包含 `latest`、语义化版本（如 `1.2.3`、`1.2`）和短 SHA。

> 镜像内默认 `PORT=3000`；示例显式设置为 `8080` 以匹配 `EXPOSE 8080`。如果省略 `-e PORT=8080`，请把端口映射改为 `3000:3000`。

## 快速开始 / Quick Start

### 前置条件

- [OpenCode Mobile](https://github.com/thntime/opencode-mobile) 应用，并已在应用中创建账号
- 电脑上已安装 [OpenCode](https://github.com/sst/opencode)
- 通过 npm 安装时需要 Node.js 14+

### 首次运行

```bash
# 安装并启动
npm install -g @thntime/opencode-tunnel
opencode-tunnel
```

首次运行会依次完成：

1. **登录** — 输入邮箱和密码，或通过 `--token` / `OPENCODE_RELAY_TOKEN` 提供令牌。
2. **注册主机** — 调用 `POST /api/devices` 注册主机，把稳定身份写入 `device.json`。
3. **包裹密钥** — 使用 `--passphrase`、`OPENCODE_TUNNEL_PASSPHRASE` 或交互式输入的口令包裹 E2E 密钥，并上报包裹材料。
4. **连接 App** — 在 App 中登录同一账号，选择这台主机，输入口令解包密钥。
5. **建立隧道** — 连接成功后，你可以从任何网络访问本机 OpenCode。

客户端还会：

- 在需要时自动启动 OpenCode
- 配置开机自启（launchd / systemd / Windows Task Scheduler）
- 断线后自动重连

## Tunnel Client 使用 / Tunnel Client Usage

```bash
# 省略 start（默认命令）
opencode-tunnel -relay https://your-relay.example.com -port 4096

# 显式 start（与上面完全等价）
opencode-tunnel start -relay https://your-relay.example.com -port 4096

# 查看状态
opencode-tunnel status

# 发现本机正在运行的 OpenCode 实例
opencode-tunnel discover

# 退出登录并清除凭据
opencode-tunnel logout

# 查看版本
opencode-tunnel --version

# 查看帮助
opencode-tunnel help
```

> `opencode-tunnel` 是 npm 安装后的命令名（`npm install -g @thntime/opencode-tunnel`）。
> 从源码构建时二进制名为 `tunnel-client`，参数与子命令完全相同，把命令名替换即可。

受支持的 flag：`-port`、`-relay`、`--name`、`--token`、`--email`、`--password`、`--reset`、`--attach`、`--install`、`--passphrase`、`--rewrap`（说明见下方参数表）。省略 `start` 时，第一个以 `-` 开头的参数会被当作启动参数；未知 token 会被忽略，**flag 拼错不会报错**，只会被静默跳过，因此请以上表为准。

> `opencode-tunnel pair` 仍保留用于兼容旧版配对流程；正常使用 `start` 时会自动注册主机，不需要扫码。

### 参数

| 参数 | 说明 |
|------|------|
| `-relay, --relay <url>` | Relay 地址（默认：`https://opencode-relay.azurewebsites.net`） |
| `-port, --port <port>` | 本地 OpenCode 端口（默认 `4096`，或读取 `opencode.json` 的 `port`） |
| `--name <name>` | 主机显示名（默认：操作系统主机名） |
| `--token <token>` | Relay API 令牌（覆盖已保存的凭据） |
| `--email <email>` | 登录邮箱 |
| `--password <pass>` | 登录密码 |
| `--passphrase <pass>` | E2E 密钥口令，用于包裹主机密钥（不会落盘） |
| `--rewrap` | 使用新口令重新包裹 E2E 主机密钥 |
| `--reset` | 丢弃本地身份并重新注册主机 |
| `--attach` | 复用已在 `--port` 上运行的 `opencode serve`；只探测端口（2 秒超时），失败时给出提示而不是启动新进程 |
| `--install` | 校验身份与包裹密钥后安装开机自启项并退出（不启动隧道） |

### `--install` 说明

`--install` 只负责配置开机自启；执行前必须先完成主机注册和密钥包裹：

- 如果 `device.json` 不存在、缺少 `Host ID` 或没有包裹密钥，客户端会先在前台执行 `resolveAuth → ensureHost → prepareHostKey` 流程，再写入 launchd / systemd / Task Scheduler 配置。
- 没有口令（`--passphrase`、`OPENCODE_TUNNEL_PASSPHRASE` 或交互式输入）且没有 TTY 时，`--install` 会以非零状态失败，而不是静默安装一个无法上报 E2E 密钥的主机。
- 普通 `start` 在没有口令时仍可运行，但会跳过 E2E 密钥上报并打印警告（`未提供 E2E 口令…跳过密钥上报；隧道仍可建立`）；在密钥上报前，隧道本身不启用端到端加密。

在 Linux 上，`--install` 写入 `~/.config/systemd/user/opencode-relay.service`，并执行 `systemctl --user enable`。unit 会把**解析后的** relay 地址与本地复用模式写进 `ExecStart`，例如：

```ini
[Service]
ExecStart=/home/you/.local/bin/opencode-tunnel start -relay https://your-relay.example.com --attach -port 4096
Restart=always
RestartSec=5
```

- 未显式指定 `-relay` 时写入的是默认地址，而不是省略该参数。
- `--attach` 只在安装时使用了它才会出现。
- 使用 `--attach` 自启时，开机后必须已有 OpenCode 在监听 `--port`（示例中为 4096）；否则客户端只会探测失败并退出，配合 `Restart=always` 就会变成无限重启循环。请保证 OpenCode 先于该服务启动，例如为 OpenCode 单独建一个 systemd unit，并让本服务用 `After=`（以及必要时 `Wants=`）依赖它。
- 换 relay 地址或改变 attach 方式后，需要重新执行 `--install`；已安装的 unit 不会自动更新。
- 注销后仍要运行，必须先允许 linger：

```bash
loginctl enable-linger "$USER"
systemctl --user status opencode-relay.service
```

- 口令不会写入 unit。需要在自启时提供口令，可在 unit 中加入指向受限文件的 `EnvironmentFile`（文件权限 `600`，内容形如 `OPENCODE_TUNNEL_PASSPHRASE=change-me`）：

```ini
[Service]
EnvironmentFile=%h/.config/opencode-tunnel/env
```

重新执行 `--install` 会覆盖 unit，手动添加的 `EnvironmentFile` 需要重新加回。

### 配置文件

位于 `~/.opencode-tunnel/`：

| 文件 | 用途 |
|------|------|
| `auth.json` | 登录凭据（JWT 令牌、邮箱） |
| `device.json` | 主机身份（host ID、subdomain、认证信息）和包裹后的 E2E 密钥材料 |
| `opencode.json` | OpenCode 自动启动配置 |

## 自托管 Relay Server / Self-Hosting the Relay Server

### 前置条件

- Go 1.24+
- 一台有公网 IP 的服务器，或使用 Cloudflare Tunnel

### 构建与运行

```bash
# 克隆仓库
git clone https://github.com/thntime/opencode-relay-server.git
cd opencode-relay-server

# 构建
go build -o server ./cmd/server

# 运行
PORT=8080 JWT_SECRET=your-secret ./server
```

也可以直接使用 Docker Hub 镜像，见[通过 Docker Hub 运行服务端](#通过-docker-hub-运行服务端)。

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `3000` | 服务端监听端口；Docker 示例显式设置为 `8080` |
| `JWT_SECRET` | `change-me-in-production` | JWT 签名密钥；生产环境必须替换为足够长的随机值 |
| `DATABASE_PATH` | `data/relay.db` | SQLite 数据库路径 |
| `SINGLE_USER_MODE` | `false` | 设为 `true` 时跳过邮箱验证 |
| `RELAY_LEGACY_PROXY` | `false` | 重新启用已弃用且无认证的 `/proxy/*` 路由 |
| `RESEND_API_KEY` | 空 | Resend 邮件服务密钥，用于发送验证码 |
| `EMAIL_FROM` | `OpenCode <noreply@liuyao16.dpdns.org>` | 验证邮件发件人 |
| `ADMIN_EMAIL` | 空 | 管理员邮箱，可选；仅在空库首次启动时用于创建管理员，语义见[管理员账号](#管理员账号) |
| `ADMIN_PASSWORD` | 空 | 管理员密码，可选；仅在空库首次启动时用于创建管理员 |
| `FRPS_HOST` | 空 | **下发给客户端**的 frps 主机名；未设置时 frpc-config 返回 503 |
| `FRPS_PORT` | 空 | **下发给客户端**的 frps 端口；未设置时 frpc-config 返回 503 |
| `FRPS_TOKEN` | 空 | frp 认证令牌 |
| `FRPS_DOMAIN` | 空 | **下发给客户端**的 frp 域名；未设置时 frpc-config 返回 503 |

> `FRPS_HOST` / `FRPS_PORT` / `FRPS_DOMAIN` 描述的是**客户端要连接的 frps 地址**，不是 relay 容器自身监听的地址。relay 只把这三个值原样写进下发的 frpc 配置。

### 从旧版本升级（BREAKING）

这三个变量的默认值已从内置值（`127.0.0.1` / `7000` / 上游作者的域名）改为**空**。升级后如果未显式配置：

- `GET /api/devices/{id}/frpc-config` 返回 **503**，响应中会指出缺失的变量名；客户端因此拿不到 frpc 配置，隧道无法建立。
- 设备不存在仍然返回 **404**，与「服务端配置缺失」区分开。

内置默认值属于上游作者的部署环境，对任何自托管实例都是错的地址，所以这里选择显式失败而不是静默下发。最小配置如下（完整文件见仓库内 `docker-compose.example.yml`）：

```yaml
services:
  relay:
    image: thntime/opencode-relay-server:latest
    container_name: opencode-relay
    ports:
      - "14096:8080"
    environment:
      PORT: "8080"
      JWT_SECRET: change-me
      ADMIN_EMAIL: admin@example.com
      ADMIN_PASSWORD: change-me
      FRPS_HOST: frps.example.com
      FRPS_PORT: "7000"
      FRPS_DOMAIN: example.com
    volumes:
      - relay-data:/home/data

volumes:
  relay-data:
```

`FRPS_HOST` 填客户端可访问的 frps 地址，`FRPS_PORT` 是 frps 的 `bind_port`，`FRPS_DOMAIN` 是 frps 的 `subdomain_host`。

> 上面沿用示例的端口映射：宿主机 **14096** → 容器 **8080**（healthcheck 在容器内访问 8080，不受影响）。如果你改成了 `8080:8080`，下面命令里的 `14096` 也相应换成 `8080`。

重新部署并确认：

```bash
# 1. 参考 docker-compose.example.yml 准备自己的 docker-compose.yml，填入真实值
# 2. 重新创建容器使新环境变量生效
docker compose up -d

# 3. 确认健康状态
docker compose ps

# 4. 用真实设备 ID 与令牌确认下发结果（配置正确时返回 200 与 frps 地址）
#    14096 是示例映射的宿主端口；换成你自己的宿主端口
curl -i -H "Authorization: Bearer $RELAY_TOKEN" \
  http://127.0.0.1:14096/api/devices/<device-id>/frpc-config
```

### 容器代理与健康检查

如果宿主机通过 `HTTP_PROXY` / `HTTPS_PROXY` 让容器走代理，BusyBox `wget` 的 healthcheck 也会走代理——即使设置了 `NO_PROXY=127.0.0.1` 也不生效，因为 BusyBox `wget` 不读取 `NO_PROXY`。典型表现是：宿主机访问 `/api/health` 正常，容器却一直是 `unhealthy`，健康检查日志里出现代理返回的 502。

`docker-compose.example.yml` 的 healthcheck 用 `wget -Y off` 让**这一条命令**绕过代理，容器内其他出站请求（发邮件、访问 frps 等）仍按原代理设置走：

```yaml
healthcheck:
  test: ["CMD", "wget", "-Y", "off", "-qO-", "http://127.0.0.1:8080/api/health"]
  interval: 30s
  timeout: 5s
  retries: 3
```

排查与验证：

```bash
# 容器健康状态
docker inspect --format '{{.State.Health.Status}}' opencode-relay

# 绕过代理直接请求健康端点（应输出 JSON）
docker exec opencode-relay wget -Y off -qO- http://127.0.0.1:8080/api/health

# 对照：不带 -Y off 时，若容器内配置了代理，通常会失败或返回代理错误
docker exec opencode-relay wget -qO- http://127.0.0.1:8080/api/health
```

`-Y off` 依赖 BusyBox `wget` 的参数；运行时镜像基于 Alpine，更换基础镜像时需要重新确认该参数仍可用。

### 管理员账号

`ADMIN_EMAIL` / `ADMIN_PASSWORD` 只在**空库首次启动**时创建管理员。服务端的 `EnsureAdminUser` 按邮箱判断用户是否存在：邮箱不存在就创建，存在就什么都不做。

因此：

- 之后修改 `ADMIN_EMAIL` 并重启，会**新增**一个用户，而不是把已有管理员改成新邮箱；旧用户仍然保留，旧邮箱仍可登录。
- 修改 `ADMIN_PASSWORD` 对已存在的邮箱**不会**更新密码。

需要修正已有账号时，用服务端二进制内置的 `users` 子命令（直接操作数据库，不经过 HTTP；需要数据库凭据，属于运维操作）：

```bash
# 列出所有用户
docker exec opencode-relay /app/opencode-relay-server users list

# 修改邮箱（old@example.com 必须已存在）
docker exec opencode-relay /app/opencode-relay-server users set-email old@example.com new@example.com

# 重置密码（交互式输入两次，不回显）
docker exec -it opencode-relay /app/opencode-relay-server users set-password new@example.com

# 脚本中使用 --password（注意 shell 历史与进程列表会暴露口令）
docker exec opencode-relay /app/opencode-relay-server users set-password new@example.com --password 'change-me'
```

镜像内的二进制路径为 `/app/opencode-relay-server`（见 `Dockerfile`）。从源码自行构建时二进制名可能不同（例如 `go build -o server ./cmd/server`），请把命令中的路径换成镜像内实际路径。SQLite 与 MongoDB 两种后端都支持。

用 `users set-email` 改完邮箱后，还有两处需要一起处理，否则会“看起来改了、实际没改”：

- **同步 compose 里的 `ADMIN_EMAIL`**。如果环境变量仍是旧邮箱，下次重启时 `EnsureAdminUser` 会认为旧邮箱不存在，**再建一个用户**，于是数据库里同时存在新旧两个账号。改完邮箱后立刻把 `ADMIN_EMAIL` 更新为新值（`ADMIN_PASSWORD` 只在创建用户时使用，改邮箱不涉及它）。
- **客户端重新登录**。客户端 `~/.opencode-tunnel/auth.json` 里记录的仍是旧邮箱：JWT 令牌不受影响，隧道照常可用，但 `opencode-tunnel status` 显示的邮箱与账号实际邮箱对不上，后续需要重新登录时也会用旧邮箱失败。先 `opencode-tunnel logout` 清掉本地凭据，再执行 `opencode-tunnel start --email new@example.com`（交互输入密码，或加 `--password`）刷新 `auth.json`。

### 部署到 fly.io

```bash
fly launch
fly secrets set JWT_SECRET=your-secret
fly deploy
```

仓库中的 `fly.toml` 是一个可用的起点；请按自己的应用名、区域和域名调整。

### 使用 nginx 反向代理

`deploy/nginx-relay.conf` 提供了 `/relay/` 的反向代理配置，包含流式响应和 WebSocket 所需设置（HTTP/1.1 上游、`Upgrade`/`Connection`、关闭缓冲、延长超时）。`map $http_upgrade $connection_upgrade` 必须放在 `http` 块中，不能放在 `server` 块内。

示例配置中的域名和证书路径需要替换为你自己的域名。该配置尚未在装有 nginx 的机器上验证；上线前请在目标主机运行 `nginx -t` 并做一次端到端验证。

### 更新客户端默认 Relay 地址

自托管后，让客户端指向你的 Relay：

```bash
opencode-tunnel -relay https://your-relay.example.com
```

## API 端点 / API Endpoints

### 认证

| 方法 | 端点 | 说明 |
|------|------|------|
| POST | `/api/send-verification` | 发送邮箱验证码 |
| POST | `/api/register` | 注册账号 |
| POST | `/api/login` | 登录并获取 JWT |
| POST | `/api/auto-login` | 刷新令牌 |

### 设备

| 方法 | 端点 | 说明 |
|------|------|------|
| GET | `/api/devices` | 列出用户的设备 |
| POST | `/api/devices` | 注册新设备（`name` 可选，默认 `OpenCode Host`） |
| GET | `/api/devices/{id}` | 获取设备详情 |
| PUT | `/api/devices/{id}` | 更新设备 |
| DELETE | `/api/devices/{id}` | 删除设备 |
| GET | `/api/heartbeat` | 设备心跳 |

### 主机

主机是用户的机器，`id` 是稳定主机标识（设备的 `subdomain`）。

| 方法 | 端点 | 说明 |
|------|------|------|
| GET | `/api/hosts` | 列出调用者的主机（`id`、`name`、`online`、`last_seen`） |
| GET | `/api/hosts/{hostId}/key` | 获取包裹后的 E2E 密钥材料 |
| PUT | `/api/hosts/{hostId}/key` | 上报包裹后的 E2E 密钥材料 |

以上端点都需要 `Authorization: Bearer <jwt>`，且只返回该用户拥有的主机。

### 配对

| 方法 | 端点 | 说明 |
|------|------|------|
| POST | `/api/pairing` | 创建配对请求 |
| GET | `/api/pairing/{id}/status` | 轮询配对状态 |
| POST | `/api/pairing/{id}/complete` | 完成配对 |

### 隧道

| 方法 | 端点 | 说明 |
|------|------|------|
| GET | `/api/tunnel/{subdomain}` | WebSocket 隧道连接 |
| GET | `/api/events/{subdomain}` | SSE 事件转发 |
| ANY | `/proxy/*` | 已弃用的 HTTP 代理；除非 `RELAY_LEGACY_PROXY=true`，否则返回 410 |

### Relay 路由

| 方法 | 端点 | 说明 |
|------|------|------|
| ANY | `/relay/{hostId}/{path...}` | 把已认证请求转发到该主机的隧道 |

`/relay/...` 需要 `Authorization: Bearer <jwt>`，并校验 `hostId` 属于调用者。返回码：

- `401`：缺少或无效令牌
- `403`：主机属于其他用户
- `404`：未知主机
- `503`：主机离线

`Authorization` 头会被 Relay 消费，不会转发给主机。Relay 还会剥离 `Cookie`、`X-Real-IP` 和所有 `X-Forwarded-*` 头，因此主机上的 OpenCode 收到的请求不携带任何凭据。

有效的 Relay JWT 等价于对该用户所有主机的完全访问权。JWT 有效期为 30 天，且没有吊销机制；如果令牌泄露，轮换 `JWT_SECRET` 是唯一的补救方式。

### 流式与 WebSocket

协议 v2 在原有请求/响应帧之上增加了增量响应流（SSE）和 WebSocket 转发（隧道化终端 PTY）。两者都复用同一条隧道 WebSocket，不需要新端点或新端口。

#### 帧

| 帧 | 方向 | 用途 |
|----|------|------|
| `res-head` | client → relay | 上游响应的状态码和头；在第一个数据块之前发送一次 |
| `res-chunk` | client → relay | 一个响应体数据块（`seq` + base64 `data`），按顺序投递 |
| `res-end` | client → relay | 正常结束响应流 |
| `res-error` | client → relay | 中止流；携带可读的 `message` |
| `req-cancel` | relay → client | 消费方已断开；客户端取消本地请求 |
| `ws-open` | relay → client | 要求客户端连接 `ws://127.0.0.1:<port><path>`，并转发原始头和子协议 |
| `ws-ack` | client → relay | 连接结果：`ok`、`status`、`error`、`subprotocol` |
| `ws-frame` | 双向 | 一条 WebSocket 消息（opcode + base64 `data`），双向转发 |
| `ws-close` | 双向 | 关闭 WebSocket；原样传递 `code` 和 `reason` |

#### 能力协商

客户端在隧道握手请求中发送 `X-Tunnel-Protocol: 2`；Relay 在**握手响应头**中回显相同版本。如果响应中没有该头，客户端保持 v1（整包 `io.ReadAll`），这也是检测 Relay 降级的方式。v1 连接上收到流式请求（`Accept: text/event-stream` 或 `Upgrade: websocket`）时，Relay 返回 **501** 并提示升级客户端。

#### 限制

| 限制 | 值 | 超限行为 |
|------|----|----------|
| 单帧 | 1 MiB（base64 前） | 413，或 WebSocket 关闭码 1009 |
| 每连接并发流 | 16 | 429，或 WebSocket 关闭码 1011 |
| 每流发送缓冲 | 32 帧或 4 MiB | 关闭流并通知主机停止 |
| 慢消费者 | 30 秒无写入进展 | 关闭流 |

这些值目前是编译期常量；有真实压力数据后再改为可配置。

#### HTTP/1.1 要求

WebSocket 转发使用 `http.Hijacker`，HTTP/2 无法提供该能力。因此 Relay 要求 HTTP/1.1，并在 `ProtoMajor != 1` 时返回 **426**。请在反向代理处终止 TLS，并让代理以 HTTP/1.1 访问 Relay（参见[使用 nginx 反向代理](#使用-nginx-反向代理)）；nginx 侧的 `listen ... http2` 对这一跳没问题，因为 nginx 会以 HTTP/1.1 重新发起上游请求。注意 nginx 无法升级通过 HTTP/2 到达的 WebSocket，所以手机端发起 WebSocket 请求时必须使用 HTTP/1.1。

#### 端到端加密

主机上报密钥后，`res-chunk` 和 `ws-frame` 的载荷会逐帧使用 AES-256-GCM 加密。`res-head` 携带 `X-E2E-Stream: 1`，`ws-ack` 报告 `encrypted=true`，App 据此逐帧解密。没有密钥时，流为明文，与 v1 载荷一致；Relay 始终不解密，只按 `id` 路由帧。

#### 回滚

把 Relay 回滚到 v1 构建是安全的：客户端在握手响应中找不到 `X-Tunnel-Protocol`，会自动回退到 `handleRequestLegacy`（整包）。WebSocket 和流式请求会得到 501/426，普通请求不受影响，不涉及数据迁移。

#### nginx

必须关闭 `proxy_buffering` 并延长 `proxy_read_timeout` / `proxy_send_timeout`。开启缓冲时，nginx 会等到响应结束才发送 SSE 事件，表现为事件流卡住；使用旧的 130 秒超时时，任何空闲超过该时间的流或终端会话都会被截断。参见 `deploy/nginx-relay.conf`。

### 从 `/proxy/` 迁移

1. 部署带新 schema（自动新增三个可空密钥列）和 `/relay/` 路由的 Relay。
2. 升级 tunnel client；它们仍可通过 `/api/tunnel/{subdomain}` 工作。
3. 把移动端请求指向 `/relay/{hostId}/...`。
4. 只有在仍有旧客户端需要 `/proxy/` 时，才在一个版本内设置 `RELAY_LEGACY_PROXY=true`。旧模式仍会在转发前剥离 `Authorization`、`Cookie`、`X-Real-IP` 和 `X-Forwarded-*`。

回滚只需要还原路由注册，不涉及破坏性数据迁移。

## 开发 / Development

### 构建所有二进制

```bash
# 构建服务端
go build -o server ./cmd/server

# 构建各平台 tunnel client
GOOS=darwin GOARCH=arm64 go build -o tunnel-client-darwin-arm64 ./cmd/tunnel-client
GOOS=darwin GOARCH=amd64 go build -o tunnel-client-darwin-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=amd64 go build -o tunnel-client-linux-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=arm64 go build -o tunnel-client-linux-arm64 ./cmd/tunnel-client
GOOS=windows GOARCH=amd64 go build -o tunnel-client-windows-amd64.exe ./cmd/tunnel-client
```

### 测试

```bash
go test ./...
```

### 项目结构

```text
├── cmd/
│   ├── server/          # Relay 服务端入口
│   └── tunnel-client/   # Tunnel client 入口
├── internal/
│   ├── config/          # 配置加载
│   ├── database/        # SQLite 数据层
│   ├── handlers/        # HTTP 处理器
│   ├── middleware/      # 认证中间件
│   ├── models/          # 数据模型
│   ├── services/        # 业务逻辑
│   └── tunnel/          # WebSocket 隧道管理
├── deploy/              # 部署配置（nginx、systemd 等）
├── npm/                 # npm 客户端包
├── scripts/             # 安装 / 卸载脚本
└── test-ws/             # WebSocket 测试工具
```

## 发布与维护 / Release & Maintenance

### 版本与触发条件

- **GitHub Release**：推送以 `v` 开头的标签触发（建议使用 `v<major>.<minor>.<patch>` 格式）；预发布标签（如 `v1.2.3-rc1`）同样会触发 Release 与 npm 发布。工作流构建五个平台的 `tunnel-client`，生成 `SHA256SUMS`，创建 Release 并自动生成发布说明。
- **Docker Hub**：推送到 `main` 时发布 `latest` + 短 SHA；推送严格 `v<major>.<minor>.<patch>` 标签时发布语义化版本、`latest` 和短 SHA；非 semver 的 `v*` 标签只生成 `latest` 和短 SHA；手动触发可以只构建不推送。
- **npm**：Release 创建成功后，按标签版本发布 `@thntime/opencode-tunnel`。

同一标签对应的 Release、Docker 镜像和 npm 包使用相同版本号。

### 必需的 GitHub Secrets

| Secret | 用途 |
|--------|------|
| `DOCKERHUB_USERNAME` | Docker Hub 用户名（当前镜像命名空间为 `thntime`） |
| `DOCKERHUB_TOKEN` | Docker Hub Access Token，需要 Read & Write 权限 |
| `NPM_TOKEN` | npm Granular Access Token，需要 Read and write 权限；迁移到 Trusted Publishing 后删除，见下一节 |

### npm 发布凭据与 Trusted Publishing 迁移

现状（尚未迁移）：

- npm 发布使用 **Granular Access Token**（`NPM_TOKEN`），并依赖该 token 的 **Bypass 2FA** 能力在 CI 中免交互发布。
- npm 已宣布 **2027 年 1 月**移除 Granular Access Token 绕过 2FA 的能力。到期后现有 `NPM_TOKEN` 发布链路会失败，`release.yml` 的 npm 任务会中断。
- 本次只记录现状与迁移路径，不修改 `.github/workflows/release.yml`。

迁移到 **Trusted Publishing**（GitHub OIDC，无需长期 token）的步骤。

`publish-npm` job 当前的实际步骤是 `Setup Node.js` → `Check npm token` → `Update package version` → `Publish to npm`。其中 `Check npm token` 在 `NODE_AUTH_TOKEN` 为空时直接 `exit 1`，所以**必须显式删掉该步骤**；只加 OIDC 权限而不删它，第一次 tag 发布会硬失败。

1. **在 npm 登记发布者**：`@thntime/opencode-tunnel` → Settings → Trusted Publisher → GitHub Actions，填写仓库（组织/仓库名）与 workflow 文件名 `release.yml`。`publish-npm` 目前没有 `environment:`，所以 Environment 留空；如果以后给该 job 加了 environment，这里必须填同一个名字。
2. **只给 `publish-npm` job 加 OIDC 权限**，不要动 `release` job 的 `contents: write`：

   ```yaml
   publish-npm:
     needs: release
     runs-on: ubuntu-latest
     permissions:
       id-token: write
       contents: read
   ```

   workflow 顶层是 `permissions: contents: read`，job 级声明会**整体覆盖**它，所以 `contents: read` 要一起写（checkout 需要）。不要改顶层权限——那会给 test/build 等所有 job 都发 OIDC 令牌。
3. **删除 token 相关配置**：删掉整个 `Check npm token` 步骤，并删掉 `Publish to npm` 步骤下的 `env: NODE_AUTH_TOKEN: ${{ secrets.NPM_TOKEN }}`。`Setup Node.js` 的 `registry-url: 'https://registry.npmjs.org'` 可以保留：它只负责写 `.npmrc` 的 registry 地址，不再需要 token 认证。
4. **保证发布环境的 npm ≥ 11.5.1**：`actions/setup-node@v4` 配 Node 20 自带的是 npm 10.x，不满足 Trusted Publishing 要求。在 `Setup Node.js` 之后加一步升级并断言版本：

   ```yaml
   - name: Use npm that supports Trusted Publishing
     run: |
       npm install -g npm@latest
       npm --version
       node -e 'const v=require("child_process").execSync("npm --version").toString().trim();const [a,b,c]=v.split(".").map(Number);if(a<11||(a===11&&b<5)||(a===11&&b===5&&c<1)){console.error("npm "+v+" < 11.5.1");process.exit(1)}'
   ```

   `npm publish` 建议显式加上 `--provenance`：Trusted Publishing 下 provenance 会自动生成，但写出来能让人一眼看出这条链路依赖来源证明。
5. **验证后删除 secret**：用测试标签完整跑一次发布，确认 npm 包页面出现 provenance 与来源仓库，再删除仓库 secret `NPM_TOKEN`，并同步更新上面的「必需的 GitHub Secrets」表。

前置条件：npm 包已存在且当前账号有发布权限；GitHub 仓库允许 Actions 获取 OIDC 令牌；工作流由 npm 上登记的仓库与文件名触发。

### 前置条件

- Docker Hub 上已创建 `thntime/opencode-relay-server` 仓库。
- npm 上 `@thntime` scope 可用；如果实际 scope 不同，请同步修改 `npm/opencode-tunnel/package.json` 的 `name` 和本文档中的安装命令。
- GitHub 仓库已配置上述 secrets（迁移到 Trusted Publishing 后 `NPM_TOKEN` 可以删除）。

### 发布流程

```bash
# 1. 确认工作区干净并完成测试
go test ./...

# 2. 打标签并推送
git tag v1.2.3
git push origin v1.2.3

# 3. 在 GitHub Actions 中确认 Release、Docker、npm 三个任务成功
```

发布后检查：

- GitHub Release 包含五个平台二进制和 `SHA256SUMS`
- Docker Hub 上存在 `1.2.3`、`1.2`、`latest` 和短 SHA 标签
- npm 上存在 `1.2.3` 版本

### 回滚

- **停止自动发布**：还原 `.github/workflows/release.yml` 和 `docker.yml`。
- **Docker**：删除或重新指向有问题的标签；`latest` 回退到上一个稳定版本。
- **npm**：在 72 小时内可以 `npm unpublish`；超过窗口建议 `npm deprecate` 并发布修复版本。
- **GitHub Release**：编辑或删除有问题的 Release，不影响已下载的二进制。

## 相关项目 / Related Projects

- **[OpenCode Mobile](https://github.com/thntime/opencode-mobile)** — 配套移动端应用
- **[OpenCode](https://github.com/sst/opencode)** — AI 编码助手

## 许可证 / License

MIT

## 致谢 / Credits

为 [OpenCode](https://github.com/sst/opencode) 的安全远程访问而构建。OpenCode 由 SST 开发。
