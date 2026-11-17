export type Runner = { id: string; name: string; online: boolean; agents?: string[]; labels?: string[]; allowed_paths?: string[]; os: string; arch: string; revoked: boolean; created_at: string };
export type PairingReview = { id: string; name: string; os: string; arch: string; runner_id?: string; expires_at: string };
export type ConversationPreview = { turn_id: string; role: string; text: string };
export type ConversationRecord = {
  id: string; principal_id: string; session_id: string; agent: string; model: string; title: string;
  storage: "active" | "archived"; session_state: string; created_at: string; last_interaction_at: string | null;
  order_at: string; archive_after: string; migrated_at?: string; event_cursor: number; generation: number;
  history_complete: boolean; preview?: ConversationPreview[]; resume_available: boolean; archive_id?: string;
  archived_through: number; deleting: boolean; delete_error?: string;
};
export type ConversationListPage = { conversations: ConversationRecord[]; next_cursor?: string };
export type Workspace = { mode: string; target?: string };
export type Approval = { approval_id: string; command: string; context: string; turn_id: string; expires_at: string };
export type TurnSummary = { turn_id: string; tokens: number; duration_ms: number; exit_code: number };
export type AgentCheckpoint = { version: number; native_session_id: string; native_log_id: string; digest: string; ready: boolean };
export type Session = {
  id: string; conversation_id: string; principal_id: string; client_id: string; agent: string; model: string;
  reasoning_effort: string; workspace: Workspace; permission_mode: string; state: string; runner_id?: string;
  lease_expires_at: string; turn_id?: string; created_at: string; updated_at: string; closed_at?: string;
  turns: number; closed_reason?: string; approvals: Approval[]; turn_summaries: TurnSummary[];
  checkpoint?: AgentCheckpoint; resume_requested: boolean; pending_input?: { id: string; input: string };
};
export type SessionEvent = { id: number; type: string; data: unknown; at?: string };
export type HistoryPage = { conversation: ConversationRecord; session: Session; events: SessionEvent[]; before: number; through: number; has_older: boolean };

export class AgentsApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

function object(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === "object" && !Array.isArray(value); }

// Backend messages are already actionable and several statuses drive distinct
// UI flows (403 wrong code, 409/410 stale pairing, 404 deleted), so errors keep
// the verbatim message plus the status instead of a generic retry suffix.
async function request(path: string, method = "GET", body?: unknown): Promise<unknown> {
  const csrf = document.cookie.split("; ").find((part) => part.startsWith("pool_csrf="))?.slice("pool_csrf=".length) ?? "";
  const response = await fetch(path, {
    method,
    credentials: "same-origin",
    cache: "no-store",
    headers: { ...(method === "GET" ? {} : { "X-CSRF-Token": csrf }), ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const text = await response.text();
  let payload: unknown = null;
  if (text) { try { payload = JSON.parse(text); } catch { payload = null; } }
  if (!response.ok) {
    const value = payload as { error?: unknown } | null;
    const message = typeof value?.error === "string" ? value.error : object(value?.error) && typeof value.error.message === "string" ? value.error.message : `Request failed (${response.status})`;
    throw new AgentsApiError(response.status, message);
  }
  return payload;
}

function runner(value: unknown): Runner {
  if (!object(value) || typeof value.id !== "string" || typeof value.name !== "string" || typeof value.online !== "boolean" || typeof value.os !== "string" || typeof value.arch !== "string" || typeof value.revoked !== "boolean" || typeof value.created_at !== "string") throw new Error("Invalid runner response");
  return value as Runner;
}
function pairing(value: unknown): PairingReview {
  if (!object(value) || typeof value.id !== "string" || typeof value.name !== "string" || typeof value.os !== "string" || typeof value.arch !== "string" || typeof value.expires_at !== "string") throw new Error("Invalid pairing response");
  return value as PairingReview;
}
function conversation(value: unknown): ConversationRecord {
  if (!object(value) || typeof value.id !== "string" || typeof value.session_id !== "string" || typeof value.agent !== "string" || typeof value.model !== "string" || typeof value.title !== "string" || !["active", "archived"].includes(String(value.storage)) || typeof value.session_state !== "string" || typeof value.created_at !== "string" || typeof value.history_complete !== "boolean" || typeof value.deleting !== "boolean" || !Number.isSafeInteger(value.event_cursor)) throw new Error("Invalid conversation response");
  return value as ConversationRecord;
}
function session(value: unknown): Session {
  if (!object(value) || typeof value.id !== "string" || typeof value.agent !== "string" || typeof value.model !== "string" || typeof value.state !== "string" || !object(value.workspace) || typeof value.workspace.mode !== "string" || !Array.isArray(value.approvals) || !Array.isArray(value.turn_summaries)) throw new Error("Invalid session response");
  return value as Session;
}
function sessionEvent(value: unknown): value is SessionEvent {
  return object(value) && Number.isSafeInteger(value.id) && typeof value.type === "string" && "data" in value;
}

export async function listRunners(): Promise<Runner[]> {
  const value = await request("/api/me/runners");
  if (!object(value) || !Array.isArray(value.runners)) throw new Error("Invalid runners response");
  return value.runners.map(runner);
}
export async function revokeRunner(id: string): Promise<void> {
  await request(`/api/me/runners/${encodeURIComponent(id)}`, "DELETE");
}
export async function listPairings(): Promise<PairingReview[]> {
  const value = await request("/api/me/runner-pairings");
  if (!object(value) || !Array.isArray(value.pairings)) throw new Error("Invalid pairings response");
  return value.pairings.map(pairing);
}
export async function decidePairing(id: string, decision: "approve" | "deny", userCode: string): Promise<void> {
  await request(`/api/me/runner-pairings/${encodeURIComponent(id)}`, "POST", { decision, user_code: userCode });
}
export async function listConversations(storage: "active" | "archived", limit: number, cursor?: string): Promise<ConversationListPage> {
  const params = new URLSearchParams({ storage, limit: String(limit) });
  if (cursor) params.set("cursor", cursor);
  const value = await request(`/api/conversations?${params}`);
  if (!object(value) || !Array.isArray(value.conversations) || (value.next_cursor !== undefined && typeof value.next_cursor !== "string")) throw new Error("Invalid conversations response");
  return { conversations: value.conversations.map(conversation), ...(typeof value.next_cursor === "string" ? { next_cursor: value.next_cursor } : {}) };
}
export async function getConversation(id: string): Promise<ConversationRecord> {
  return conversation(await request(`/api/conversations/${encodeURIComponent(id)}`));
}
export async function deleteConversation(id: string): Promise<void> {
  await request(`/api/conversations/${encodeURIComponent(id)}`, "DELETE");
}
export async function conversationHistory(id: string, page: { limit: number; before?: number; through?: number }): Promise<HistoryPage> {
  const params = new URLSearchParams({ limit: String(page.limit) });
  if (page.before !== undefined) params.set("before", String(page.before));
  if (page.through !== undefined) params.set("through", String(page.through));
  const value = await request(`/api/conversations/${encodeURIComponent(id)}/history?${params}`);
  if (!object(value) || !Array.isArray(value.events) || !value.events.every(sessionEvent) || !Number.isSafeInteger(value.before) || !Number.isSafeInteger(value.through) || typeof value.has_older !== "boolean") throw new Error("Invalid history response");
  return { conversation: conversation(value.conversation), session: session(value.session), events: value.events as SessionEvent[], before: value.before as number, through: value.through as number, has_older: value.has_older };
}
export async function getSession(id: string): Promise<Session> {
  return session(await request(`/api/sessions/${encodeURIComponent(id)}`));
}
export async function closeSession(id: string): Promise<void> {
  await request(`/api/sessions/${encodeURIComponent(id)}`, "DELETE");
}
export async function cancelTurn(sessionId: string): Promise<void> {
  await request(`/api/sessions/${encodeURIComponent(sessionId)}/turns:cancel`, "POST");
}
export async function denyApproval(sessionId: string, approvalId: string): Promise<void> {
  await request(`/api/sessions/${encodeURIComponent(sessionId)}/approvals/${encodeURIComponent(approvalId)}`, "POST", { decision: "deny" });
}

export type LiveStatus = "connecting" | "live" | "reconnecting" | "disconnected";

export const SESSION_EVENT_TYPES = [
  "state", "session.settings", "session.capabilities", "turn.input", "turn.started", "turn.delta",
  "turn.finished", "approval.requested", "approval.resolved", "runner.status", "session.closed", "error",
] as const;

// The server also emits a named "error" event for terminal failures (expired
// auth, slow consumer). It arrives as a MessageEvent with data, unlike native
// connection errors, and must close the stream instead of letting the browser
// reconnect forever against a 4xx.
export function openConversationEvents(conversationId: string, handlers: {
  onEvent: (event: SessionEvent) => void;
  onStatus: (status: LiveStatus) => void;
}): () => void {
  const source = new EventSource(`/api/conversations/${encodeURIComponent(conversationId)}/events`);
  let closed = false;
  const close = () => { if (!closed) { closed = true; source.close(); } };
  const parse = (raw: string): unknown => { try { return JSON.parse(raw); } catch { return null; } };
  const eventId = (message: MessageEvent): number => {
    const id = Number(message.lastEventId || 0);
    return Number.isSafeInteger(id) ? id : 0;
  };
  source.onopen = () => { if (!closed) handlers.onStatus("live"); };
  for (const type of SESSION_EVENT_TYPES) {
    if (type === "error") continue;
    source.addEventListener(type, (raw) => {
      if (closed) return;
      const message = raw as MessageEvent;
      handlers.onEvent({ id: eventId(message), type, data: parse(String(message.data)) });
    });
  }
  source.addEventListener("error", (raw) => {
    if (closed) return;
    const message = raw as MessageEvent;
    if (typeof message.data === "string" && message.data !== "") {
      close();
      handlers.onEvent({ id: eventId(message), type: "error", data: parse(message.data) });
      handlers.onStatus("disconnected");
      return;
    }
    if (source.readyState === EventSource.CLOSED) {
      close();
      handlers.onStatus("disconnected");
    } else {
      handlers.onStatus("reconnecting");
    }
  });
  return close;
}
