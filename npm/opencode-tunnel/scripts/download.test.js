'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const net = require('node:net');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');

const {
  ensureBinary,
  buildUrls,
  isNoProxyHost,
  createProxyAgent,
  PLATFORM_MAP
} = require('./download.js');

const BINARY_NAME = 'tunnel-client-linux-amd64';
const DOWNLOAD_PREFIX = '/releases/download';
const BINARY_ROUTE = `${DOWNLOAD_PREFIX}/v0.9.10/${BINARY_NAME}`;
const SUMS_ROUTE = `${DOWNLOAD_PREFIX}/v0.9.10/SHA256SUMS`;
const PAYLOAD = Buffer.from('#!/bin/sh\necho fake tunnel client\n');
const GOOD_HASH = crypto.createHash('sha256').update(PAYLOAD).digest('hex');
const GOOD_SUMS = `${GOOD_HASH}  ${BINARY_NAME}\n`;

// `undefined` deletes the variable; anything else sets it. Every test that
// touches ensureBinary() isolates these first, so a developer machine with a
// proxy or mirror configured cannot change the outcome.
const ISOLATED_ENV_KEYS = [
  'OPENCODE_TUNNEL_DOWNLOAD_BASE',
  'OPENCODE_TUNNEL_SHA256SUMS_URL',
  'HTTP_PROXY',
  'HTTPS_PROXY',
  'http_proxy',
  'https_proxy',
  'NO_PROXY',
  'no_proxy'
];

function withEnv(vars) {
  const saved = {};
  for (const [key, value] of Object.entries(vars)) {
    saved[key] = process.env[key];
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
  return () => {
    for (const [key, value] of Object.entries(saved)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  };
}

function isolateEnv(t) {
  const cleared = {};
  for (const key of ISOLATED_ENV_KEYS) cleared[key] = undefined;
  const restore = withEnv(cleared);
  t.after(restore);
}

async function withServer(handler, fn) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${server.address().port}/releases/download`;
  try {
    return await fn(base);
  } finally {
    if (typeof server.closeAllConnections === 'function') server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

function makeBinDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'opencode-tunnel-test-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// A local proxy that only speaks CONNECT: it tunnels the client to whatever
// host:port the CONNECT line names, so no test ever needs the real internet.
// `connects` records every CONNECT target for assertions.
async function withConnectProxy(fn) {
  const connects = [];
  const upstreams = new Set();
  const proxy = http.createServer((req, res) => {
    res.writeHead(405);
    res.end('CONNECT only');
  });

  proxy.on('connect', (req, clientSocket, head) => {
    connects.push(req.url);
    const separator = req.url.lastIndexOf(':');
    const host = req.url.slice(0, separator);
    const port = Number(req.url.slice(separator + 1));

    const upstream = net.connect(port, host, () => {
      clientSocket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
      if (head && head.length > 0) upstream.write(head);
      upstream.pipe(clientSocket);
      clientSocket.pipe(upstream);
    });
    upstreams.add(upstream);
    upstream.on('close', () => upstreams.delete(upstream));
    upstream.on('error', () => clientSocket.destroy());
    clientSocket.on('error', () => upstream.destroy());
  });

  await new Promise((resolve) => proxy.listen(0, '127.0.0.1', resolve));
  try {
    return await fn(`http://127.0.0.1:${proxy.address().port}`, connects);
  } finally {
    for (const upstream of upstreams) upstream.destroy();
    if (typeof proxy.closeAllConnections === 'function') proxy.closeAllConnections();
    await new Promise((resolve) => proxy.close(resolve));
  }
}

// Serves the binary and its SHA256SUMS; shared by the proxy tests below.
function serveRelease(req, res) {
  if (req.url === BINARY_ROUTE) {
    res.writeHead(200, { 'content-type': 'application/octet-stream' });
    res.end(PAYLOAD);
    return;
  }
  if (req.url === SUMS_ROUTE) {
    res.writeHead(200, { 'content-type': 'text/plain' });
    res.end(GOOD_SUMS);
    return;
  }
  res.writeHead(404);
  res.end('not found');
}

function fastOptions(binDir, extra) {
  return Object.assign(
    {
      platform: 'linux',
      arch: 'x64',
      binDir,
      retryDelays: [0, 0, 0],
      requestTimeoutMs: 2000,
      totalTimeoutMs: 5000,
      log: process.env.DEBUG_TUNNEL ? console.error : () => {}
    },
    extra
  );
}

test('downloads the binary, verifies the checksum and chmods it', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        res.writeHead(200, { 'content-type': 'application/octet-stream' });
        res.end(PAYLOAD);
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200, { 'content-type': 'text/plain' });
        res.end(GOOD_SUMS);
        return;
      }
      res.writeHead(404);
      res.end('not found');
    },
    async (base) => {
      process.env.OPENCODE_TUNNEL_DOWNLOAD_BASE = base;
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, true);
      assert.equal(result.reason, 'downloaded');
      const binaryPath = path.join(binDir, BINARY_NAME);
      assert.equal(result.binaryPath, binaryPath);
      assert.deepEqual(fs.readFileSync(binaryPath), PAYLOAD);
      assert.equal(fs.statSync(binaryPath).mode & 0o777, 0o755);
      assert.equal(fs.existsSync(`${binaryPath}.download`), false);
    }
  );
});

test('short-circuits when the binary already exists', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  const binaryPath = path.join(binDir, BINARY_NAME);
  // A real binary starts with a known magic; a plain text placeholder is now
  // treated as a truncated leftover and re-downloaded.
  fs.writeFileSync(binaryPath, Buffer.concat([Buffer.from([0x7f, 0x45, 0x4c, 0x46]), PAYLOAD]));

  let requests = 0;
  await withServer(
    (req, res) => {
      requests += 1;
      res.writeHead(500);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));
      assert.equal(result.ok, true);
      assert.equal(result.reason, 'already_present');
      assert.equal(requests, 0);
    }
  );
});

test('re-downloads when the existing file is not a real binary', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  const binaryPath = path.join(binDir, BINARY_NAME);
  fs.writeFileSync(binaryPath, 'truncated');

  await withServer(
    (req, res) => {
      if (req.url.endsWith('SHA256SUMS')) {
        res.writeHead(200);
        res.end(`${GOOD_HASH}  ${BINARY_NAME}\n`);
        return;
      }
      res.writeHead(200);
      res.end(PAYLOAD);
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));
      assert.equal(result.ok, true);
      assert.equal(result.reason, 'downloaded');
      assert.deepEqual(fs.readFileSync(binaryPath), PAYLOAD);
    }
  );
});

test('rejects a checksum mismatch and leaves no binary behind', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  const badSums = `${'0'.repeat(64)}  ${BINARY_NAME}\n`;

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        res.writeHead(200);
        res.end(PAYLOAD);
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200);
        res.end(badSums);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'checksum_mismatch');
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
      assert.equal(fs.existsSync(path.join(binDir, `${BINARY_NAME}.download`)), false);
    }
  );
});

test('reports checksum_missing when SHA256SUMS does not list the binary', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        res.writeHead(200);
        res.end(PAYLOAD);
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200);
        res.end(`${GOOD_HASH}  some-other-binary\n`);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'checksum_missing');
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
    }
  );
});

test('reports checksums_unavailable when SHA256SUMS cannot be downloaded', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        res.writeHead(200);
        res.end(PAYLOAD);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'checksums_unavailable');
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
    }
  );
});

test('does not retry a 404', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  let binaryRequests = 0;

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        binaryRequests += 1;
        res.writeHead(404);
        res.end('not found');
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200);
        res.end(GOOD_SUMS);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'download_failed');
      assert.equal(result.attempts, 1);
      assert.equal(binaryRequests, 1);
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
    }
  );
});

test('retries a transient failure and succeeds on the third attempt', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  let binaryRequests = 0;

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        binaryRequests += 1;
        if (binaryRequests < 3) {
          res.writeHead(502);
          res.end('bad gateway');
          return;
        }
        res.writeHead(200);
        res.end(PAYLOAD);
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200);
        res.end(GOOD_SUMS);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, true);
      assert.equal(result.reason, 'downloaded');
      assert.equal(binaryRequests, 3);
      assert.deepEqual(fs.readFileSync(path.join(binDir, BINARY_NAME)), PAYLOAD);
    }
  );
});

test('gives up after three retries (four attempts)', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  let binaryRequests = 0;

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        binaryRequests += 1;
        res.writeHead(500);
        res.end('boom');
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'download_failed');
      assert.equal(result.attempts, 4);
      assert.equal(binaryRequests, 4);
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
      assert.equal(fs.existsSync(path.join(binDir, `${BINARY_NAME}.download`)), false);
    }
  );
});

test('aborts a stalled request and retries before giving up', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  let binaryRequests = 0;

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        binaryRequests += 1;
        // Intentionally never respond: the client must enforce its own timeout.
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(
        fastOptions(binDir, { requestTimeoutMs: 50, totalTimeoutMs: 200 })
      );

      assert.equal(result.ok, false);
      assert.equal(result.reason, 'download_failed');
      assert.equal(result.attempts, 4);
      assert.equal(binaryRequests, 4);
      assert.equal(fs.existsSync(path.join(binDir, BINARY_NAME)), false);
    }
  );
});

test('follows a relative redirect before downloading', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);
  const finalRoute = '/releases/files/binary';

  await withServer(
    (req, res) => {
      if (req.url === BINARY_ROUTE) {
        res.writeHead(302, { location: '../../files/binary' });
        res.end();
        return;
      }
      if (req.url === finalRoute) {
        res.writeHead(200);
        res.end(PAYLOAD);
        return;
      }
      if (req.url === SUMS_ROUTE) {
        res.writeHead(200);
        res.end(GOOD_SUMS);
        return;
      }
      res.writeHead(404);
      res.end();
    },
    async (base) => {
      const restoreEnv = withEnv({ OPENCODE_TUNNEL_DOWNLOAD_BASE: base });
      t.after(restoreEnv);
      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, true);
      assert.deepEqual(fs.readFileSync(path.join(binDir, BINARY_NAME)), PAYLOAD);
    }
  );
});

test('downloads through an HTTP CONNECT proxy and still verifies the checksum', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);

  await withConnectProxy(async (proxyUrl, connects) => {
    await withServer(serveRelease, async (base) => {
      // isolateEnv() cleared every proxy variable; set them only now.
      const restoreEnv = withEnv({
        OPENCODE_TUNNEL_DOWNLOAD_BASE: base,
        HTTP_PROXY: proxyUrl
      });
      t.after(restoreEnv);

      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, true);
      assert.equal(result.reason, 'downloaded');
      assert.deepEqual(fs.readFileSync(path.join(binDir, BINARY_NAME)), PAYLOAD);
      assert.ok(
        connects.length >= 1,
        `expected at least one CONNECT request, saw ${connects.length}`
      );
      assert.equal(connects[0], new URL(base).host);
    });
  });
});

test('bypasses the proxy when NO_PROXY matches the download host', async (t) => {
  const binDir = makeBinDir(t);
  isolateEnv(t);

  await withConnectProxy(async (proxyUrl, connects) => {
    await withServer(serveRelease, async (base) => {
      const restoreEnv = withEnv({
        OPENCODE_TUNNEL_DOWNLOAD_BASE: base,
        HTTP_PROXY: proxyUrl,
        NO_PROXY: '127.0.0.1'
      });
      t.after(restoreEnv);

      const result = await ensureBinary(fastOptions(binDir));

      assert.equal(result.ok, true);
      assert.equal(result.reason, 'downloaded');
      assert.deepEqual(fs.readFileSync(path.join(binDir, BINARY_NAME)), PAYLOAD);
      assert.equal(
        connects.length,
        0,
        `expected a direct connection, saw ${connects.length} CONNECT(s)`
      );
    });
  });
});

test('returns unsupported_platform for an unknown platform', async (t) => {
  isolateEnv(t);
  const result = await ensureBinary({
    platform: 'freebsd',
    arch: 'riscv64',
    retryDelays: [0, 0, 0],
    log: () => {}
  });
  assert.equal(result.ok, false);
  assert.equal(result.reason, 'unsupported_platform');
  assert.equal(result.binaryPath, null);
});

test('isNoProxyHost matches wildcard, suffix, exact host and rejects others', () => {
  const target = new URL('https://api.example.com/releases/download/v1/file');

  assert.equal(isNoProxyHost(target, { NO_PROXY: '*' }), true);
  assert.equal(isNoProxyHost(target, { NO_PROXY: '.example.com' }), true);
  assert.equal(
    isNoProxyHost(new URL('http://127.0.0.1:1/x'), { NO_PROXY: '127.0.0.1' }),
    true
  );
  assert.equal(isNoProxyHost(target, { NO_PROXY: 'other.com' }), false);
  assert.equal(isNoProxyHost(target, {}), false);
});

test('createProxyAgent returns an agent only when a proxy applies', () => {
  const target = 'http://127.0.0.1:1/x';

  const agent = createProxyAgent(target, { HTTP_PROXY: 'http://127.0.0.1:2' });
  assert.notEqual(
    agent,
    null,
    'createProxyAgent returned null; run `npm install` in npm/opencode-tunnel so https-proxy-agent is present'
  );
  assert.equal(typeof agent.addRequest, 'function');

  // NO_PROXY wins over a configured proxy.
  assert.equal(
    createProxyAgent(target, { HTTP_PROXY: 'http://127.0.0.1:2', NO_PROXY: '*' }),
    null
  );

  // No proxy variables at all.
  assert.equal(createProxyAgent(target, {}), null);

  // An https target falls back to HTTP_PROXY.
  const httpsAgent = createProxyAgent('https://example.com/file', {
    HTTP_PROXY: 'http://127.0.0.1:2'
  });
  assert.notEqual(
    httpsAgent,
    null,
    'createProxyAgent returned null for an https target; run `npm install` in npm/opencode-tunnel'
  );
  assert.equal(typeof httpsAgent.addRequest, 'function');
});

test('builds default and overridden download URLs', () => {
  const defaults = buildUrls({ version: '0.9.10', binaryName: BINARY_NAME, env: {} });
  assert.equal(defaults.tag, 'v0.9.10');
  assert.equal(
    defaults.url,
    `https://github.com/thntime/opencode-relay-server/releases/download/v0.9.10/${BINARY_NAME}`
  );
  assert.equal(
    defaults.checksumsUrl,
    'https://github.com/thntime/opencode-relay-server/releases/download/v0.9.10/SHA256SUMS'
  );

  const overridden = buildUrls({
    version: 'v0.9.10',
    binaryName: BINARY_NAME,
    env: {
      OPENCODE_TUNNEL_DOWNLOAD_BASE: 'https://mirror.example.com/tunnel/',
      OPENCODE_TUNNEL_SHA256SUMS_URL: 'https://mirror.example.com/tunnel/SHA256SUMS'
    }
  });
  assert.equal(overridden.tag, 'v0.9.10');
  assert.equal(overridden.url, `https://mirror.example.com/tunnel/v0.9.10/${BINARY_NAME}`);
  assert.equal(overridden.checksumsUrl, 'https://mirror.example.com/tunnel/SHA256SUMS');
});

test('exposes exactly the supported platform table', () => {
  assert.deepEqual(Object.keys(PLATFORM_MAP).sort(), [
    'darwin-arm64',
    'darwin-x64',
    'linux-arm64',
    'linux-x64',
    'win32-x64'
  ]);
});
