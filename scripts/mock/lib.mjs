// Shared helpers for the local mock stack drivers (setup.mjs, turn.mjs).
// Everything here is plain Node: no dependencies, no global TLS bypass — the
// bridge's self-signed certificate is passed as an explicit CA instead.

import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// ---------------------------------------------------------------- state dir

export function readJSON(file, fallback = null) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return fallback; }
}

export function writeSecret(file, contents) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const tmp = `${file}.tmp`;
  fs.writeFileSync(tmp, contents, { mode: 0o600 });
  fs.renameSync(tmp, file);
}

export function writeJSON(file, value, { secret = true } = {}) {
  writeSecret(file, `${JSON.stringify(value, null, 2)}\n`);
  if (!secret) fs.chmodSync(file, 0o644);
}

export function readSecret(file) {
  return fs.readFileSync(file, 'utf8').trim();
}

// stackContext resolves the repo root from this file, so the drivers work
// from any working directory and never hardcode a checkout path.
export function stackContext() {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  const dir = process.env.MOCK_STACK_DIR ?? path.join(root, 'tmp', 'mock-stack');
  const origin = process.env.MOCK_STACK_ORIGIN ?? readJSON(path.join(dir, 'stack.json'), {})?.origin ?? 'http://127.0.0.1:8989';
  const certFile = path.join(dir, 'tls', 'cert.pem');
  const ca = origin.startsWith('https:') && fs.existsSync(certFile) ? fs.readFileSync(certFile) : undefined;
  const env = fs.readFileSync(path.join(root, '.env'), 'utf8');
  return {
    root, dir, origin, ca, env,
    adminToken: envValue(env, 'ADMIN_TOKEN'),
    credentials: readJSON(path.join(dir, 'credentials.json'), {}),
    file: (...parts) => path.join(dir, ...parts),
  };
}

// ------------------------------------------------------------------- timing

export const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

export async function waitFor(probe, { label = 'condition', timeout = 45000, interval = 350 } = {}) {
  const deadline = Date.now() + timeout;
  let last;
  for (;;) {
    last = await probe();
    if (last) return last;
    if (Date.now() >= deadline) throw new Error(`timed out after ${timeout}ms waiting for ${label}`);
    await sleep(interval);
  }
}

// -------------------------------------------------------------- HTTP client

export function cookieJar() {
  const cookies = new Map();
  return {
    absorb(res) {
      for (const line of res.headers['set-cookie'] ?? []) {
        const [pair] = line.split(';');
        const at = pair.indexOf('=');
        if (at > 0) cookies.set(pair.slice(0, at).trim(), pair.slice(at + 1).trim());
      }
    },
    header() {
      return [...cookies].map(([name, value]) => `${name}=${value}`).join('; ');
    },
    csrf() {
      return cookies.get('pool_csrf') ?? '';
    },
  };
}

export function request(origin, method, target, { body, headers = {}, ca } = {}) {
  const url = new URL(target, origin);
  const transport = url.protocol === 'https:' ? https : http;
  const payload = body === undefined ? null : Buffer.from(JSON.stringify(body));
  const options = {
    method,
    headers: { ...headers },
  };
  if (payload) {
    options.headers['content-type'] = 'application/json';
    options.headers['content-length'] = payload.length;
  }
  if (url.protocol === 'https:' && ca) options.ca = ca;
  return new Promise((resolve, reject) => {
    const req = transport.request(url, options, (res) => {
      const chunks = [];
      res.on('data', (chunk) => chunks.push(chunk));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, text: Buffer.concat(chunks).toString('utf8') }));
    });
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

// api() carries the console session (cookies + CSRF) or a bearer token, and
// fails loudly on any status the caller did not ask for.
export async function api(origin, method, target, { body, jar, bearer, adminToken, headers = {}, ca, expect = [200, 201, 204] } = {}) {
  const sent = { ...headers };
  if (jar) {
    const cookie = jar.header();
    if (cookie) sent.cookie = cookie;
    if (method !== 'GET' && method !== 'HEAD') sent['x-csrf-token'] = jar.csrf();
  }
  if (bearer) sent.authorization = `Bearer ${bearer}`;
  if (adminToken) sent['x-admin-token'] = adminToken;
  const res = await request(origin, method, target, { body, headers: sent, ca });
  if (jar) jar.absorb(res);
  const json = res.text && (res.headers['content-type'] ?? '').includes('json') ? JSON.parse(res.text) : null;
  if (!expect.includes(res.status)) {
    throw new Error(`${method} ${target} -> ${res.status}: ${res.text.slice(0, 400)}`);
  }
  return { status: res.status, json, text: res.text };
}

export async function login(origin, jar, email, password, { ca } = {}) {
  await api(origin, 'POST', '/api/auth/login', { body: { email, password }, jar, ca });
  return jar;
}

// App tokens authenticate the runner-facing endpoints (catalog, pairing).
export async function appToken(origin, email, password, { ca } = {}) {
  const { json } = await api(origin, 'POST', '/api/auth/token', { body: { email, password }, ca });
  return json.token;
}

// --------------------------------------------------------------------- SSE

// openEvents streams /api/... SSE frames. Returns { close, events } where
// events is an async iterator of { type, data, id }.
export function openEvents(origin, target, { bearer, jar, ca } = {}) {
  const url = new URL(target, origin);
  const transport = url.protocol === 'https:' ? https : http;
  const headers = { accept: 'text/event-stream' };
  if (bearer) headers.authorization = `Bearer ${bearer}`;
  if (jar?.header()) headers.cookie = jar.header();
  const options = { headers };
  if (url.protocol === 'https:' && ca) options.ca = ca;

  const queue = [];
  let notify = null;
  let done = false;
  let failure = null;
  const push = (event) => { queue.push(event); if (notify) { notify(); notify = null; } };

  const req = transport.get(url, options, (res) => {
    if (res.statusCode !== 200) {
      failure = new Error(`SSE ${target} -> ${res.statusCode}`);
      done = true;
      if (notify) { notify(); notify = null; }
      res.resume();
      return;
    }
    let buffer = '';
    res.setEncoding('utf8');
    res.on('data', (chunk) => {
      buffer += chunk;
      let at;
      while ((at = buffer.indexOf('\n\n')) >= 0) {
        const frame = buffer.slice(0, at);
        buffer = buffer.slice(at + 2);
        const event = { type: 'message', data: '', id: '' };
        for (const line of frame.split('\n')) {
          if (line.startsWith('event:')) event.type = line.slice(6).trim();
          else if (line.startsWith('data:')) event.data += line.slice(5).trim();
          else if (line.startsWith('id:')) event.id = line.slice(3).trim();
        }
        if (event.data) {
          try { event.json = JSON.parse(event.data); } catch { event.json = null; }
          push(event);
        }
      }
    });
    res.on('end', () => { done = true; if (notify) { notify(); notify = null; } });
    res.on('error', (error) => { failure = error; done = true; if (notify) { notify(); notify = null; } });
  });
  req.on('error', (error) => { failure = error; done = true; if (notify) { notify(); notify = null; } });

  return {
    close() { req.destroy(); done = true; if (notify) { notify(); notify = null; } },
    async next(timeout = 45000) {
      const deadline = Date.now() + timeout;
      while (!queue.length) {
        if (failure) throw failure;
        if (done) return null;
        const left = deadline - Date.now();
        if (left <= 0) return null;
        let timer;
        await Promise.race([
          new Promise((resolve) => { notify = resolve; }),
          new Promise((resolve) => { timer = setTimeout(resolve, left); }),
        ]);
        clearTimeout(timer);
        notify = null;
      }
      return queue.shift();
    },
    async until(type, timeout = 45000) {
      const deadline = Date.now() + timeout;
      for (;;) {
        const event = await this.next(Math.max(1, deadline - Date.now()));
        if (!event) throw new Error(`timed out waiting for SSE event ${type}`);
        if (event.type === type) return event;
      }
    },
  };
}

// ----------------------------------------------------------- command signs

const TURN_DOMAIN = 'codexpool-turn/1';
const APPROVAL_DOMAIN = 'codexpool-approval/1';
const KEY_ID_DOMAIN = 'codexpool-command-key-id';

const mac = (key, input) => crypto.createHmac('sha256', key).update(input).digest();
const sha256hex = (value) => crypto.createHash('sha256').update(value, 'utf8').digest('hex');

// Length-prefixed fields ("<bytes>:<field>;") mirror runnerproto.signingInput,
// so no field value can shift the boundary of another.
export function signingInput(...fields) {
  return fields.map((field) => `${Buffer.byteLength(field, 'utf8')}:${field};`).join('');
}

export function commandKeyID(key) {
  return mac(key, KEY_ID_DOMAIN).subarray(0, 16).toString('base64url');
}

export function signTurn(key, { sessionID, issuedAt, agent, permissionMode, workspace, input }) {
  const input_ = signingInput(TURN_DOMAIN, sessionID, String(issuedAt), agent, permissionMode, workspace.mode, workspace.target ?? '', input);
  return { issued_at: issuedAt, signatures: { [commandKeyID(key)]: mac(key, input_).toString('base64url') } };
}

export function signApproval(key, { sessionID, approvalID, decision, issuedAt, command }) {
  const input = signingInput(APPROVAL_DOMAIN, sessionID, approvalID, decision, String(issuedAt), sha256hex(command));
  return { issued_at: issuedAt, signatures: { [commandKeyID(key)]: mac(key, input).toString('base64url') } };
}

// -------------------------------------------------------------- .env merge

export function envValue(text, key) {
  const match = text.match(new RegExp(`^${key}=(.*)$`, 'm'));
  return match ? match[1].trim() : null;
}

// mergeEnv rewrites only the requested keys, keeping every other line (and
// every comment) byte for byte. A commented-out key is activated in place.
export function mergeEnv(text, updates) {
  const lines = text.split('\n');
  const pending = new Map(Object.entries(updates));
  const out = lines.map((line) => {
    const active = line.match(/^([A-Za-z_][A-Za-z0-9_]*)=/);
    const commented = line.match(/^#\s*([A-Za-z_][A-Za-z0-9_]*)=/);
    const key = active?.[1] ?? commented?.[1];
    if (!key || !pending.has(key)) return line;
    const value = pending.get(key);
    pending.delete(key);
    return `${key}=${value}`;
  });
  if (pending.size && out[out.length - 1] === '') out.pop();
  for (const [key, value] of pending) out.push(`${key}=${value}`);
  let merged = out.join('\n');
  if (!merged.endsWith('\n')) merged += '\n';
  return merged;
}

// ------------------------------------------------------------- config.toml

export function renderConfig({ provider, baseURL, tokenEnv, model, modelName, healthPath = '/models', intervalSeconds = 30 }) {
  return `[providers.${provider}]
protocol = "openai"
base_url = "${baseURL}"

[providers.${provider}.auth]
type = "bearer"
token_env = "${tokenEnv}"

[providers.${provider}.health]
path = "${healthPath}"
interval_seconds = ${intervalSeconds}

[[providers.${provider}.models]]
id = "${model}"
name = "${modelName}"
context_window = 128000
max_output_tokens = 32768
modalities = ["text"]
`;
}
