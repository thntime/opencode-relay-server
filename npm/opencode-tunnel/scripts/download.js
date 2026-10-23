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
  const host = parsed.hostname.toLowerCase();
  return String(raw)
    .split(/[,\s]+/)
    .filter(Boolean)
    .some((entry) => {
      const value = entry.trim().toLowerCase();
      if (!value) return false;
      if (value === '*') return true;
      const withoutPort = value.includes(':') && !value.startsWith('[') ? value.split(':')[0] : value;
      const bare = withoutPort.startsWith('.') ? withoutPort.slice(1) : withoutPort;
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

  try {
    // Loaded lazily so the module still works if the dependency is missing.
    const { HttpsProxyAgent } = require('https-proxy-agent');
    return new HttpsProxyAgent(raw);
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
const DEFAULT_TOTAL_TIMEOUT_MS = 120000;
const MAX_REDIRECTS = 5;

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
              Object.assign(new Error(`Download timed out after ${totalTimeoutMs}ms: ${url}`), {
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
            fail(new Error(`Too many redirects while downloading ${url}`));
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
            Object.assign(new Error(`HTTP ${status}: ${targetUrl}`), {
              statusCode: status
            })
          );
          return;
        }

        if (!dest) {
          const chunks = [];
          res.on('data', (chunk) => chunks.push(chunk));
          res.on('end', () => succeed(Buffer.concat(chunks)));
          res.on('error', fail);
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
          Object.assign(new Error(`Request timed out after ${requestTimeoutMs}ms: ${targetUrl}`), {
            code: 'ETIMEDOUT'
          })
        );
      });
      req.on('error', fail);
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

async function downloadWithRetry(url, dest, options) {
  const retryDelays = options.retryDelays || DEFAULT_RETRY_DELAYS;
  const maxAttempts =
    options.maxAttempts === undefined ? DEFAULT_MAX_ATTEMPTS : options.maxAttempts;
  const log = options.log;
  let lastError = null;
  let attempts = 0;

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    attempts = attempt + 1;
    try {
      await fetchUrl(url, {
        dest,
        env: options.env,
        requestTimeoutMs: options.requestTimeoutMs,
        totalTimeoutMs: options.totalTimeoutMs
      });
      return { ok: true, attempts };
    } catch (err) {
      lastError = err;
      removeQuietly(dest);

      if (!isRetryable(err) || attempt === maxAttempts - 1) {
        break;
      }

      const delay = retryDelays[Math.min(attempt, retryDelays.length - 1)];
      log(
        `Download attempt ${attempts}/${maxAttempts} failed (${err.message}); retrying in ${delay}ms`
      );
      await sleep(delay);
    }
  }

  return { ok: false, attempts, error: lastError };
}

async function fetchWithRetry(url, options) {
  const retryDelays = options.retryDelays || DEFAULT_RETRY_DELAYS;
  const maxAttempts =
    options.maxAttempts === undefined ? DEFAULT_MAX_ATTEMPTS : options.maxAttempts;
  const log = options.log || (() => {});
  let lastError = null;

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    try {
      return await fetchUrl(url, {
        env: options.env,
        requestTimeoutMs: options.requestTimeoutMs,
        totalTimeoutMs: options.totalTimeoutMs
      });
    } catch (err) {
      lastError = err;
      if (!isRetryable(err) || attempt === maxAttempts - 1) break;
      const delay = retryDelays[Math.min(attempt, retryDelays.length - 1)];
      log(`Fetch ${url} failed (${err.message}); retrying in ${delay}ms`);
      await sleep(delay);
    }
  }

  throw lastError;
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
    if (existingSize > 0) {
      return { ok: true, binaryPath, reason: 'already_present' };
    }
    removeQuietly(binaryPath);
    log(`Removing empty leftover binary: ${binaryPath}`);
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

  log(`Downloading ${binaryName} (${urls.tag}) from ${urls.url}`);
  const download = await downloadWithRetry(urls.url, tmpPath, {
    retryDelays,
    maxAttempts,
    requestTimeoutMs,
    totalTimeoutMs,
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
    log(`Failed to download checksums from ${urls.checksumsUrl}: ${err.message}`);
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
    log(`No checksum entry for ${binaryName} in ${urls.checksumsUrl}`);
    return {
      ok: false,
      binaryPath,
      reason: 'checksum_missing',
      url: urls.url,
      checksumsUrl: urls.checksumsUrl,
      attempts: download.attempts
    };
  }

  const actual = await sha256File(tmpPath);
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

  fs.renameSync(tmpPath, binaryPath);

  if (platform !== 'win32') {
    fs.chmodSync(binaryPath, 0o755);
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
