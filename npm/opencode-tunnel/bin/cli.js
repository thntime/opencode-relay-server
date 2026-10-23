#!/usr/bin/env node

const { spawn, execFileSync } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');

function getBinaryName() {
  const platform = os.platform();
  const arch = os.arch();
  
  if (platform === 'darwin') {
    return arch === 'arm64' ? 'tunnel-client-darwin-arm64' : 'tunnel-client-darwin-amd64';
  } else if (platform === 'linux') {
    return arch === 'arm64' ? 'tunnel-client-linux-arm64' : 'tunnel-client-linux-amd64';
  } else if (platform === 'win32') {
    return 'tunnel-client-windows-amd64.exe';
  }
  
  console.error(`Unsupported platform: ${platform}`);
  process.exit(1);
}

function main() {
  const binaryPath = path.join(__dirname, getBinaryName());
  
  if (!fs.existsSync(binaryPath)) {
    console.error(`Binary not found: ${binaryPath}`);
    console.error('Downloading tunnel-client...');
    try {
      execFileSync(process.execPath, [path.join(__dirname, '..', 'scripts', 'postinstall.js')], {
        stdio: 'inherit'
      });
    } catch (err) {
      console.error('Download failed.');
      console.error('Try reinstalling: npm install -g @thntime/opencode-tunnel');
      process.exit(1);
    }
    if (!fs.existsSync(binaryPath)) {
      console.error(`Binary still not found: ${binaryPath}`);
      process.exit(1);
    }
  }
  
  if (os.platform() !== 'win32') {
    try { fs.chmodSync(binaryPath, 0o755); } catch (e) {}
  }
  
  const child = spawn(binaryPath, process.argv.slice(2), {
    stdio: 'inherit',
    windowsHide: false
  });
  
  child.on('error', (err) => {
    console.error(`Failed to start: ${err.message}`);
    process.exit(1);
  });
  
  child.on('exit', (code, signal) => {
    process.exit(signal ? 1 : (code || 0));
  });
}

main();
