// Provisioning driver for the local mock stack. Normally invoked by
// scripts/mock-stack.sh, which sets MOCK_STACK_DIR / MOCK_STACK_ORIGIN.
//
//   node scripts/mock/setup.mjs --configure --upstream-url http://GATEWAY:PORT/v1
//       Render pool/config.toml, merge .env (CONFIG_PATH + MOCK_TOKEN) and,
//       only when ./data holds no state, write the fake account roster.
//   node scripts/mock/setup.mjs
//       Bootstrap operator + member, mint client keys, share the mock account
//       with the member, then send success and failure traffic.
//   node scripts/mock/setup.mjs --check
//       Read-only verification of accounts, exports and recent errors.
//
// Secrets are written 0600 under the state dir and never printed.

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { parseArgs } from 'node:util';
import {
  api, cookieJar, envValue, login, mergeEnv, readSecret,
  renderConfig, stackContext, waitFor, writeJSON, writeSecret,
} from './lib.mjs';

const PROVIDER = 'mock';
const MODEL = 'mock-model';
const MODEL_ID = `${PROVIDER}/${MODEL}`;
const CONFIGURED_ACCOUNT = `configured-${PROVIDER}`;

const { values: flags } = parseArgs({
  options: {
    configure: { type: 'boolean', default: false },
    check: { type: 'boolean', default: false },
    'upstream-url': { type: 'string' },
    help: { type: 'boolean', default: false },
  },
});

if (flags.help) {
  console.log('Usage: node scripts/mock/setup.mjs [--configure --upstream-url URL] [--check]');
  process.exit(0);
}

const ctx = stackContext();

// ------------------------------------------------------------- configure

function fakeAccounts() {
  const now = Date.now();
  const future = now + 20 * 3600 * 1000;
  const iso = new Date(now).toISOString();
  const codexTokens = (id) => ({ id_token: 'fake-id-token', access_token: 'fake-access-token', refresh_token: 'fake-refresh-token', account_id: id });
  return {
    'codex/codex_healthy.json': { tokens: codexTokens('acct-codex-healthy'), last_refresh: iso },
    'codex/codex_dead.json': { tokens: codexTokens('acct-codex-dead'), last_refresh: iso, dead: true, auth_error: 'refresh token revoked' },
    'claude/claude_pro.json': {
      claudeAiOauth: { accessToken: 'fake-claude-access', refreshToken: 'fake-claude-refresh', expiresAt: future, scopes: ['user:inference'], subscriptionType: 'pro', rateLimitTier: 'default' },
      account_uuid: 'claude-uuid-pro', added_at: iso,
    },
    'claude/claude_disabled.json': { api_key: 'sk-ant-fake-disabled', plan_type: 'max', disabled: true, added_at: iso },
    'gemini/gemini_ultra.json': {
      access_token: 'fake-gemini-access', refresh_token: 'fake-gemini-refresh', token_type: 'Bearer',
      scope: 'https://www.googleapis.com/auth/generative-language', expiry_date: future, plan_type: 'ultra', last_refresh: iso,
    },
    'antigravity/antigravity_healthy.json': {
      type: 'antigravity', access_token: 'fake-ag-access', refresh_token: 'fake-ag-refresh', token_type: 'Bearer',
      email: 'healthy@mock.test', project_id: 'mock-project-healthy', plan_type: 'antigravity', expiry_date: future, last_refresh: iso,
    },
    'antigravity/antigravity_needsver.json': {
      type: 'antigravity', access_token: 'fake-ag-access-2', refresh_token: 'fake-ag-refresh-2',
      email: 'needsver@mock.test', project_id: 'mock-project-needsver', plan_type: 'antigravity', expiry_date: future,
      needs_verification: true, verification_url: 'https://mock.test/verify/needsver', last_refresh: iso,
    },
    'antigravity/antigravity_healtherr.json': {
      type: 'antigravity', access_token: 'fake-ag-access-3', refresh_token: 'fake-ag-refresh-3',
      email: 'healtherr@mock.test', project_id: 'mock-project-healtherr', plan_type: 'antigravity', expiry_date: future,
      health_error: 'quota probe returned 503', last_refresh: iso,
    },
  };
}

function configure() {
  if (!flags['upstream-url']) throw new Error('--configure requires --upstream-url');
  const configPath = path.join(ctx.root, 'pool', 'config.toml');
  const active = envValue(ctx.env, 'CONFIG_PATH');
  if (active && active !== '/app/pool/config.toml') {
    throw new Error(`CONFIG_PATH already points at ${active}; refusing to redirect an existing setup`);
  }
  fs.mkdirSync(path.dirname(configPath), { recursive: true });
  writeSecret(configPath, renderConfig({
    provider: PROVIDER, baseURL: flags['upstream-url'], tokenEnv: 'MOCK_TOKEN',
    model: MODEL, modelName: 'Mock Model',
  }));
  fs.chmodSync(configPath, 0o644);

  // Reuse an existing MOCK_TOKEN: rotating it would invalidate the running
  // container's environment until the next deploy.
  const token = envValue(ctx.env, 'MOCK_TOKEN') || crypto.randomBytes(32).toString('hex');
  const backup = ctx.file('env.backup');
  if (!fs.existsSync(backup)) writeSecret(backup, ctx.env);
  const merged = mergeEnv(ctx.env, { CONFIG_PATH: '/app/pool/config.toml', MOCK_TOKEN: token });
  if (merged !== ctx.env) writeSecret(path.join(ctx.root, '.env'), merged);
  writeSecret(ctx.file('mock-token.txt'), `${token}\n`);
  console.log('config.toml rendered; .env merged (CONFIG_PATH active, MOCK_TOKEN reused or created)');

  // Only seed a fresh instance: overwriting credentials of a pool that already
  // has data would silently replace real accounts. An empty data/ is fresh —
  // the orchestrator pre-creates it so docker does not own it as root.
  const dataDir = path.join(ctx.root, 'data');
  if (fs.existsSync(dataDir) && fs.readdirSync(dataDir).length > 0) {
    console.log('data/ holds state: keeping the existing account roster');
    return;
  }
  const files = fakeAccounts();
  for (const [rel, contents] of Object.entries(files)) {
    writeSecret(path.join(ctx.root, 'pool', rel), `${JSON.stringify(contents, null, 2)}\n`);
  }
  console.log(`seeded ${Object.keys(files).length} fake account files`);
}

// -------------------------------------------------------------- bootstrap

function passwordFor(who) {
  const file = ctx.file(`${who}-password.txt`);
  if (fs.existsSync(file)) return readSecret(file);
  const password = crypto.randomBytes(24).toString('hex');
  writeSecret(file, password);
  return password;
}

async function bootstrap() {
  const operatorPassword = passwordFor('operator');
  const memberPassword = passwordFor('member');
  const operator = cookieJar();
  const credentials = { origin: ctx.origin, operator: { email: 'operator@mock.local' }, member: { email: 'member@mock.local' } };

  const existing = ctx.credentials?.operator?.principal;
  if (existing) {
    await login(ctx.origin, operator, credentials.operator.email, operatorPassword, { ca: ctx.ca });
    credentials.operator.principal = existing;
    credentials.member.principal = ctx.credentials.member?.principal;
    console.log('reusing the provisioned principals');
  } else {
    const created = await api(ctx.origin, 'POST', '/api/setup/operator', {
      body: { username: 'operator', email: credentials.operator.email, password: operatorPassword, display_name: 'Mock Operator' },
      adminToken: ctx.adminToken, jar: operator, ca: ctx.ca,
    });
    credentials.operator.principal = created.json.principal?.id;
    await login(ctx.origin, operator, credentials.operator.email, operatorPassword, { ca: ctx.ca });
    console.log('operator bootstrapped');

    const member = await api(ctx.origin, 'POST', '/api/console/members', {
      body: { email: credentials.member.email, display_name: 'Mock Member', purpose: 'onboard' },
      jar: operator, ca: ctx.ca,
    });
    credentials.member.principal = member.json.principal?.id;
    const token = (member.json.link ?? '').split('#')[1];
    if (!token) throw new Error('member invite carried no recovery token');
    const memberJar = cookieJar();
    await api(ctx.origin, 'POST', '/api/auth/recover', { body: { token, password: memberPassword }, jar: memberJar, ca: ctx.ca });
    console.log('member created and recovered');
  }

  for (const who of ['operator', 'member']) {
    const jar = who === 'operator' ? operator : await login(ctx.origin, cookieJar(), credentials[who].email, memberPassword, { ca: ctx.ca });
    const keyFile = ctx.file(`${who}-key.txt`);
    if (ctx.credentials?.[who]?.client_id && fs.existsSync(keyFile)) {
      credentials[who].client_id = ctx.credentials[who].client_id;
      continue;
    }
    const client = await api(ctx.origin, 'POST', '/api/me/clients', { body: { label: `mock-${who}-key` }, jar, ca: ctx.ca });
    const link = await api(ctx.origin, 'POST', `/api/me/clients/${client.json.id}/setup-link`, { jar, ca: ctx.ca });
    const setupURL = link.json.setup_urls?.claude;
    if (!setupURL) throw new Error(`${who}: setup link carried no claude config url`);
    const config = await api(new URL(setupURL).origin, 'GET', setupURL, { ca: ctx.ca });
    if (!config.json?.access_token) throw new Error(`${who}: config carried no access token`);
    writeSecret(keyFile, config.json.access_token);
    credentials[who].client_id = client.json.id;
    console.log(`${who} client key minted`);
  }

  writeJSON(ctx.file('credentials.json'), credentials);
  return { credentials, operator };
}

async function shareWithMember(credentials, operator) {
  const view = await api(ctx.origin, 'GET', `/api/console/policies/${credentials.member.principal}`, { jar: operator, ca: ctx.ca });
  await api(ctx.origin, 'PUT', `/api/console/policies/${credentials.member.principal}`, {
    body: { revision: view.json.revision ?? 0, target: 'principal', policy: { providers: { allow: [PROVIDER] }, models: { allow: [MODEL_ID] } } },
    jar: operator, ca: ctx.ca,
  });
  const grants = await api(ctx.origin, 'GET', `/api/accounts/${PROVIDER}/${CONFIGURED_ACCOUNT}/grants`, { jar: operator, ca: ctx.ca });
  await api(ctx.origin, 'POST', `/api/accounts/${PROVIDER}/${CONFIGURED_ACCOUNT}/grants`, {
    body: {
      revision: grants.json.revision ?? 0,
      id: 'mock-member-grant',
      recipient_id: credentials.member.principal,
      models: [MODEL_ID],
      budget: { daily_requests: 500, monthly_requests: 5000 },
      expires_at: new Date(Date.now() + 30 * 24 * 3600 * 1000).toISOString(),
      reason: 'mock validation',
    },
    jar: operator, ca: ctx.ca, expect: [200, 409],
  });
  console.log('member policy and account grant applied');
}

async function traffic(credentials) {
  const counts = { ok: 0, err: 0 };
  const send = async (who, total, fail) => {
    const key = readSecret(ctx.file(`${who}-key.txt`));
    for (let i = 0; i < total; i++) {
      const responses = i % 3 === 0;
      const prompt = `mock ${who} request ${i}`;
      const body = responses
        ? { model: MODEL_ID, input: prompt }
        : { model: MODEL_ID, messages: [{ role: 'user', content: prompt }] };
      const headers = fail ? { 'x-mock-fail': i % 2 ? '500' : '429' } : {};
      const res = await api(ctx.origin, 'POST', responses ? '/v1/responses' : '/v1/chat/completions', {
        body, bearer: key, headers, ca: ctx.ca, expect: [200, 429, 500, 502, 503],
      });
      if (res.status === 200) counts.ok++; else counts.err++;
    }
  };
  await send('operator', 24, false);
  await send('member', 14, false);
  await send('operator', 4, true);
  await send('member', 3, true);
  console.log(`traffic sent: ok=${counts.ok} err=${counts.err}`);
  return counts;
}

async function exportRows(target, options) {
  const { json, text } = await api(ctx.origin, 'GET', target, options);
  if (json) return Array.isArray(json) ? json : (json.rows ?? json.events ?? []);
  return text.trim().split('\n').slice(1).filter(Boolean);
}

async function check({ operator, member, minRows = 1 }) {
  const admin = await api(ctx.origin, 'GET', '/admin/accounts', { jar: operator, ca: ctx.ca });
  const adminRoster = Array.isArray(admin.json) ? admin.json : (admin.json?.accounts ?? []);
  const configured = adminRoster.find((account) => account.id === CONFIGURED_ACCOUNT);
  if (!configured) throw new Error(`${CONFIGURED_ACCOUNT} missing from the admin roster`);
  const stats = await api(ctx.origin, 'GET', '/api/pool/stats', { jar: operator, ca: ctx.ca });
  const roster = stats.json?.accounts ?? [];
  // The operator roster masks ids; the admin roster carries the public_id map.
  const mock = roster.find((account) => account.id === configured.public_id);
  if (!mock) throw new Error(`${CONFIGURED_ACCOUNT} missing from the pool roster`);
  console.log(`mock account: type=${mock.type} status=${mock.status}`);
  if (mock.status !== 'healthy') throw new Error(`mock account is ${mock.status}, expected healthy`);

  const consoleRows = await waitFor(
    async () => {
      const rows = await exportRows('/api/console/usage-export?format=json&hours=24&limit=500', { jar: operator, ca: ctx.ca });
      return rows.length >= minRows ? rows : null;
    },
    { label: 'console usage export to drain', timeout: 60000, interval: 1000 },
  );
  const csv = await exportRows('/api/console/usage-export?format=csv&hours=24&limit=500', { jar: operator, ca: ctx.ca });
  const memberRows = await exportRows('/api/me/usage-export?format=json&hours=24&limit=500', { jar: member, ca: ctx.ca });
  const errors = await api(ctx.origin, 'GET', '/api/pool/recent-errors', { jar: operator, ca: ctx.ca });
  const errorCount = (errors.json?.errors ?? []).length;
  console.log(`exports: console json=${consoleRows.length} csv=${csv.length} member=${memberRows.length}; recent errors=${errorCount}`);
  if (!csv.length) throw new Error('console CSV export has no data rows');
  if (!memberRows.length) throw new Error('member export has no rows');
  if (!errorCount) throw new Error('recent errors panel is empty');
  return { accounts: roster.length, consoleRows: consoleRows.length, memberRows: memberRows.length, errorCount };
}

if (flags.configure) {
  configure();
} else {
  const { credentials, operator } = await bootstrap();
  const member = await login(ctx.origin, cookieJar(), credentials.member.email, readSecret(ctx.file('member-password.txt')), { ca: ctx.ca });
  if (flags.check) {
    await check({ operator, member });
    console.log('CHECK ok');
  } else {
    await shareWithMember(credentials, operator);
    const counts = await traffic(credentials);
    await check({ operator, member, minRows: counts.ok });
    console.log('PROVISION ok');
  }
}
