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
# 方式一：安装时一次性放行（推荐，全局安装）
npm install -g --allow-scripts=@thntime/opencode-tunnel

# 方式二：写入用户级配置，之后的全局安装都生效
npm config set allow-scripts=@thntime/opencode-tunnel --location=user
npm config get allow-scripts                          # 查看当前配置
```

> `npm install-scripts approve @thntime/opencode-tunnel` 只适用于**项目内**安装（需要项目里的
> `package.json`）；对全局安装（`-g`）使用会报 `EGLOBAL: npm install-scripts does not work for
> global installs`，不要混用。
>
> `npm config set allow-scripts` 会**整体替换**已有清单。如果之前放行过别的包（例如
> `allow-scripts=@opencode/cli`），请用逗号追加，避免把它们的放行一起清掉：
> `npm config set allow-scripts=@opencode/cli,@thntime/opencode-tunnel --location=user`。

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
空闲超时为 10s、单次尝试上限 300s。10s 空闲超时才是「卡死」的判定依据；300s 只用于兜住极端
慢速链路（例如极慢的镜像），避免正常但很慢的下载被误杀。`SHA256SUMS` 下载同样会重试。校验不通过
不会落地或执行二进制，临时文件 `<binary>.download` 会被清理。

### 代理与镜像

下载会遵守标准代理环境变量，适用于只能通过代理访问 GitHub 的网络：

| 变量 | 作用 |
| --- | --- |
| `HTTPS_PROXY` / `https_proxy` | https 目标（默认下载地址）使用的代理 |
| `HTTP_PROXY` / `http_proxy` | http 目标使用的代理；https 目标未设置 `HTTPS_PROXY` 时回退使用 |
| `NO_PROXY` / `no_proxy` | 不走代理的主机，支持 `*`、`*.example.com`、`.example.com`、`127.0.0.1`、`host:port`、`[::1]` 等写法 |

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
# Option 1: allow it once at install time (recommended, global install)
npm install -g --allow-scripts=@thntime/opencode-tunnel

# Option 2: persist it in the user config for future global installs
npm config set allow-scripts=@thntime/opencode-tunnel --location=user
npm config get allow-scripts                          # show the current setting
```

> `npm install-scripts approve @thntime/opencode-tunnel` only works for **project-local** installs
> (it needs the project's `package.json`). With `-g` npm 12 fails with
> `EGLOBAL: npm install-scripts does not work for global installs`, so do not use it there.
>
> `npm config set allow-scripts` **replaces** the existing list. If other packages were already
> allowed (for example `allow-scripts=@opencode/cli`), append with a comma so their approval is
> not dropped:
> `npm config set allow-scripts=@opencode/cli,@thntime/opencode-tunnel --location=user`.

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
socket idle timeout and a 300s cap; the 10s idle timeout is what detects a stuck connection, while the
300s cap only guards against extremely slow links (e.g. a very slow mirror) so a slow-but-working
download is not killed. `SHA256SUMS` downloads are retried the same way. A checksum mismatch never
leaves an executable behind, and the temporary `<binary>.download` file is cleaned up.

### Proxy and mirror

Downloads honour the standard proxy environment variables, which is what makes installs work on
networks where only a proxy can reach GitHub:

| Variable | Purpose |
| --- | --- |
| `HTTPS_PROXY` / `https_proxy` | Proxy for https targets (the default download URL) |
| `HTTP_PROXY` / `http_proxy` | Proxy for http targets; also used for https when `HTTPS_PROXY` is unset |
| `NO_PROXY` / `no_proxy` | Hosts that bypass the proxy; `*`, `*.example.com`, `.example.com`, `127.0.0.1`, `host:port`, and `[::1]` are supported |

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
