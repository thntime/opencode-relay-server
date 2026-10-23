# OpenCode Relay Server

[中文](README.md) | [English](README.en.md)

<div align="center">

**Secure remote access to your OpenCode instances over encrypted tunnels**

The relay infrastructure for [OpenCode Mobile](https://github.com/zhangjinglei137/opencode-mobile) — bringing your AI coding assistant to any network, safely.

[![GitHub Stars](https://img.shields.io/github/stars/zhangjinglei137/opencode-relay-server?style=for-the-badge)](https://github.com/zhangjinglei137/opencode-relay-server)
[![Docker](https://img.shields.io/badge/docker-thntime%2Fopencode--relay--server-blue?style=for-the-badge&logo=docker)](https://hub.docker.com/r/thntime/opencode-relay-server)
[![npm](https://img.shields.io/badge/npm-%40thntime%2Fopencode--tunnel-cb3837?style=for-the-badge&logo=npm)](https://www.npmjs.com/package/@thntime/opencode-tunnel)
[![License](https://img.shields.io/badge/license-MIT-blue?style=for-the-badge)](https://opensource.org/licenses/MIT)

</div>

---

## 项目简介 / Overview

This repository contains two components:

1. **Relay Server** — a Go-based WebSocket relay server that can run on Azure, Fly.io, Docker, or any self-hosted machine.
2. **Tunnel Client** — a CLI tool that runs on your computer and maintains the secure tunnel.

The relay only forwards encrypted data. It never holds your end-to-end key and never reads your OpenCode content.

## 架构 / Architecture

```text
┌─────────────────┐         E2E         ┌─────────────────┐         E2E         ┌─────────────────┐
│                 │◄───────────────────►│                 │◄───────────────────►│  tunnel-client  │
│  OpenCode App   │     encrypted       │  Relay Server   │     encrypted       │                 │
│                 │                     │ (Azure/Fly/self)│                     │                 │
└─────────────────┘                     └─────────────────┘                     └────────┬────────┘
                                                                                         │
                                                                                         │ HTTP
                                                                                         ▼
                                                                                ┌─────────────────┐
                                                                                │ OpenCode Server │
                                                                                │   (localhost)   │
                                                                                └─────────────────┘
```

### How it works

1. `tunnel-client` connects to the Relay Server over WebSocket.
2. The app sends encrypted requests to the Relay Server.
3. The Relay Server forwards encrypted data to the `tunnel-client` that owns the host.
4. `tunnel-client` decrypts the request and forwards it to the local OpenCode server.
5. The response travels back along the same path, staying end-to-end encrypted.

### Security model

| Layer | Protection |
|-------|------------|
| **Transport** | TLS 1.3 (WSS/HTTPS) between all components |
| **Payload** | AES-256-GCM end-to-end encryption |
| **Key exchange** | The host generates a 256-bit key, wraps it with a passphrase-derived key, and reports only the wrapped material; the relay stores only that material |
| **Authentication** | JWT tokens plus per-device credentials |
| **Zero knowledge** | The relay only sees encrypted blobs and cannot decrypt them |

### How end-to-end encryption works

1. `tunnel-client` generates a random 256-bit AES key for the host.
2. The key is wrapped with a passphrase-derived KEK (scrypt, N=2^15, r=8, p=1). `device.json` stores only `key_wrapped`, `key_salt`, and `key_kdf`.
3. The client reports the wrapped material via `PUT /api/hosts/{hostId}/key`.
4. The app fetches it via `GET /api/hosts/{hostId}/key` and unwraps it locally with the passphrase.
5. The relay never receives the plaintext key. Without a passphrase, the client skips key reporting and prints a warning; the tunnel still works, but end-to-end encryption is not enabled.

## 安装与发行 / Installation & Distribution

Choose the channel that fits your use case:

| Use case | Channel | Command / entry point |
|----------|---------|-----------------------|
| Install the tunnel client | npm | `npm install -g @thntime/opencode-tunnel` |
| Download the client manually | GitHub Releases | [Releases](https://github.com/zhangjinglei137/opencode-relay-server/releases) |
| Run the Relay Server | Docker Hub | `docker pull thntime/opencode-relay-server:latest` |

### Install the client via npm

Requires [Node.js](https://nodejs.org/) 14+ and works on macOS, Linux, and Windows:

```bash
npm install -g @thntime/opencode-tunnel
opencode-tunnel
```

`postinstall` downloads the matching `tunnel-client` binary from this repository's GitHub Release, using the current platform and package version.

> If your npm scope is not `@thntime`, update both `name` in `npm/opencode-tunnel/package.json` and the install commands in this document.

### Download from GitHub Releases

Every `v*` tag produces the following assets:

- `tunnel-client-linux-amd64`
- `tunnel-client-linux-arm64`
- `tunnel-client-darwin-amd64`
- `tunnel-client-darwin-arm64`
- `tunnel-client-windows-amd64.exe`
- `SHA256SUMS`

Download the file for your platform, rename it if you like, and make it executable:

```bash
chmod +x tunnel-client-darwin-arm64
./tunnel-client-darwin-arm64
```

### Run the server from Docker Hub

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

The image supports `linux/amd64` and `linux/arm64`. Tags include `latest`, semantic versions such as `1.2.3` and `1.2`, and a short SHA.

> The image listens on `PORT=3000` by default; the example sets it to `8080` explicitly to match `EXPOSE 8080`. If you omit `-e PORT=8080`, change the port mapping to `3000:3000`.

## 快速开始 / Quick Start

### Prerequisites

- [OpenCode Mobile](https://github.com/zhangjinglei137/opencode-mobile), with an account created in the app
- [OpenCode](https://github.com/sst/opencode) installed on your computer
- Node.js 14+ if you install the client via npm

### First run

```bash
# Install and start
npm install -g @thntime/opencode-tunnel
opencode-tunnel
```

The first run walks through:

1. **Login** — enter your email and password, or provide a token via `--token` / `OPENCODE_RELAY_TOKEN`.
2. **Register the host** — call `POST /api/devices`, then persist the stable identity in `device.json`.
3. **Wrap the key** — use `--passphrase`, `OPENCODE_TUNNEL_PASSPHRASE`, or an interactive prompt to wrap the E2E key and report the wrapped material.
4. **Connect the app** — log in to the same account in the app, select this host, and enter the passphrase to unwrap the key.
5. **Tunnel established** — the app can now reach your local OpenCode from any network.

The client also:

- Starts OpenCode automatically when needed
- Configures autostart on boot (launchd / systemd / Windows Task Scheduler)
- Reconnects automatically after network interruptions

## Tunnel Client 使用 / Tunnel Client Usage

```bash
# Start the tunnel (default command)
opencode-tunnel

# Use a custom local OpenCode port
opencode-tunnel -port 4096

# Show status
opencode-tunnel status

# Discover running OpenCode instances
opencode-tunnel discover

# Log out and clear credentials
opencode-tunnel logout

# Help
opencode-tunnel help
```

> `opencode-tunnel pair` is kept for compatibility with the legacy pairing flow. A normal `start` registers the host automatically; no QR scan is required.

### Options

| Option | Description |
|--------|-------------|
| `-relay, --relay <url>` | Relay URL (default: `https://opencode-relay.azurewebsites.net`) |
| `-port, --port <port>` | Local OpenCode port (default `4096`, or `port` from `opencode.json`) |
| `--name <name>` | Host display name (default: OS hostname) |
| `--token <token>` | Relay API token (overrides saved credentials) |
| `--email <email>` | Login email |
| `--password <pass>` | Login password |
| `--passphrase <pass>` | E2E key passphrase used to wrap the host key (never stored) |
| `--rewrap` | Re-wrap the E2E host key with a new passphrase |
| `--reset` | Discard the local identity and register a new host |
| `--attach` | Reuse an existing `opencode serve` on `--port`; probes the port (2s timeout) and fails with a hint instead of spawning a new process |
| `--install` | Validate identity and wrapped key, install the OS autostart entry, then exit (does not start the tunnel) |

### `--install` notes

`--install` only configures autostart. Host registration and key wrapping must succeed first:

- If `device.json` is missing, has no `Host ID`, or has no wrapped key, the client runs `resolveAuth → ensureHost → prepareHostKey` in the foreground before writing the launchd / systemd / Task Scheduler entry.
- Without a passphrase (`--passphrase`, `OPENCODE_TUNNEL_PASSPHRASE`, or an interactive prompt) and without a TTY, `--install` exits non-zero instead of silently installing a host that cannot report its E2E key.
- A normal `start` still works without a passphrase, but skips key reporting and prints a warning (`未提供 E2E 口令…跳过密钥上报；隧道仍可建立`); until a key is reported, the tunnel itself is not end-to-end encrypted.

### Configuration files

Located in `~/.opencode-tunnel/`:

| File | Purpose |
|------|---------|
| `auth.json` | Login credentials (JWT token, email) |
| `device.json` | Host identity (host ID, subdomain, auth) and wrapped E2E key material |
| `opencode.json` | OpenCode autostart configuration |

## 自托管 Relay Server / Self-Hosting the Relay Server

### Prerequisites

- Go 1.24+
- A server with a public IP, or Cloudflare Tunnel

### Build and run

```bash
# Clone
git clone https://github.com/zhangjinglei137/opencode-relay-server.git
cd opencode-relay-server

# Build
go build -o server ./cmd/server

# Run
PORT=8080 JWT_SECRET=your-secret ./server
```

You can also use the Docker Hub image directly; see [Run the server from Docker Hub](#run-the-server-from-docker-hub).

### Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `3000` | Server listen port; the Docker examples set it to `8080` explicitly |
| `JWT_SECRET` | `change-me-in-production` | JWT signing secret; replace it with a long random value in production |
| `DATABASE_PATH` | `data/relay.db` | SQLite database path |
| `SINGLE_USER_MODE` | `false` | Set to `true` to skip email verification |
| `RELAY_LEGACY_PROXY` | `false` | Re-enable the deprecated, unauthenticated `/proxy/*` route |
| `RESEND_API_KEY` | empty | Resend API key used to send verification emails |
| `EMAIL_FROM` | `OpenCode <noreply@liuyao16.dpdns.org>` | Verification email sender |
| `ADMIN_EMAIL` | empty | Optional admin email |
| `ADMIN_PASSWORD` | empty | Optional admin password |
| `FRPS_HOST` | `127.0.0.1` | frp server host (legacy deployment compatibility) |
| `FRPS_PORT` | `7000` | frp server port |
| `FRPS_TOKEN` | empty | frp auth token |
| `FRPS_DOMAIN` | `liuyao16.dpdns.org` | frp domain |

### Deploy to fly.io

```bash
fly launch
fly secrets set JWT_SECRET=your-secret
fly deploy
```

The included `fly.toml` is a working starting point; adjust the app name, region, and domain for your deployment.

### Deploy behind nginx

`deploy/nginx-relay.conf` contains the reverse-proxy configuration for `/relay/`, including the streaming and WebSocket settings (HTTP/1.1 upstream, `Upgrade`/`Connection`, buffering off, long timeouts). The `map $http_upgrade $connection_upgrade` block must live in the `http` block, not inside a `server` block.

Replace the example domain and certificate paths with your own. The configuration has not been validated on a machine with nginx; run `nginx -t` on the target host and do an end-to-end check before reloading.

### Point the client at your relay

After self-hosting, tell the client where your relay lives:

```bash
opencode-tunnel -relay https://your-relay.example.com
```

## API 端点 / API Endpoints

### Authentication

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/send-verification` | Send an email verification code |
| POST | `/api/register` | Register an account |
| POST | `/api/login` | Log in and get a JWT |
| POST | `/api/auto-login` | Refresh the token |

### Devices

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/devices` | List the user's devices |
| POST | `/api/devices` | Register a device (`name` optional, defaults to `OpenCode Host`) |
| GET | `/api/devices/{id}` | Get device details |
| PUT | `/api/devices/{id}` | Update a device |
| DELETE | `/api/devices/{id}` | Delete a device |
| GET | `/api/heartbeat` | Device heartbeat |

### Hosts

A host is one of the user's machines. `id` is the stable host identifier (the device's `subdomain`).

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/hosts` | List the caller's hosts (`id`, `name`, `online`, `last_seen`) |
| GET | `/api/hosts/{hostId}/key` | Fetch the wrapped E2E key material |
| PUT | `/api/hosts/{hostId}/key` | Report the wrapped E2E key material |

All of these require `Authorization: Bearer <jwt>` and only return hosts owned by that user.

### Pairing

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/pairing` | Create a pairing request |
| GET | `/api/pairing/{id}/status` | Poll pairing status |
| POST | `/api/pairing/{id}/complete` | Complete pairing |

### Tunnel

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/tunnel/{subdomain}` | WebSocket tunnel connection |
| GET | `/api/events/{subdomain}` | SSE event forwarding |
| ANY | `/proxy/*` | Deprecated HTTP proxy; returns 410 unless `RELAY_LEGACY_PROXY=true` |

### Relay routing

| Method | Endpoint | Description |
|--------|----------|-------------|
| ANY | `/relay/{hostId}/{path...}` | Forward an authenticated request to that host's tunnel |

`/relay/...` requires `Authorization: Bearer <jwt>` and checks that `hostId` belongs to the caller. Status codes:

- `401`: missing or invalid token
- `403`: the host belongs to another user
- `404`: unknown host
- `503`: host offline

The `Authorization` header is consumed by the relay and is not forwarded to the host. The relay also strips `Cookie`, `X-Real-IP`, and every `X-Forwarded-*` header, so the host's local OpenCode server receives requests carrying no credentials.

A valid relay JWT is equivalent to full access to every host of its user. JWTs are valid for 30 days and there is no revocation mechanism; if one leaks, rotating `JWT_SECRET` is the only remediation.

### Streaming and WebSocket

Protocol v2 adds incremental response streaming (SSE) and WebSocket forwarding (tunneled terminal PTY) on top of the existing request/response frames. Both ride the same tunnel WebSocket, so no new endpoint or port is needed.

#### Frames

| Frame | Direction | Purpose |
|-------|-----------|---------|
| `res-head` | client → relay | Status code and headers of the upstream response; sent once, before any chunk |
| `res-chunk` | client → relay | One response body chunk (`seq` + base64 `data`), delivered in order |
| `res-end` | client → relay | Clean end of the response stream |
| `res-error` | client → relay | Abort the stream; carries a human-readable `message` |
| `req-cancel` | relay → client | The consumer went away; the client cancels the local request |
| `ws-open` | relay → client | Ask the client to dial `ws://127.0.0.1:<port><path>`, forwarding the original headers and subprotocols |
| `ws-ack` | client → relay | Dial result: `ok`, `status`, `error`, `subprotocol` |
| `ws-frame` | both | One WebSocket message (opcode + base64 `data`), relayed in both directions |
| `ws-close` | both | Close the WebSocket; carries `code` and `reason` through unchanged |

#### Capability negotiation

The client sends `X-Tunnel-Protocol: 2` in the tunnel handshake request; the relay echoes the same version in the **handshake response** header. If the header is missing on the response, the client stays on v1 (whole-payload `io.ReadAll`) — that is also how it detects a downgraded relay. A relay that receives a streaming request (`Accept: text/event-stream` or `Upgrade: websocket`) on a v1 connection answers **501** with a hint to upgrade the client.

#### Limits

| Limit | Value | On exceed |
|-------|-------|-----------|
| Single frame | 1 MiB (before base64) | 413, or WebSocket close code 1009 |
| Concurrent streams per connection | 16 | 429, or WebSocket close code 1011 |
| Per-stream send buffer | 32 frames or 4 MiB | The stream is closed and the host is told to stop |
| Slow consumer | 30s without write progress | The stream is closed |

These are compile-time constants for now; they become configurable once there is real pressure data.

#### HTTP/1.1 requirement

WebSocket forwarding is implemented with `http.Hijacker`, which HTTP/2 cannot provide. The relay therefore requires HTTP/1.1 and answers **426** when `ProtoMajor != 1`. Terminate TLS at a reverse proxy that speaks HTTP/1.1 to the relay (see [Deploy behind nginx](#deploy-behind-nginx)); `listen ... http2` on the nginx side is fine for that hop, because nginx re-originates the upstream request as HTTP/1.1. Note that nginx itself cannot upgrade a WebSocket that arrived over HTTP/2, so the phone must use HTTP/1.1 for WebSocket requests.

#### End-to-end encryption

When the host has reported a host key, `res-chunk` and `ws-frame` payloads are encrypted frame by frame with AES-256-GCM. `res-head` carries `X-E2E-Stream: 1` and `ws-ack` reports `encrypted=true`, so the app knows to decrypt each frame. Without a key the stream is plaintext, exactly like v1 payloads. The relay never decrypts either way; it only routes frames by `id`.

#### Rollback

Rolling the relay back to a v1 build is safe: the client finds no `X-Tunnel-Protocol` in the handshake response and automatically falls back to `handleRequestLegacy` (whole-payload). WebSocket and streaming requests get 501/426; ordinary requests are unaffected. No data migration is involved.

#### nginx

`proxy_buffering off` and the long `proxy_read_timeout` / `proxy_send_timeout` are required. With buffering on, nginx holds SSE events until the response ends and the event stream appears stuck; with the old 130s timeout, any stream or terminal session idle for longer is truncated. See `deploy/nginx-relay.conf`.

### Migrating from `/proxy/`

1. Deploy the relay with the new schema (three nullable key columns are added automatically) and the `/relay/` routes.
2. Upgrade tunnel clients; they keep working over `/api/tunnel/{subdomain}`.
3. Point mobile clients at `/relay/{hostId}/...`.
4. Only if a legacy client still needs `/proxy/`, set `RELAY_LEGACY_PROXY=true` for one release. Legacy mode still strips `Authorization`, `Cookie`, `X-Real-IP`, and `X-Forwarded-*` before forwarding.

Rollback only needs the route registration reverted; no data migration is destructive.

## 开发 / Development

### Build all binaries

```bash
# Build the server
go build -o server ./cmd/server

# Build the tunnel client for each platform
GOOS=darwin GOARCH=arm64 go build -o tunnel-client-darwin-arm64 ./cmd/tunnel-client
GOOS=darwin GOARCH=amd64 go build -o tunnel-client-darwin-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=amd64 go build -o tunnel-client-linux-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=arm64 go build -o tunnel-client-linux-arm64 ./cmd/tunnel-client
GOOS=windows GOARCH=amd64 go build -o tunnel-client-windows-amd64.exe ./cmd/tunnel-client
```

### Test

```bash
go test ./...
```

### Project structure

```text
├── cmd/
│   ├── server/          # Relay server entry point
│   └── tunnel-client/   # Tunnel client entry point
├── internal/
│   ├── config/          # Configuration loading
│   ├── database/        # SQLite data layer
│   ├── handlers/        # HTTP handlers
│   ├── middleware/      # Auth middleware
│   ├── models/          # Data models
│   ├── services/        # Business logic
│   └── tunnel/          # WebSocket tunnel management
├── deploy/              # Deployment configs (nginx, systemd, etc.)
├── npm/                 # npm client package
├── scripts/             # Install / uninstall scripts
└── test-ws/             # WebSocket test utilities
```

## 发布与维护 / Release & Maintenance

### Versions and triggers

- **GitHub Release**: pushing a tag that starts with `v` triggers the workflow (the recommended format is `v<major>.<minor>.<patch>`); pre-release tags such as `v1.2.3-rc1` also trigger both the Release and the npm publish. The workflow builds the `tunnel-client` for five platforms, generates `SHA256SUMS`, creates the Release, and writes release notes automatically.
- **Docker Hub**: a push to `main` publishes `latest` plus a short SHA; a strict `v<major>.<minor>.<patch>` tag publishes semantic versions, `latest`, and a short SHA; a non-semver `v*` tag only produces `latest` and the short SHA; manual dispatch can build without pushing.
- **npm**: after the Release succeeds, publish `@thntime/opencode-tunnel` at the tag version.

The Release, Docker image, and npm package for the same tag share the same version number.

### Required GitHub Secrets

| Secret | Purpose |
|--------|---------|
| `DOCKERHUB_USERNAME` | Docker Hub username (the current image namespace is `thntime`) |
| `DOCKERHUB_TOKEN` | Docker Hub access token with Read & Write permission |
| `NPM_TOKEN` | npm granular access token with Read and write permission |

### Prerequisites

- The Docker Hub repository `thntime/opencode-relay-server` exists.
- The npm scope `@thntime` is available; if your actual scope differs, update `name` in `npm/opencode-tunnel/package.json` and the install commands in this document.
- The GitHub repository has the three secrets above.

### Release steps

```bash
# 1. Make sure the worktree is clean and tests pass
go test ./...

# 2. Tag and push
git tag v1.2.3
git push origin v1.2.3

# 3. Confirm the Release, Docker, and npm jobs succeed in GitHub Actions
```

After publishing, verify:

- The GitHub Release contains the five platform binaries and `SHA256SUMS`.
- Docker Hub has the `1.2.3`, `1.2`, `latest`, and short-SHA tags.
- npm has version `1.2.3`.

### Rollback

- **Stop automatic publishing**: revert `.github/workflows/release.yml` and `docker.yml`.
- **Docker**: delete or retarget the bad tag; move `latest` back to the previous stable version.
- **npm**: `npm unpublish` is possible within 72 hours; after that, prefer `npm deprecate` and publish a fixed version.
- **GitHub Release**: edit or delete the bad Release; already-downloaded binaries are unaffected.

## 相关项目 / Related Projects

- **[OpenCode Mobile](https://github.com/zhangjinglei137/opencode-mobile)** — the companion mobile app
- **[OpenCode](https://github.com/sst/opencode)** — the AI coding assistant

## 许可证 / License

MIT

## 致谢 / Credits

Built to enable secure remote access to [OpenCode](https://github.com/sst/opencode). OpenCode is developed by SST.
