import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { after, before, test } from 'node:test';
import {
  api, commandKeyID, cookieJar, envValue, mergeEnv, openEvents, readJSON,
  renderConfig, signApproval, signingInput, signTurn, waitFor, writeJSON, writeSecret,
} from './lib.mjs';

// Vectors shared with internal/runnerproto/signing_test.go: both sides of the
// protocol must keep producing the same bytes.
const key = Buffer.from(Array.from({ length: 32 }, (_, i) => i));
const session = '5d1d9c3e-9f1a-4f0e-8a51-2f4b7c1d0e11';
const issuedAt = 1761379200000;

test('command key id matches the Go vector', () => {
  assert.equal(commandKeyID(key), 'CYxfwqA9-ufHeg_CwDnJkw');
});

test('turn signing input and signature match the Go vector', () => {
  const workspace = { mode: 'repo', target: 'https://example.com/r.git' };
  const input = 'olá; 1:2\nrun tests';
  assert.equal(
    signingInput('codexpool-turn/1', session, String(issuedAt), 'codex', 'gated', workspace.mode, workspace.target, input),
    `16:codexpool-turn/1;36:${session};13:1761379200000;5:codex;5:gated;4:repo;25:https://example.com/r.git;19:olá; 1:2\nrun tests;`,
  );
  const signed = signTurn(key, { sessionID: session, issuedAt, agent: 'codex', permissionMode: 'gated', workspace, input });
  assert.equal(signed.issued_at, issuedAt);
  assert.deepEqual(signed.signatures, { 'CYxfwqA9-ufHeg_CwDnJkw': 'ahaf6ty62ZJHooWFfwoZqQDLZJfcuxdYTJezViYcY18' });
});

test('approval signature matches the Go vector', () => {
  const signed = signApproval(key, { sessionID: session, approvalID: 'call_7', decision: 'approve', issuedAt, command: 'go test ./...' });
  assert.deepEqual(signed.signatures, { 'CYxfwqA9-ufHeg_CwDnJkw': '9juZdutr3gbV9kd2RiYJDNxNbAYzZECcxn1OWyeXfx8' });
});

test('ephemeral workspaces sign an empty target', () => {
  const signed = signTurn(key, { sessionID: session, issuedAt, agent: 'codex', permissionMode: 'gated', workspace: { mode: 'ephemeral' }, input: 'hi' });
  assert.equal(Object.keys(signed.signatures)[0], commandKeyID(key));
  assert.notEqual(signed.signatures[commandKeyID(key)], signTurn(key, { sessionID: session, issuedAt, agent: 'codex', permissionMode: 'auto', workspace: { mode: 'ephemeral' }, input: 'hi' }).signatures[commandKeyID(key)]);
});

test('mergeEnv keeps unrelated lines and activates commented keys', () => {
  const before = '# pool env\nADMIN_TOKEN=keep\n# CONFIG_PATH=/old/config.toml\nPOOL_DIR=/app/pool\n';
  const merged = mergeEnv(before, { CONFIG_PATH: '/app/pool/config.toml', MOCK_TOKEN: 'fresh' });
  assert.equal(merged, '# pool env\nADMIN_TOKEN=keep\nCONFIG_PATH=/app/pool/config.toml\nPOOL_DIR=/app/pool\nMOCK_TOKEN=fresh\n');
  assert.equal(envValue(merged, 'ADMIN_TOKEN'), 'keep');
  assert.equal(envValue(merged, 'CONFIG_PATH'), '/app/pool/config.toml');
});

test('mergeEnv replaces an active value and appends a missing key', () => {
  const merged = mergeEnv('MOCK_TOKEN=old\nOTHER=1', { MOCK_TOKEN: 'new', EXTRA: '2' });
  assert.equal(merged, 'MOCK_TOKEN=new\nOTHER=1\nEXTRA=2\n');
  assert.equal(envValue(merged, 'MISSING'), null);
});

test('renderConfig emits a generic openai provider', () => {
  const toml = renderConfig({ provider: 'mock', baseURL: 'http://gateway:8991/v1', tokenEnv: 'MOCK_TOKEN', model: 'mock-model', modelName: 'Mock Model' });
  assert.ok(toml.includes('[providers.mock]'));
  assert.ok(toml.includes('base_url = "http://gateway:8991/v1"'));
  assert.ok(toml.includes('token_env = "MOCK_TOKEN"'));
  assert.ok(toml.includes('[[providers.mock.models]]'));
  assert.ok(toml.includes('id = "mock-model"'));
});

test('state helpers write secrets 0600 and read back', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'mock-lib-'));
  try {
    const file = path.join(dir, 'nested', 'state.json');
    writeJSON(file, { a: 1 });
    assert.equal(fs.statSync(file).mode & 0o777, 0o600);
    assert.deepEqual(readJSON(file), { a: 1 });
    assert.deepEqual(readJSON(path.join(dir, 'missing.json'), { fallback: true }), { fallback: true });
    writeSecret(path.join(dir, 'token.txt'), 'secret\n');
    assert.equal(fs.readFileSync(path.join(dir, 'token.txt'), 'utf8'), 'secret\n');
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('waitFor polls until the probe succeeds and times out otherwise', async () => {
  let calls = 0;
  assert.equal(await waitFor(() => ++calls >= 3, { interval: 1 }), true);
  await assert.rejects(() => waitFor(() => false, { label: 'never', timeout: 20, interval: 5 }), /timed out .*never/);
});

let server;
let origin;

before(async () => {
  server = http.createServer((req, res) => {
    if (req.url === '/login') {
      res.writeHead(200, { 'content-type': 'application/json', 'set-cookie': ['pool_session=abc; Path=/', 'pool_csrf=tok123; Path=/'] });
      res.end('{"ok":true}');
      return;
    }
    if (req.url === '/guarded') {
      const ok = req.headers.cookie === 'pool_session=abc; pool_csrf=tok123' && req.headers['x-csrf-token'] === 'tok123';
      res.writeHead(ok ? 200 : 403, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ ok }));
      return;
    }
    if (req.url === '/events') {
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      res.write('event: turn.finished\ndata: {"turn_id":"t1"}\nid: 7\n\n');
      res.write('event: approval.requested\ndata: {"id":"a1"}\n\n');
      return;
    }
    res.writeHead(404).end('{}');
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  origin = `http://127.0.0.1:${server.address().port}`;
});

after(() => new Promise((resolve) => server.close(resolve)));

test('cookie jar absorbs cookies and api sends the CSRF header', async () => {
  const jar = cookieJar();
  await api(origin, 'POST', '/login', { body: {}, jar });
  assert.equal(jar.csrf(), 'tok123');
  const { json } = await api(origin, 'POST', '/guarded', { body: {}, jar });
  assert.equal(json.ok, true);
  await assert.rejects(() => api(origin, 'POST', '/guarded', { body: {} }), /403/);
});

test('SSE reader parses typed frames in order', async () => {
  const stream = openEvents(origin, '/events');
  try {
    const first = await stream.until('turn.finished', 5000);
    assert.deepEqual(first.json, { turn_id: 't1' });
    assert.equal(first.id, '7');
    const second = await stream.until('approval.requested', 5000);
    assert.deepEqual(second.json, { id: 'a1' });
  } finally {
    stream.close();
  }
});
