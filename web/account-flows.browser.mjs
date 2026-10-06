import assert from 'node:assert/strict';

import puppeteer from 'puppeteer-core';
assert.ok(process.env.CHROME_PATH, 'CHROME_PATH is required');
assert.ok(process.env.UI_TEST_ORIGIN, 'UI_TEST_ORIGIN is required');
const origin = process.env.UI_TEST_ORIGIN;
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH, headless: true, args: process.env.BROWSER_NO_SANDBOX === '1' ? ['--no-sandbox'] : [] });
const failures = [];
const principal = { id: 'fixture-member', kind: 'operator', status: 'active', display_name: 'Alex Morgan', email: 'alex@example.test' };
const clients = [{ id: 'laptop', label: 'Work laptop', status: 'active' }, { id: 'server', label: 'Home server', status: 'active' }, { id: 'old', label: 'Old machine', status: 'revoked' }];
const pass = { id: 'guest', note: 'Sam', display_name: 'Sam', status: 'active', expires_at: '2026-10-12T18:30:00Z', clients: 1, link: '/join#fixture-pass' };
const stats = { accounts: [], active_accounts: 0, total_accounts: 0, last_24h_tokens: 0, generated_at: '2026-09-04T12:00:00Z', aggregate: { total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 } };
const open = async (path = '/?view=mine', mode = 'signed-in') => {
  const page = await browser.newPage();
  await page.emulateTimezone('America/Los_Angeles');
  await page.setViewport({ width: 1365, height: 1000 });
  const errors = [];
  const ownMode = mode.startsWith('my-');
  let owned = ownMode ? [{ id: 'private-kimi', provider: 'kimi', status: 'active', state: 'ready', revision: 2 }] : [];
  let createdClient;
  let accountControls = { provider: 'kimi', id: 'private-kimi', revision: 2, inflight: 1, controls: { state: 'enabled', max_concurrent: 2 } };
  let sharing = { provider: 'kimi', id: 'private-kimi', revision: 2, owner: true, operator_may_delegate: false, grants: [] };
  const emptyPolicy = () => ({ models: {}, providers: {}, limits: {}, routing: {} });
  let policyView = { principal_id: principal.id, revision: 0, principal_policy: emptyPolicy(), clients: [{ id: 'laptop', label: 'Laptop', status: 'active', policy: emptyPolicy() }], sources: [{ source: 'global', policy: emptyPolicy() }], effective: emptyPolicy(), allowed: true, reasons: [], usage: [{ scope: `principal:${principal.id}`, limits: {}, minute: { requests: 0, tokens: 0 }, day: { requests: 1, tokens: 5, reserved_tokens: 2 }, month: { requests: 1, tokens: 5 }, inflight: 1 }] };
  await page.browserContext().setCookie({ name: 'pool_csrf', value: 'browser-fixture', domain: '127.0.0.1', path: '/' });
  page.on('pageerror', e => errors.push(e.message));
  await page.setRequestInterception(true);
  page.on('request', async request => {
    const url = new URL(request.url());
    if (url.origin !== origin) { await request.abort(); return; }
    if (!url.pathname.startsWith('/api/') && url.pathname !== '/admin/accounts') { await request.continue(); return; }
    let body = {};
    let status = 200;
    if (url.pathname === '/api/auth/me') { body = mode === 'guest' ? { ...principal, kind: 'guest' } : ownMode ? { ...principal, kind: 'member', can_contribute: mode !== 'my-disabled' } : principal; if (mode === 'signed-out') { status = 401; body = { error: 'Sign in required' }; } }
    else if (url.pathname === '/api/auth/join') body = { switch_required: true, current: principal };
    else if (url.pathname === '/api/auth/config') body = { operator_exists: true, legacy_signup: true };
    else if (url.pathname === '/api/auth/recover/status') body = { valid: true, expires_at: new Date(Date.now() + 600000).toISOString() };
    else if (url.pathname === '/admin/accounts') body = [];
    else if (url.pathname === '/api/me/clients') {
      body = createdClient ? [createdClient] : ['empty', 'create'].includes(mode) ? [] : clients;
      if (request.method() === 'POST') {
        if (mode === 'create') body = createdClient = { id: 'new-client', label: JSON.parse(request.postData()).label, status: 'active' };
        else { status = 503; body = { error: 'Unable to create client. Try again.' }; }
      }
    }
    else if (url.pathname.endsWith('/setup-link')) {
      const id = url.pathname.split('/')[4];
      // Hold the older response behind a newer selection.
      if (id === 'server') await new Promise(resolve => setTimeout(resolve, 250));
      body = { id, setup_urls: { codex: `${origin}/config/codex/fixture-${id}` }, nonce_expires_at: '2099-01-01T00:00:00Z' };
    }
    else if (url.pathname === '/api/me/usage' || url.pathname.endsWith('/usage')) body = { hourly: mode === 'chart-data' ? [0, 1, 2].map(i => ({ hour: `2026-09-30T${12+i}:00:00Z`, billable_tokens: (i+1)*100, api_equivalent_cost_usd: .01 })) : [] };
    else if (url.pathname === '/api/me/accounts') {
      body = owned;
      if (mode === 'my-unavailable') { status = 503; body = { error: 'Account authority unavailable' }; }
    }
    else if (url.pathname.startsWith('/api/me/accounts/')) {
      assert.equal(request.headers()['x-csrf-token'], 'browser-fixture');
      assert.equal(JSON.parse(request.postData()).revision, 2);
      if (mode === 'my-conflict') { status = 409; body = { error: { code: 'account_revision_conflict', message: 'Account changed; reload before withdrawing' } }; }
      else { owned = owned.map(account => ({ ...account, status: 'withdrawn', state: 'withdrawn', revision: 3 })); body = { status: 'withdrawn' }; }
    }
    else if (url.pathname === '/api/pool/accounts/kimi/add' && mode === 'my-create') {
      assert.equal(request.headers()['x-csrf-token'], 'browser-fixture');
      owned.push({ id: 'new-private-kimi', provider: 'kimi', status: 'active', state: 'ready', revision: 2 });
      body = { success: true, account_id: 'new-private-kimi' };
    }
    else if (url.pathname === '/api/accounts/kimi/private-kimi/controls') {
      if (request.method() === 'PUT') { assert.equal(request.headers()['x-csrf-token'], 'browser-fixture'); const value = JSON.parse(request.postData()); assert.equal(value.revision, accountControls.revision); accountControls = { ...accountControls, revision: accountControls.revision + 1, controls: value.controls }; owned = owned.map(item => ({ ...item, revision: accountControls.revision })); }
      body = accountControls;
    }
    else if (url.pathname === '/api/accounts/kimi/private-kimi/grants') {
      if (request.method() === 'POST') { assert.equal(request.headers()['x-csrf-token'], 'browser-fixture'); const value = JSON.parse(request.postData()); assert.equal(value.revision, sharing.revision); sharing = { ...sharing, revision: sharing.revision + 1, grants: [...sharing.grants, { ...value, revision: 1 }] }; }
      body = sharing;
    }
    else if (url.pathname.startsWith('/api/account-grants/')) {
      assert.equal(request.method(), 'DELETE'); assert.equal(request.headers()['x-csrf-token'], 'browser-fixture'); const id = url.pathname.split('/').at(-1); sharing = { ...sharing, grants: sharing.grants.map(grant => grant.id === id ? { ...grant, revision: 2, revoked_at: new Date().toISOString() } : grant) }; body = { id, revoked: true };
    }
    else if (url.pathname.startsWith('/api/console/policies/')) {
      if (request.method() !== 'GET') { assert.equal(request.headers()['x-csrf-token'], 'browser-fixture'); const value = JSON.parse(request.postData()); assert.equal(value.revision, policyView.revision); body = { ...policyView, effective: value.policy }; if (request.method() === 'PUT') { policyView = { ...body, revision: policyView.revision + 1, principal_policy: value.policy }; body = policyView; } }
      else body = policyView;
    }
    else if (url.pathname === '/api/me/passkeys') body = [];
    else if (url.pathname === '/api/passes') body = [pass];
    else if (url.pathname === '/api/console/principals') body = { principals: [{ ...principal, note: '', billable_tokens: 0, api_equivalent_cost_usd: 0, request_count: 0 }, { ...pass, kind: 'guest', billable_tokens: 0, api_equivalent_cost_usd: 0, request_count: 0 }] };
    else if (url.pathname === '/api/console/members') body = { principal, link: `${origin}/recover#fixture-invite`, expires_at: '2026-09-04T12:30:00Z' };
    else if (url.pathname === '/api/console/audit') body = [];
    else if (url.pathname === '/api/console/analytics-health') body = { health: { state: 'CURRENT', outbox_depth: 0 }, accounting_gaps: [] };
    else if (url.pathname === '/api/pool/stats') body = stats;
    else if (url.pathname === '/api/pool/signal') body = { hourly: [], economics: [], origin_weekly: [], model_daily: [], quota_capacity: [], model_efficiency: [], reset_observations: [] };
    else if (url.pathname === '/api/pool/catalog') body = { models: [{ id: 'gpt-5.5', provider: 'codex', protocol: 'responses', available_now: true }, { id: 'claude-opus-5', provider: 'claude', protocol: 'messages', available_now: false }] };
    else { status = 503; body = { error: 'Request failed. Try again.' }; }
    await request.respond({ status, contentType: 'application/json', body: JSON.stringify(body) });
  });
  await page.goto(origin + path);
  await page.waitForSelector('h1');
  return { page, errors };
};
const clickText = async (page, selector, text) => {
  await page.waitForFunction((selector, text) => [...document.querySelectorAll(selector)].some(el => el.textContent.trim() === text), {}, selector, text);
  await page.evaluate((selector, text) => [...document.querySelectorAll(selector)].find(el => el.textContent.trim() === text).click(), selector, text);
};
const check = async (name, test) => {
  try { await test(); console.log(`PASS ${name}`); }
  catch (error) { failures.push(name); console.error(`FAIL ${name}: ${error.message}`); }
};
try {
  await check('recovery waits for blur and keeps submit enabled', async () => {
    const { page } = await open('/recover#fixture-recovery', 'signed-out');
    await page.waitForSelector('input[autocomplete="new-password"]');
    await page.type('input[autocomplete="new-password"]', 'a-long-password');
    await page.type('label:nth-child(2) input', 'a');
    assert.equal(await page.$eval('label:nth-child(2) input', el => el.getAttribute('aria-invalid')), 'false');
    assert.equal(await page.$eval('button[type="submit"], .threshold-submit', el => el.disabled), false);
    await page.close();
  });
  await check('setup keeps the latest selection and excludes revoked credentials', async () => {
    const { page } = await open('/?view=setup');
    await clickText(page, 'button', 'Generate link');
    await page.waitForFunction(() => document.querySelector('.tool-detail')?.textContent.includes('fixture-laptop'));
    await clickText(page, '.setup-clients button', 'Home server');
    await clickText(page, 'button', 'Generate link');
    await clickText(page, '.setup-clients button', 'Work laptop');
    await clickText(page, 'button', 'Generate link');
    await page.waitForNetworkIdle();
    assert.equal(await page.$eval('.client-pill.active', el => el.textContent), 'Work laptop');
    await page.waitForFunction(() => document.querySelector('.tool-detail')?.textContent.includes('fixture-laptop'));
    const old = await page.$eval('.setup-clients', el => [...el.querySelectorAll('button')].find(button => button.textContent === 'Old machine')?.disabled ?? true);
    assert.equal(old, true);
    await page.close();
  });
  await check('clipboard rejection does not report success', async () => {
    const { page } = await open('/?view=setup');
    await page.waitForSelector('.copy-btn');
    await page.evaluate(() => Object.defineProperty(navigator.clipboard, 'writeText', { value: () => Promise.reject(new Error('Denied')) }));
    await page.click('.copy-btn');
    await page.waitForFunction(() => document.querySelector('[role="alert"]')?.textContent.includes('Copy failed'));
    assert.equal(await page.$eval('.copy-btn', el => el.textContent), 'Copy');
    await page.close();
  });
  await check('pass editing preserves local expiry', async () => {
    const { page } = await open('/?view=passes');
    await page.waitForSelector('.pass-row');
    await page.evaluate(() => [...document.querySelectorAll('.pass-row button')].find(el => el.textContent.toLowerCase() === 'edit').click());
    assert.equal(await page.$eval('input[type="datetime-local"]', el => el.value), '2026-10-12T11:30');
    await page.close();
  });
  await check('declining a guest pass keeps the current account', async () => {
    const { page } = await open('/join#fixture-pass');
    await clickText(page, 'button', 'Stay signed in as Alex Morgan');
    await page.waitForSelector('.identity-strip');
    assert.ok(await page.$eval('.identity-strip', el => el.textContent.includes('Alex Morgan')));
    await page.close();
  });
  await check('guests do not see unavailable pool metrics or a no-op refresh', async () => {
    const { page } = await open('/?view=mine', 'guest');
    await page.waitForSelector('.identity-strip');
    assert.equal(await page.$eval('.rail-readouts', el => el.textContent.includes('Refresh')), false);
    assert.equal(await page.$eval('.rail-readouts', el => el.textContent.includes('accounts live')), false);
    await page.close();
  });
  await check('failed client creation preserves input', async () => {
    const { page } = await open('/?view=setup', 'empty');
    await clickText(page, 'button', 'Create a client');
    await page.type('.setup-client-create input', 'Travel laptop');
    await page.click('.setup-client-create .gold-button');
    await page.waitForSelector('[role="alert"]');
    assert.equal(await page.$eval('.setup-client-create input', el => el.value), 'Travel laptop');
    await page.close();
  });
  await check('new clients select their own setup and generate a link explicitly', async () => {
    const { page } = await open('/?view=setup', 'create');
    await clickText(page, 'button', 'Create a client');
    await page.type('.setup-client-create input', 'Travel laptop');
    await page.click('.setup-client-create .gold-button');
    await page.waitForSelector('.client-pill.active');
    assert.equal(await page.$eval('.client-pill.active', el => el.textContent), 'Travel laptop');
    await clickText(page, 'button', 'Generate link');
    await page.waitForFunction(() => document.querySelector('.tool-detail')?.textContent.includes('fixture-new-client'));
    assert.ok(await page.$eval('.tool-detail', el => el.textContent.includes('fixture-new-client')));
    await page.close();
  });
  await check('member invitations show the private link and expiry', async () => {
    const { page } = await open('/?view=console');
    await clickText(page, 'button', 'Add member');
    await page.type('.member-admin input[type=email]', 'new@example.test');
    await page.click('.member-admin .gold-button');
    await page.waitForSelector('.member-link-result');
    assert.ok(await page.$eval('.member-link-result', el => el.textContent.includes('30 minutes')));
    assert.ok(await page.$eval('.member-link-result code', el => el.textContent.endsWith('#fixture-invite')));
    await page.close();
  });
  await check('pool account forms preserve credentials on failure', async () => {
    const { page, errors } = await open('/?view=accounts');
    await clickText(page, 'button', 'Add pool account');
    await clickText(page, '.contribution-providers button', 'Kimi');
    await page.type('.contribution-field input', 'fixture-provider-key');
    await page.click('.contribution-dialog .gold-button');
    await page.waitForSelector('.contribution-dialog [role=alert]');
    assert.equal(await page.$eval('.contribution-field input', el => el.value), 'fixture-provider-key');
    for (const width of [1365, 390]) {
      await page.setViewport({ width, height: 1000 });
      await page.screenshot({ path: `test-results/account-provider-${width}.png`, fullPage: true });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), JSON.stringify(await page.evaluate(() => [...document.querySelectorAll('body *')].filter(el => el.getBoundingClientRect().right > innerWidth + 1).map(el => `${el.tagName}.${el.className}`).slice(0, 20))));
    }
    assert.deepEqual(errors, []);
    await page.close();
  });
  for (const view of ['mine', 'setup', 'passes', 'console']) {
    await check(`${view} renders at desktop and mobile widths`, async () => {
      const { page, errors } = await open(`/?view=${view}`);
      await page.waitForNetworkIdle();
      if (view === 'mine') await clickText(page, 'button', 'Edit profile');
      if (view === 'passes') await clickText(page, 'button', 'Create a pass');
      if (view === 'console') await clickText(page, 'button', 'Add member');
      for (const width of [1365, 390]) {
        await page.setViewport({ width, height: 1000 });
        await page.screenshot({ path: `test-results/account-${view}-${width}.png`, fullPage: true });
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `Page overflows at ${width}px`);
      }
      assert.deepEqual(errors, []);
      await page.close();
    });
  }
  await check('private account contribution and withdrawal work for members', async () => {
    const { page, errors } = await open('/?view=mine', 'my-create');
    await page.waitForSelector('.mine-accounts');
    await clickText(page, '.mine-accounts button', 'Add account');
    await clickText(page, '.contribution-providers button', 'Kimi');
    await page.type('.contribution-field input', 'synthetic-private-key');
    await page.click('.contribution-dialog .gold-button');
    await page.waitForFunction(() => document.querySelector('.mine-accounts').textContent.includes('new-private-kimi'));
    await page.waitForFunction(() => !document.querySelector('.contribution-dialog'));
    await page.click('.mine-accounts .danger-action');
    await clickText(page, '.mine-accounts button', 'Confirm withdrawal');
    await page.waitForFunction(() => document.querySelector('.mine-accounts').textContent.includes('withdrawn'));
    assert.equal(await page.$$eval('.mine-accounts .danger-action', nodes => nodes.length), 0);
    assert.deepEqual(errors, []);
    await page.close();
  });
  await check('private account conflicts and authority failures remain explicit', async () => {
    const { page } = await open('/?view=mine', 'my-conflict');
    await page.waitForSelector('.mine-accounts .danger-action');
    await page.click('.mine-accounts .danger-action');
    await clickText(page, '.mine-accounts button', 'Confirm withdrawal');
    await page.waitForFunction(() => document.querySelector('.mine-accounts [role=alert]')?.textContent.includes('Account changed'));
    assert.ok(await page.$eval('.mine-accounts', el => el.textContent.includes('private-kimi')));
    await page.close();
    const { page: unavailable } = await open('/?view=mine', 'my-unavailable');
    await unavailable.waitForSelector('.mine-accounts [role=alert]');
    assert.equal(await unavailable.$eval('.mine-accounts', el => el.textContent.includes('No provider accounts yet.')), false);
    await unavailable.close();
  });
  await check('private account list fits mobile and respects contribution permission', async () => {
    const { page } = await open('/?view=mine', 'my-disabled');
    await page.waitForSelector('.mine-accounts .client-card');
    assert.equal(await page.$$eval('.mine-accounts button', nodes => nodes.some(el => el.textContent === 'Add account')), false);
    for (const width of [390, 768, 1365]) {
      await page.setViewport({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `My accounts overflows at ${width}px`);
    }
    await page.close();
  });
  await check('persistent controls save and fit mobile', async () => {
    const { page, errors } = await open('/?view=mine', 'my-controls');
    await clickText(page, 'button', 'Controls and sharing');
    await page.waitForSelector('.governance-form select');
    await page.select('.governance-form select', 'draining');
    await page.type('.governance-form input[maxlength="240"]', 'Finish active conversations');
    await clickText(page, 'button', 'Save account controls');
    await page.waitForFunction(() => document.querySelector('.mine-accounts .governance-panel button')?.getAttribute('aria-expanded') === 'false');
    await clickText(page, 'button', 'Controls and sharing');
    await page.waitForSelector('.governance-form select');
    assert.equal(await page.$eval('.governance-form select', el => el.value), 'draining');
    for (const width of [390, 768, 1365]) { await page.setViewport({ width, height: 900 }); assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `Controls overflow at ${width}px`); }
    assert.deepEqual(errors, []); await page.close();
  });
  await check('sharing creates a scoped grant and requires confirmation to revoke', async () => {
    const { page, errors } = await open('/?view=mine', 'my-sharing');
    await clickText(page, 'button', 'Controls and sharing'); await clickText(page, 'button', 'Sharing');
    await page.waitForSelector('.governance-form input[type="datetime-local"]');
    await page.evaluate(() => { const values = { 'Recipient principal ID': 'guest', 'Granted models': 'kimi-k2.5', 'Expires at': '2026-12-01T12:00', 'Grant reason': 'Team access' }; const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set; for (const label of document.querySelectorAll('.governance-form label')) { const input = label.querySelector('input'); const text = label.firstChild?.textContent; if (input && values[text]) { setter.call(input, values[text]); input.dispatchEvent(new Event('input', { bubbles: true })); } } });
    await clickText(page, 'button', 'Create grant');
    await page.waitForFunction(() => document.querySelector('.mine-accounts .governance-panel button')?.getAttribute('aria-expanded') === 'false');
    await clickText(page, 'button', 'Controls and sharing'); await clickText(page, 'button', 'Sharing');
    await clickText(page, 'button', 'Revoke grant');
    assert.equal(await page.$$eval('.governance-form span', nodes => nodes.some(el => el.textContent === 'Revoked')), false);
    await clickText(page, 'button', 'Confirm revoke');
    await page.waitForFunction(() => document.querySelector('.mine-accounts .governance-panel button')?.getAttribute('aria-expanded') === 'false');
    await clickText(page, 'button', 'Controls and sharing'); await clickText(page, 'button', 'Sharing');
    await page.waitForFunction(() => document.querySelector('.governance-form')?.textContent.includes('Revoked'));
    assert.deepEqual(errors, []); await page.close();
  });
  await check('policy models use checkboxes and save directly with optional preview', async () => {
    const { page, errors } = await open('/?view=console');
    await clickText(page, 'button', 'Effective policy'); await page.waitForSelector('.governance-form');
    await page.waitForSelector('fieldset[aria-label="Allowed models"] input[type="checkbox"]');
    assert.equal(await page.$$eval('button', nodes => nodes.find(el => el.textContent === 'Save policy').disabled), false);
    await page.click('fieldset[aria-label="Allowed models"] input[aria-label="gpt-5.5"]');
    await clickText(page, 'button', 'Save policy');
    await page.waitForFunction(() => document.querySelector('.governance-form')?.textContent.includes('Policy saved'));
    assert.equal(await page.$eval('fieldset[aria-label="Allowed models"] input[aria-label="gpt-5.5"]', el => el.checked), true);
    await clickText(page, 'button', 'Preview policy'); await page.waitForFunction(() => document.querySelector('.governance-form')?.textContent.includes('Sample request allowed'));
    await page.evaluate(() => { const label = [...document.querySelectorAll('label')].find(el => el.textContent.startsWith('Sample model')); const input = label.querySelector('input'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, 'gpt-5.5'); input.dispatchEvent(new Event('input', { bubbles: true })); });
    assert.equal(await page.$$eval('button', nodes => nodes.find(el => el.textContent === 'Save policy').disabled), false);
    assert.equal(await page.$eval('.governance-form', el => el.textContent.includes('Sample request allowed')), false);
    await clickText(page, 'button', 'Preview policy'); await page.waitForFunction(() => document.querySelector('.governance-form')?.textContent.includes('Sample request allowed')); await clickText(page, 'button', 'Save policy');
    await page.waitForFunction(() => document.querySelector('.governance-form')?.textContent.includes('Revision 2.'));
    for (const width of [390, 768, 1365]) { await page.setViewport({ width, height: 900 }); assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `Policy editor overflows at ${width}px`); }
    assert.deepEqual(errors, []); await page.close();
  });
  await check('charts load on scroll and expose keyboard-readable values on mobile', async () => {
    const { page, errors } = await open('/?view=mine', 'chart-data');
    await page.setViewport({ width: 390, height: 700 });
    await page.waitForSelector('.chart-placeholder');
    assert.equal(await page.$('.chart-stage canvas'), null);
    await page.$eval('.chart-stage', node => node.scrollIntoView());
    await page.waitForSelector('.chart-stage canvas');
    await page.focus('.chart-data summary'); await page.keyboard.press('Enter');
    await page.waitForSelector('.chart-data[open] table');
    assert.ok(await page.$eval('.chart-data table', node => node.textContent.includes('100')));
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.select('.mobile-nav select', 'setup');
    await page.waitForFunction(() => document.activeElement?.id === 'main-content');
    assert.deepEqual(errors, []); await page.close();
  });
  await check('sign-in renders and retains input on failure', async () => {
    const { page } = await open('/', 'signed-out');
    await page.type('input[autocomplete="username"]', 'alex@example.test');
    await page.type('input[type="password"]', 'fixture-password');
    await page.click('.threshold-submit');
    await page.waitForSelector('[role="alert"]');
    assert.equal(await page.$eval('input[autocomplete="username"]', el => el.value), 'alex@example.test');
    await page.screenshot({ path: 'test-results/account-signin.png', fullPage: true });
    await page.close();
  });
} finally { await browser.close(); }
if (failures.length) process.exitCode = 1;
