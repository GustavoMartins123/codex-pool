// Conversation driver for the local mock stack: signs prompts and approval
// decisions exactly like the mobile app, using the command key the runner
// produced at pairing (the pool never sees it).
//
//   node scripts/mock/turn.mjs seed
//       Seed operator conversations (approve / deny / cancel / closed), a
//       member conversation, and one live pending approval for the web UI.
//   node scripts/mock/turn.mjs turn --who operator --action approve \
//     --input "Run the tests." [--session SESSION_ID]
//       Actions: hold (leave pending), approve, deny, cancel.
//
// Requires a paired runner: tmp/mock-stack/runner-state.json.

import fs from 'node:fs';
import { parseArgs } from 'node:util';
import {
  api, appToken, openEvents, readJSON, readSecret, signApproval, signTurn,
  stackContext, waitFor, writeJSON, writeSecret,
} from './lib.mjs';

const { values: flags, positionals } = parseArgs({
  allowPositionals: true,
  options: {
    who: { type: 'string', default: 'operator' },
    action: { type: 'string', default: 'hold' },
    input: { type: 'string', default: 'List the files in this workspace.' },
    session: { type: 'string' },
    help: { type: 'boolean', default: false },
  },
});

if (flags.help) {
  console.log('Usage: node scripts/mock/turn.mjs seed | turn [--who operator|member] [--action hold|approve|deny|cancel] [--input TEXT] [--session ID]');
  process.exit(0);
}

const ctx = stackContext();
const command = positionals[0] ?? 'seed';

async function tokenFor(who) {
  const file = ctx.file(`${who}-app-token.txt`);
  const email = ctx.credentials?.[who]?.email;
  if (!email) throw new Error(`${who} is not provisioned; run setup.mjs first`);
  if (fs.existsSync(file)) {
    const cached = readSecret(file);
    const probe = await api(ctx.origin, 'GET', '/api/me/runner-catalog', { bearer: cached, ca: ctx.ca, expect: [200, 401, 403] });
    if (probe.status === 200) return cached;
  }
  const token = await appToken(ctx.origin, email, readSecret(ctx.file(`${who}-password.txt`)), { ca: ctx.ca });
  writeSecret(file, token);
  return token;
}

function runnerKey(who) {
  const file = ctx.file(who === 'member' ? 'runner-member-state.json' : 'runner-state.json');
  const state = readJSON(file);
  if (!state?.command_key) throw new Error(`no paired runner state at ${file}; pair a runner first`);
  const key = Buffer.from(state.command_key, 'base64url');
  if (key.length !== 32) throw new Error('command key must be 32 bytes');
  return { key, runnerID: state.runner_id };
}

const lastIssued = new Map();
function nextIssuedAt(id) {
  const value = Math.max((lastIssued.get(id) ?? 0) + 1, Date.now());
  lastIssued.set(id, value);
  return value;
}

function driver(token, key) {
  const call = (method, target, body, expect) => api(ctx.origin, method, target, { body, bearer: token, ca: ctx.ca, expect });
  return {
    call,
    session: async (id) => (await call('GET', `/api/sessions/${id}`)).json,
    settle: (id, predicate, label) => waitFor(async () => {
      const state = await call('GET', `/api/sessions/${id}`);
      return predicate(state.json) ? state.json : null;
    }, { label }),
    events: (conversationID) => openEvents(ctx.origin, `/api/conversations/${conversationID}/events`, { bearer: token, ca: ctx.ca }),
    async sendTurn(session, input) {
      const issuedAt = nextIssuedAt(session.id);
      const signed = signTurn(key, {
        sessionID: session.id, issuedAt, agent: session.agent,
        permissionMode: session.permission_mode, workspace: session.workspace, input,
      });
      const res = await call('POST', `/api/sessions/${session.id}/turns`, { input, ...signed }, [200, 202]);
      return res.json?.turn_id;
    },
    async decide(session, approval, decision) {
      const body = { decision };
      if (decision === 'approve') {
        Object.assign(body, signApproval(key, {
          sessionID: session.id, approvalID: approval.approval_id, decision,
          issuedAt: nextIssuedAt(session.id), command: approval.command,
        }));
      }
      await call('POST', `/api/sessions/${session.id}/approvals/${approval.approval_id}`, body, [204]);
    },
    cancel: (id) => call('POST', `/api/sessions/${id}/turns:cancel`, undefined, [204]),
  };
}

// runTurn sends one prompt and applies the action once the runner asks for
// approval. Returns the outcome so callers can assert on it.
async function runTurn(op, session, input, action, stream) {
  const turnID = await op.sendTurn(session, input);
  const deadline = Date.now() + 30000;
  let approval = null;
  let finished = null;
  while (!approval && !finished) {
    const event = await stream.next(Math.max(1, deadline - Date.now()));
    if (!event) throw new Error(`turn ${turnID} produced neither an approval nor a finish`);
    if (event.json?.turn_id !== turnID) continue;
    if (event.type === 'approval.requested') approval = event.json;
    if (event.type === 'turn.finished') finished = event.json;
  }
  if (approval) {
    if (action === 'hold') return { turnID, action: 'left-pending', approval };
    if (action === 'none') throw new Error(`turn ${turnID} asked for approval but the action is none`);
    if (action === 'cancel') await op.cancel(session.id);
    else await op.decide(session, approval, action === 'approve' ? 'approve' : 'deny');
  }
  if (!finished) await stream.until('turn.finished', 30000);
  await op.settle(session.id, (state) => state.state === 'waiting_input', `${session.id} back to waiting_input`);
  return { turnID, action: approval ? action : 'no-approval' };
}

async function seedConversation(op, label, turns, { close = false } = {}) {
  const created = await op.call('POST', '/api/sessions', {
    agent: 'codex', model: 'gpt-5.6-sol', reasoning_effort: '',
    workspace: { mode: 'ephemeral' }, permission_mode: 'gated', runner_id: runnerKey('operator').runnerID,
  }, [201]);
  const id = created.json.id;
  const session = await op.settle(id, (state) => state.state === 'waiting_input', `${label} runner accept`);
  const stream = op.events(session.conversation_id);
  const record = { label, id, conversation_id: session.conversation_id, actions: [] };
  try {
    for (const turn of turns) {
      const outcome = await runTurn(op, session, turn.input, turn.action, stream);
      record.actions.push(outcome.action);
      if (outcome.action === 'left-pending') {
        record.state = 'waiting_approval';
        record.pending_approval = outcome.approval;
        return record;
      }
    }
    record.state = (await op.session(id)).state;
    if (close) {
      await op.call('DELETE', `/api/sessions/${id}`, undefined, [200, 202, 204]);
      const closed = await op.settle(id, (state) => state.state === 'closed', `${label} closed`);
      record.state = closed.state;
      record.closed_reason = closed.closed_reason;
    }
    return record;
  } finally {
    stream.close();
  }
}

async function seed() {
  const { key } = runnerKey('operator');
  const token = await tokenFor('operator');
  const op = driver(token, key);

  const active = await op.call('GET', '/api/conversations?storage=active&limit=50');
  for (const conversation of active.json?.conversations ?? []) {
    if (conversation.session_state !== 'closed') {
      await op.call('DELETE', `/api/sessions/${conversation.session_id}`, undefined, [200, 202, 204, 404, 409]);
    }
  }

  const results = [];
  results.push(await seedConversation(op, 'operator-active-1', [
    { input: 'List the files in this workspace and summarize them.', action: 'approve' },
    { input: 'report-settings', action: 'none' },
  ]));
  results.push(await seedConversation(op, 'operator-active-2', [
    { input: 'Run the deploy script.', action: 'deny' },
    { input: 'Refactor the whole repository.', action: 'cancel' },
  ]));
  results.push(await seedConversation(op, 'operator-closed', [
    { input: 'Say hello, then we are done.', action: 'approve' },
  ], { close: true }));

  const memberToken = await tokenFor('member');
  const member = driver(memberToken, key);
  const memberSession = await member.call('POST', '/api/sessions', {
    agent: 'codex', model: 'mock/mock-model', reasoning_effort: '',
    workspace: { mode: 'ephemeral' }, permission_mode: 'gated',
  }, [201]);
  const memberList = await member.call('GET', '/api/conversations?storage=active&limit=20');
  const cross = await member.call('GET', `/api/conversations/${results[0].conversation_id}`, undefined, [200, 403, 404]);
  const memberRecord = {
    id: memberSession.json.id,
    state: memberSession.json.state,
    sees_only_own: (memberList.json?.conversations ?? []).every((c) => c.principal_id === ctx.credentials.member.principal),
    cross_principal_get_status: cross.status,
  };

  results.push(await seedConversation(op, 'operator-live-pending', [
    { input: 'Delete the build directory.', action: 'hold' },
  ]));

  writeJSON(ctx.file('conversations-result.json'), { results, member: memberRecord });
  for (const record of results) console.log(`[${record.label}] ${record.id} state=${record.state} actions=${record.actions.join(',')}`);
  console.log(`[member] ${memberRecord.id} own-only=${memberRecord.sees_only_own} cross-get=${memberRecord.cross_principal_get_status}`);
  console.log('SEED ok');
}

async function turn() {
  const who = flags.who;
  const { key } = runnerKey(who);
  const op = driver(await tokenFor(who), key);
  let id = flags.session;
  if (!id) {
    const active = await op.call('GET', '/api/conversations?storage=active&limit=50');
    const candidate = (active.json?.conversations ?? []).find((c) => c.session_state === 'waiting_input');
    if (!candidate) throw new Error('no conversation is waiting for input; pass --session or run seed');
    id = candidate.session_id;
  }
  const session = await op.session(id);
  const stream = op.events(session.conversation_id);
  try {
    const outcome = await runTurn(op, session, flags.input, flags.action, stream);
    console.log(`[${who}] session=${id} turn=${outcome.turnID} action=${outcome.action}`);
    if (outcome.approval) console.log(`pending approval ${outcome.approval.approval_id} expires ${outcome.approval.expires_at}`);
  } finally {
    stream.close();
  }
}

if (command === 'seed') await seed();
else if (command === 'turn') await turn();
else throw new Error(`unknown command ${command}; expected seed or turn`);
