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

Streaming responses (SSE) and WebSocket forwarding (terminal PTY) are not implemented yet; the tunnel
protocol is request/response only, so SSE clients fall back to polling until that work lands.

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
