// Live browser smokes for the local mock stack. Opt-in: they drive a real
// Chrome against a running stack, so they are not part of the unit suites.
//
//   CHROME_PATH=/usr/bin/google-chrome node web/mock-stack.browser.mjs [suites] [flags]
//   bash scripts/mock-stack.sh smoke                 # items conversations runners approve
//   bash scripts/mock-stack.sh smoke runners --revoke mock-member-runner
//
// Suites:
//   items          Recent provider errors, account verify controls, the generic
//                  provider in Analytics, and usage exports (operator + member).
//   conversations  Transcript, archive/expiry copy, then a UI action on the
//                  seeded pending approval (cancel by default).
//   runners        Runner roster and pairing cards; --revoke NAME revokes one.
//   approve        Approves a pending pairing in the web UI. The machine code
//                  comes from --code or from the orchestrator's pair log, so
//                  run it while `mock-stack.sh up|pair` waits for approval.
//
// Flags:
//   --who operator|member      principal for the runners/approve suites
//   --only errors|verify|analytics|export   limit the items suite
//   --action cancel|deny|close|delete|none  conversations action
//   --conversation ID|LABEL    deep-link target (default: the pending seed)
//   --account ID               account to verify individually
//   --revoke NAME              runner to revoke in the runners suite
//   --code CODE                pairing machine code for the approve suite
//   --runner NAME              pairing card to approve (default: first card)
//
// BROWSER_NO_SANDBOX=1 adds --no-sandbox for containers without user namespaces.
// Screenshots and downloads land in the state dir (tmp/mock-stack), never in git.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { api, readJSON, readSecret, stackContext } from '../scripts/mock/lib.mjs';

const ctx = stackContext();
const known = ['items', 'conversations', 'runners', 'approve'];
const flags = { who: 'operator', only: 'all', action: '', conversation: '', account: 'codex_healthy', revoke: '', code: '', runner: '' };
const suites = [];
const args = process.argv.slice(2);
for (let i = 0; i < args.length; i++) {
  const arg = args[i];
  if (arg === '--help' || arg === '-h') {
    const lines = fs.readFileSync(new URL(import.meta.url), 'utf8').split('\n');
    const end = lines.findIndex((line) => !line.startsWith('//'));
    console.log(lines.slice(0, end).map((line) => line.replace(/^\/\/ ?/, '')).join('\n'));
    process.exit(0);
  }
  if (arg.startsWith('--')) {
    const key = arg.slice(2);
    if (!(key in flags)) throw new Error(`unknown flag ${arg}; see --help`);
    flags[key] = args[++i] ?? '';
    if (!flags[key]) throw new Error(`${arg} needs a value`);
    continue;
  }
  if (!known.includes(arg)) throw new Error(`unknown suite ${arg}; expected ${known.join(', ')}`);
  suites.push(arg);
}
if (suites.length === 0) suites.push(...known);
if (!['operator', 'member'].includes(flags.who)) throw new Error('--who must be operator or member');
if (!['all', 'errors', 'verify', 'analytics', 'export'].includes(flags.only)) throw new Error('--only must be all, errors, verify, analytics or export');
if (flags.action && !['cancel', 'deny', 'close', 'delete', 'none'].includes(flags.action)) throw new Error('--action must be cancel, deny, close, delete or none');
assert.ok(process.env.CHROME_PATH, 'CHROME_PATH must point at a Chrome/Chromium binary');

// Chrome can lose a race with its own singleton lock on a busy host; retry the
// launch only, never the assertions.
async function launchBrowser(options) {
  for (let attempt = 1; ; attempt++) {
    try { return await puppeteer.launch(options); } catch (error) {
      if (attempt >= 3) throw error;
      console.error(`Chrome launch attempt ${attempt} failed: ${error.message}; retrying`);
    }
  }
}
const browser = await launchBrowser({
  executablePath: process.env.CHROME_PATH,
  headless: true,
  timeout: 120000,
  args: ['--disable-dev-shm-usage', ...(process.env.BROWSER_NO_SANDBOX === '1' ? ['--no-sandbox'] : [])],
});

const setInput = (page, selector, value) => page.evaluate((selector, value) => {
  const input = document.querySelector(selector);
  if (!input) throw new Error(`missing input ${selector}`);
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, value);
  input.dispatchEvent(new Event('input', { bubbles: true }));
}, selector, value);
const clickText = (page, selector, text) => page.evaluate((selector, text) => {
  const node = [...document.querySelectorAll(selector)].find((el) => el.textContent.trim() === text);
  if (!node) throw new Error(`missing button ${selector} "${text}"`);
  node.click();
}, selector, text);
const hasText = (page, selector, text, timeout = 30000) => page.waitForFunction(
  (selector, text) => [...document.querySelectorAll(selector)].some((el) => el.textContent.trim() === text),
  { timeout }, selector, text,
);
const shot = (page, name) => page.screenshot({ path: ctx.file(`smoke-${name}.png`), fullPage: true });
const only = (part) => flags.only === 'all' || flags.only === part;

async function login(page, who) {
  const email = ctx.credentials?.[who]?.email;
  assert.ok(email, `no ${who} credentials in ${ctx.file('credentials.json')}; run mock-stack.sh up first`);
  await page.goto(`${ctx.origin}/`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('input[autocomplete="username"]', { timeout: 30000 });
  await setInput(page, 'input[autocomplete="username"]', email);
  await setInput(page, 'input[type="password"][autocomplete="current-password"]', readSecret(ctx.file(`${who}-password.txt`)));
  await page.click('button.threshold-submit');
  await page.waitForFunction(() => !document.querySelector('.threshold-form'), { timeout: 30000 });
  console.log(`LOGIN ok (${who})`);
}

async function withPage(who, run) {
  // Pages in the default context share cookies, so a second principal would
  // reuse the operator session and never see the login form.
  const context = await browser.createBrowserContext();
  const page = await context.newPage();
  await page.setViewport({ width: 1500, height: 1150 });
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const downloads = ctx.file(`downloads-${who}`);
  fs.mkdirSync(downloads, { recursive: true });
  const cdp = await page.createCDPSession();
  await cdp.send('Browser.setDownloadBehavior', {
    behavior: 'allow', downloadPath: downloads, eventsEnabled: true, browserContextId: context.id,
  });
  try {
    await login(page, who);
    await run(page, downloads);
    assert.deepEqual(pageErrors, [], `page errors: ${pageErrors.join('; ')}`);
  } catch (error) {
    await shot(page, `failure-${who}`).catch(() => {});
    throw error;
  } finally {
    await page.close();
    await context.close();
  }
}

// The transcript and runner views hold an SSE connection open, so networkidle0
// never settles there.
const gotoView = (page, view, extra = '') => page.goto(`${ctx.origin}/?view=${view}${extra}`, { waitUntil: 'domcontentloaded' });

// The recent-errors panel is fed from memory, so a pool restart empties it.
// Provoke one upstream failure instead of relying on earlier provisioning traffic.
async function provokeUpstreamError() {
  const bearer = readSecret(ctx.file('operator-key.txt'));
  const roster = await api(ctx.origin, 'GET', '/admin/accounts', { adminToken: ctx.adminToken, ca: ctx.ca });
  const accounts = Array.isArray(roster.json) ? roster.json : (roster.json?.accounts ?? []);
  const generic = accounts.find((account) => String(account.id).startsWith('configured-'));
  assert.ok(generic, 'no generic provider account in the roster');
  const models = await api(ctx.origin, 'GET', '/v1/models', { bearer, ca: ctx.ca });
  const model = models.json?.data?.find((entry) => entry.owned_by === generic.type)?.id;
  assert.ok(model, `no model served by ${generic.type}`);
  const res = await api(ctx.origin, 'POST', '/v1/chat/completions', {
    body: { model, messages: [{ role: 'user', content: 'mock smoke error' }] },
    bearer, headers: { 'x-mock-fail': '500' }, ca: ctx.ca, expect: [429, 500, 502, 503],
  });
  assert.notEqual(res.status, 200, 'the failing mock request unexpectedly succeeded');
}

async function items(page, who, downloads) {
  if (who === 'operator' && only('errors')) {
    await provokeUpstreamError();
    await gotoView(page, 'pulse');
    await page.waitForSelector('.error-list', { timeout: 30000 });
    const rows = await page.evaluate(() => [...document.querySelectorAll('.error-list > div')].map((row) => ({
      at: row.querySelector('time')?.getAttribute('datetime'), message: row.querySelector('span')?.textContent,
    })));
    console.log('ITEM errors rows=', rows.length, JSON.stringify(rows.slice(0, 3)));
    assert.ok(rows.length > 0, 'recent provider errors panel is empty');
    await shot(page, 'pulse-errors');
    console.log('PASS recent-errors panel');
  }

  if (who === 'operator' && only('verify')) {
    await gotoView(page, 'accounts');
    await page.waitForSelector('.account-row:not(.account-head)', { timeout: 30000 });
    const roster = await page.evaluate(() => [...document.querySelectorAll('.account-row:not(.account-head)')].map((row) => ({
      id: row.querySelector('.account-identity small span')?.textContent,
      provider: row.querySelector('.account-identity b')?.textContent,
      state: row.querySelector('.state')?.textContent,
    })));
    console.log('ITEM accounts:', JSON.stringify(roster));
    const mock = roster.find((row) => row.id === 'configured-mock');
    assert.ok(mock, 'configured-mock missing from the accounts roster');
    assert.equal(mock.provider, 'Mock', `generic provider label is ${mock.provider}, expected Mock`);
    assert.equal(mock.state, 'healthy', `generic provider state is ${mock.state}, expected healthy`);

    await hasText(page, 'button', 'Verify all accounts');
    await page.evaluate((id) => {
      const row = [...document.querySelectorAll('.account-row:not(.account-head)')].find((el) => el.textContent.includes(id));
      if (!row) throw new Error(`account row not found: ${id}`);
      row.click();
    }, flags.account);
    await page.waitForFunction((id) => document.querySelector('#account-inspector-title')?.textContent === id, { timeout: 30000 }, flags.account);
    await hasText(page, '.accounts-layout button', 'Verify access');
    const previous = await page.evaluate(() => document.querySelector('.account-inspector .verify-result')?.textContent ?? '');
    await clickText(page, '.accounts-layout button', 'Verify access');
    await page.waitForFunction((previous) => {
      const el = document.querySelector('.account-inspector .verify-result');
      return el && el.textContent !== previous;
    }, { timeout: 60000 }, previous);
    console.log('ITEM single verify:', JSON.stringify(await page.evaluate(() => {
      const el = document.querySelector('.account-inspector .verify-result');
      return { class: el.className, text: el.textContent, link: el.querySelector('a')?.getAttribute('href') ?? null };
    })));
    await shot(page, 'accounts-verify');

    await clickText(page, 'button', 'Verify all accounts');
    await page.waitForFunction(() => !document.querySelector('button.operator-badge')?.textContent.includes('Verifying'), { timeout: 120000 });
    for (const id of ['antigravity_needsver', 'claude_pro', 'gemini_ultra']) {
      await page.evaluate((name) => {
        const row = [...document.querySelectorAll('.account-row:not(.account-head)')].find((el) => el.textContent.includes(name));
        if (!row) throw new Error(`missing account row ${name}`);
        row.click();
      }, id);
      // The inspector keeps the previous account's result until the new one is
      // selected, so wait for both the title and a result before reading it.
      await page.waitForFunction((id) => document.querySelector('#account-inspector-title')?.textContent === id
        && document.querySelector('.account-inspector .verify-result'), { timeout: 60000 }, id);
      console.log(`ITEM batch result for ${id}:`, JSON.stringify(await page.evaluate(() => {
        const el = document.querySelector('.account-inspector .verify-result');
        return { class: el.className, text: el.textContent };
      })));
    }
    await shot(page, 'accounts-verify-all');
    console.log('PASS verify controls');
  }

  if (who === 'operator' && only('analytics')) {
    await gotoView(page, 'pulse');
    await page.waitForSelector('.provider-lane', { timeout: 30000 });
    const lanes = await page.$$eval('.provider-lane .provider-name b', (nodes) => nodes.map((node) => node.textContent));
    console.log('ITEM analytics lanes:', JSON.stringify(lanes));
    assert.ok(lanes.includes('Mock'), `generic provider missing from the lanes: ${lanes.join(', ')}`);
    await page.waitForFunction(() => [...document.querySelectorAll('.origin-row .footprint i')]
      .some((node) => (node.getAttribute('title') || '').startsWith('Mock:')), { timeout: 60000 });
    const footprint = await page.$$eval('.origin-row .footprint i', (nodes) => nodes.map((node) => node.getAttribute('title')));
    console.log('ITEM origin footprint:', JSON.stringify(footprint));
    const burn = await page.$eval('.burn-panel', (node) => node.textContent.slice(0, 200));
    console.log('ITEM burn panel:', burn);
    await shot(page, 'analytics-mock');
    console.log('PASS analytics generic provider');
  }

  if (only('export')) {
    const view = who === 'operator' ? 'console' : 'mine';
    await gotoView(page, view);
    await page.waitForSelector('.usage-export', { timeout: 30000 });
    if (who === 'operator') {
      await page.evaluate(() => {
        const row = [...document.querySelectorAll('.principal-row')].find((el) => el.textContent.includes('member'));
        if (row) row.click();
      });
      await page.waitForFunction(() => document.querySelectorAll('.usage-export').length > 0, { timeout: 30000 });
    }
    console.log(`ITEM export widgets on ${view}:`, await page.evaluate(() => document.querySelectorAll('.usage-export').length));
    for (const [label, ext] of [['Export CSV', 'csv'], ['Export JSON', 'json']]) {
      const before = fs.readdirSync(downloads);
      await clickText(page, '.usage-export button', label);
      let landed = null;
      const deadline = Date.now() + 30000;
      while (!landed && Date.now() < deadline) {
        landed = fs.readdirSync(downloads).find((file) => !before.includes(file) && !file.endsWith('.crdownload')) ?? null;
        if (!landed) await new Promise((resolve) => setTimeout(resolve, 200));
      }
      assert.ok(landed, `${label} produced no download`);
      assert.ok(landed.endsWith(`.${ext}`), `${label} downloaded ${landed}`);
      const body = fs.readFileSync(path.join(downloads, landed), 'utf8');
      const lines = body.trim().split('\n');
      console.log(`ITEM ${label} -> ${landed} bytes=${body.length} lines=${lines.length}`);
      console.log('ITEM header:', lines[0].slice(0, 200));
      if (ext === 'csv') assert.ok(lines.length > 1, 'CSV export has no data rows');
      else assert.ok(JSON.parse(body).rows?.length > 0, 'JSON export has no rows');
      const alert = await page.evaluate(() => document.querySelector('.usage-export small[role="alert"]')?.textContent ?? null);
      assert.equal(alert, null, `export error: ${alert}`);
    }
    await shot(page, `export-${who}`);
    console.log('PASS usage export', who);
  }
}

async function conversations(page) {
  await gotoView(page, 'conversations');
  await page.waitForFunction(() => document.querySelector('h1')?.textContent === 'Conversations', { timeout: 30000 });
  await page.waitForSelector('.conversation-row', { timeout: 30000 });
  const seeded = readJSON(ctx.file('conversations-result.json'), {})?.results ?? [];
  const pending = seeded.find((record) => record.state === 'waiting_approval');
  let target = flags.conversation;
  if (!target) target = (pending ?? seeded[0])?.conversation_id ?? '';
  else target = seeded.find((record) => record.label === target)?.conversation_id ?? target;
  assert.ok(target, 'no conversation to open; run mock-stack.sh up (it seeds conversations)');
  await gotoView(page, 'conversations', `&conversation=${target}`);
  await page.waitForSelector('.conversation-detail h3', { timeout: 30000 });
  await page.waitForFunction(() => (document.querySelector('.transcript')?.innerText || '').trim().length > 0, { timeout: 30000 });

  const detail = await page.evaluate(() => ({
    title: document.querySelector('.conversation-detail h3')?.textContent,
    storage: document.querySelector('.detail-heading span')?.textContent,
    facts: [...document.querySelectorAll('.detail-facts span')].map((node) => node.textContent),
    actions: [...document.querySelectorAll('.detail-actions button')].map((node) => node.textContent.trim()),
    live: document.querySelector('.live-status')?.textContent,
    approvals: [...document.querySelectorAll('.approval-card')].map((card) => card.textContent),
    markers: [...document.querySelectorAll('.turn-marker')].map((node) => node.textContent),
  }));
  console.log('CONVERSATION detail:', JSON.stringify(detail, null, 1));
  if (detail.storage === 'active') {
    assert.ok(detail.facts.some((fact) => fact.startsWith('Archives')), `archive timing missing from the facts: ${detail.facts.join(' | ')}`);
  }
  for (const card of detail.approvals) {
    assert.ok(card.includes('Denied automatically when it expires.'), `approval card does not explain expiry: ${card}`);
  }
  await shot(page, 'conversations');

  const action = flags.action || (detail.approvals.length > 0 ? 'cancel' : 'none');
  if (action !== 'none') {
    const twoClick = async (first, second) => {
      await hasText(page, '.detail-actions button', first, 20000);
      await clickText(page, '.detail-actions button', first);
      await hasText(page, '.detail-actions button', second, 20000);
      await clickText(page, '.detail-actions button', second);
    };
    if (action === 'cancel' || action === 'deny') {
      const selector = action === 'deny' ? '.approval-card button' : '.detail-actions button';
      const label = action === 'deny' ? 'Deny' : 'Cancel turn';
      await hasText(page, selector, label, 20000);
      await clickText(page, selector, label);
      const word = action === 'deny' ? 'denied' : 'cancelled';
      await page.waitForFunction((word) => (document.querySelector('.transcript')?.innerText || '').includes(word), { timeout: 30000 }, word);
      // Regression: a finished turn must not leave a stuck approval card behind.
      await page.waitForFunction(() => document.querySelectorAll('.approval-card').length === 0, { timeout: 20000 });
      assert.equal(await page.evaluate(() => document.querySelectorAll('.approval-card').length), 0, `approval card survived ${action}`);
    } else if (action === 'close') {
      await twoClick('Close session', 'Confirm close');
      await page.waitForFunction(() => [...document.querySelectorAll('.detail-facts span')].some((node) => node.textContent.includes('closed')), { timeout: 30000 });
    } else if (action === 'delete') {
      const before = Number(await page.$eval('.console-toolbar strong', (node) => node.textContent));
      await twoClick('Delete conversation', 'Confirm delete');
      await page.waitForFunction((count) => Number(document.querySelector('.console-toolbar strong')?.textContent) < count, { timeout: 30000 }, before);
    }
    console.log(`CONVERSATION after ${action}:`, JSON.stringify(await page.evaluate(() => ({
      facts: [...document.querySelectorAll('.detail-facts span')].map((node) => node.textContent),
      actions: [...document.querySelectorAll('.detail-actions button')].map((node) => node.textContent.trim()),
      approvals: document.querySelectorAll('.approval-card').length,
      errors: [...document.querySelectorAll('.signal-error')].map((node) => node.textContent),
      tail: document.querySelector('.transcript')?.innerText?.slice(-200),
    })), null, 1));
    await shot(page, `conversations-${action}`);
  }

  await clickText(page, '.storage-toggle button', 'Archived');
  await page.waitForFunction(() => document.querySelector('.console-toolbar span')?.textContent === 'Archived conversations', { timeout: 15000 });
  const archived = await page.evaluate(() => ({
    count: document.querySelector('.console-toolbar strong')?.textContent,
    empty: document.querySelector('.conversation-roster .empty-state')?.textContent ?? null,
  }));
  console.log('CONVERSATION archived:', JSON.stringify(archived));
  if (archived.empty) {
    assert.ok(/archived after 7 days/.test(archived.empty), `archived empty state does not explain the delay: ${archived.empty}`);
  }
  await clickText(page, '.storage-toggle button', 'Active');
  await page.waitForSelector('.conversation-row', { timeout: 15000 });
  console.log('PASS conversations');
}

async function runners(page) {
  await gotoView(page, 'runners');
  await page.waitForFunction(() => document.querySelector('h1')?.textContent === 'Runners', { timeout: 30000 });
  await page.waitForSelector('.runner-row', { timeout: 30000 });
  await page.waitForFunction(() => [...document.querySelectorAll('.runner-row')].some((row) => row.innerText.includes('online')), { timeout: 30000 });
  const dump = () => page.evaluate(() => ({
    runners: [...document.querySelectorAll('.runner-row')].map((row) => ({
      name: row.querySelector('strong')?.textContent,
      text: row.innerText.replace(/\n+/g, ' | '),
      inactive: row.classList.contains('inactive'),
      buttons: [...row.querySelectorAll('button')].map((node) => node.textContent.trim()),
    })),
    pairings: [...document.querySelectorAll('.pairing-card')].map((card) => card.innerText.replace(/\n+/g, ' | ')),
    message: document.querySelector('.account-message')?.textContent ?? null,
    errors: [...document.querySelectorAll('.signal-error')].map((node) => node.textContent),
  }));
  const before = await dump();
  console.log('RUNNERS', JSON.stringify(before.runners, null, 1));
  console.log('PAIRINGS', JSON.stringify(before.pairings));
  await shot(page, `runners-${flags.who}`);

  if (flags.revoke) {
    const clickIn = (name, label) => page.evaluate((name, label) => {
      const row = [...document.querySelectorAll('.runner-row')].find((el) => el.querySelector('strong')?.textContent === name);
      if (!row) throw new Error(`runner row not found: ${name}`);
      const button = [...row.querySelectorAll('button')].find((el) => el.textContent.trim() === label);
      if (!button) throw new Error(`button "${label}" not found on ${name}`);
      button.click();
    }, name, label);
    await clickIn(flags.revoke, 'Revoke');
    await page.waitForFunction((name) => {
      const row = [...document.querySelectorAll('.runner-row')].find((el) => el.querySelector('strong')?.textContent === name);
      return row && [...row.querySelectorAll('button')].some((el) => el.textContent.trim() === 'Confirm revoke');
    }, { timeout: 20000 }, flags.revoke);
    await clickIn(flags.revoke, 'Confirm revoke');
    await page.waitForFunction((name) => (document.querySelector('.account-message')?.textContent || '').includes(`Revoked ${name}`), { timeout: 30000 }, flags.revoke);
    await page.waitForFunction((name) => {
      const row = [...document.querySelectorAll('.runner-row')].find((el) => el.querySelector('strong')?.textContent === name);
      return row && (row.classList.contains('inactive') || /revoked/i.test(row.innerText));
    }, { timeout: 20000 }, flags.revoke);
    const after = await dump();
    console.log('RUNNERS after revoke:', JSON.stringify(after, null, 1));
    await shot(page, `runners-${flags.who}-revoked`);
    const target = after.runners.find((row) => row.name === flags.revoke);
    assert.ok(target, 'revoked runner disappeared from the roster');
    assert.ok(target.inactive || /revoked/i.test(target.text), 'revoked runner is not marked inactive');
    // A principal may own only the disposable runner, so check the blast radius
    // instead of demanding a survivor.
    if (before.runners.some((row) => row.name !== flags.revoke && /online/.test(row.text))) {
      assert.ok(after.runners.some((row) => row.name !== flags.revoke && /online/.test(row.text)), 'the revoke took down an unrelated runner');
    }
    assert.deepEqual(after.errors, [], `revoke surfaced errors: ${after.errors.join('; ')}`);
  }
  console.log('PASS runners', flags.revoke || '(no revoke)');
}

async function approve(page) {
  await gotoView(page, 'runners');
  await page.waitForFunction(() => document.querySelector('h1')?.textContent === 'Runners', { timeout: 30000 });
  await page.waitForSelector('.pairing-card', { timeout: 15000 }).catch(() => {});
  const cards = await page.$$eval('.pairing-card .pairing-identity strong', (nodes) => nodes.map((node) => node.textContent));
  if (cards.length === 0) {
    console.log('SKIP approve: no pending pairing card. Start one with `bash scripts/mock-stack.sh pair` and run this suite while it waits.');
    return;
  }
  const name = flags.runner || cards[0];
  assert.ok(cards.includes(name), `pairing card ${name} not found; pending: ${cards.join(', ')}`);
  let code = flags.code;
  if (!code) {
    const log = ctx.file('logs', `${flags.who}-pair.log`);
    code = fs.existsSync(log) ? /^Machine code: (.+)$/m.exec(fs.readFileSync(log, 'utf8'))?.[1]?.trim() ?? '' : '';
  }
  assert.ok(code, `no machine code for ${name}; pass --code or run the pairing through mock-stack.sh`);
  console.log('APPROVE card:', name);
  await page.evaluate((name, code) => {
    const card = [...document.querySelectorAll('.pairing-card')].find((el) => el.querySelector('.pairing-identity strong')?.textContent === name);
    if (!card) throw new Error(`pairing card not found: ${name}`);
    const input = card.querySelector('.pairing-code input');
    if (!input) throw new Error('pairing code input missing');
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, code);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }, name, code);
  await page.waitForFunction((name) => {
    const card = [...document.querySelectorAll('.pairing-card')].find((el) => el.querySelector('.pairing-identity strong')?.textContent === name);
    const button = card && [...card.querySelectorAll('button')].find((el) => el.textContent.trim() === 'Approve');
    return button && !button.disabled;
  }, { timeout: 15000 }, name);
  await page.evaluate((name) => {
    const card = [...document.querySelectorAll('.pairing-card')].find((el) => el.querySelector('.pairing-identity strong')?.textContent === name);
    [...card.querySelectorAll('button')].find((el) => el.textContent.trim() === 'Approve').click();
  }, name);
  await page.waitForFunction((name) => {
    const body = document.body.textContent;
    return body.includes(`Approved ${name}`) || !!document.querySelector('.signal-error') || !document.querySelector('.pairing-card');
  }, { timeout: 30000 }, name);
  const state = await page.evaluate(() => ({
    message: document.querySelector('.account-message')?.textContent ?? null,
    errors: [...document.querySelectorAll('.signal-error')].map((node) => node.textContent),
    cards: document.querySelectorAll('.pairing-card').length,
  }));
  console.log('APPROVE result:', JSON.stringify(state));
  await shot(page, `approve-${flags.who}`);
  assert.deepEqual(state.errors, [], `approval surfaced errors: ${state.errors.join('; ')}`);
  assert.ok(state.message?.includes(`Approved ${name}`) || state.cards === 0, `approval did not succeed: ${JSON.stringify(state)}`);
  console.log('PASS approve', name);
}

try {
  for (const suite of suites) {
    console.log(`\n=== ${suite} ===`);
    if (suite === 'items') {
      await withPage('operator', (page, downloads) => items(page, 'operator', downloads));
      if (only('export')) await withPage('member', (page, downloads) => items(page, 'member', downloads));
    } else if (suite === 'conversations') {
      await withPage('operator', conversations);
    } else if (suite === 'runners') {
      await withPage(flags.who, runners);
    } else {
      await withPage(flags.who, approve);
    }
  }
  console.log(`\nPASS mock-stack smokes: ${suites.join(', ')}`);
} finally {
  await browser.close();
}
