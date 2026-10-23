'use strict';

const { ensureBinary, printManualInstructions } = require('./download.js');

async function main() {
  const result = await ensureBinary();

  if (result.ok) {
    if (result.reason === 'already_present') {
      console.log(`Binary already exists: ${result.binaryPath}`);
    } else {
      console.log(`Installed: ${result.binaryPath}`);
    }
    return;
  }

  console.error(`Failed to install tunnel-client (${result.reason}).`);
  if (result.error && result.error.message) {
    console.error(`Reason: ${result.error.message}`);
  }
  if (result.url) {
    console.error(`URL: ${result.url}`);
  }
  printManualInstructions(result);
  console.error('');
  console.error('Try reinstalling: npm install -g @thntime/opencode-tunnel');
  process.exit(1);
}

main().catch((err) => {
  console.error(`Failed to install tunnel-client: ${err && err.message ? err.message : err}`);
  process.exit(1);
});
