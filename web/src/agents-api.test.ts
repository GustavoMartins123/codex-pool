// @vitest-environment jsdom
import { beforeEach, afterEach, expect, it, vi } from "vitest";
import {
  AgentsApiError, cancelTurn, closeSession, conversationHistory, decidePairing, deleteConversation,
  denyApproval, getConversation, getSession, listConversations, listPairings, listRunners, revokeRunner,
} from "./agents-api";

const runnerFixture = { id: "r1", name: "desk", online: true, os: "linux", arch: "amd64", revoked: false, created_at: "2026-11-01T00:00:00Z" };
const pairingFixture = { id: "p1", name: "laptop", os: "linux", arch: "amd64", expires_at: "2026-11-01T00:05:00Z" };
const conversationFixture = {
  id: "c1", principal_id: "u1", session_id: "s1", agent: "codex", model: "gpt-5.6-sol", title: "Fix build",
  storage: "active", session_state: "running", created_at: "2026-11-01T00:00:00Z", last_interaction_at: null,
  order_at: "2026-11-01T00:00:00Z", archive_after: "2026-12-01T00:00:00Z", event_cursor: 4, generation: 1,
  history_complete: true, resume_available: false, archived_through: 0, deleting: false,
};
const sessionFixture = {
  id: "s1", conversation_id: "c1", principal_id: "u1", client_id: "cl1", agent: "codex", model: "gpt-5.6-sol",
  reasoning_effort: "medium", workspace: { mode: "ephemeral" }, permission_mode: "gated", state: "running",
  lease_expires_at: "2026-11-01T01:00:00Z", created_at: "2026-11-01T00:00:00Z", updated_at: "2026-11-01T00:00:00Z",
  turns: 1, approvals: [], turn_summaries: [], resume_requested: false,
};
const historyFixture = {
  conversation: conversationFixture, session: sessionFixture,
  events: [{ id: 4, type: "turn.delta", data: { turn_id: "t1", kind: "text", content: "hi" } }],
  before: 4, through: 4, has_older: true,
};

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn(async () => new Response(body === null ? null : JSON.stringify(body), { status }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}
function lastCall(fetchMock: ReturnType<typeof stubFetch>) {
  return fetchMock.mock.calls[0] as unknown as [string, RequestInit];
}

beforeEach(() => { document.cookie = "pool_csrf=agents-test"; });
afterEach(() => vi.unstubAllGlobals());

it("reads runners and pairings without CSRF or Content-Type headers", async () => {
  const fetchMock = stubFetch({ runners: [runnerFixture] });
  expect(await listRunners()).toEqual([runnerFixture]);
  let [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/me/runners");
  expect(options.credentials).toBe("same-origin");
  expect(options.headers ?? {}).not.toHaveProperty("X-CSRF-Token");
  expect(options.headers ?? {}).not.toHaveProperty("Content-Type");

  stubFetch({ pairings: [pairingFixture] });
  expect(await listPairings()).toEqual([pairingFixture]);
});

it("sends CSRF on mutations and resolves empty 204/202 bodies", async () => {
  let fetchMock = stubFetch(null, 204);
  await revokeRunner("r1");
  let [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/me/runners/r1");
  expect(options.method).toBe("DELETE");
  expect(options.headers).toMatchObject({ "X-CSRF-Token": "agents-test" });
  expect(options.body).toBeUndefined();

  fetchMock = stubFetch(null, 204);
  await decidePairing("p1", "approve", "ABC-123");
  [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/me/runner-pairings/p1");
  expect(options.method).toBe("POST");
  expect(options.headers).toMatchObject({ "X-CSRF-Token": "agents-test", "Content-Type": "application/json" });
  expect(JSON.parse(String(options.body))).toEqual({ decision: "approve", user_code: "ABC-123" });

  fetchMock = stubFetch(null, 202);
  await deleteConversation("c1");
  [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/conversations/c1");
  expect(options.method).toBe("DELETE");

  fetchMock = stubFetch(null, 204);
  await closeSession("s1");
  [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/sessions/s1");
  expect(options.method).toBe("DELETE");
});

it("cancels turns with POST, CSRF and no body or Content-Type", async () => {
  const fetchMock = stubFetch(null, 204);
  await cancelTurn("s1");
  const [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/sessions/s1/turns:cancel");
  expect(options.method).toBe("POST");
  expect(options.headers).toMatchObject({ "X-CSRF-Token": "agents-test" });
  expect(options.headers ?? {}).not.toHaveProperty("Content-Type");
  expect(options.body).toBeUndefined();
});

it("denies approvals unsigned with only the deny decision", async () => {
  const fetchMock = stubFetch(null, 204);
  await denyApproval("s1", "a1");
  const [path, options] = lastCall(fetchMock);
  expect(path).toBe("/api/sessions/s1/approvals/a1");
  expect(JSON.parse(String(options.body))).toEqual({ decision: "deny" });
});

it("keeps backend error messages verbatim with their status", async () => {
  stubFetch({ error: "machine code does not match" }, 403);
  const wrongCode = await decidePairing("p1", "approve", "bad").catch((cause) => cause);
  expect(wrongCode).toBeInstanceOf(AgentsApiError);
  expect(wrongCode.status).toBe(403);
  expect(wrongCode.message).toBe("machine code does not match");

  stubFetch({ error: { message: "close the execution before deleting its conversation" } }, 409);
  const conflict = await deleteConversation("c1").catch((cause) => cause);
  expect(conflict.status).toBe(409);
  expect(conflict.message).toBe("close the execution before deleting its conversation");

  vi.stubGlobal("fetch", vi.fn(async () => new Response("<html>bad gateway</html>", { status: 502 })));
  const opaque = await listRunners().catch((cause) => cause);
  expect(opaque.status).toBe(502);
  expect(opaque.message).toBe("Request failed (502)");
});

it("rejects malformed payloads with explicit validation errors", async () => {
  stubFetch({ runners: [{ id: "r1" }] });
  await expect(listRunners()).rejects.toThrow("Invalid runner response");
  stubFetch({});
  await expect(listRunners()).rejects.toThrow("Invalid runners response");
  stubFetch({ pairings: [{ id: "p1", name: "laptop" }] });
  await expect(listPairings()).rejects.toThrow("Invalid pairing response");
  stubFetch({ conversations: [{ ...conversationFixture, storage: "trash" }] });
  await expect(listConversations("active", 50)).rejects.toThrow("Invalid conversation response");
  stubFetch({ ...sessionFixture, approvals: null });
  await expect(getSession("s1")).rejects.toThrow("Invalid session response");
  stubFetch({ ...historyFixture, events: [{ id: "four", type: "state", data: {} }] });
  await expect(conversationHistory("c1", { limit: 100 })).rejects.toThrow("Invalid history response");
});

it("always sends the conversation limit and only the allowed history parameters", async () => {
  let fetchMock = stubFetch({ conversations: [conversationFixture], next_cursor: "cur2" });
  const page = await listConversations("active", 50);
  expect(page.next_cursor).toBe("cur2");
  expect(lastCall(fetchMock)[0]).toBe("/api/conversations?storage=active&limit=50");
  fetchMock = stubFetch({ conversations: [] });
  await listConversations("archived", 50, "cur2");
  expect(lastCall(fetchMock)[0]).toBe("/api/conversations?storage=archived&limit=50&cursor=cur2");

  fetchMock = stubFetch(historyFixture);
  const history = await conversationHistory("c1", { limit: 100, before: 4 });
  expect(history.through).toBe(4);
  expect(history.has_older).toBe(true);
  expect(lastCall(fetchMock)[0]).toBe("/api/conversations/c1/history?limit=100&before=4");
  fetchMock = stubFetch(historyFixture);
  await conversationHistory("c1", { limit: 100, through: 4 });
  expect(lastCall(fetchMock)[0]).toBe("/api/conversations/c1/history?limit=100&through=4");
});

it("encodes resource ids in paths", async () => {
  const fetchMock = stubFetch(null, 204);
  await revokeRunner("r/1");
  expect(lastCall(fetchMock)[0]).toBe("/api/me/runners/r%2F1");
  const second = stubFetch(conversationFixture);
  await getConversation("c/1");
  expect(lastCall(second)[0]).toBe("/api/conversations/c%2F1");
});
