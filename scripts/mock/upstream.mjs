// Mock OpenAI-compatible upstream for the local validation stack.
// Serves /v1/models, /v1/chat/completions and /v1/responses with plausible
// usage numbers, plus an opt-in failure mode so the pool's error panels and
// account health paths have something real to report.
//
//   node scripts/mock/upstream.mjs [--port 8991] [--bind 127.0.0.1] [--model mock-model]
//
// Failure mode: send "x-mock-fail: 429" (or 500), or request a model whose id
// contains "fail". Everything else answers 200.

import http from 'node:http';
import { parseArgs } from 'node:util';

const REPLY = 'Mock reply from the local upstream.';

function tokens(body) {
  const prompt = JSON.stringify(body?.messages ?? body?.input ?? '').length;
  const promptTokens = 120 + (prompt % 41);
  const completionTokens = 60 + (REPLY.length % 41);
  return { promptTokens, completionTokens };
}

function failure(req, body) {
  const header = String(req.headers['x-mock-fail'] ?? '');
  const model = String(body?.model ?? '');
  if (!header && !/fail/i.test(model)) return null;
  const code = /500/.test(header) || /500/.test(model) ? 500 : 429;
  return { status: code, body: { error: { message: `mock upstream failure (${code})`, type: 'mock_error', code } } };
}

export function createUpstream({ model = 'mock-model' } = {}) {
  return http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const raw = Buffer.concat(chunks).toString('utf8');
    let body = null;
    if (raw) {
      try { body = JSON.parse(raw); } catch { body = null; }
    }
    const send = (status, payload) => {
      const text = JSON.stringify(payload);
      res.writeHead(status, { 'content-type': 'application/json', 'content-length': Buffer.byteLength(text) });
      res.end(text);
    };
    const url = (req.url ?? '').split('?')[0];

    if (url === '/v1/models' || url === '/models') {
      return send(200, { object: 'list', data: [{ id: model, object: 'model', created: 1700000000, owned_by: 'mock' }] });
    }

    if (req.method !== 'POST') return send(404, { error: { message: 'not found', type: 'mock_error' } });

    const failed = failure(req, body);
    if (failed) return send(failed.status, failed.body);

    const { promptTokens, completionTokens } = tokens(body);
    const now = Math.floor(Date.now() / 1000);
    const served = body?.model ?? model;

    if (url === '/v1/chat/completions') {
      return send(200, {
        id: `chatcmpl-mock-${Date.now()}`,
        object: 'chat.completion',
        created: now,
        model: served,
        choices: [{ index: 0, message: { role: 'assistant', content: REPLY }, finish_reason: 'stop' }],
        usage: { prompt_tokens: promptTokens, completion_tokens: completionTokens, total_tokens: promptTokens + completionTokens },
      });
    }

    if (url === '/v1/responses') {
      return send(200, {
        id: `resp-mock-${Date.now()}`,
        object: 'response',
        created_at: now,
        model: served,
        status: 'completed',
        output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: REPLY }] }],
        usage: { input_tokens: promptTokens, output_tokens: completionTokens, total_tokens: promptTokens + completionTokens },
      });
    }

    return send(404, { error: { message: 'not found', type: 'mock_error' } });
  });
}

export function listen(server, port, bind) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, bind, () => {
      server.removeListener('error', reject);
      resolve(server.address());
    });
  });
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const { values } = parseArgs({
    options: {
      port: { type: 'string', default: '8991' },
      bind: { type: 'string', default: '127.0.0.1' },
      model: { type: 'string', default: 'mock-model' },
      help: { type: 'boolean', default: false },
    },
  });
  if (values.help) {
    console.log('Usage: node scripts/mock/upstream.mjs [--port N] [--bind IP] [--model ID]');
    process.exit(0);
  }
  const address = await listen(createUpstream({ model: values.model }), Number(values.port), values.bind);
  console.log(`mock upstream listening on ${address.address}:${address.port} (model ${values.model})`);
}
