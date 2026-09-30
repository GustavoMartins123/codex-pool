import { spawn } from 'node:child_process';
import { mkdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import assert from 'node:assert/strict';

const directory = fileURLToPath(new URL('.', import.meta.url));
assert.ok(process.env.CHROME_PATH, 'CHROME_PATH is required');
const origin = new URL('http://127.0.0.1:5179');
await mkdir(new URL('test-results/', import.meta.url), { recursive: true });
const server = spawn(process.execPath, ['node_modules/vite/bin/vite.js', 'preview', '--host', origin.hostname, '--port', origin.port, '--strictPort'], { cwd: directory, windowsHide: true, stdio: 'ignore' });
try {
  let ready = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    if (server.exitCode !== null) throw new Error('Preview server exited before becoming ready');
    try { const response = await fetch(origin); if (response.ok) { ready = true; break; } } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  assert.ok(ready, 'Preview server did not become ready');
  const runner = spawn(process.execPath, ['account-flows.browser.mjs'], { cwd: directory, windowsHide: true, stdio: 'inherit', env: { ...process.env, UI_TEST_ORIGIN: origin.origin } });
  process.exitCode = await new Promise((resolve, reject) => { runner.once('error', reject); runner.once('exit', code => resolve(code === null ? 1 : code)); });
} finally { server.kill(); }
