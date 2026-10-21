# OpenCode Relay Server

<div align="center">

**Secure remote access to your OpenCode instances**

The relay infrastructure for [OpenCode Anywhere](https://github.com/zero469/opencode-anywhere) - enabling encrypted tunnels to your AI coding assistant from anywhere in the world.

[![Download iOS App](https://img.shields.io/badge/iOS_App-TestFlight-blue?style=for-the-badge&logo=apple)](https://testflight.apple.com/join/your-link)
[![GitHub Stars](https://img.shields.io/github/stars/zero469/opencode-relay-server?style=for-the-badge)](https://github.com/zero469/opencode-relay-server)

</div>

---

## Overview

This repository contains two components:

1. **Relay Server** - A Go-based WebSocket relay server (deployed on fly.io)
2. **Tunnel Client** - A CLI tool that runs on your computer to establish secure tunnels

## Quick Start

### Prerequisites

- [OpenCode Anywhere iOS App](https://github.com/zero469/opencode-anywhere) - Create an account first
- [OpenCode](https://github.com/sst/opencode) installed on your computer

### Install via npm

```bash
npm install -g @zero469/opencode-tunnel
opencode-tunnel
```

Requires [Node.js](https://nodejs.org/) 14+. Works on macOS, Linux, and Windows.

### What Happens

1. **Login** - Enter your email/password (account created in iOS app)
2. **QR Code** - A pairing QR code appears in your terminal
3. **Scan** - Use the iOS app to scan the QR code
4. **Connected** - Your computer is now accessible from anywhere!

The tunnel client will:
- Auto-start OpenCode if not running
- Configure auto-start on boot (launchd/systemd/Windows Task Scheduler)
- Maintain persistent connection with automatic reconnection

## Architecture

```
┌─────────────────┐         ┌─────────────────┐         ┌─────────────────┐
│                 │   E2E   │                 │   E2E   │                 │
│    iOS App      │◄───────►│  Relay Server   │◄───────►│  tunnel-client  │
│                 │ Encrypt │   (fly.io)      │ Encrypt │                 │
└─────────────────┘         └─────────────────┘         └────────┬────────┘
                                                                 │
                                                                 │ HTTP
                                                                 ▼
                                                        ┌─────────────────┐
                                                        │ OpenCode Server │
                                                        │  (localhost)    │
                                                        └─────────────────┘
```

### How It Works

1. **tunnel-client** connects to Relay Server via WebSocket
2. **iOS App** sends encrypted requests to Relay Server
3. **Relay Server** forwards encrypted data to tunnel-client
4. **tunnel-client** decrypts and forwards to local OpenCode
5. Response travels back the same path (encrypted)

### Security Model

| Layer | Protection |
|-------|------------|
| **Transport** | TLS 1.3 (WSS/HTTPS) between all components |
| **Payload** | AES-256-GCM end-to-end encryption |
| **Key Exchange** | Encryption key embedded in QR code, never sent to relay |
| **Authentication** | JWT tokens + per-device credentials |
| **Zero Knowledge** | Relay server only sees encrypted blobs |

### How E2E Encryption Works

1. `tunnel-client` generates a random 256-bit AES key during pairing
2. Key is embedded in QR code data (JSON encoded)
3. iOS app scans QR → extracts key → stores locally
4. All subsequent requests/responses are encrypted with this key
5. Relay server forwards encrypted blobs, cannot decrypt

## Tunnel Client Usage

```bash
# First run: logs in, registers the host, and starts the tunnel
opencode-tunnel

# Start with custom port
opencode-tunnel -port 8080

# Show status
opencode-tunnel status

# Logout and clear credentials
opencode-tunnel logout

# Help
opencode-tunnel help
```

首次运行会登录并注册主机，不再扫码配对：`tunnel-client` 用 `--token`（或
`OPENCODE_RELAY_TOKEN`、已保存的 `auth.json`、`--email/--password` 登录）取得凭据，
调用 `POST /api/devices` 注册主机并把身份写入 `device.json`。E2E 密钥不再放进二维码，
而是用口令包裹后上报给 Relay（见下方 `--passphrase`）。

### Options

| Option | Description |
|--------|-------------|
| `-relay, --relay <url>` | Relay server URL (default: `https://opencode-relay.azurewebsites.net`) |
| `-port, --port <port>` | Local OpenCode port (default: `4096`, or `opencode.json`'s `port`) |
| `--name <name>` | Host display name (default: OS hostname) |
| `--token <token>` | Relay API token (overrides saved credentials) |
| `--email <email>` | Account email for login |
| `--password <pass>` | Account password for login |
| `--passphrase <pass>` | E2E key passphrase, used to wrap the host key before reporting it (never stored) |
| `--rewrap` | Re-wrap the E2E host key with a new passphrase |
| `--reset` | Discard the local identity and register a new host |
| `--attach` | Reuse an already running `opencode serve` on `--port`; the client only probes the port (2s timeout) and fails with a hint instead of spawning OpenCode |
| `--install` | Validate identity + wrapped key, then install the OS autostart entry and exit (does not start the tunnel) |

### `--install` notes

`--install` only configures autostart; it requires registration and key wrapping to
succeed first:

- If `device.json` is missing, has no `Host ID`, or has no wrapped key, the client
  runs the normal `resolveAuth → ensureHost → prepareHostKey` flow in the foreground
  before writing the launchd/systemd/Task Scheduler entry.
- Without a passphrase (`--passphrase`, `OPENCODE_TUNNEL_PASSPHRASE`, or an
  interactive prompt) and without a TTY, `--install` fails with a non-zero exit
  status instead of silently installing a host that cannot report its E2E key.
- Normal `start` runs without a passphrase still work, but skip the E2E key report
  and print a warning (`未提供 E2E 口令…跳过密钥上报；隧道仍可建立`); the tunnel itself
  is established unencrypted end-to-end until a key is reported.

### Configuration Files

Located in `~/.opencode-tunnel/`:

| File | Purpose |
|------|---------|
| `auth.json` | Login credentials (JWT token, email) |
| `device.json` | Host identity (host ID, subdomain, auth) and wrapped E2E key material |
| `opencode.json` | OpenCode auto-start config |

## Self-Hosting the Relay Server

Want to run your own relay server? Here's how:

### Prerequisites

- Go 1.21+
- A server with public IP (or use Cloudflare Tunnel)

### Build & Run

```bash
# Clone
git clone https://github.com/zero469/opencode-relay-server.git
cd opencode-relay-server

# Build
go build -o server ./cmd/server

# Run
PORT=8080 JWT_SECRET=your-secret ./server
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `JWT_SECRET` | (required) | Secret for JWT signing |
| `DATABASE_PATH` | `./data/relay.db` | SQLite database path |
| `SINGLE_USER_MODE` | `false` | Skip email verification |
| `RELAY_LEGACY_PROXY` | `false` | Re-enable the deprecated, unauthenticated `/proxy/*` route |

### Deploy to fly.io

```bash
fly launch
fly secrets set JWT_SECRET=your-secret
fly deploy
```

### Deploying behind nginx

`deploy/nginx-relay.conf` is the reverse-proxy config for the `/relay/` route, including the
streaming and WebSocket settings (HTTP/1.1 upstream, `Upgrade`/`Connection`, buffering off,
long timeouts). The `map $http_upgrade $connection_upgrade` block must be placed in the
`http` block, not inside `server`.

These snippets have **not** been validated on a machine with nginx; run `nginx -t` on the
target host before reloading. Nothing here has been tested end-to-end against a live relay.

### Update Tunnel Client

After self-hosting, update the default relay URL:

```bash
opencode-tunnel -relay https://your-relay.fly.dev
```

## API Endpoints

### Authentication

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/send-verification` | Send email verification code |
| POST | `/api/register` | Register new account |
| POST | `/api/login` | Login and get JWT |
| POST | `/api/auto-login` | Refresh token |

### Devices

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/devices` | List user's devices |
| POST | `/api/devices` | Register new device (`name` optional, defaults to `OpenCode Host`) |
| GET | `/api/devices/{id}` | Get device details |
| PUT | `/api/devices/{id}` | Update device |
| DELETE | `/api/devices/{id}` | Delete device |

### Hosts

Hosts are the user's machines. `id` is the stable host identifier (the device's `subdomain`).

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/hosts` | List the caller's hosts (`id`, `name`, `online`, `last_seen`) |
| GET | `/api/hosts/{hostId}/key` | Fetch the wrapped E2E key material for a host |

Both require `Authorization: Bearer <jwt>` and only return hosts owned by that user.

### Pairing

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/pairing` | Create pairing request |
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
| ANY | `/relay/{hostId}/{path...}` | Authenticated request forwarding to that host's tunnel |

`/relay/...` requires `Authorization: Bearer <jwt>`, checks that `hostId` belongs to the caller, and returns
401 (no/invalid token), 403 (someone else's host), 404 (unknown host) or 503 (host offline).
The `Authorization` header is consumed by the relay and is not forwarded to the host.

The relay also strips `Cookie`, `X-Real-IP` and every `X-Forwarded-*` header, so the host's local OpenCode
server receives requests carrying no credentials whatsoever. A valid relay JWT is therefore equivalent to
full access to every host of its user. JWTs are valid for 30 days and there is no revocation mechanism: if
one leaks, rotating `JWT_SECRET` is the only remediation.

Streaming responses (SSE) and WebSocket forwarding are covered in
[Streaming and WebSocket](#streaming-and-websocket); plain request/response traffic keeps
working unchanged for clients that do not negotiate protocol v2.

### Streaming and WebSocket

Protocol v2 adds incremental response streaming (SSE) and WebSocket forwarding (tunneled
terminal PTY) on top of the existing request/response frames. Both ride the same tunnel
WebSocket, so no new endpoint or port is needed.

#### Frames

| Frame | Direction | Purpose |
|-------|-----------|---------|
| `res-head` | client → relay | Status code and headers of the upstream response; sent once, before any chunk. |
| `res-chunk` | client → relay | One response body chunk (`seq` + base64 `data`), delivered in order. |
| `res-end` | client → relay | Clean end of the response stream. |
| `res-error` | client → relay | Abort the stream; carries a human-readable `message`. |
| `req-cancel` | relay → client | The consumer went away; the client cancels the local request. |
| `ws-open` | relay → client | Ask the client to dial `ws://127.0.0.1:<port><path>`, forwarding the original headers and subprotocols. |
| `ws-ack` | client → relay | Dial result: `ok`, `status`, `error`, `subprotocol`. |
| `ws-frame` | both | One WebSocket message (opcode + base64 `data`), relayed in both directions. |
| `ws-close` | both | Close the WebSocket; carries the `code` and `reason` through unchanged. |

#### Capability negotiation

The client sends `X-Tunnel-Protocol: 2` in the tunnel handshake request; the relay echoes the
same version in the **handshake response** header. If the header is missing on the response,
the client stays on v1 (whole-payload `io.ReadAll`) — that is also how it detects a
downgraded relay. A relay that receives a streaming request (`Accept: text/event-stream` or
`Upgrade: websocket`) on a v1 connection answers **501** with a hint to upgrade the client.

#### Limits

| Limit | Value | On exceed |
|-------|-------|-----------|
| Single frame | 1 MiB (before base64) | 413, or WebSocket close code 1009 |
| Concurrent streams per connection | 16 | 429, or WebSocket close code 1011 |
| Per-stream send buffer | 32 frames or 4 MiB | Stream is closed and the host is told to stop |
| Slow consumer | 30s without write progress | Stream is closed |

These are compile-time constants for now; they become configurable once there is real
pressure data.

#### HTTP/1.1 requirement

WebSocket forwarding is implemented with `http.Hijacker`, which HTTP/2 cannot provide. The
relay therefore requires HTTP/1.1 and answers **426** when `ProtoMajor != 1`. Terminate TLS
at a reverse proxy that speaks HTTP/1.1 to the relay (see [Deploying behind nginx](#deploying-behind-nginx));
`listen ... http2` on the nginx side is fine for that hop, because nginx re-originates the
upstream request as HTTP/1.1. Note that nginx itself cannot upgrade a WebSocket that arrived
over HTTP/2, so the phone must use HTTP/1.1 for WebSocket requests.

#### End-to-end encryption

When the host has reported a host key, `res-chunk` and `ws-frame` payloads are encrypted
frame by frame with AES-256-GCM. `res-head` carries `X-E2E-Stream: 1` and `ws-ack` reports
`encrypted=true` so the phone knows to decrypt each frame. Without a key the stream is
plaintext, exactly like v1 payloads. The relay never decrypts either way; it only routes
frames by `id`.

#### Rollback

Rolling the relay back to a v1 build is safe: the client finds no `X-Tunnel-Protocol` in the
handshake response and automatically falls back to `handleRequestLegacy` (whole-payload).
WebSocket and streaming requests get 501/426, ordinary requests are unaffected. No data
migration is involved.

#### nginx

`proxy_buffering off` and the long `proxy_read_timeout`/`proxy_send_timeout` are required.
With buffering on, nginx holds SSE events until the response ends and the event stream
appears stuck; with the old 130s timeout, any stream or terminal session idle for longer is
truncated. See `deploy/nginx-relay.conf`.

### Migrating from `/proxy/`

1. Deploy the relay with the new schema (three nullable key columns are added automatically) and the `/relay/` routes.
2. Upgrade tunnel clients; they keep working over `/api/tunnel/{subdomain}`.
3. Point mobile clients at `/relay/{hostId}/...`.
4. Only if a legacy client still needs `/proxy/`, set `RELAY_LEGACY_PROXY=true` for one release. Legacy mode still strips `Authorization`, `Cookie`, `X-Real-IP` and `X-Forwarded-*` before forwarding.

Rollback only needs the route registration reverted; no data migration is destructive.

## Development

### Build All Binaries

```bash
# Build server
go build -o server ./cmd/server

# Build tunnel client for all platforms
GOOS=darwin GOARCH=arm64 go build -o tunnel-client-darwin-arm64 ./cmd/tunnel-client
GOOS=darwin GOARCH=amd64 go build -o tunnel-client-darwin-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=amd64 go build -o tunnel-client-linux-amd64 ./cmd/tunnel-client
GOOS=linux GOARCH=arm64 go build -o tunnel-client-linux-arm64 ./cmd/tunnel-client
GOOS=windows GOARCH=amd64 go build -o tunnel-client-windows-amd64.exe ./cmd/tunnel-client
```

### Project Structure

```
├── cmd/
│   ├── server/          # Relay server entry point
│   └── tunnel-client/   # Tunnel client entry point
├── internal/
│   ├── config/          # Configuration loading
│   ├── database/        # SQLite database layer
│   ├── handlers/        # HTTP handlers
│   ├── middleware/      # Auth middleware
│   ├── models/          # Data models
│   ├── services/        # Business logic
│   └── tunnel/          # WebSocket tunnel management
├── scripts/             # Deployment scripts
└── dist/                # Pre-built binaries
```

## Related Projects

- **[opencode-anywhere](https://github.com/zero469/opencode-anywhere)** - The iOS app
- **[OpenCode](https://github.com/sst/opencode)** - The AI coding assistant

## License

MIT

## Credits

Built to enable secure remote access to [OpenCode](https://github.com/sst/opencode) by SST.
