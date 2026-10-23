#!/usr/bin/env node
'use strict';

const { spawn } = require('child_process');
const path = require('path');
const fs = require('fs');
const { ensureBinary, printManualInstructions, redactUrl, PLATFORM_MAP } = require('../scripts/download.js');

async function main() {
  const platformKey = `${process.platform}-${process.arch}`;
  const binaryName = PLATFORM_MAP[platformKey];

  if (!binaryName) {
    console.error(`Unsupported platform: ${platformKey}`);
    console.error(`Supported: ${Object.keys(PLATFORM_MAP).join(', ')}`);
    process.exit(1);
  }

  const binaryPath = path.join(__dirname, binaryName);

  if (!fs.existsSync(binaryPath)) {
    console.error(`Binary not found: ${binaryPath}`);
    console.error(
      'The postinstall download did not run (npm blocks install scripts by default); downloading now on first run...'
    );

    let result;
    try {
      result = await ensureBinary();
    } catch (err) {
      console.error(`Download failed: ${err && err.message ? err.message : err}`);
      console.error('Try reinstalling: npm install -g @thntime/opencode-tunnel');
      process.exit(1);
    }

    if (!result.ok) {
      console.error(`Download failed (${result.reason}).`);
      if (result.error && result.error.message) {
        console.error(`Reason: ${result.error.message}`);
      }
      if (result.url) {
        console.error(`URL: ${redactUrl(result.url)}`);
      }
      printManualInstructions(result);
      console.error('');
      console.error('Try reinstalling: npm install -g @thntime/opencode-tunnel');
      process.exit(1);
    }

    if (!fs.existsSync(binaryPath)) {
      console.error(`Binary still not found: ${binaryPath}`);
      console.error('Try reinstalling: npm install -g @thntime/opencode-tunnel');
      process.exit(1);
    }
  }

  if (process.platform !== 'win32') {
    try {
      fs.chmodSync(binaryPath, 0o755);
    } catch (err) {
      // ignore: the file may already be executable
    }
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
    process.exit(signal ? 1 : code || 0);
  });
}

main().catch((err) => {
  console.error(`Failed to start: ${err && err.message ? err.message : err}`);
  process.exit(1);
});
