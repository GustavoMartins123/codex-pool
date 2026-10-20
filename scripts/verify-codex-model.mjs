import { readFile, writeFile, mkdtemp, rm, realpath } from 'node:fs/promises';
import { homedir, tmpdir } from 'node:os';
import { join, resolve, sep } from 'node:path';
import { spawn } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { setGlobalProxyFromEnv } from 'node:http';

const suite = 'codex-pool/1';
const pass = code => ({ status: 'passed', code });
const fail = code => ({ status: 'failed', code });
const notRun = () => ({ status: 'not_tested', code: 'not_run' });
const textOf = response => (response.output ?? []).filter(item => item.type === 'message').flatMap(item => item.content ?? []).filter(item => item.type === 'output_text').map(item => item.text).join('');

export async function responseJSON(base, token, body) {
  const response = await fetch(`${base}/responses`, { method: 'POST', redirect: 'error', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'User-Agent': 'Codex-Pool-Verification/1.0' }, body: JSON.stringify(body), signal: AbortSignal.timeout(60_000) });
  if (!response.ok || !response.headers.get('content-type')?.startsWith('application/json')) throw new Error('request_failed');
  const raw = await response.text();
  if (raw.length > 1_000_000) throw new Error('unexpected_response');
  const result = JSON.parse(raw);
  if (result.model !== body.model || result.status !== 'completed' || result.error) throw new Error('unexpected_response');
  return result;
}

export async function checkStreaming(base, token, model, nonce) {
  const response = await fetch(`${base}/responses`, { method: 'POST', redirect: 'error', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'User-Agent': 'Codex-Pool-Verification/1.0' }, body: JSON.stringify({ model, input: `Reply with exactly ${nonce}`, max_output_tokens: 128, stream: true }), signal: AbortSignal.timeout(60_000) });
  if (!response.ok || !response.headers.get('content-type')?.startsWith('text/event-stream')) throw new Error('request_failed');
  let pending = '', text = '', deltas = 0, completed = false, bytes = 0, done = false;
  const decoder = new TextDecoder('utf-8', { fatal: true });
  for await (const chunk of response.body) {
    bytes += chunk.length;
    if (bytes > 1_000_000) throw new Error('unexpected_response');
    pending += decoder.decode(chunk, { stream: true }).replace(/\r/g, '');
    let boundary;
    while ((boundary = pending.indexOf('\n\n')) !== -1) {
      const block = pending.slice(0, boundary); pending = pending.slice(boundary + 2);
      const data = block.split('\n').filter(line => line.startsWith('data:')).map(line => line.slice(5).trimStart()).join('\n');
      if (!data) continue;
      if (done) throw new Error('unexpected_response');
      if (data === '[DONE]') { if (!completed) throw new Error('unexpected_response'); done = true; continue; }
      if (completed) throw new Error('unexpected_response');
      const event = JSON.parse(data);
      if (['error', 'response.failed', 'response.incomplete'].includes(event.type)) throw new Error('unexpected_response');
      if (event.type === 'response.output_text.delta') {
        if (completed || typeof event.delta !== 'string') throw new Error('unexpected_response');
        text += event.delta; deltas++;
      }
      if (event.type === 'response.completed') {
        if (event.response?.status !== 'completed' || event.response?.model !== model || event.response?.error || textOf(event.response).trim() !== nonce) throw new Error('unexpected_response');
        completed = true;
      }
    }
  }
  pending += decoder.decode();
  if (pending.trim() || !completed || deltas < 1 || text.trim() !== nonce) throw new Error('unexpected_response');
  return pass('completed_stream');
}

export async function checkTools(base, token, model) {
  const tools = [{ type: 'function', name: 'lookup_probe', description: 'Get the synthetic verification value.', strict: true, parameters: { type: 'object', properties: { query: { type: 'string', enum: ['probe'] } }, required: ['query'], additionalProperties: false } }];
  const input = [{ role: 'user', content: 'Call lookup_probe with query probe, then reply with exactly the value returned by the tool.' }];
  const first = await responseJSON(base, token, { model, input, tools, tool_choice: { type: 'function', name: 'lookup_probe' }, max_output_tokens: 256, stream: false });
  const calls = first.output.filter(item => item.type === 'function_call');
  if (calls.length !== 1 || calls[0].name !== 'lookup_probe' || typeof calls[0].call_id !== 'string' || !calls[0].call_id || JSON.parse(calls[0].arguments).query !== 'probe') throw new Error('unexpected_response');
  const value = randomUUID();
  const second = await responseJSON(base, token, { model, input: [...input, ...first.output, { type: 'function_call_output', call_id: calls[0].call_id, output: value }], tools, tool_choice: 'none', max_output_tokens: 128, stream: false });
  if (textOf(second).trim() !== value || second.output.some(item => item.type === 'function_call')) throw new Error('unexpected_response');
  return pass('tool_round_trip');
}

function runProcess(command, args, options = {}) {
  return new Promise((done, reject) => {
    const child = spawn(command, args, { windowsHide: true, detached: process.platform !== 'win32', stdio: ['ignore', 'pipe', 'pipe'], ...options });
    let stdout = '', stderr = '', size = 0, timedOut = false, overflow = false;
    let terminating = false;
    const terminate = () => {
      if (terminating || !Number.isInteger(child.pid)) return;
      terminating = true;
      if (process.platform === 'win32') {
        const killer = spawn(join(process.env.SystemRoot, 'System32', 'taskkill.exe'), ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, stdio: 'ignore' });
        killer.once('error', () => reject(new Error('process_termination_failed')));
      } else { try { process.kill(-child.pid, 'SIGKILL'); } catch { reject(new Error('process_termination_failed')); } }
    };
    const timer = setTimeout(() => { timedOut = true; terminate(); }, 180_000);
    child.stdout.on('data', chunk => { size += chunk.length; if (size > 2_000_000) { overflow = true; terminate(); } else stdout += chunk; });
    // Private CLI diagnostics are deliberately not persisted or printed.
    child.stderr.on('data', chunk => { if (stderr.length < 100_000) stderr += chunk; });
    child.once('error', error => { clearTimeout(timer); reject(error); });
    child.once('close', code => {
      clearTimeout(timer);
      done({ code, stdout, timedOut, overflow });
    });
  });
}

export async function checkCodex(base, model, codexPath) {
  const command = codexPath.endsWith('.js') ? process.execPath : codexPath;
  const prefix = codexPath.endsWith('.js') ? [codexPath] : [];
  const version = await runProcess(command, [...prefix, '--version']);
  const codexVersion = version.stdout.trim();
  if (version.code !== 0 || !/^codex-cli [a-zA-Z0-9 ._+()/-]{1,65}$/.test(codexVersion)) return { check: notRun(), version: 'unavailable' };
  const root = await realpath(tmpdir());
  const directory = await mkdtemp(join(root, 'codex-pool-verify-'));
  const nonce = randomUUID();
  let cleanupAllowed = true;
  try {
    await writeFile(join(directory, 'probe.txt'), nonce + '\n', { flag: 'wx' });
    const provider = [
      '-c', 'model_provider="codex-pool"',
      '-c', 'model_providers.codex-pool.name="Codex pool"',
      '-c', `model_providers.codex-pool.base_url=${JSON.stringify(base)}`,
      '-c', 'model_providers.codex-pool.wire_api="responses"',
      '-c', 'model_providers.codex-pool.requires_openai_auth=true',
      '-c', 'model_providers.codex-pool.request_max_retries=0',
      '-c', 'model_providers.codex-pool.stream_max_retries=0',
    ];
    const result = await runProcess(command, [...prefix, 'exec', '--ignore-user-config', '--ephemeral', '--skip-git-repo-check', '--json', '-C', directory, '-s', 'workspace-write', '-m', model, ...provider,
      'This is a synthetic compatibility check in a disposable directory. Read probe.txt using a tool. Create verified.txt containing the exact unchanged contents of probe.txt followed by CODEX_EDIT_OK and a newline. Do not modify probe.txt. Do not access other files, network, or run unrelated commands. Reply DONE after writing the file.']);
    if (result.timedOut) return { check: fail('timeout'), version: codexVersion };
    if (result.code !== 0 || result.overflow) return { check: fail('client_failed'), version: codexVersion };
    const events = result.stdout.trim().split(/\r?\n/).map(line => JSON.parse(line));
    const toolUsed = events.some(event => event.type === 'item.completed' && ['command_execution', 'file_change'].includes(event.item?.type));
    const complete = events.some(event => event.type === 'turn.completed');
    const failed = events.some(event => ['error', 'turn.failed'].includes(event.type));
    const original = await readFile(join(directory, 'probe.txt'), 'utf8');
    const edited = await readFile(join(directory, 'verified.txt'), 'utf8');
    return { check: toolUsed && complete && !failed && original === nonce + '\n' && edited === nonce + '\nCODEX_EDIT_OK\n' ? pass('file_edit_verified') : fail('unexpected_response'), version: codexVersion };
  } catch (error) { cleanupAllowed = error.message !== 'process_termination_failed'; return { check: fail('client_failed'), version: codexVersion }; }
  finally {
    if (cleanupAllowed) {
      const resolved = await realpath(directory);
      if (!resolved.startsWith(root + sep) || !resolved.slice(root.length + 1).startsWith('codex-pool-verify-')) throw new Error('unsafe_cleanup');
      await rm(resolved, { recursive: true, force: true });
    }
  }
}

export async function main(argv) {
  setGlobalProxyFromEnv();
  const args = {};
  for (let i = 0; i < argv.length; i += 2) {
    if (!['--model', '--provider', '--output', '--codex-path'].includes(argv[i]) || !argv[i + 1] || args[argv[i]]) throw new Error('invalid_arguments');
    args[argv[i]] = argv[i + 1];
  }
  const model = args['--model'], provider = args['--provider'];
  if (!model || !provider || !args['--output'] || !/^[a-zA-Z0-9_.:/-]{1,200}$/.test(model) || !/^[a-z_]{1,40}$/.test(provider)) throw new Error('invalid_arguments');
  const config = await readFile(join(homedir(), '.codex', 'config.toml'), 'utf8');
  const section = config.match(/^\[model_providers\.codex-pool\]\s*\r?\n([\s\S]*?)(?=^\[|(?![\s\S]))/m)?.[1];
  const base = section?.match(/^\s*base_url\s*=\s*"([^"\r\n]+)"\s*$/m)?.[1]?.replace(/\/$/, '');
  if (!base) throw new Error('pool_configuration_unavailable');
  const url = new URL(base);
  if (url.username || url.password || url.search || url.hash || url.pathname !== '/v1' || url.protocol !== 'https:' && !(url.protocol === 'http:' && ['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname))) throw new Error('invalid_pool_url');
  const token = JSON.parse(await readFile(join(homedir(), '.codex', 'auth.json'), 'utf8')).OPENAI_API_KEY;
  if (typeof token !== 'string' || !token.trim()) throw new Error('credential_unavailable');
  const targetResponse = await fetch(`${url.origin}/api/verification/target`, { headers: { Authorization: `Bearer ${token}`, 'User-Agent': 'Codex-Pool-Verification/1.0' }, redirect: 'error', signal: AbortSignal.timeout(15_000) });
  if (!targetResponse.ok) throw new Error('verification_target_unavailable');
  const target = await targetResponse.json();
  if (target.suite !== suite || !/^[a-f0-9]{64}$/.test(target.runtime)) throw new Error('invalid_verification_target');
  const result = { suite, model, provider, runtime: target.runtime, codex_version: 'unavailable', started_at: new Date().toISOString(), finished_at: '', checks: { text: notRun(), streaming: notRun(), tools: notRun(), codex_edit: notRun() }, expired: false };
  for (const name of ['text', 'streaming', 'tools']) {
    try {
      const nonce = randomUUID();
      if (name === 'text') {
        const response = await responseJSON(base, token, { model, input: `Reply with exactly ${nonce}`, max_output_tokens: 128, stream: false });
        if (textOf(response).trim() !== nonce) throw new Error('unexpected_response');
        result.checks.text = pass('exact_response');
      } else result.checks[name] = name === 'streaming' ? await checkStreaming(base, token, model, nonce) : await checkTools(base, token, model);
    } catch (error) { result.checks[name] = fail(error.name === 'TimeoutError' ? 'timeout' : error.message === 'unexpected_response' ? 'unexpected_response' : 'request_failed'); }
  }
  const codexPath = args['--codex-path'] ?? (process.platform === 'win32' ? join(process.env.APPDATA, 'npm', 'node_modules', '@openai', 'codex', 'bin', 'codex.js') : 'codex');
  try { const codex = await checkCodex(base, model, codexPath); result.checks.codex_edit = codex.check; result.codex_version = codex.version; }
  catch { result.checks.codex_edit = notRun(); }
  result.finished_at = new Date().toISOString();
  await writeFile(resolve(args['--output']), JSON.stringify(result, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
  for (const [name, check] of Object.entries(result.checks)) process.stdout.write(`${name}: ${check.status} (${check.code})\n`);
  if (Object.values(result.checks).some(check => check.status !== 'passed')) process.exitCode = 1;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).catch(() => { process.stderr.write('Verification could not run. No retry or model substitution was attempted; private details suppressed.\n'); process.exitCode = 1; });
}
