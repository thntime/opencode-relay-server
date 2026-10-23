# @thntime/opencode-tunnel

[中文](#中文) | [English](#english)

OpenCode Mobile 的 tunnel client：通过加密隧道，从任何网络访问本机 OpenCode。

## 中文

### 安装

```bash
npm install -g @thntime/opencode-tunnel
```

安装时（`postinstall`）会根据当前平台和包版本，从
[GitHub Releases](https://github.com/thntime/opencode-relay-server/releases)
下载对应的 `tunnel-client` 二进制，并用 Release 中的 `SHA256SUMS` 校验摘要。

> **npm 12 起默认阻止 install scripts**（`--ignore-scripts` 或 `ignore-scripts=true`）。
> 此时 `postinstall` 不会执行，这是预期行为：包本身仍会安装成功，
> `opencode-tunnel` **首次运行时会自动下载**二进制，无需额外操作。

### 放行安装脚本

npm 12 起默认不运行依赖的 install scripts（等价于 `--ignore-scripts` 或
`ignore-scripts=true`），因此本包的 `postinstall` 可能被拦截。放行方式：

```bash
# 方式一：安装时一次性放行（npm 自己给出的提示命令）
npm install -g --allow-scripts=@thntime/opencode-tunnel

# 方式二：安装后显式批准
npm install -g @thntime/opencode-tunnel
npm install-scripts approve @thntime/opencode-tunnel   # 允许该包运行 postinstall
npm install-scripts ls                                 # 查看当前放行清单
```

不放行也**不会**导致安装失败：包本身仍会安装成功，首次运行 `opencode-tunnel` 时会自动补下载
二进制（同样经过 `SHA256SUMS` 校验），区别只是下载时机从安装时推迟到首次运行。

### 使用

```bash
# 启动隧道（默认命令）
opencode-tunnel

# 省略 start 与显式 start 等价
opencode-tunnel -port 4096
opencode-tunnel start -port 4096

# 查看状态
opencode-tunnel status

# 发现本机正在运行的 OpenCode 实例
opencode-tunnel discover

# 退出登录
opencode-tunnel logout

# 查看版本 / 帮助
opencode-tunnel --version
opencode-tunnel help
```

首次运行会登录、注册主机，并使用口令包裹 E2E 密钥。完整参数、自托管和协议说明见
[主仓库 README](https://github.com/thntime/opencode-relay-server#readme)。

### 下载地址与校验

默认下载地址为：

```text
https://github.com/thntime/opencode-relay-server/releases/download/<tag>/<binary>
```

其中 `<tag>` 取自包版本（如 `v0.9.10`），`<binary>` 见下方平台表。校验和默认取同一目录下的
`SHA256SUMS`。

两个环境变量可覆盖默认地址（适合镜像或内网分发）：

| 变量 | 作用 | 默认值 |
| --- | --- | --- |
| `OPENCODE_TUNNEL_DOWNLOAD_BASE` | 二进制与校验和的基址 | `https://github.com/thntime/opencode-relay-server/releases/download` |
| `OPENCODE_TUNNEL_SHA256SUMS_URL` | 单独指定 `SHA256SUMS` 地址 | `<BASE>/<tag>/SHA256SUMS` |

```bash
export OPENCODE_TUNNEL_DOWNLOAD_BASE="https://mirror.example.com/opencode-relay"
export OPENCODE_TUNNEL_SHA256SUMS_URL="https://mirror.example.com/opencode-relay/v0.9.10/SHA256SUMS"
opencode-tunnel
```

镜像通常只同步二进制，不提供 `SHA256SUMS`，此时必须用 `OPENCODE_TUNNEL_SHA256SUMS_URL`
指向可用的校验和文件。下载最多尝试 **4 次**（3 次重试，退避 1s / 2s / 4s）；每次尝试的 socket
空闲超时为 10s、单次尝试上限 120s。`SHA256SUMS` 下载同样会重试。校验不通过不会落地或执行
二进制，临时文件 `<binary>.download` 会被清理。

### 代理与镜像

下载会遵守标准代理环境变量，适用于只能通过代理访问 GitHub 的网络：

| 变量 | 作用 |
| --- | --- |
| `HTTPS_PROXY` / `https_proxy` | https 目标（默认下载地址）使用的代理 |
| `HTTP_PROXY` / `http_proxy` | http 目标使用的代理；https 目标未设置 `HTTPS_PROXY` 时回退使用 |
| `NO_PROXY` / `no_proxy` | 不走代理的主机，支持 `*`、`.example.com`、`127.0.0.1` 等写法 |

```bash
export HTTPS_PROXY="http://proxy.corp.example:8080"
export NO_PROXY="localhost,127.0.0.1,.internal.example.com"
opencode-tunnel
```

代理通过 HTTP CONNECT 隧道建立（`https-proxy-agent`），不需要额外配置。镜像/内网分发则用上表
的 `OPENCODE_TUNNEL_DOWNLOAD_BASE` 与 `OPENCODE_TUNNEL_SHA256SUMS_URL` 指向自己的地址。

### 手动下载与校验

自动下载失败时，可以按下面的命令手动安装（以 Linux x64、版本 `0.9.10` 为例）：

```bash
VERSION=0.9.10
BASE=https://github.com/thntime/opencode-relay-server/releases/download
ASSET=tunnel-client-linux-amd64

curl -fL "$BASE/v$VERSION/$ASSET" -o ./tunnel-client
curl -fL "$BASE/v$VERSION/SHA256SUMS" -o ./SHA256SUMS
sha256sum ./tunnel-client            # macOS 用：shasum -a 256 ./tunnel-client
chmod +x ./tunnel-client
./tunnel-client --version
```

把 `sha256sum` 的输出与 `SHA256SUMS` 中对应文件名的行比对，一致后再执行；Windows 上用
`certutil -hashfile <path> SHA256` 替代 `sha256sum`。若使用镜像，
把 `BASE` 换成镜像基址，或通过 `OPENCODE_TUNNEL_SHA256SUMS_URL` 指定校验和地址。

### 支持平台

| 平台 | 下载的文件名 |
| --- | --- |
| macOS（Apple Silicon） | `tunnel-client-darwin-arm64` |
| macOS（Intel） | `tunnel-client-darwin-amd64` |
| Linux x64 | `tunnel-client-linux-amd64` |
| Linux arm64 | `tunnel-client-linux-arm64` |
| Windows x64 | `tunnel-client-windows-amd64.exe` |

### 许可证

MIT

## English

OpenCode Mobile's tunnel client: reach your local OpenCode from any network over an encrypted tunnel.

### Install

```bash
npm install -g @thntime/opencode-tunnel
```

The `postinstall` script downloads the matching `tunnel-client` binary for your platform and package
version from [GitHub Releases](https://github.com/thntime/opencode-relay-server/releases) and
verifies it against the release `SHA256SUMS`.

> **npm 12 blocks install scripts by default** (`--ignore-scripts` or `ignore-scripts=true`).
> This is expected: the package still installs, and the binary is downloaded automatically on the
> first `opencode-tunnel` run.

### Allowing install scripts

Since npm 12, dependency install scripts are not run by default (the same effect as
`--ignore-scripts` or `ignore-scripts=true`), so this package's `postinstall` may be blocked. To
allow it:

```bash
npm install -g @thntime/opencode-tunnel
npm install-scripts approve @thntime/opencode-tunnel   # allow this package to run postinstall
npm install-scripts ls                                 # list the current allow list
```

Not allowing it does **not** break the install: the package still installs, and the first
`opencode-tunnel` run downloads the binary (verified against `SHA256SUMS` as well). The only
difference is when the download happens.

### Usage

```bash
# Start the tunnel (default command)
opencode-tunnel

# Omitting start is equivalent to passing it explicitly
opencode-tunnel -port 4096
opencode-tunnel start -port 4096

# Show status
opencode-tunnel status

# Discover running OpenCode instances
opencode-tunnel discover

# Log out
opencode-tunnel logout

# Print version / help
opencode-tunnel --version
opencode-tunnel help
```

The first run logs in, registers the host, and wraps the E2E key with a passphrase. See the
[main README](https://github.com/thntime/opencode-relay-server#readme) for all options,
self-hosting, and protocol details.

### Download source and checksums

The default download URL is
`https://github.com/thntime/opencode-relay-server/releases/download/<tag>/<binary>`, where `<tag>`
comes from the package version and the checksum is read from `SHA256SUMS` in the same directory.

| Variable | Purpose | Default |
| --- | --- | --- |
| `OPENCODE_TUNNEL_DOWNLOAD_BASE` | Base URL for binaries and checksums | `https://github.com/thntime/opencode-relay-server/releases/download` |
| `OPENCODE_TUNNEL_SHA256SUMS_URL` | Override the `SHA256SUMS` URL | `<BASE>/<tag>/SHA256SUMS` |

```bash
export OPENCODE_TUNNEL_DOWNLOAD_BASE="https://mirror.example.com/opencode-relay"
export OPENCODE_TUNNEL_SHA256SUMS_URL="https://mirror.example.com/opencode-relay/v0.9.10/SHA256SUMS"
opencode-tunnel
```

Downloads make up to **4 attempts** (3 retries with 1s / 2s / 4s backoff). Each attempt has a 10s
socket idle timeout and a 120s cap; `SHA256SUMS` downloads are retried the same way. A checksum
mismatch never leaves an executable behind, and the temporary `<binary>.download` file is cleaned up.

### Proxy and mirror

Downloads honour the standard proxy environment variables, which is what makes installs work on
networks where only a proxy can reach GitHub:

| Variable | Purpose |
| --- | --- |
| `HTTPS_PROXY` / `https_proxy` | Proxy for https targets (the default download URL) |
| `HTTP_PROXY` / `http_proxy` | Proxy for http targets; also used for https when `HTTPS_PROXY` is unset |
| `NO_PROXY` / `no_proxy` | Hosts that bypass the proxy; `*`, `.example.com`, and `127.0.0.1` are supported |

```bash
export HTTPS_PROXY="http://proxy.corp.example:8080"
export NO_PROXY="localhost,127.0.0.1,.internal.example.com"
opencode-tunnel
```

The proxy is used as an HTTP CONNECT tunnel (`https-proxy-agent`); no extra configuration is
needed. For a mirror or internal distribution, point `OPENCODE_TUNNEL_DOWNLOAD_BASE` and
`OPENCODE_TUNNEL_SHA256SUMS_URL` (table above) at your own location.

### Manual download

```bash
VERSION=0.9.10
BASE=https://github.com/thntime/opencode-relay-server/releases/download
ASSET=tunnel-client-linux-amd64

curl -fL "$BASE/v$VERSION/$ASSET" -o ./tunnel-client
curl -fL "$BASE/v$VERSION/SHA256SUMS" -o ./SHA256SUMS
sha256sum ./tunnel-client            # macOS: shasum -a 256 ./tunnel-client
chmod +x ./tunnel-client
./tunnel-client --version
```

Compare the output with the matching line in `SHA256SUMS` before running the binary. On Windows, use
`certutil -hashfile <path> SHA256` instead of `sha256sum`.

### Supported platforms

- macOS (Intel and Apple Silicon)
- Linux (x64 and arm64)
- Windows (x64)

### License

MIT
