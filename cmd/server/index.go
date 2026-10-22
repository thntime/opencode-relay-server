package main

// indexHTML is a tiny landing page so visiting the relay root shows what this
// service is instead of a bare 404. It deliberately has no external assets:
// the relay is deployed on small hosts and must render without network access.
const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OpenCode Relay</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; font: 16px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
         display: flex; min-height: 100vh; align-items: center; justify-content: center; }
  main { width: min(680px, 92vw); padding: 32px 0; }
  h1 { font-size: 28px; margin: 0 0 4px; }
  p.sub { margin: 0 0 24px; opacity: .65; }
  .status { display: inline-flex; align-items: center; gap: 8px; padding: 6px 12px; border-radius: 999px;
            background: rgba(127,127,127,.12); font-size: 14px; margin-bottom: 28px; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: #999; }
  .dot.ok { background: #2e9e5b; } .dot.bad { background: #d94a4a; }
  table { width: 100%; border-collapse: collapse; font-size: 14px; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid rgba(127,127,127,.18); }
  th { font-weight: 600; opacity: .7; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
  footer { margin-top: 28px; font-size: 13px; opacity: .55; }
</style>
</head>
<body>
<main>
  <h1>OpenCode Relay</h1>
  <p class="sub">手机远程访问本机 OpenCode 的中继服务</p>
  <div class="status"><span class="dot" id="dot"></span><span id="status">检查中…</span></div>
  <table>
    <tr><th>用途</th><th>端点</th></tr>
    <tr><td>健康检查</td><td><code>GET /api/health</code></td></tr>
    <tr><td>登录</td><td><code>POST /api/login</code></td></tr>
    <tr><td>主机列表</td><td><code>GET /api/hosts</code></td></tr>
    <tr><td>请求转发</td><td><code>/relay/{hostId}/...</code></td></tr>
    <tr><td>隧道连接</td><td><code>GET /api/tunnel/{hostId}</code></td></tr>
  </table>
  <footer>这是 API 服务，没有管理界面。主机端运行 <code>opencode-tunnel start --relay &lt;本地址&gt;</code>，手机端在 App 里选择「使用自建 Relay」。</footer>
</main>
<script>
  fetch('/api/health', { cache: 'no-store' })
    .then(r => r.ok ? r.json() : Promise.reject(r.status))
    .then(d => {
      document.getElementById('dot').className = 'dot ok';
      document.getElementById('status').textContent = '运行中 · ' + (d.status || 'ok');
    })
    .catch(() => {
      document.getElementById('dot').className = 'dot bad';
      document.getElementById('status').textContent = '不可用';
    });
</script>
</body>
</html>
`
