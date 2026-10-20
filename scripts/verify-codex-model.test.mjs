import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { responseJSON, checkStreaming, checkTools, checkCodex } from './verify-codex-model.mjs';

const model = 'fixture-model';
const message = text => ({ type: 'message', content: [{ type: 'output_text', text }] });
const completed = output => ({ model, status: 'completed', output });
async function withServer(handler, run) {
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    try { await handler(JSON.parse(body), res); } catch { res.writeHead(500).end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try { await run(`http://127.0.0.1:${server.address().port}/v1`); }
  finally { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
}
const json = (res, body) => { res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); };
const event = value => `data: ${JSON.stringify(value)}\n\n`;

test('JSON requires the requested model, terminal success and JSON content type', async () => {
  for (const response of [{ ...completed([]), model: 'other' }, { ...completed([]), status: 'incomplete' }, { ...completed([]), error: { code: 'failed' } }]) {
    await withServer((_, res) => json(res, response), base => assert.rejects(responseJSON(base, 'synthetic', { model }), /unexpected_response/));
  }
  await withServer((_, res) => res.end('{}'), base => assert.rejects(responseJSON(base, 'synthetic', { model }), /request_failed/));
});

test('streaming requires matching incremental text and a complete terminal response', async () => {
  const delta = event({ type: 'response.output_text.delta', delta: 'nonce' });
  const terminal = event({ type: 'response.completed', response: completed([message('nonce')]) });
  for (const stream of [delta, terminal, delta + event({ type: 'response.failed' }), delta + terminal + delta,
    delta + event({ type: 'response.completed', response: { ...completed([message('nonce')]), model: 'other' } }),
    delta + event({ type: 'response.completed', response: completed([message('different')]) }), delta + terminal + 'data: {',
    'data: [DONE]\n\n' + delta + terminal]) {
    await withServer((_, res) => { res.writeHead(200, { 'content-type': 'text/event-stream' }); res.end(stream); },
      base => assert.rejects(checkStreaming(base, 'synthetic', model, 'nonce'), /unexpected_response/));
  }
  await withServer((_, res) => { res.writeHead(200, { 'content-type': 'text/event-stream' }); res.end(delta + terminal + 'data: [DONE]\n\n'); },
    async base => assert.equal((await checkStreaming(base, 'synthetic', model, 'nonce')).status, 'passed'));
});

test('tools must round-trip the actual call ID and unpredictable function output', async () => {
  let requests = 0;
  await withServer((body, res) => {
    requests++;
    if (requests === 1) return json(res, completed([{ type: 'function_call', name: 'lookup_probe', call_id: 'probe-call', arguments: '{"query":"probe"}' }]));
    const output = body.input.at(-1);
    assert.equal(output.call_id, 'probe-call');
    assert.equal(output.type, 'function_call_output');
    json(res, completed([message(output.output)]));
  }, async base => assert.equal((await checkTools(base, 'synthetic', model)).status, 'passed'));
  assert.equal(requests, 2);
  for (const first of [message('tool_round_trip'), { type: 'function_call', name: 'wrong', call_id: 'x', arguments: '{}' }]) {
    await withServer((_, res) => json(res, completed([first])), base => assert.rejects(checkTools(base, 'synthetic', model), /unexpected_response/));
  }
  let calls = 0;
  await withServer((_, res) => json(res, completed(++calls === 1 ? [{ type: 'function_call', name: 'lookup_probe', call_id: 'x', arguments: '{"query":"probe"}' }] : [message('not-the-tool-output')])),
    base => assert.rejects(checkTools(base, 'synthetic', model), /unexpected_response/));
});

test('Codex editing requires a completed tool event and exact file contents', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'codex-verifier-test-'));
  try {
    for (const scenario of ['passed', 'no_tool', 'wrong_edit', 'failed_turn']) {
      const cli = join(directory, `${scenario}.js`);
      await writeFile(cli, `
        const fs = require('node:fs'), path = require('node:path');
        if (process.argv.includes('--version')) { console.log('codex-cli 0.159.1'); process.exit(0); }
        const dir = process.argv[process.argv.indexOf('-C') + 1];
        const original = fs.readFileSync(path.join(dir, 'probe.txt'), 'utf8');
        fs.writeFileSync(path.join(dir, 'verified.txt'), ${JSON.stringify(scenario)} === 'wrong_edit' ? 'wrong' : original + 'CODEX_EDIT_OK\\n');
        if (${JSON.stringify(scenario)} !== 'no_tool') console.log(JSON.stringify({ type:'item.completed', item:{type:'command_execution'} }));
        console.log(JSON.stringify({ type: ${JSON.stringify(scenario)} === 'failed_turn' ? 'turn.failed' : 'turn.completed' }));
      `);
      const result = await checkCodex('http://127.0.0.1/v1', model, cli);
      assert.equal(result.check.status, scenario === 'passed' ? 'passed' : 'failed', scenario);
    }
  } finally { await rm(directory, { recursive: true }); }
});
