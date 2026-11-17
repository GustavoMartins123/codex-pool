// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AgentsApiError, openConversationEvents, type ConversationRecord, type HistoryPage, type Session } from "../agents-api";
import { Conversations, foldTranscript, isArmedConversationAction, mergeHistory } from "./Conversations";

vi.mock("../agents-api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../agents-api")>(),
  listConversations: vi.fn(),
  getConversation: vi.fn(),
  deleteConversation: vi.fn(),
  conversationHistory: vi.fn(),
  getSession: vi.fn(),
  closeSession: vi.fn(),
  cancelTurn: vi.fn(),
  denyApproval: vi.fn(),
  listRunners: vi.fn(),
}));

import { cancelTurn, closeSession, conversationHistory, deleteConversation, denyApproval, getConversation, getSession, listConversations, listRunners } from "../agents-api";

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  url: string;
  readyState = 0;
  onopen: (() => void) | null = null;
  closeCalls = 0;
  listeners = new Map<string, Array<(event: unknown) => void>>();
  constructor(url: string) { this.url = url; FakeEventSource.instances.push(this); }
  addEventListener(type: string, listener: (event: unknown) => void) {
    this.listeners.set(type, [...this.listeners.get(type) ?? [], listener]);
  }
  close() { this.closeCalls++; this.readyState = 2; }
  open() { this.readyState = 1; this.onopen?.(); }
  emit(type: string, data: unknown, id?: number) {
    const message = { data: JSON.stringify(data), lastEventId: id === undefined ? "" : String(id) };
    for (const listener of this.listeners.get(type) ?? []) listener(message);
  }
  emitConnectionError() { for (const listener of this.listeners.get("error") ?? []) listener({}); }
}

const conversationFixture: ConversationRecord = {
  id: "c1", principal_id: "p1", session_id: "s1", agent: "codex", model: "gpt-5", title: "Fix login",
  storage: "active", session_state: "running", created_at: "2026-11-17T10:00:00Z", last_interaction_at: "2026-11-17T11:00:00Z",
  order_at: "2026-11-17T11:00:00Z", archive_after: "2026-11-24T11:00:00Z", event_cursor: 2, generation: 1,
  history_complete: true, preview: [], resume_available: false, archived_through: 0, deleting: false,
};
const secondConversation: ConversationRecord = { ...conversationFixture, id: "c2", session_id: "s2", title: "Refactor pool" };
const sessionFixture: Session = {
  id: "s1", conversation_id: "c1", principal_id: "p1", client_id: "cl1", agent: "codex", model: "gpt-5",
  reasoning_effort: "medium", workspace: { mode: "ephemeral" }, permission_mode: "gated", state: "running", runner_id: "r1",
  lease_expires_at: "2026-11-17T12:00:00Z", turn_id: "t1", created_at: "2026-11-17T10:00:00Z", updated_at: "2026-11-17T11:00:00Z",
  turns: 1, approvals: [], turn_summaries: [], resume_requested: false,
};
const historyFixture: HistoryPage = {
  conversation: conversationFixture,
  session: sessionFixture,
  events: [
    { id: 1, type: "turn.input", data: { turn_id: "t1", input: "hello" } },
    { id: 2, type: "turn.delta", data: { turn_id: "t1", kind: "text", content: "Hi" } },
  ],
  before: 1, through: 2, has_older: false,
};
const runnerFixture = { id: "r1", name: "desk", online: true, os: "linux", arch: "x64", revoked: false, created_at: "2026-11-17T09:00:00Z" };

function button(name: string | RegExp): HTMLButtonElement {
  return screen.getByRole("button", { name }) as HTMLButtonElement;
}
async function renderLoaded() {
  render(<Conversations />);
  await screen.findByRole("button", { name: "Open Fix login" });
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  await screen.findByText("Hi");
  const source = FakeEventSource.instances[0];
  act(() => source.open());
  return source;
}

beforeEach(() => {
  FakeEventSource.instances = [];
  vi.stubGlobal("EventSource", FakeEventSource);
  window.history.replaceState(null, "", "/");
  vi.mocked(listConversations).mockResolvedValue({ conversations: [conversationFixture] });
  vi.mocked(conversationHistory).mockResolvedValue(historyFixture);
  vi.mocked(listRunners).mockResolvedValue([runnerFixture]);
  vi.mocked(getSession).mockResolvedValue(sessionFixture);
  vi.mocked(getConversation).mockResolvedValue(conversationFixture);
  vi.mocked(deleteConversation).mockResolvedValue(undefined);
  vi.mocked(closeSession).mockResolvedValue(undefined);
  vi.mocked(cancelTurn).mockResolvedValue(undefined);
  vi.mocked(denyApproval).mockResolvedValue(undefined);
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.clearAllMocks(); });

describe("openConversationEvents", () => {
  it("opens the encoded stream URL and reports live on open", () => {
    const statuses: string[] = [];
    openConversationEvents("c/1", { onEvent: () => undefined, onStatus: (status) => statuses.push(status) });
    const source = FakeEventSource.instances[0];
    expect(source.url).toBe("/api/conversations/c%2F1/events");
    act(() => source.open());
    expect(statuses).toEqual(["live"]);
  });

  it("parses named events with their ids and treats missing ids as snapshot zero", () => {
    const events: Array<{ id: number; type: string }> = [];
    openConversationEvents("c1", { onEvent: (event) => events.push(event), onStatus: () => undefined });
    const source = FakeEventSource.instances[0];
    act(() => { source.emit("turn.delta", { turn_id: "t1", kind: "text", content: "x" }, 7); source.emit("state", { state: "running" }); });
    expect(events).toEqual([
      { id: 7, type: "turn.delta", data: { turn_id: "t1", kind: "text", content: "x" } },
      { id: 0, type: "state", data: { state: "running" } },
    ]);
  });

  it("closes permanently on a server error event instead of reconnecting", () => {
    const statuses: string[] = [];
    const events: string[] = [];
    openConversationEvents("c1", { onEvent: (event) => events.push(event.type), onStatus: (status) => statuses.push(status) });
    const source = FakeEventSource.instances[0];
    act(() => { source.open(); source.emit("error", { message: "authentication expired" }, 9); });
    expect(source.closeCalls).toBe(1);
    expect(events).toEqual(["error"]);
    expect(statuses).toEqual(["live", "disconnected"]);
    act(() => { source.emit("turn.delta", { content: "late" }, 10); source.emitConnectionError(); });
    expect(events).toEqual(["error"]);
    expect(FakeEventSource.instances.length).toBe(1);
  });

  it("separates reconnecting connection errors from terminal closed ones", () => {
    const statuses: string[] = [];
    const close = openConversationEvents("c1", { onEvent: () => undefined, onStatus: (status) => statuses.push(status) });
    const source = FakeEventSource.instances[0];
    source.readyState = 0;
    act(() => source.emitConnectionError());
    expect(statuses).toEqual(["reconnecting"]);
    source.readyState = 2;
    act(() => source.emitConnectionError());
    expect(statuses).toEqual(["reconnecting", "disconnected"]);
    expect(source.closeCalls).toBe(1);
    close();
    expect(source.closeCalls).toBe(1);
  });
});

describe("transcript helpers", () => {
  it("folds consecutive deltas of the same turn and kind into one block", () => {
    const blocks = foldTranscript([
      { id: 1, type: "turn.input", data: { turn_id: "t1", input: "run ls" } },
      { id: 2, type: "turn.delta", data: { turn_id: "t1", kind: "text", content: "a" } },
      { id: 3, type: "turn.delta", data: { turn_id: "t1", kind: "text", content: "b" } },
      { id: 4, type: "turn.delta", data: { turn_id: "t1", kind: "thinking", content: "hmm" } },
      { id: 5, type: "session.closed", data: { reason: "user" } },
      { id: 6, type: "error", data: { message: "boom" } },
    ]);
    expect(blocks.map((block) => [block.kind, block.text])).toEqual([
      ["user", "run ls"], ["text", "ab"], ["thinking", "hmm"], ["banner", "Session closed — user"], ["error", "boom"],
    ]);
  });

  it("merges buffered live events past the history cursor without duplicates", () => {
    const history = [{ id: 1, type: "state", data: {} }, { id: 2, type: "state", data: {} }];
    const buffered = [
      { id: 2, type: "state", data: {} },
      { id: 3, type: "turn.delta", data: {} },
      { id: 3, type: "turn.delta", data: {} },
      { id: 0, type: "state", data: {} },
    ];
    expect(mergeHistory(history, 2, buffered).map((event) => event.id)).toEqual([1, 2, 3, 0]);
  });

  it("scopes armed actions to one conversation and kind", () => {
    expect(isArmedConversationAction({ id: "c1", kind: "delete" }, "c1", "delete")).toBe(true);
    expect(isArmedConversationAction({ id: "c1", kind: "delete" }, "c1", "close")).toBe(false);
    expect(isArmedConversationAction({ id: "c1", kind: "delete" }, "c2", "delete")).toBe(false);
    expect(isArmedConversationAction(null, "c1", "delete")).toBe(false);
  });
});

describe("Conversations page", () => {
  it("renders the roster, transcript, runner name, and syncs the URL", async () => {
    await renderLoaded();
    expect(listConversations).toHaveBeenCalledWith("active", 50, undefined);
    expect(conversationHistory).toHaveBeenCalledWith("c1", { limit: 100 });
    expect(screen.getByText("hello")).toBeTruthy();
    expect(screen.getByText("desk")).toBeTruthy();
    expect(window.location.search).toContain("conversation=c1");
    expect(screen.getByText("live", { selector: ".live-status" })).toBeTruthy();
  });

  it("buffers live events during history load and merges without gaps or overlap", async () => {
    let resolveHistory!: (page: HistoryPage) => void;
    vi.mocked(conversationHistory).mockImplementation(() => new Promise((resolve) => { resolveHistory = resolve; }));
    render(<Conversations />);
    await screen.findByRole("button", { name: "Open Fix login" });
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const source = FakeEventSource.instances[0];
    act(() => {
      source.open();
      source.emit("turn.delta", { turn_id: "t1", kind: "text", content: " there" }, 3);
      source.emit("turn.delta", { turn_id: "t1", kind: "text", content: "Hi" }, 2);
    });
    await act(async () => { resolveHistory(historyFixture); });
    expect(await screen.findByText("Hi there")).toBeTruthy();
    expect(screen.queryByText("HiHi")).toBeNull();
  });

  it("deduplicates replayed live events and replaces zero-id snapshots", async () => {
    const source = await renderLoaded();
    act(() => {
      source.open();
      source.emit("turn.delta", { turn_id: "t1", kind: "text", content: " there" }, 3);
      source.emit("turn.delta", { turn_id: "t1", kind: "text", content: " there" }, 3);
      source.emit("state", { state: "waiting_approval" });
      source.emit("state", { state: "running" });
    });
    expect(screen.getByText("Hi there")).toBeTruthy();
    const markers = screen.getAllByText(/^State: /);
    expect(markers.length).toBe(1);
    expect(markers[0].textContent).toBe("State: running");
  });

  it("shows Disconnected — Reload on terminal failure and opens a fresh stream", async () => {
    const source = await renderLoaded();
    act(() => { source.open(); source.emit("error", { message: "authentication expired" }); });
    expect(source.closeCalls).toBe(1);
    expect(screen.getByText("disconnected", { selector: ".live-status" })).toBeTruthy();
    fireEvent.click(button("Disconnected — Reload"));
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    expect(FakeEventSource.instances[1].url).toBe("/api/conversations/c1/events");
    expect(conversationHistory).toHaveBeenCalledTimes(2);
  });

  it("treats a closed connection as terminal without reopening", async () => {
    const source = await renderLoaded();
    source.readyState = 2;
    act(() => source.emitConnectionError());
    expect(screen.getByText("disconnected", { selector: ".live-status" })).toBeTruthy();
    expect(FakeEventSource.instances.length).toBe(1);
  });

  it("closes the stream on selection change and on unmount", async () => {
    vi.mocked(listConversations).mockResolvedValue({ conversations: [conversationFixture, secondConversation] });
    vi.mocked(conversationHistory).mockImplementation(async (id) => id === "c2"
      ? { ...historyFixture, conversation: secondConversation, session: { ...sessionFixture, id: "s2", conversation_id: "c2" }, events: [] }
      : historyFixture);
    const first = await renderLoaded();
    fireEvent.click(button("Open Refactor pool"));
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(2));
    expect(first.closeCalls).toBe(1);
    expect(window.location.search).toContain("conversation=c2");
    cleanup();
    expect(FakeEventSource.instances[1].closeCalls).toBe(1);
  });

  it("loads older events with the before cursor and prepends them", async () => {
    vi.mocked(conversationHistory).mockResolvedValue({ ...historyFixture, has_older: true });
    await renderLoaded();
    vi.mocked(conversationHistory).mockResolvedValue({
      ...historyFixture,
      events: [{ id: 0, type: "turn.input", data: { turn_id: "t0", input: "earlier question" } }],
      before: 0, through: 0, has_older: false,
    });
    fireEvent.click(button("Load older"));
    expect(await screen.findByText("earlier question")).toBeTruthy();
    expect(conversationHistory).toHaveBeenLastCalledWith("c1", { limit: 100, before: 1 });
    expect(screen.queryByRole("button", { name: "Load older" })).toBeNull();
    const transcript = screen.getByRole("log", { name: "Conversation transcript" });
    expect(transcript.textContent!.indexOf("earlier question")).toBeLessThan(transcript.textContent!.indexOf("hello"));
  });

  it("switches storage and paginates with the next cursor", async () => {
    vi.mocked(listConversations).mockResolvedValue({ conversations: [conversationFixture], next_cursor: "n1" });
    await renderLoaded();
    vi.mocked(listConversations).mockResolvedValue({ conversations: [secondConversation] });
    fireEvent.click(button("Load more"));
    await screen.findByRole("button", { name: "Open Refactor pool" });
    expect(listConversations).toHaveBeenLastCalledWith("active", 50, "n1");
    vi.mocked(listConversations).mockResolvedValue({ conversations: [] });
    fireEvent.click(button("Archived"));
    await waitFor(() => expect(listConversations).toHaveBeenLastCalledWith("archived", 50, undefined));
    expect(await screen.findByText("No archived conversations.")).toBeTruthy();
  });

  it("cancels a running turn with one click and closes the session with two", async () => {
    const source = await renderLoaded();
    expect(screen.queryByRole("button", { name: /Delete conversation/ })).toBeNull();
    fireEvent.click(button("Cancel turn"));
    await waitFor(() => expect(cancelTurn).toHaveBeenCalledWith("s1"));
    fireEvent.click(button("Close session"));
    expect(closeSession).not.toHaveBeenCalled();
    fireEvent.click(button("Confirm close"));
    await waitFor(() => expect(closeSession).toHaveBeenCalledWith("s1"));
    act(() => source.emit("session.closed", { reason: "user closed" }, 4));
    expect(await screen.findByText("Session closed — user closed")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("button", { name: /Close session|Confirm close/ })).toBeNull());
    expect(button("Delete conversation")).toBeTruthy();
  });

  it("denies approvals unsigned and tracks live approval requests", async () => {
    const source = await renderLoaded();
    act(() => source.emit("approval.requested", { approval_id: "a1", command: "rm -rf /tmp/x", context: "cleanup", turn_id: "t1", expires_at: new Date(Date.now() + 120000).toISOString() }, 3));
    expect(await screen.findByText("rm -rf /tmp/x")).toBeTruthy();
    expect(screen.getByText("Approve in the mobile app.")).toBeTruthy();
    fireEvent.click(button("Deny"));
    await waitFor(() => expect(denyApproval).toHaveBeenCalledWith("s1", "a1"));
    await waitFor(() => expect(screen.queryByText("rm -rf /tmp/x")).toBeNull());
    act(() => source.emit("approval.requested", { approval_id: "a2", command: "ls", context: "", turn_id: "t1", expires_at: new Date(Date.now() + 120000).toISOString() }, 4));
    expect(await screen.findByText("ls")).toBeTruthy();
    act(() => source.emit("approval.resolved", { approval_id: "a2", decision: "approve" }, 5));
    await waitFor(() => expect(screen.queryByText("ls")).toBeNull());
  });

  it("deletes a closed conversation and polls until it disappears", async () => {
    const closedRecord = { ...conversationFixture, session_state: "closed" };
    vi.mocked(listConversations).mockResolvedValue({ conversations: [closedRecord] });
    vi.mocked(conversationHistory).mockResolvedValue({ ...historyFixture, conversation: closedRecord, session: { ...sessionFixture, state: "closed", turn_id: undefined } });
    vi.mocked(getConversation).mockRejectedValue(new AgentsApiError(404, "conversation not found"));
    render(<Conversations />);
    await screen.findByRole("button", { name: "Open Fix login" });
    fireEvent.click(await screen.findByRole("button", { name: "Delete conversation" }));
    fireEvent.click(button("Confirm delete"));
    await waitFor(() => expect(deleteConversation).toHaveBeenCalledWith("c1"));
    expect(await screen.findByText("Deleting…", { selector: ".deleting-badge" })).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("button", { name: "Open Fix login" })).toBeNull(), { timeout: 4500 });
    expect(screen.getByText("Select a conversation to view its transcript.")).toBeTruthy();
    expect(window.location.search).not.toContain("conversation=");
  }, 10000);

  it("surfaces delete failures verbatim and offers a single-click retry", async () => {
    const closedRecord = { ...conversationFixture, session_state: "closed" };
    vi.mocked(listConversations).mockResolvedValue({ conversations: [closedRecord] });
    vi.mocked(conversationHistory).mockResolvedValue({ ...historyFixture, conversation: closedRecord, session: { ...sessionFixture, state: "closed", turn_id: undefined } });
    vi.mocked(getConversation).mockResolvedValue({ ...closedRecord, deleting: false, delete_error: "runner unreachable; archive retained" });
    render(<Conversations />);
    await screen.findByRole("button", { name: "Open Fix login" });
    fireEvent.click(await screen.findByRole("button", { name: "Delete conversation" }));
    fireEvent.click(button("Confirm delete"));
    expect(await screen.findByText("runner unreachable; archive retained", { selector: ".signal-error" }, { timeout: 4500 })).toBeTruthy();
    vi.mocked(getConversation).mockRejectedValue(new AgentsApiError(404, "conversation not found"));
    fireEvent.click(button("Retry delete"));
    await waitFor(() => expect(deleteConversation).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Open Fix login" })).toBeNull(), { timeout: 4500 });
  }, 15000);

  it("falls back to the saved preview when history was not recorded", async () => {
    const unrecorded = { ...conversationFixture, history_complete: false, preview: [{ turn_id: "t1", role: "user", text: "saved question" }] };
    vi.mocked(listConversations).mockResolvedValue({ conversations: [unrecorded] });
    vi.mocked(conversationHistory).mockRejectedValue(new AgentsApiError(409, "history was not recorded for this conversation"));
    vi.mocked(getSession).mockResolvedValue({ ...sessionFixture, turn_summaries: [{ turn_id: "t1", tokens: 12, duration_ms: 900, exit_code: 0 }] });
    render(<Conversations />);
    await screen.findByRole("button", { name: "Open Fix login" });
    expect(await screen.findByText(/history was not recorded for this conversation/)).toBeTruthy();
    expect(screen.getByText("saved question")).toBeTruthy();
    expect(screen.getByText(/exit 0 · 12 tokens/)).toBeTruthy();
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const source = FakeEventSource.instances[0];
    act(() => { source.open(); source.emit("turn.delta", { turn_id: "t2", kind: "text", content: "live goes on" }, 5); });
    expect(await screen.findByText("live goes on")).toBeTruthy();
  });

  it("restores the selection from the URL on popstate", async () => {
    vi.mocked(listConversations).mockResolvedValue({ conversations: [conversationFixture, secondConversation] });
    vi.mocked(conversationHistory).mockImplementation(async (id) => id === "c2"
      ? { ...historyFixture, conversation: secondConversation, session: { ...sessionFixture, id: "s2", conversation_id: "c2" }, events: [] }
      : historyFixture);
    await renderLoaded();
    fireEvent.click(button("Open Refactor pool"));
    await waitFor(() => expect(window.location.search).toContain("conversation=c2"));
    window.history.replaceState(null, "", "/?conversation=c1");
    act(() => { window.dispatchEvent(new PopStateEvent("popstate")); });
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(3));
    expect(FakeEventSource.instances[2].url).toBe("/api/conversations/c1/events");
  });
});
