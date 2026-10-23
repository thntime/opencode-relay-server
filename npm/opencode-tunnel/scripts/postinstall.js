const https = require('https');
const fs = require('fs');
const path = require('path');
const os = require('os');
const { execSync } = require('child_process');

const REPO = 'zhangjinglei137/opencode-relay-server';
const VERSION = require('../package.json').version;

const PLATFORM_MAP = {
  'darwin-arm64': 'tunnel-client-darwin-arm64',
  'darwin-x64': 'tunnel-client-darwin-amd64',
  'linux-arm64': 'tunnel-client-linux-arm64',
  'linux-x64': 'tunnel-client-linux-amd64',
  'win32-x64': 'tunnel-client-windows-amd64.exe'
};

function getPlatformKey() {
  return `${os.platform()}-${os.arch()}`;
}

function getBinaryName() {
  const key = getPlatformKey();
  const name = PLATFORM_MAP[key];
  if (!name) {
    console.error(`Unsupported platform: ${key}`);
    console.error(`Supported: ${Object.keys(PLATFORM_MAP).join(', ')}`);
    process.exit(1);
  }
  return name;
}

function download(url, dest) {
  return new Promise((resolve, reject) => {
    const file = fs.createWriteStream(dest);
    
    const request = (url) => {
      https.get(url, (response) => {
        if (response.statusCode === 302 || response.statusCode === 301) {
          request(response.headers.location);
          return;
        }
        
        if (response.statusCode !== 200) {
          reject(new Error(`HTTP ${response.statusCode}: ${url}`));
          return;
        }
        
        response.pipe(file);
        file.on('finish', () => {
          file.close();
          resolve();
        });
      }).on('error', (err) => {
        fs.unlink(dest, () => {});
        reject(err);
      });
    };
    
    request(url);
  });
}

async function main() {
  const binaryName = getBinaryName();
  const binDir = path.join(__dirname, '..', 'bin');
  const binaryPath = path.join(binDir, binaryName);
  
  if (fs.existsSync(binaryPath)) {
    console.log(`Binary already exists: ${binaryName}`);
    return;
  }
  
  const tag = VERSION.startsWith('v') ? VERSION : `v${VERSION}`;
  const url = `https://github.com/${REPO}/releases/download/${tag}/${binaryName}`;
  
  console.log(`Downloading ${binaryName} (${tag})...`);
  
  try {
    await download(url, binaryPath);
    
    if (os.platform() !== 'win32') {
      fs.chmodSync(binaryPath, 0o755);
    }
    
    console.log(`Installed: ${binaryPath}`);
  } catch (err) {
    console.error(`Failed to download: ${err.message}`);
    console.error(`URL: ${url}`);
    console.error('\nYou can manually download from:');
    console.error(`https://github.com/${REPO}/releases/tag/${tag}`);
    process.exit(1);
  }
}

main();
