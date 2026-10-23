# @thntime/opencode-tunnel

[中文](#中文) | [English](#english)

OpenCode Mobile 的 tunnel client：通过加密隧道，从任何网络访问本机 OpenCode。

## 中文

### 安装

```bash
npm install -g @thntime/opencode-tunnel
```

安装脚本会根据当前平台和包版本，从
[GitHub Releases](https://github.com/thntime/opencode-relay-server/releases)
下载对应的 `tunnel-client` 二进制。

### 使用

```bash
# 启动隧道（默认命令）
opencode-tunnel

# 指定本地 OpenCode 端口
opencode-tunnel -port 4096

# 查看状态
opencode-tunnel status

# 退出登录
opencode-tunnel logout
```

首次运行会登录、注册主机，并使用口令包裹 E2E 密钥。完整参数、自托管和协议说明见
[主仓库 README](https://github.com/thntime/opencode-relay-server#readme)。

### 支持平台

- macOS（Intel 与 Apple Silicon）
- Linux（x64 与 arm64）
- Windows（x64）

### 许可证

MIT

## English

OpenCode Mobile's tunnel client: reach your local OpenCode from any network over an encrypted tunnel.

### Install

```bash
npm install -g @thntime/opencode-tunnel
```

The install script downloads the matching `tunnel-client` binary for your platform and package
version from [GitHub Releases](https://github.com/thntime/opencode-relay-server/releases).

### Usage

```bash
# Start the tunnel (default command)
opencode-tunnel

# Use a custom local OpenCode port
opencode-tunnel -port 4096

# Show status
opencode-tunnel status

# Log out
opencode-tunnel logout
```

The first run logs in, registers the host, and wraps the E2E key with a passphrase. See the
[main README](https://github.com/thntime/opencode-relay-server#readme) for all options,
self-hosting, and protocol details.

### Supported platforms

- macOS (Intel and Apple Silicon)
- Linux (x64 and arm64)
- Windows (x64)

### License

MIT
