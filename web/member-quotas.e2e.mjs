import assert from 'node:assert/strict';
import { mkdir } from 'node:fs/promises';
import puppeteer from 'puppeteer-core';

for (const key of ['CHROME_PATH', 'UI_TEST_ORIGIN', 'E2E_MEMBER_ID', 'E2E_MEMBER_SESSION', 'E2E_MEMBER_CSRF', 'E2E_OPERATOR_SESSION', 'E2E_OPERATOR_CSRF', 'E2E_VERIFICATION_RUNTIME']) assert.ok(process.env[key], `${key} is required`);
const origin = process.env.UI_TEST_ORIGIN;
const memberID = process.env.E2E_MEMBER_ID;
const call = async (path, { operator = false, method = 'GET', body, csrf = true } = {}) => {
  const token = operator ? process.env.E2E_OPERATOR_SESSION : process.env.E2E_MEMBER_SESSION;
  const secret = operator ? process.env.E2E_OPERATOR_CSRF : process.env.E2E_MEMBER_CSRF;
  return fetch(origin + path, { method, headers: { Cookie: `pool_session=${token}; pool_csrf=${secret}`, 'Content-Type': 'application/json', ...(csrf ? { 'X-CSRF-Token': secret } : {}) }, body: body === undefined ? undefined : JSON.stringify(body) });
};
let revision = 0;
const policyPath = `/api/console/policies/${memberID}`;
const save = async (providers, models = {}) => {
  const result = await call(policyPath, { operator: true, method: 'PUT', body: { revision, target: 'principal', policy: { providers, models } } });
  assert.equal(result.status, 200, await result.text());
  revision++;
};
const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH, headless: true });
let checks = 0;
try {
  const page = await browser.newPage();
  const errors = [];
  const requests = [];
  page.on('pageerror', error => errors.push(error.message));
  page.on('request', request => requests.push(new URL(request.url()).pathname));
  await page.browserContext().setCookie(
    { name: 'pool_session', value: process.env.E2E_MEMBER_SESSION, url: origin, httpOnly: true },
    { name: 'pool_csrf', value: process.env.E2E_MEMBER_CSRF, url: origin },
  );
  await page.setViewport({ width: 1440, height: 1000 });
  await page.goto(`${origin}/?view=accounts`, { waitUntil: 'networkidle0' });
  const text = () => page.evaluate(() => document.body.innerText);
  assert.match(await text(), /No models available/); checks++;
  assert.equal(await page.$$eval('meter', nodes => nodes.length), 0); checks++;
  assert.ok(!requests.some(path => path === '/admin/accounts' || path === '/api/pool/stats')); checks++;

  await save({ allow: ['claude'] });
  await page.click('.member-accounts .view-title button');
  await page.waitForFunction(() => document.body.innerText.includes('75%'));
  assert.match(await text(), /Claude Opus 5\.5/); checks++;
  assert.match(await text(), /40%/); checks++;
  assert.doesNotMatch(await text(), /gpt-5\.6-sol|secret-id|never-expose|owner@example/); checks++;
  const quota = await (await call('/api/me/quotas?principal_id=operator&provider=codex')).json();
  assert.deepEqual(quota.models.map(row => row.provider), ['claude']); checks++;
  assert.equal(quota.models[0].supporting_accounts, 1); checks++;
  assert.match(await text(), /No test results registered/); checks++;
  const evidence = {
    suite: 'codex-pool/1', model: 'claude-opus-5-5', provider: 'claude',
    runtime: process.env.E2E_VERIFICATION_RUNTIME, codex_version: 'codex-cli 0.159.1',
    started_at: new Date(Date.now() - 60_000).toISOString(), finished_at: new Date(Date.now() - 1000).toISOString(), expired: false,
    checks: { text: { status: 'passed', code: 'exact_response' }, streaming: { status: 'passed', code: 'completed_stream' }, tools: { status: 'failed', code: 'unexpected_response' }, codex_edit: { status: 'not_tested', code: 'not_run' } },
  };
  const verificationPath = '/api/console/model-verifications';
  assert.equal((await call(verificationPath, { method: 'POST', body: evidence })).status, 403); checks++;
  assert.equal((await call(verificationPath, { operator: true, method: 'POST', csrf: false, body: evidence })).status, 403); checks++;
  const opContext = await browser.createBrowserContext();
  await opContext.setCookie(
    { name: 'pool_session', value: process.env.E2E_OPERATOR_SESSION, url: origin, httpOnly: true },
    { name: 'pool_csrf', value: process.env.E2E_OPERATOR_CSRF, url: origin },
  );
  const opPage = await opContext.newPage();
  await opPage.goto(`${origin}/?view=models`, { waitUntil: 'networkidle0' });
  await opPage.$eval('.verification-import input', (input, data) => {
    const transfer = new DataTransfer();
    transfer.items.add(new File([JSON.stringify(data)], 'synthetic-e2e.json', { type: 'application/json' }));
    input.files = transfer.files;
    input.dispatchEvent(new Event('change', { bubbles: true }));
  }, evidence);
  await opPage.waitForFunction(() => document.body.innerText.includes('Verification registered.'));
  checks++;
  await page.click('.member-accounts .view-title button');
  await page.waitForFunction(() => document.body.innerText.includes('Operator-attested'));
  assert.equal(await page.$$eval('.compatibility-check.passed', nodes => nodes.length), 2); checks++;
  assert.equal(await page.$$eval('.compatibility-check.failed', nodes => nodes.length), 1); checks++;
  assert.equal(await page.$$eval('.compatibility-check.not_tested', nodes => nodes.length), 1); checks++;
  assert.equal(await page.$('.verification-import'), null); checks++;
  assert.equal((await call(verificationPath, { operator: true, method: 'POST', body: evidence })).status, 409); checks++;
  await opContext.close();

  for (const path of ['/admin/accounts', '/api/pool/stats', policyPath]) {
    assert.equal((await call(path)).status, 403, path); checks++;
  }
  assert.equal((await call(policyPath, { method: 'PUT', body: { revision, target: 'principal', policy: { providers: { allow: ['*'] } } } })).status, 403); checks++;
  assert.equal((await call(policyPath, { operator: true, method: 'PUT', csrf: false, body: { revision, target: 'principal', policy: {} } })).status, 403); checks++;
  assert.equal((await call(policyPath, { operator: true, method: 'PUT', body: { revision: 0, target: 'principal', policy: {} } })).status, 409); checks++;
  assert.equal((await call('/api/me/quotas', { method: 'POST', body: {} })).status, 405); checks++;

  await save({ allow: ['*'] });
  await page.reload({ waitUntil: 'networkidle0' });
  assert.match(await text(), /gpt-5\.6-sol/); checks++;
  await page.select('.member-quota-filter select', 'codex');
  assert.match(await text(), /gpt-5\.6-sol/); checks++;
  assert.doesNotMatch(await text(), /Claude Opus 5\.5/); checks++;
  await page.select('.member-quota-filter select', '');

  await mkdir(new URL('test-results/', import.meta.url), { recursive: true });
  await page.screenshot({ path: new URL('test-results/member-quotas-desktop.png', import.meta.url).pathname.replace(/^\/(\w:)/, '$1'), fullPage: true });
  await page.setViewport({ width: 390, height: 844 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), 'mobile horizontal overflow'); checks++;
  await page.screenshot({ path: new URL('test-results/member-quotas-mobile.png', import.meta.url).pathname.replace(/^\/(\w:)/, '$1'), fullPage: true });

  await save({ allow: ['claude'] }, { deny: ['claude-opus-5-5'] });
  await page.reload({ waitUntil: 'networkidle0' });
  assert.match(await text(), /No models available/); checks++;
  assert.equal(await page.$('.model-compatibility'), null); checks++;
  await save({ allow: [], deny: ['*'] });
  await page.reload({ waitUntil: 'networkidle0' });
  assert.match(await text(), /No models available/); checks++;
  assert.equal(await page.$$eval('meter', nodes => nodes.length), 0); checks++;

  // A second member cannot obtain the first member's shared quotas by IDOR.
  const invite = await call('/api/console/members', { operator: true, method: 'POST', body: { email: 'other-browser-member@example.test', display_name: 'Other member', purpose: 'onboard' } });
  assert.equal(invite.status, 200); checks++;
  const link = await invite.json();
  const token = new URL(link.link).hash.slice(1);
  const redemption = await fetch(origin + '/api/auth/recover', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token, password: 'browser-test-password-12345' }) });
  assert.equal(redemption.status, 200, await redemption.text()); checks++;
  const cookies = redemption.headers.getSetCookie().map(value => value.split(';')[0]).join('; ');
  const otherQuota = await fetch(origin + `/api/me/quotas?principal_id=${memberID}`, { headers: { Cookie: cookies } });
  assert.equal(otherQuota.status, 200); checks++;
  assert.deepEqual((await otherQuota.json()).models, []); checks++;
  const anonymous = await fetch(origin + '/api/me/quotas');
  assert.equal(anonymous.status, 401); checks++;
  assert.deepEqual(errors, []); checks++;
  console.log(`PASS: ${checks} real-browser/backend quota and security assertions`);
} finally { await browser.close(); }
