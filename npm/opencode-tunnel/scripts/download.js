'use strict';

const fs = require('fs');
const path = require('path');
const http = require('http');
const https = require('https');
const crypto = require('crypto');

const REPO = 'thntime/opencode-relay-server';
const DEFAULT_DOWNLOAD_BASE = `https://github.com/${REPO}/releases/download`;

// Node has no built-in proxy support for https.get, so honour the standard
// proxy environment variables explicitly. This is what makes installs work on
// corporate/restricted networks where only the proxy can reach GitHub.
function isNoProxyHost(parsed, env) {
  const raw = env.NO_PROXY || env.no_proxy;
  if (!raw) return false;
  const host = String(parsed.hostname || '')
    .toLowerCase()
    .replace(/^\[|\]$/g, '');

  return String(raw)
    .split(/[,\s]+/)
    .filter(Boolean)
    .some((entry) => {
      let value = entry.trim().toLowerCase();
      if (!value) return false;
      if (value === '*') return true;

      value = value.replace(/^[a-z][a-z0-9+.-]*:\/\//, '');
      if (value.startsWith('[')) {
        const close = value.indexOf(']');
        if (close === -1) return false;
        value = value.slice(1, close);
      } else if ((value.match(/:/g) || []).length === 1) {
        value = value.split(':')[0];
      }

      const bare = value.startsWith('*.')
        ? value.slice(2)
        : value.startsWith('.')
          ? value.slice(1)
          : value;
      if (!bare) return false;
      return host === bare || host.endsWith(`.${bare}`);
    });
}

function createProxyAgent(targetUrl, env) {
  let parsed;
  try {
    parsed = new URL(targetUrl);
  } catch (err) {
    return null;
  }
  if (isNoProxyHost(parsed, env)) return null;

  const isHttps = parsed.protocol === 'https:';
  const raw = isHttps
    ? env.HTTPS_PROXY || env.https_proxy || env.HTTP_PROXY || env.http_proxy
    : env.HTTP_PROXY || env.http_proxy;
  if (!raw) return null;

  // curl accepts "proxy.corp:8080" without a scheme; normalise before parsing
  // so a scheme-less value cannot silently produce an agent with an empty host.
  const candidate = /^[a-z][a-z0-9+.-]*:\/\//i.test(raw) ? raw : `http://${raw}`;
  let proxyUrl;
  try {
    proxyUrl = new URL(candidate);
  } catch (err) {
    return null;
  }
  if (!/^https?:$/.test(proxyUrl.protocol) || !proxyUrl.hostname) {
    return null;
  }

  try {
    // Loaded lazily so the module still works if the dependency is missing.
    const { HttpsProxyAgent } = require('https-proxy-agent');
    return new HttpsProxyAgent(proxyUrl.toString());
  } catch (err) {
    return null;
  }
}

function redactUrl(rawUrl) {
  try {
    const parsed = new URL(rawUrl);
    if (parsed.username || parsed.password) {
      parsed.username = '***';
      parsed.password = '';
    }
    return parsed.toString();
  } catch (err) {
    return rawUrl;
  }
}

function printManualInstructions(result, write) {
  const out = write || console.error;
  if (!result.url || !result.binaryPath) return;

  const url = redactUrl(result.url);
  out('');
  out('Manual download:');
  out(`  curl -fL ${url} -o ${result.binaryPath}`);
  if (process.platform === 'win32') {
    out(`  certutil -hashfile ${result.binaryPath} SHA256`);
  } else {
    out(`  sha256sum ${result.binaryPath}`);
  }
  if (result.checksumsUrl) {
    out('');
    out(`Verify the output against ${redactUrl(result.checksumsUrl)}`);
  }
}

// The single source of truth for platform -> release asset names.
const PLATFORM_MAP = {
  'darwin-arm64': 'tunnel-client-darwin-arm64',
  'darwin-x64': 'tunnel-client-darwin-amd64',
  'linux-arm64': 'tunnel-client-linux-arm64',
  'linux-x64': 'tunnel-client-linux-amd64',
  'win32-x64': 'tunnel-client-windows-amd64.exe'
};

// 3 retries: up to 4 attempts, with 1s/2s/4s backoff between them.
const DEFAULT_RETRY_DELAYS = [1000, 2000, 4000];
const DEFAULT_MAX_ATTEMPTS = 4;
const DEFAULT_REQUEST_TIMEOUT_MS = 10000;
const DEFAULT_TOTAL_TIMEOUT_MS = 300000;
const MAX_REDIRECTS = 5;
// Buffered responses (the SHA256SUMS file) are tiny; cap them so a wrong URL
// cannot stream an arbitrary amount of data into memory.
const MAX_BUFFER_BYTES = 1024 * 1024;

function buildUrls(options) {
  const opts = options || {};
  const env = opts.env || process.env;
  const base = (env.OPENCODE_TUNNEL_DOWNLOAD_BASE || DEFAULT_DOWNLOAD_BASE).replace(/\/+$/, '');
  const rawVersion = String(opts.version);
  const tag = rawVersion.charAt(0) === 'v' ? rawVersion : `v${rawVersion}`;
  const checksumsUrl =
    env.OPENCODE_TUNNEL_SHA256SUMS_URL || `${base}/${tag}/SHA256SUMS`;

  return {
    base,
    tag,
    url: `${base}/${tag}/${opts.binaryName}`,
    checksumsUrl
  };
}

function removeQuietly(filePath) {
  try {
    fs.unlinkSync(filePath);
  } catch (err) {
    // best effort: the temp file may not exist
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Fetch a URL, following redirects manually (Node >= 14 has no fetch) and
 * enforcing both an idle timeout and an overall timeout per call.
 *
 * Resolves with a Buffer when `dest` is not provided, otherwise writes the
 * response body to `dest` and resolves with undefined.
 */
function fetchUrl(url, options) {
  const dest = options.dest;
  const requestTimeoutMs =
    options.requestTimeoutMs === undefined ? DEFAULT_REQUEST_TIMEOUT_MS : options.requestTimeoutMs;
  const totalTimeoutMs =
    options.totalTimeoutMs === undefined ? DEFAULT_TOTAL_TIMEOUT_MS : options.totalTimeoutMs;
  const maxRedirects = options.maxRedirects === undefined ? MAX_REDIRECTS : options.maxRedirects;

  return new Promise((resolve, reject) => {
    let currentReq = null;
    let currentFile = null;
    let settled = false;

    const totalTimer =
      totalTimeoutMs > 0
        ? setTimeout(() => {
            fail(
              Object.assign(new Error(`Download timed out after ${totalTimeoutMs}ms: ${redactUrl(url)}`), {
                code: 'ETIMEDOUT'
              })
            );
          }, totalTimeoutMs)
        : null;

    function fail(err) {
      if (settled) return;
      settled = true;
      clearTimeout(totalTimer);
      if (currentFile) {
        try {
          currentFile.destroy();
        } catch (destroyErr) {
          // ignore: stream may already be closed
        }
      }
      if (currentReq) {
        try {
          currentReq.destroy();
        } catch (destroyErr) {
          // ignore: socket may already be gone
        }
      }
      reject(err);
    }

    function succeed(value) {
      if (settled) return;
      settled = true;
      clearTimeout(totalTimer);
      resolve(value);
    }

    function start(targetUrl, redirectsLeft) {
      let parsed;
      try {
        parsed = new URL(targetUrl);
      } catch (err) {
        fail(err);
        return;
      }

      if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
        fail(
          new Error(`Unsupported protocol ${parsed.protocol} while downloading ${redactUrl(url)}`)
        );
        return;
      }

      const client = parsed.protocol === 'http:' ? http : https;
      // Disable connection pooling: a destroyed socket must never be reused by
      // the next retry attempt. A proxy agent is only used when the standard
      // proxy environment variables point at one and NO_PROXY does not match.
      const agent = createProxyAgent(targetUrl, options.env || process.env) || false;

      function onResponse(res) {
        const status = res.statusCode;

        if (status >= 300 && status < 400 && res.headers.location) {
          res.resume();
          if (redirectsLeft <= 0) {
            fail(new Error(`Too many redirects while downloading ${redactUrl(url)}`));
            return;
          }
          let nextUrl;
          try {
            nextUrl = new URL(res.headers.location, targetUrl).toString();
          } catch (err) {
            fail(err);
            return;
          }
          start(nextUrl, redirectsLeft - 1);
          return;
        }

        if (status !== 200) {
          res.resume();
          fail(
            Object.assign(new Error(`HTTP ${status}: ${redactUrl(targetUrl)}`), {
              statusCode: status
            })
          );
          return;
        }

        if (!dest) {
          const chunks = [];
          let total = 0;
          res.on('data', (chunk) => {
            total += chunk.length;
            if (total > MAX_BUFFER_BYTES) {
              res.destroy();
              fail(new Error(`Response too large (limit ${MAX_BUFFER_BYTES} bytes): ${redactUrl(targetUrl)}`));
              return;
            }
            chunks.push(chunk);
          });
          res.on('end', () => succeed(Buffer.concat(chunks)));
          res.on('error', fail);
          res.on('aborted', () => fail(new Error(`Response aborted: ${redactUrl(targetUrl)}`)));
          return;
        }

        const file = fs.createWriteStream(dest);
        currentFile = file;
        file.on('error', (err) => {
          file.destroy();
          fail(err);
        });
        res.on('error', (err) => {
          file.destroy();
          fail(err);
        });
        res.on('aborted', () => {
          file.destroy();
          fail(new Error(`Response aborted: ${redactUrl(targetUrl)}`));
        });
        res.pipe(file);
        file.on('finish', () => {
          currentFile = null;
          file.close((err) => {
            if (err) fail(err);
            else succeed(undefined);
          });
        });
      }

      let req;
      try {
        req = client.get(parsed, { agent }, onResponse);
      } catch (err) {
        // https.get throws synchronously for protocol/option problems; without
        // this guard the exception escapes the Promise and crashes the caller.
        fail(err);
        return;
      }

      currentReq = req;
      req.setTimeout(requestTimeoutMs, () => {
        req.destroy(
          Object.assign(
            new Error(`Request timed out after ${requestTimeoutMs}ms: ${redactUrl(targetUrl)}`),
            { code: 'ETIMEDOUT' }
          )
        );
      });
      // Only the current hop may settle the promise; a late error from a
      // previous redirect hop must not kill the hop that replaced it.
      req.on('error', (err) => {
        if (currentReq === req) fail(err);
      });
    }

    start(url, maxRedirects);
  });
}

// Only transient transport failures are retried. Local filesystem errors
// (EACCES/ENOSPC/EROFS) and protocol errors will not fix themselves, so they
// fail fast instead of burning four attempts.
const RETRYABLE_ERROR_CODES = new Set([
  'ETIMEDOUT',
  'ECONNRESET',
  'ECONNREFUSED',
  'ENOTFOUND',
  'EAI_AGAIN',
  'EPIPE',
  'EHOSTUNREACH',
  'ENETUNREACH',
  'ECONNABORTED'
]);

function isRetryable(err) {
  if (!err) return false;
  if (err.statusCode) {
    return err.statusCode === 408 || err.statusCode === 429 || err.statusCode >= 500;
  }
  if (err.code === 'EPROTO') return false;
  return RETRYABLE_ERROR_CODES.has(err.code);
}

// Shared retry loop: one implementation so the binary and the checksum fetch
// cannot drift apart in attempt count, backoff or env handling.
async function withRetry(task, options) {
  const retryDelays = options.retryDelays || DEFAULT_RETRY_DELAYS;
  const maxAttempts =
    options.maxAttempts === undefined ? DEFAULT_MAX_ATTEMPTS : options.maxAttempts;
  const log = typeof options.log === 'function' ? options.log : () => {};
  const label = options.label || 'Request';
  let lastError = null;

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    try {
      const value = await task(attempt + 1);
      return { ok: true, value, attempts: attempt + 1 };
    } catch (err) {
      lastError = err;
      if (typeof options.onError === 'function') options.onError(err);
      if (!isRetryable(err) || attempt === maxAttempts - 1) {
        return { ok: false, attempts: attempt + 1, error: lastError };
      }
      const delay = retryDelays[Math.min(attempt, retryDelays.length - 1)];
      log(
        `${label} attempt ${attempt + 1}/${maxAttempts} failed (${err.message}); retrying in ${delay}ms`
      );
      await sleep(delay);
    }
  }

  return { ok: false, attempts: maxAttempts, error: lastError };
}

function fetchOnce(url, options) {
  return fetchUrl(url, {
    dest: options.dest,
    env: options.env,
    requestTimeoutMs: options.requestTimeoutMs,
    totalTimeoutMs: options.totalTimeoutMs
  });
}

async function downloadWithRetry(url, dest, options) {
  const result = await withRetry(() => fetchOnce(url, { ...options, dest }), {
    retryDelays: options.retryDelays,
    maxAttempts: options.maxAttempts,
    log: options.log,
    label: 'Download',
    onError: () => removeQuietly(dest)
  });
  return { ok: result.ok, attempts: result.attempts, error: result.error };
}

async function fetchWithRetry(url, options) {
  const result = await withRetry(() => fetchOnce(url, options), {
    retryDelays: options.retryDelays,
    maxAttempts: options.maxAttempts,
    log: options.log,
    label: 'Fetch'
  });
  if (!result.ok) throw result.error;
  return result.value;
}

function parseChecksums(text) {
  const entries = new Map();
  const lines = String(text).split(/\r?\n/);

  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const match = /^([0-9a-fA-F]{64})\s+\*?(.+)$/.exec(trimmed);
    if (!match) continue;
    entries.set(match[2].trim(), match[1].toLowerCase());
  }

  return entries;
}

function sha256File(filePath) {
  return new Promise((resolve, reject) => {
    const hash = crypto.createHash('sha256');
    const stream = fs.createReadStream(filePath);
    stream.on('data', (chunk) => hash.update(chunk));
    stream.on('end', () => resolve(hash.digest('hex')));
    stream.on('error', reject);
  });
}

// The old installer wrote straight to the final path, so an interrupted
// install could leave a truncated file behind. Reject anything that does not
// start with a known executable magic instead of spawning it.
function hasBinaryMagic(filePath) {
  let fd;
  try {
    fd = fs.openSync(filePath, 'r');
    const header = Buffer.alloc(4);
    const read = fs.readSync(fd, header, 0, 4, 0);
    if (read < 4) return false;
    if (header.readUInt32BE(0) === 0x7f454c46) return true; // ELF
    if (header[0] === 0x4d && header[1] === 0x5a) return true; // MZ (Windows)
    const magic = header.readUInt32BE(0);
    return [0xfeedface, 0xfeedfacf, 0xcafebabe, 0xbebafeca, 0xcefaedfe, 0xcffaedfe].includes(magic);
  } catch (err) {
    return false;
  } finally {
    if (fd !== undefined) {
      try {
        fs.closeSync(fd);
      } catch (closeErr) {
        // ignore
      }
    }
  }
}

/**
 * Ensure the platform binary exists in the package's bin directory.
 *
 * Never calls process.exit: callers decide how to surface a failure.
 * Resolves to { ok, binaryPath, reason, url?, checksumsUrl?, attempts?, error? }.
 */
async function ensureBinary(options) {
  const opts = options || {};
  const log = typeof opts.log === 'function' ? opts.log : console.error;
  const platform = opts.platform || process.platform;
  const arch = opts.arch || process.arch;
  const platformKey = `${platform}-${arch}`;
  const binaryName = PLATFORM_MAP[platformKey];

  if (!binaryName) {
    log(`Unsupported platform: ${platformKey}`);
    log(`Supported: ${Object.keys(PLATFORM_MAP).join(', ')}`);
    return { ok: false, binaryPath: null, reason: 'unsupported_platform' };
  }

  const binDir = opts.binDir || path.join(__dirname, '..', 'bin');
  const binaryPath = path.join(binDir, binaryName);

  if (fs.existsSync(binaryPath)) {
    // The previous installer wrote straight to the final path, so an aborted
    // install could leave a truncated binary behind. Treat an empty file as
    // missing instead of spawning something that cannot execute.
    let existingSize = 0;
    try {
      existingSize = fs.statSync(binaryPath).size;
    } catch (err) {
      existingSize = 0;
    }
    if (existingSize > 0 && hasBinaryMagic(binaryPath)) {
      return { ok: true, binaryPath, reason: 'already_present' };
    }
    removeQuietly(binaryPath);
    log(`Removing unusable leftover binary: ${binaryPath}`);
  }

  const version = opts.version || require('../package.json').version;
  const urls = buildUrls({ version, binaryName, env: opts.env });
  const retryDelays = opts.retryDelays || DEFAULT_RETRY_DELAYS;
  const maxAttempts = opts.maxAttempts === undefined ? DEFAULT_MAX_ATTEMPTS : opts.maxAttempts;
  const requestTimeoutMs =
    opts.requestTimeoutMs === undefined ? DEFAULT_REQUEST_TIMEOUT_MS : opts.requestTimeoutMs;
  const totalTimeoutMs =
    opts.totalTimeoutMs === undefined ? DEFAULT_TOTAL_TIMEOUT_MS : opts.totalTimeoutMs;
  const tmpPath = `${binaryPath}.download`;

  fs.mkdirSync(binDir, { recursive: true });

  log(`Downloading ${binaryName} (${urls.tag}) from ${redactUrl(urls.url)}`);
  const download = await downloadWithRetry(urls.url, tmpPath, {
    retryDelays,
    maxAttempts,
    requestTimeoutMs,
    totalTimeoutMs,
    env: opts.env,
    log
  });

  if (!download.ok) {
    removeQuietly(tmpPath);
    log(`Download failed after ${download.attempts} attempt(s): ${download.error.message}`);
    return {
      ok: false,
      binaryPath,
      reason: 'download_failed',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts,
      error: download.error
    };
  }

  let checksumsText;
  try {
    // The checksum file travels the same flaky link as the binary, so it gets
    // the same retry budget; otherwise a single blip throws away a good binary.
    const body = await fetchWithRetry(urls.checksumsUrl, {
      retryDelays,
      maxAttempts,
      requestTimeoutMs,
      totalTimeoutMs,
      env: opts.env,
      log
    });
    checksumsText = body.toString('utf8');
  } catch (err) {
    removeQuietly(tmpPath);
    log(`Failed to download checksums from ${redactUrl(urls.checksumsUrl)}: ${err.message}`);
    return {
      ok: false,
      binaryPath,
      reason: 'checksums_unavailable',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts,
      error: err
    };
  }

  const expected = parseChecksums(checksumsText).get(binaryName);
  if (!expected) {
    removeQuietly(tmpPath);
    log(`No checksum entry for ${binaryName} in ${redactUrl(urls.checksumsUrl)}`);
    return {
      ok: false,
      binaryPath,
      reason: 'checksum_missing',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts
    };
  }

  let actual;
  try {
    actual = await sha256File(tmpPath);
  } catch (err) {
    removeQuietly(tmpPath);
    return {
      ok: false,
      binaryPath,
      reason: 'fs_error',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts,
      error: err
    };
  }
  if (actual !== expected) {
    removeQuietly(tmpPath);
    log(`Checksum mismatch for ${binaryName}: expected ${expected}, got ${actual}`);
    return {
      ok: false,
      binaryPath,
      reason: 'checksum_mismatch',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts,
      expected,
      actual
    };
  }

  try {
    fs.renameSync(tmpPath, binaryPath);
    if (platform !== 'win32') {
      fs.chmodSync(binaryPath, 0o755);
    }
  } catch (err) {
    removeQuietly(tmpPath);
    return {
      ok: false,
      binaryPath,
      reason: 'fs_error',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts,
      error: err
    };
  }

  return { ok: true, binaryPath, reason: 'downloaded', attempts: download.attempts };
}

module.exports = {
  ensureBinary,
  buildUrls,
  parseChecksums,
  printManualInstructions,
  redactUrl,
  createProxyAgent,
  isNoProxyHost,
  PLATFORM_MAP,
  DEFAULT_DOWNLOAD_BASE
};
