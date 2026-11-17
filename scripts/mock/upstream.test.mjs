import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createUpstream, listen } from './upstream.mjs';

let server;
let origin;

before(async () => {
  server = createUpstream({ model: 'mock-model' });
  const address = await listen(server, 0, '127.0.0.1');
  origin = `http://127.0.0.1:${address.port}`;
});

after(() => new Promise((resolve) => server.close(resolve)));

const post = async (path, body, headers = {}) => {
  const res = await fetch(origin + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...headers },
    body: JSON.stringify(body),
  });
  return { status: res.status, json: await res.json() };
};

test('models route lists the configured model', async () => {
  for (const path of ['/v1/models', '/models']) {
    const res = await fetch(origin + path);
    assert.equal(res.status, 200);
    const body = await res.json();
    assert.equal(body.object, 'list');
    assert.deepEqual(body.data.map((m) => m.id), ['mock-model']);
  }
});

test('chat completions return a reply and consistent usage', async () => {
  const { status, json } = await post('/v1/chat/completions', { model: 'mock-model', messages: [{ role: 'user', content: 'hi' }] });
  assert.equal(status, 200);
  assert.equal(json.object, 'chat.completion');
  assert.equal(json.choices[0].finish_reason, 'stop');
  assert.ok(json.choices[0].message.content.length > 0);
  assert.ok(json.usage.prompt_tokens > 0);
  assert.ok(json.usage.completion_tokens > 0);
  assert.equal(json.usage.total_tokens, json.usage.prompt_tokens + json.usage.completion_tokens);
});

test('responses route returns output_text and input/output usage', async () => {
  const { status, json } = await post('/v1/responses', { model: 'mock-model', input: 'hi' });
  assert.equal(status, 200);
  assert.equal(json.status, 'completed');
  assert.equal(json.output[0].content[0].type, 'output_text');
  assert.ok(json.output[0].content[0].text.length > 0);
  assert.equal(json.usage.total_tokens, json.usage.input_tokens + json.usage.output_tokens);
});

test('x-mock-fail header selects the failure status', async () => {
  for (const [header, expected] of [['429', 429], ['500', 500]]) {
    const { status, json } = await post('/v1/chat/completions', { model: 'mock-model' }, { 'x-mock-fail': header });
    assert.equal(status, expected);
    assert.equal(json.error.type, 'mock_error');
    assert.equal(json.error.code, expected);
  }
});

test('a fail model triggers the failure mode without the header', async () => {
  const { status } = await post('/v1/responses', { model: 'mock-model-fail-500', input: 'hi' });
  assert.equal(status, 500);
});

test('unknown paths answer 404', async () => {
  const { status } = await post('/v1/nope', {});
  assert.equal(status, 404);
});
