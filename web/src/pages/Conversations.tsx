import { AgentsApiError, cancelTurn, closeSession, conversationHistory, deleteConversation, denyApproval, getConversation, getSession, listConversations, listRunners, openConversationEvents, type Approval, type ConversationRecord, type LiveStatus, type Session, type SessionEvent } from "../agents-api";
import { queryValue, updateURL } from "../navigation";
import { ResponseVersion } from "../response-version";
import { classNames, formatAdmission } from "../ui";
import { pairingCountdown } from "./Runners";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

export type ConversationAction = { id: string; kind: "close" | "delete" };

export function isArmedConversationAction(armed: ConversationAction | null, id: string, kind: ConversationAction["kind"]) {
  return armed !== null && armed.id === id && armed.kind === kind;
}

export type TranscriptBlock = { key: string; kind: string; label?: string; text: string };

function payload(event: SessionEvent): Record<string, unknown> {
  return event.data !== null && typeof event.data === "object" && !Array.isArray(event.data) ? event.data as Record<string, unknown> : {};
}
function text(value: unknown): string { return typeof value === "string" ? value : ""; }

export function foldTranscript(events: SessionEvent[]): TranscriptBlock[] {
  const blocks: TranscriptBlock[] = [];
  events.forEach((event, index) => {
    const data = payload(event);
    const key = `${event.id}:${index}`;
    const turnID = text(data.turn_id);
    switch (event.type) {
      case "turn.input":
        blocks.push({ key, kind: "user", text: text(data.input) });
        break;
      case "turn.delta": {
        const kind = text(data.kind) || "text";
        const last = blocks[blocks.length - 1];
        if (last && last.kind === kind && last.label === turnID) last.text += text(data.content);
        else blocks.push({ key, kind, label: turnID, text: text(data.content) });
        break;
      }
      case "turn.started":
        blocks.push({ key, kind: "marker", text: `Turn started${turnID ? ` · ${turnID.slice(0, 8)}` : ""}` });
        break;
      case "turn.finished": {
        const exit = typeof data.exit_code === "number" ? ` · exit ${data.exit_code}` : "";
        const tokens = typeof data.tokens === "number" ? ` · ${data.tokens} tokens` : "";
        blocks.push({ key, kind: "marker", text: `Turn finished${exit}${tokens}` });
        break;
      }
      case "state":
        blocks.push({ key, kind: "marker", text: `State: ${text(data.state)}` });
        break;
      case "session.closed":
        blocks.push({ key, kind: "banner", text: `Session closed${text(data.reason) ? ` — ${text(data.reason)}` : ""}` });
        break;
      case "runner.status":
        blocks.push({ key, kind: "banner", text: `Runner ${text(data.status)}` });
        break;
      case "error":
        blocks.push({ key, kind: "error", text: text(data.message) || "Stream error" });
        break;
    }
  });
  return blocks;
}

export function mergeHistory(events: SessionEvent[], through: number, buffered: SessionEvent[]): SessionEvent[] {
  const seen = new Set(events.map((event) => event.id).filter((id) => id > 0));
  const merged = [...events];
  for (const event of buffered) {
    if (event.id === 0) { merged.push(event); continue; }
    if (event.id <= through || seen.has(event.id)) continue;
    seen.add(event.id);
    merged.push(event);
  }
  return merged;
}

export function Conversations() {
  const [storage, setStorage] = useState<"active" | "archived">("active");
  const [conversations, setConversations] = useState<ConversationRecord[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [listBusy, setListBusy] = useState(false);
  const [selected, setSelected] = useState<ConversationRecord | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [hasOlder, setHasOlder] = useState(false);
  const [olderBusy, setOlderBusy] = useState(false);
  const [historyNotice, setHistoryNotice] = useState("");
  const [liveStatus, setLiveStatus] = useState<LiveStatus>("connecting");
  const [reloadKey, setReloadKey] = useState(0);
  const [runnerNames, setRunnerNames] = useState<Record<string, string>>({});
  const [armed, setArmed] = useState<ConversationAction | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const [listGuard] = useState(() => new ResponseVersion());
  const detailVersion = useRef(0);
  const deleteVersion = useRef(0);
  const beforeRef = useRef(0);
  const bufferRef = useRef<SessionEvent[]>([]);
  const historyLoadedRef = useRef(false);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const nearBottomRef = useRef(true);

  const refresh = useCallback(async (cursor?: string) => {
    const version = listGuard.begin();
    setListBusy(true);
    try {
      const page = await listConversations(storage, 50, cursor);
      if (!listGuard.isCurrent(version)) return;
      setConversations((current) => cursor ? [...current, ...page.conversations] : page.conversations);
      setNextCursor(page.next_cursor ?? null);
      setError("");
      if (!cursor) setSelected((current) => {
        const requestedID = queryValue("conversation") || current?.id;
        return requestedID ? page.conversations.find((item) => item.id === requestedID) || page.conversations[0] || null : page.conversations[0] || null;
      });
    } catch (cause) {
      if (!listGuard.isCurrent(version)) return;
      setError(cause instanceof Error ? cause.message : "Unable to load conversations");
    } finally {
      if (listGuard.isCurrent(version)) setListBusy(false);
    }
  }, [storage, listGuard]);

  useEffect(() => { void refresh(); }, [refresh]);
  useEffect(() => {
    if (selected) updateURL({ conversation: selected.id }, "replace");
  }, [selected]);
  useEffect(() => {
    const restoreConversation = () => {
      const requestedID = queryValue("conversation");
      setSelected(requestedID ? conversations.find((item) => item.id === requestedID) || conversations[0] || null : conversations[0] || null);
    };
    window.addEventListener("popstate", restoreConversation);
    return () => window.removeEventListener("popstate", restoreConversation);
  }, [conversations]);
  useEffect(() => {
    listRunners().then((list) => setRunnerNames(Object.fromEntries(list.map((item) => [item.id, item.name])))).catch(() => undefined);
    return () => { deleteVersion.current++; };
  }, []);
  useEffect(() => { setArmed(null); }, [selected?.id]);
  useEffect(() => {
    if (!session?.approvals.length) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [session?.approvals.length]);
  useEffect(() => {
    if (!session) return;
    setConversations((list) => list.map((item) => item.id === session.conversation_id && item.session_state !== session.state ? { ...item, session_state: session.state } : item));
  }, [session?.state, session?.conversation_id]); // eslint-disable-line react-hooks/exhaustive-deps

  const applyLiveEvent = useCallback((event: SessionEvent) => {
    const data = payload(event);
    setSession((current) => {
      if (!current) return current;
      switch (event.type) {
        case "state":
          return { ...current, state: text(data.state) || current.state };
        case "session.settings":
          return {
            ...current,
            model: text(data.model) || current.model,
            reasoning_effort: text(data.reasoning_effort) || current.reasoning_effort,
            permission_mode: text(data.permission_mode) || current.permission_mode,
          };
        case "session.closed":
          return { ...current, state: "closed", closed_reason: text(data.reason) || current.closed_reason, turn_id: undefined, approvals: [] };
        case "turn.started":
          return { ...current, turn_id: text(data.turn_id) || current.turn_id };
        // Both events drop every pending approval server-side without an
        // approval.resolved event, so the cards have to go with the turn.
        case "turn.finished":
          return current.turn_id === text(data.turn_id) ? { ...current, turn_id: undefined, approvals: [] } : current;
        case "approval.requested": {
          const approvalID = text(data.approval_id);
          if (!approvalID || current.approvals.some((approval) => approval.approval_id === approvalID)) return current;
          return { ...current, approvals: [...current.approvals, data as unknown as Approval] };
        }
        case "approval.resolved":
          return { ...current, approvals: current.approvals.filter((approval) => approval.approval_id !== text(data.approval_id)) };
        default:
          return current;
      }
    });
  }, []);

  useEffect(() => {
    const version = ++detailVersion.current;
    bufferRef.current = [];
    historyLoadedRef.current = false;
    setEvents([]);
    setSession(null);
    setHasOlder(false);
    setHistoryNotice("");
    setOlderBusy(false);
    setLiveStatus("connecting");
    if (!selected) return;
    const conversationID = selected.id;
    const sessionID = selected.session_id;

    // Stream first: buffer live events while history loads, then merge only
    // events past the history cursor so the transcript has no gap or overlap.
    const close = openConversationEvents(conversationID, {
      onStatus: (status) => { if (version === detailVersion.current) setLiveStatus(status); },
      onEvent: (event) => {
        if (version !== detailVersion.current) return;
        if (!historyLoadedRef.current) { bufferRef.current.push(event); return; }
        setEvents((current) => {
          if (event.id === 0) return [...current.filter((item) => !(item.id === 0 && item.type === event.type)), event];
          if (current.some((item) => item.id === event.id)) return current;
          return [...current, event];
        });
        applyLiveEvent(event);
      },
    });

    const absorbBuffer = (through: number) => {
      const buffered = bufferRef.current.filter((event) => event.id === 0 || event.id > through);
      bufferRef.current = [];
      historyLoadedRef.current = true;
      for (const event of buffered) applyLiveEvent(event);
      return buffered;
    };

    conversationHistory(conversationID, { limit: 100 }).then((page) => {
      if (version !== detailVersion.current) return;
      const buffered = absorbBuffer(page.through);
      setEvents(mergeHistory(page.events, page.through, buffered));
      setSession(page.session);
      setHasOlder(page.has_older);
      beforeRef.current = page.before;
      setSelected((current) => current && current.id === conversationID ? page.conversation : current);
    }).catch(async (cause) => {
      if (version !== detailVersion.current) return;
      if (cause instanceof AgentsApiError && cause.status === 409) {
        try {
          const fallback = await getSession(sessionID);
          if (version !== detailVersion.current) return;
          const buffered = absorbBuffer(0);
          setEvents(mergeHistory([], 0, buffered));
          setSession(fallback);
          setHistoryNotice(cause.message);
        } catch (nested) {
          if (version === detailVersion.current) setError(nested instanceof Error ? nested.message : "Unable to load session");
        }
        return;
      }
      setError(cause instanceof Error ? cause.message : "Unable to load history");
    });

    return () => { detailVersion.current++; close(); };
  }, [selected?.id, reloadKey, applyLiveEvent]); // eslint-disable-line react-hooks/exhaustive-deps

  const fallbackBlocks = useMemo<TranscriptBlock[]>(() => {
    if (!historyNotice) return [];
    const blocks: TranscriptBlock[] = (selected?.preview ?? []).map((item, index) => ({ key: `preview-${index}`, kind: item.role === "user" ? "user" : "text", text: item.text }));
    for (const summary of session?.turn_summaries ?? []) blocks.push({ key: `summary-${summary.turn_id}`, kind: "marker", text: `Turn ${summary.turn_id.slice(0, 8)} · exit ${summary.exit_code} · ${summary.tokens} tokens` });
    return blocks;
  }, [historyNotice, selected, session]);
  const blocks = useMemo(() => {
    const folded = foldTranscript(events);
    return historyNotice ? [...fallbackBlocks, ...folded] : folded;
  }, [events, historyNotice, fallbackBlocks]);

  useEffect(() => {
    const node = transcriptRef.current;
    if (node && nearBottomRef.current) node.scrollTop = node.scrollHeight;
  }, [blocks.length]);

  const loadOlder = async () => {
    if (!selected || olderBusy) return;
    const version = detailVersion.current;
    setOlderBusy(true);
    try {
      const page = await conversationHistory(selected.id, { limit: 100, before: beforeRef.current });
      if (version !== detailVersion.current) return;
      setEvents((current) => {
        const seen = new Set(current.map((event) => event.id).filter((id) => id > 0));
        return [...page.events.filter((event) => event.id === 0 || !seen.has(event.id)), ...current];
      });
      beforeRef.current = page.before;
      setHasOlder(page.has_older);
    } catch (cause) {
      if (version === detailVersion.current) setError(cause instanceof Error ? cause.message : "Unable to load older events");
    } finally {
      if (version === detailVersion.current) setOlderBusy(false);
    }
  };

  const runAction = async (label: string, run: () => Promise<void>) => {
    setBusy(label);
    try {
      await run();
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Action failed");
    } finally {
      setBusy("");
    }
  };

  const closeActiveSession = () => {
    if (!session) return;
    if (!isArmedConversationAction(armed, session.id, "close")) { setArmed({ id: session.id, kind: "close" }); return; }
    setArmed(null);
    void runAction(`close:${session.id}`, () => closeSession(session.id));
  };

  const cancelActiveTurn = () => {
    if (session) void runAction(`cancel:${session.id}`, () => cancelTurn(session.id));
  };

  const denyPendingApproval = (approvalID: string) => {
    if (!session) return;
    void runAction(`deny:${approvalID}`, async () => {
      await denyApproval(session.id, approvalID);
      setSession((current) => current ? { ...current, approvals: current.approvals.filter((approval) => approval.approval_id !== approvalID) } : current);
    });
  };

  const removeConversation = () => {
    if (!selected) return;
    if (!selected.delete_error && !isArmedConversationAction(armed, selected.id, "delete")) { setArmed({ id: selected.id, kind: "delete" }); return; }
    setArmed(null);
    const target = selected;
    const poll = ++deleteVersion.current;
    void runAction(`delete:${target.id}`, async () => {
      await deleteConversation(target.id);
      const mark = (patch: Partial<ConversationRecord>) => {
        setConversations((list) => list.map((item) => item.id === target.id ? { ...item, ...patch } : item));
        setSelected((current) => current && current.id === target.id ? { ...current, ...patch } : current);
      };
      mark({ deleting: true, delete_error: undefined });
      for (;;) {
        await new Promise((resolve) => setTimeout(resolve, 3000));
        if (poll !== deleteVersion.current) return;
        let record: ConversationRecord;
        try {
          record = await getConversation(target.id);
        } catch (cause) {
          if (cause instanceof AgentsApiError && cause.status === 404) {
            setConversations((list) => list.filter((item) => item.id !== target.id));
            setSelected((current) => current && current.id === target.id ? null : current);
            updateURL({ conversation: null }, "replace");
            return;
          }
          throw cause;
        }
        if (poll !== deleteVersion.current) return;
        if (record.delete_error) {
          mark({ deleting: record.deleting, delete_error: record.delete_error });
          setError(record.delete_error);
          return;
        }
        mark({ deleting: record.deleting });
        if (!record.deleting) return;
      }
    });
  };

  const sessionState = session?.state ?? selected?.session_state ?? "";
  const canCancel = Boolean(session && session.turn_id && ["running", "waiting_approval", "orphaned"].includes(session.state));
  const canDelete = sessionState === "closed";
  const deleteArmed = selected ? isArmedConversationAction(armed, selected.id, "delete") : false;
  const deleteLabel = selected?.deleting && !selected.delete_error ? "Deleting…" : deleteArmed ? "Confirm delete" : selected?.delete_error ? "Retry delete" : "Delete conversation";

  return <section className="view-stack conversations-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    <div className="view-title"><h1>Conversations</h1></div>
    <div className="console-toolbar">
      <div><strong>{conversations.length}</strong><span>{storage === "active" ? "Active conversations" : "Archived conversations"}</span></div>
      <div className="storage-toggle" role="group" aria-label="Conversation storage">
        <button className={classNames("quiet-button", storage === "active" && "active")} onClick={() => setStorage("active")} aria-pressed={storage === "active"}>Active</button>
        <button className={classNames("quiet-button", storage === "archived" && "active")} onClick={() => setStorage("archived")} aria-pressed={storage === "archived"}>Archived</button>
      </div>
    </div>
    <div className="console-layout">
      <div className="principal-roster conversation-roster" role="list" aria-label="Conversations">
        {conversations.length === 0 && <div className="empty-state">{listBusy ? "Loading conversations…" : storage === "active" ? "No conversations yet." : "No archived conversations. A closed conversation is archived after 7 days without interaction."}</div>}
        {conversations.map((item) => <button className={classNames("principal-row conversation-row", selected?.id === item.id && "selected")} key={item.id} onClick={() => { updateURL({ conversation: item.id }, "push"); setSelected(item); }} aria-label={`Open ${item.title || item.id}`}>
          <span className="principal-copy">
            <strong>{item.title || "Untitled conversation"}</strong>
            <small>{item.agent} · {item.model}</small>
            <small>{new Date(item.last_interaction_at || item.created_at).toLocaleString()}</small>
          </span>
          <span className="conversation-flags">
            {item.deleting && <small className="deleting-badge">Deleting…</small>}
            <span className={classNames("state", item.session_state === "closed" ? "dead" : item.session_state === "running" ? "online" : "offline")}>{item.session_state}</span>
          </span>
        </button>)}
        {nextCursor && <button className="quiet-button load-more" disabled={listBusy} onClick={() => void refresh(nextCursor)}>{listBusy ? "Loading…" : "Load more"}</button>}
      </div>
      <aside className="principal-detail conversation-detail">
        {!selected ? <div className="empty-state">Select a conversation to view its transcript.</div> : <>
          <div className="detail-heading">
            <div><span>{selected.storage}</span><h3>{selected.title || "Untitled conversation"}</h3><p>{selected.agent} · {selected.model}</p></div>
            <div className="detail-actions">
              {session && session.state !== "closed" && session.state !== "closing" && <button className={classNames("quiet-button", isArmedConversationAction(armed, session.id, "close") && "confirm")} disabled={busy === `close:${session.id}`} onClick={closeActiveSession}>{isArmedConversationAction(armed, session.id, "close") ? "Confirm close" : "Close session"}</button>}
              {canCancel && session && <button className="quiet-button" disabled={busy === `cancel:${session.id}`} onClick={cancelActiveTurn}>{busy === `cancel:${session.id}` ? "Cancelling…" : "Cancel turn"}</button>}
              {canDelete && <button className={deleteArmed ? "danger-action" : "quiet-button"} disabled={busy === `delete:${selected.id}` || Boolean(selected.deleting && !selected.delete_error)} onClick={removeConversation}>{deleteLabel}</button>}
            </div>
          </div>
          <div className="detail-facts">
            <span>State <b>{sessionState || "…"}</b></span>
            <span>Workspace <b>{session?.workspace.mode ?? "…"}</b></span>
            <span>Runner <b>{session?.runner_id ? runnerNames[session.runner_id] || session.runner_id : "—"}</b></span>
            <span>Turns <b>{session?.turns ?? 0}</b></span>
            {selected.storage === "active" && selected.archive_after && <span>Archives <b>{formatAdmission(selected.archive_after)}</b></span>}
          </div>
          {selected.delete_error && <div className="signal-error" role="alert">{selected.delete_error}</div>}
          <div className="live-status-row">
            <span className={classNames("live-status", liveStatus)}>{liveStatus}</span>
            {liveStatus === "disconnected" && <button className="quiet-button" onClick={() => setReloadKey((key) => key + 1)}>Disconnected — Reload</button>}
          </div>
          {session && session.approvals.length > 0 && <div className="approval-list">
            {session.approvals.map((approval) => <div className="approval-card" key={approval.approval_id}>
              <strong>Approval requested</strong>
              <code>{approval.command}</code>
              {approval.context && <small>{approval.context}</small>}
              <small>{pairingCountdown(approval.expires_at, now)}</small>
              <div className="row-actions">
                <button className="danger-action" disabled={busy === `deny:${approval.approval_id}`} onClick={() => denyPendingApproval(approval.approval_id)}>{busy === `deny:${approval.approval_id}` ? "Denying…" : "Deny"}</button>
                <small>Approve in the mobile app.</small>
                <small>Denied automatically when it expires.</small>
              </div>
            </div>)}
          </div>}
          {historyNotice && <div className="account-message" role="status">{historyNotice} Showing the saved preview instead.</div>}
          <div className="transcript" ref={transcriptRef} role="log" aria-label="Conversation transcript" onScroll={(event) => { const node = event.currentTarget; nearBottomRef.current = node.scrollHeight - node.scrollTop - node.clientHeight < 80; }}>
            {hasOlder && <button className="quiet-button load-older" disabled={olderBusy} onClick={() => void loadOlder()}>{olderBusy ? "Loading…" : "Load older"}</button>}
            {blocks.length === 0 && <div className="empty-state">{session || historyNotice ? "No transcript events yet." : "Loading transcript…"}</div>}
            {blocks.map((block) => block.kind === "marker" ? <div className="turn-marker" key={block.key}>{block.text}</div>
              : block.kind === "banner" ? <div className="transcript-banner" key={block.key}>{block.text}</div>
              : block.kind === "error" ? <div className="signal-error" key={block.key} role="alert">{block.text}</div>
              : <div className={classNames("turn-block", block.kind)} key={block.key}><small>{block.kind === "user" ? "You" : block.kind.replace(/_/g, " ")}</small><p>{block.text}</p></div>)}
          </div>
        </>}
      </aside>
    </div>
  </section>;
}
