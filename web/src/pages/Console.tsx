import { PolicyEditor } from "../components/PolicyEditor";
import { OperatorSharing } from "../components/AccountGovernance";
import { createMemberLink, loadAnalyticsHealth, loadConsoleAudit, loadConsolePrincipalUsage, loadConsolePrincipals, setPrincipalReasoningEffort, setPrincipalStatus } from "../api";
import { Area, AreaChart, Grid, Tooltip, XAxis, YAxis } from "../charts";
import { ResponseVersion } from "../response-version";
import { type ConsolePrincipal, type PassportAuditEntry, type PassportPrincipal, type PassportUsagePoint } from "../types";
import { queryValue, updateURL } from "../navigation";
import { CopyButton, SignalPanel, classNames, compact, formatTokens, preciseMoney } from "../ui";
import { type FormEvent, useCallback, useEffect, useRef, useState } from "react";

export function PassportConsole({ principal }: { principal: PassportPrincipal }) {
  const [principals, setPrincipals] = useState<ConsolePrincipal[]>([]);
  const [audit, setAudit] = useState<PassportAuditEntry[]>([]);
  const [selected, setSelected] = useState<ConsolePrincipal | null>(null);
  const [usage, setUsage] = useState<PassportUsagePoint[]>([]);
  const [usageLoading, setUsageLoading] = useState(false);
  const [health, setHealth] = useState<Awaited<ReturnType<typeof loadAnalyticsHealth>> | null>(null);
  const [hours, setHours] = useState(168);
  const [memberQuery, setMemberQuery] = useState("");
  const [memberEmail, setMemberEmail] = useState("");
  const [memberName, setMemberName] = useState("");
  const [memberLink, setMemberLink] = useState<{ label: string; link: string } | null>(null);
  const [showMemberForm, setShowMemberForm] = useState(false);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const usageLoadVersion = useRef(0);
  // Drops console responses superseded by a newer refresh (window switch or
  // post-mutation) so a slow older payload cannot paint the wrong window.
  const [refreshGuard] = useState(() => new ResponseVersion());

  const refresh = useCallback(async () => {
    const version = refreshGuard.begin();
    try {
      const [ranking, entries, analyticsHealth] = await Promise.all([loadConsolePrincipals(hours), loadConsoleAudit(), loadAnalyticsHealth()]);
      if (!refreshGuard.isCurrent(version)) return;
      setPrincipals(ranking.principals);
      setAudit(entries);
      setHealth(analyticsHealth);
      setError("");
      setSelected((current) => {
        const requestedID = queryValue("member") || current?.id;
        return requestedID ? ranking.principals.find((item) => item.id === requestedID) || ranking.principals[0] || null : ranking.principals[0] || null;
      });
    } catch (cause) {
      if (!refreshGuard.isCurrent(version)) return;
      setError(cause instanceof Error ? cause.message : "Unable to load console");
    }
  }, [hours, refreshGuard]);

  useEffect(() => { refresh(); }, [refresh]);
  useEffect(() => {
    if (selected) updateURL({ member: selected.id }, "replace");
  }, [selected]);
  useEffect(() => {
    const restoreMember = () => {
      const requestedID = queryValue("member");
      setSelected(requestedID ? principals.find((item) => item.id === requestedID) || principals[0] || null : principals[0] || null);
    };
    window.addEventListener("popstate", restoreMember);
    return () => window.removeEventListener("popstate", restoreMember);
  }, [principals]);
  useEffect(() => {
    const version = ++usageLoadVersion.current;
    if (!selected) {
      setUsage([]);
      setUsageLoading(false);
      return;
    }

    setUsage([]);
    setUsageLoading(true);
    loadConsolePrincipalUsage(selected.id, hours)
      .then((result) => {
        if (version === usageLoadVersion.current) setUsage(result.hourly);
      })
      .catch((cause) => {
        if (version === usageLoadVersion.current) setError(cause instanceof Error ? cause.message : "Unable to load principal usage");
      })
      .finally(() => {
        if (version === usageLoadVersion.current) setUsageLoading(false);
      });

    return () => { usageLoadVersion.current++; };
  }, [selected, hours]);

  const changeStatus = async (target: ConsolePrincipal) => {
    const status = target.status === "active" ? "suspended" : "active";
    if (!window.confirm(`${status === "suspended" ? "Suspend" : "Restore"} ${target.note || target.display_name || target.id}? ${status === "suspended" ? "Their clients will lose pool access until restored." : "Their active clients will regain pool access."}`)) return;

    setBusy(`status:${target.id}`);
    try {
      await setPrincipalStatus(target.id, status);
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Status change failed");
    } finally {
      setBusy("");
    }
  };

  const changeEffortCap = async (target: ConsolePrincipal, cap: string) => {
    setBusy(`effort:${target.id}`);
    try {
      await setPrincipalReasoningEffort(target.id, cap);
      setSelected((curr) => (curr && curr.id === target.id ? { ...curr, max_reasoning_effort: cap } : curr));
      setPrincipals((list) => list.map((p) => (p.id === target.id ? { ...p, max_reasoning_effort: cap } : p)));
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Failed to update reasoning effort cap");
    } finally {
      setBusy("");
    }
  };

  const issueMemberLink = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("onboard");
    try {
      const result = await createMemberLink(memberEmail, memberName, "onboard");
      setMemberLink({ label: `Invite link for ${memberName || memberEmail}`, link: result.link });
      setMemberEmail("");
      setMemberName("");
      setShowMemberForm(false);
      setError("");
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to create member link");
    } finally {
      setBusy("");
    }
  };

  const issueRecovery = async (target: ConsolePrincipal) => {
    if (!target.email) {
      setError("This member has no email address to recover.");
      return;
    }

    setBusy(`recover:${target.id}`);
    try {
      const result = await createMemberLink(target.email, target.display_name ?? "", "recover");
      setMemberLink({ label: `Recovery link for ${target.display_name || target.email}`, link: result.link });
      setError("");
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to create recovery link");
    } finally {
      setBusy("");
    }
  };

  const total = principals.reduce((sum, item) => sum + item.billable_tokens, 0);
  const normalizedMemberQuery = memberQuery.trim().toLowerCase();
  const filteredPrincipals = principals.filter((item) => !normalizedMemberQuery || [item.note, item.display_name, item.email, item.username, item.id, item.kind, item.status].some((value) => value?.toLowerCase().includes(normalizedMemberQuery)));
  const chartData = usage.map((row) => ({ hour: row.hour, tokens: row.billable_tokens, cost: row.api_equivalent_cost_usd }));
  const windowLabel = hours === 24 ? "24 hours" : hours === 168 ? "7 days" : hours === 720 ? "30 days" : "1 year";
  const auditLabel = (action: string) => action.split(".").map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");

  return <section className="view-stack members-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    <div className="view-title">
      <h1>Members</h1>
      {principal.kind === "operator" && !showMemberForm && <button className="quiet-button" onClick={() => setShowMemberForm(true)}>Add member</button>}
    </div>
    {showMemberForm && principal.kind === "operator" && <div className="member-admin">
      <form className="access-form" onSubmit={issueMemberLink}>
        <label><span>Email</span><input type="email" value={memberEmail} onChange={(event) => setMemberEmail(event.target.value)} required /></label>
        <label><span>Name</span><input value={memberName} onChange={(event) => setMemberName(event.target.value)} maxLength={48} /></label>
        <button className="gold-button" disabled={Boolean(busy)}>{busy === "onboard" ? "Creating…" : "Create invite link"}</button>
        <button type="button" className="quiet-button" onClick={() => setShowMemberForm(false)}>Cancel</button>
      </form>
    </div>}
    {memberLink && <div className="setup-secret member-link-result" role="status"><span>{memberLink.label}</span><code>{memberLink.link}</code><CopyButton text={memberLink.link} label="Copy link" /><small>Expires in 30 minutes. Send privately; it works once.</small></div>}
    <div className="console-toolbar">
      <div><strong>{principals.length}</strong><span>Members and guests</span></div>
      <div><strong>{formatTokens(total)}</strong><span>Tokens in window</span></div>
      <label><span>Window</span><select value={hours} onChange={(event) => setHours(Number(event.target.value))}><option value={24}>24 hours</option><option value={168}>7 days</option><option value={720}>30 days</option><option value={8760}>1 year</option></select></label>
      <div className={classNames("analytics-health", health?.health.state.toLowerCase())}><strong>{health?.health.state || "…"}</strong><span>Outbox {health?.health.outbox_depth ?? "–"}</span></div>
    </div>
    {health?.active_gap && <div className="accounting-gap" role="alert"><strong>Accounting gap open since {new Date(health.active_gap.started_at).toLocaleString()}</strong><span>Traffic is still being served. Totals spanning this interval are incomplete.</span></div>}
    {health?.health.state === "FAULTED" && <div className="accounting-gap" role="alert"><strong>Analytics reconciliation failed</strong><span>{health.health.fault || health.health.last_reconciliation?.detail}</span></div>}
    <label className="member-search"><span>Search members</span><input value={memberQuery} onChange={(event) => setMemberQuery(event.target.value)} placeholder="Name, email, role, or status" /></label>
    <div className="console-layout">
      <div className="principal-roster" role="list" aria-label="Members and guests ranked by usage">
        {filteredPrincipals.length === 0 && <div className="empty-state">{principals.length === 0 ? "No members or guests yet." : "No members match this search."}</div>}
        {filteredPrincipals.map((item, index) => {
          const primary = item.note || item.display_name || item.email || item.id;
          const secondary = item.email && item.email !== primary ? item.email : `${item.kind} · ${item.id.slice(0, 8)}`;
          return <button className={classNames("principal-row", selected?.id === item.id && "selected", item.status !== "active" && "inactive")} key={item.id} onClick={() => { updateURL({ member: item.id }, "push"); setSelected(item); }} aria-label={`Open ${primary}`}>
            <span className="rank">{String(index + 1).padStart(2, "0")}</span>
            <span className="passport-avatar small">{item.avatar_url ? <img src={item.avatar_url} alt="" /> : (item.display_name || item.email || "G").slice(0, 2).toUpperCase()}</span>
            <span className="principal-copy">
              <strong>{primary}</strong>
              <small>{secondary}</small>
              {item.max_reasoning_effort && <small className="effort-cap-badge">cap: {item.max_reasoning_effort}</small>}
            </span>
            <span className="principal-usage"><strong>{formatTokens(item.billable_tokens)}</strong><small>{preciseMoney.format(item.api_equivalent_cost_usd)}</small></span>
            <span className={classNames("pass-status", item.status !== "active" && "inactive")}>{item.status !== "active" ? item.status : ""}</span>
          </button>;
        })}
      </div>
      <aside className="principal-detail">
        {!selected ? <div className="empty-state">Select a member or guest to view usage.</div> : <>
          <div className="detail-heading">
            <div><span>{selected.kind}</span><h3>{selected.note || selected.display_name || selected.id}</h3><p>{selected.display_name || selected.email || selected.id}</p></div>
            {principal.kind === "operator" && selected.kind !== "operator" && <div className="detail-actions">
              {selected.kind === "member" && <button className="quiet-button" disabled={busy === `recover:${selected.id}`} onClick={() => issueRecovery(selected)}>{busy === `recover:${selected.id}` ? "Creating…" : "Recovery link"}</button>}
              <button className={selected.status === "active" ? "danger-action" : "quiet-button"} disabled={busy === `status:${selected.id}`} onClick={() => changeStatus(selected)}>{busy === `status:${selected.id}` ? "Updating…" : selected.status === "active" ? "Suspend" : "Restore"}</button>
            </div>}
          </div>
          {principal.kind === "operator" && <div className="effort-cap-row">
            <label>
              <span>Reasoning effort cap</span>
              <select
                value={selected.max_reasoning_effort || ""}
                disabled={busy === `effort:${selected.id}`}
                onChange={(event) => changeEffortCap(selected, event.target.value)}
                aria-label="Codex reasoning effort cap"
              >
                <option value="">No cap (Default / Unrestricted)</option>
                <option value="none">none (Reasoning disabled)</option>
                <option value="minimal">minimal</option>
                <option value="low">low</option>
                <option value="medium">medium</option>
                <option value="high">high</option>
                <option value="xhigh">xhigh</option>
                <option value="max">max</option>
              </select>
            </label>
            {busy === `effort:${selected.id}` && <small className="saving-indicator">Saving…</small>}
            {selected.max_reasoning_effort && <button
              type="button"
              className="quiet-button"
              disabled={busy === `effort:${selected.id}`}
              onClick={() => changeEffortCap(selected, "")}
            >
              Remove cap
            </button>}
          </div>}
          <div className="detail-facts"><span>Last seen <b>{selected.last_seen_at ? new Date(selected.last_seen_at).toLocaleDateString() : "Never"}</b></span><span>Requests <b>{selected.request_count.toLocaleString()}</b></span><span>Value <b>{preciseMoney.format(selected.api_equivalent_cost_usd)}</b></span>{selected.expires_at && <span>Expires <b>{new Date(selected.expires_at).toLocaleDateString()}</b></span>}</div>
          {principal.kind === "operator" && <PolicyEditor key={selected.id} principalID={selected.id} />}
          <SignalPanel title={`Usage · ${windowLabel}`}>{usageLoading ? <div className="empty-state">Loading usage…</div> : chartData.length ? <div className="chart-stage medium"><AreaChart data={chartData} config={{ tokens: { label: "Tokens", color: "orange" } }} margins={{ left: 52, bottom: 34 }}><Grid horizontal /><Area dataKey="tokens" variant="hatched" isClickable /><XAxis dataKey="hour" tickFormatter={(value) => String(value).slice(5, 13)} maxTicks={7} /><YAxis tickFormatter={(value) => compact.format(Number(value))} /><Tooltip /></AreaChart></div> : <div className="empty-state">No usage in this period.</div>}</SignalPanel>
        </>}
      </aside>
    </div>
    {principal.kind === "operator" && <OperatorSharing recipients={principals} recipientID={selected?.id} />}
    <details className="audit-disclosure">
      <summary>Audit log <span>{audit.length}</span></summary>
      <div className="audit-list" role="log" aria-label="Recent account actions">{audit.length === 0 ? <div className="empty-state">No actions recorded.</div> : audit.slice(0, 50).map((entry) => <div key={entry.id}><time>{new Date(entry.at).toLocaleString()}</time><strong>{auditLabel(entry.action)}</strong><small>{entry.detail || "No additional detail."}</small><details><summary>Technical details</summary><code>Actor {entry.actor_id} · Subject {entry.subject_id}</code></details></div>)}</div>
    </details>
  </section>;
}
