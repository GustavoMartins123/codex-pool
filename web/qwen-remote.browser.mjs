import assert from 'node:assert/strict';
import puppeteer from 'puppeteer-core';

assert.ok(process.env.CHROME_PATH, 'CHROME_PATH is required');
assert.ok(process.env.QWEN_TEST_TLS_SPKI, 'The HTTPS fixture certificate is required');
const origin = new URL(process.env.QWEN_TEST_ORIGIN);
assert.equal(origin.protocol, 'https:');
assert.equal(origin.hostname, 'pool.example.test');
const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_PATH,
  headless: true,
  args: [`--ignore-certificate-errors-spki-list=${process.env.QWEN_TEST_TLS_SPKI}`, '--host-resolver-rules=MAP pool.example.test 127.0.0.1, MAP * ~NOTFOUND', '--no-proxy-server', ...(process.env.BROWSER_NO_SANDBOX === '1' ? ['--no-sandbox'] : [])],
});
const errors = [];
const requests = [];
let closing = false;
try {
  const protocol = await browser.target().createCDPSession();
  protocol.on('Target.attachedToTarget', async ({ sessionId }) => {
    const session = protocol.connection().session(sessionId);
    try {
      session.on('Fetch.requestPaused', async ({ requestId, request }) => {
        try {
          const target = new URL(request.url);
          if (target.origin === origin.origin) {
            if (target.pathname.includes('/qwen/')) requests.push({ url: request.url, method: request.method });
            await session.send('Fetch.continueRequest', { requestId });
            return;
          }
          let html;
          let responseCode = 200;
          if (target.origin === 'https://signin.alibabacloud.com' && target.pathname === '/oauth2/v1/auth') {
            assert.equal(target.searchParams.get('code_challenge_method'), 'S256');
            assert.ok(target.searchParams.get('code_challenge'));
            const callback = new URL(target.searchParams.get('redirect_uri'));
            assert.equal(callback.href, 'http://127.0.0.1:12345/cli/callback');
            callback.searchParams.set('state', target.searchParams.get('state'));
            callback.searchParams.set('code', 'test-authorization-code');
            html = `<!doctype html><title>Alibaba OAuth fixture</title><button id="authorize-qwen">Authorize</button><script>document.getElementById('authorize-qwen').onclick=()=>location.assign(${JSON.stringify(callback.href)})</script>`;
          } else if (target.origin === 'http://127.0.0.1:12345' && target.pathname === '/cli/callback') {
            responseCode = 503;
            html = '<!doctype html><title>Callback unavailable</title><p id="callback-unavailable">Copy the callback URL from the address bar.</p>';
          } else if (target.pathname === '/favicon.ico') {
            html = '';
          } else {
            errors.push(`Unexpected external request: ${target.origin}${target.pathname}`);
            await session.send('Fetch.failRequest', { requestId, errorReason: 'BlockedByClient' });
            return;
          }
          await session.send('Fetch.fulfillRequest', { requestId, responseCode, responseHeaders: [{ name: 'Content-Type', value: 'text/html' }], body: Buffer.from(html).toString('base64') });
        } catch (error) { if (!closing) errors.push(error.message); }
      });
      await session.send('Fetch.enable', { patterns: [{ urlPattern: '*', requestStage: 'Request' }] });
      await session.send('Runtime.runIfWaitingForDebugger');
    } catch (error) { if (!closing) errors.push(error.message); }
  });
  await protocol.send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: true, flatten: true });
  await browser.defaultBrowserContext().setCookie(
    { name: 'pool_session', value: process.env.QWEN_TEST_SESSION, domain: origin.hostname, path: '/', secure: true, httpOnly: true },
    { name: 'pool_csrf', value: process.env.QWEN_TEST_CSRF, domain: origin.hostname, path: '/', secure: true },
  );
  const page = await browser.newPage();
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error' && message.text().includes('Mixed Content')) errors.push(message.text()); });
  await page.goto(origin.origin + '/?view=mine');
  await page.waitForSelector('.mine-accounts');
  const clickText = async (selector, text) => {
    await page.waitForFunction((selector, text) => [...document.querySelectorAll(selector)].some(element => element.textContent.trim() === text), {}, selector, text);
    await page.evaluate((selector, text) => [...document.querySelectorAll(selector)].find(element => element.textContent.trim() === text).click(), selector, text);
  };
  await clickText('.mine-accounts button', 'Add account');
  await clickText('.contribution-providers button', 'Qwen');
  const popupOpened = new Promise(resolve => page.once('popup', resolve));
  const initialized = page.waitForResponse(response => new URL(response.url()).pathname === '/api/pool/accounts/qwen/login/init');
  await page.click('.oauth-launch');
  const initializedResponse = await initialized;
  assert.equal(initializedResponse.status(), 200, 'HTTPS login initialization must succeed');
  const started = await initializedResponse.json();
  const popup = await popupOpened;
  await popup.waitForSelector('#authorize-qwen');
  await popup.click('#authorize-qwen');
  await popup.waitForSelector('#callback-unavailable');
  const callback = popup.url();
  assert.equal(new URL(callback).searchParams.get('state'), new URL(started.oauth_url).searchParams.get('state'));
  await popup.close();
  const invalid = new URL(callback);
  invalid.searchParams.set('state', 'another-session');
  const fillCallback = async value => {
    await page.click('.contribution-oauth input');
    await page.keyboard.down('Control');
    await page.keyboard.press('KeyA');
    await page.keyboard.up('Control');
    await page.keyboard.type(value);
  };
  await fillCallback(invalid.href);
  await page.click('.contribution-oauth .gold-button');
  await page.waitForFunction(() => document.querySelector('.contribution-dialog [role=alert]')?.textContent.includes('does not match'));
  assert.equal(await page.$eval('.contribution-oauth input', input => input.value), invalid.href);
  await fillCallback(callback);
  assert.equal(await page.$eval('.contribution-oauth input', input => input.value), callback);
  const authorized = page.waitForResponse(response => new URL(response.url()).pathname === '/api/pool/accounts/qwen/login/authorize');
  await page.click('.contribution-oauth .gold-button');
  const authorizationResponse = await authorized;
  assert.equal(JSON.parse(authorizationResponse.request().postData()).callback_url, callback);
  const authorization = await authorizationResponse.json();
  assert.equal(authorizationResponse.status(), 200, JSON.stringify(authorization));
  assert.equal(authorization.status, 'awaiting_key', JSON.stringify(authorization));
  await page.waitForSelector('.contribution-oauth input[type=password]');
  await page.type('.contribution-oauth input[type=password]', 'sk-sp-browser-test-key');
  await page.click('.contribution-oauth .gold-button');
  await page.waitForFunction(() => !document.querySelector('.contribution-dialog') && document.querySelector('.mine-accounts .client-card strong')?.textContent === 'Qwen');
  await clickText('button', 'Manage Token Plan credits');
  await page.waitForFunction(() => document.querySelector('.mine-accounts')?.textContent.includes('25.0%'));
  assert.ok(await page.$eval('.mine-accounts', element => element.textContent.includes('Automatic balance')));
  const refreshed = page.waitForResponse(response => new URL(response.url()).pathname.endsWith('/token-plan') && response.request().method() === 'POST');
  await clickText('button', 'Refresh Cloud balance');
  const response = await refreshed;
  assert.equal(response.status(), 200);
  const view = await response.json();
  assert.equal(view.credits.remaining, 33750);
  assert.equal(view.token_plan.cloud_oauth, undefined);
  assert.ok(requests.some(request => request.url.endsWith('/login/authorize')));
  assert.ok(requests.some(request => request.url.endsWith('/login/complete')));
  assert.ok(requests.every(request => new URL(request.url).protocol === 'https:'), 'Account credentials must only be sent to the pool over HTTPS');
  assert.deepEqual(errors, []);
  console.log('PASS Qwen authorization, private account persistence and automatic credits through HTTPS with the real backend');
} finally {
  closing = true;
  await browser.close();
}
