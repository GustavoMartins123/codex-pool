import { createPass, loadPasses, restorePass, revokePass, rotatePassLink, updatePass } from "../api";
import { ResponseVersion } from "../response-version";
import { type GuestPass } from "../types";
import { CopyButton, classNames } from "../ui";
import { type FormEvent, useCallback, useEffect, useRef, useState } from "react";

export function shouldShowPassFormOnLoad(passes: GuestPass[]) {
  return passes.length === 0;
}


function localDateTime(value: string) {
  const date = new Date(value);
  const pad = (part: number) => String(part).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}


export function Passes() {
  const [passes, setPasses] = useState<GuestPass[]>([]);
  const [note, setNote] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [expiry, setExpiry] = useState("");
  const [editing, setEditing] = useState<GuestPass | null>(null);
  const [fresh, setFresh] = useState<{ link: string } | null>(null);
  const [showForm, setShowForm] = useState(false);
  const initialLoad = useRef(true);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Drops pass-list responses superseded by a newer refresh so a pre-revoke
  // snapshot cannot repaint a revoked pass as active.
  const [refreshGuard] = useState(() => new ResponseVersion());
  const refresh = useCallback(async () => {
    const version = refreshGuard.begin();
    try {
      const items = await loadPasses();
      if (!refreshGuard.isCurrent(version)) return;
      setPasses(items);
      if (initialLoad.current) {
        setShowForm(shouldShowPassFormOnLoad(items));
        initialLoad.current = false;
      }
      setError("");
    } catch (cause) {
      if (!refreshGuard.isCurrent(version)) return;
      setError(cause instanceof Error ? cause.message : "Unable to load passes. Try again.");
    } finally {
      if (refreshGuard.isCurrent(version)) setLoading(false);
    }
  }, [refreshGuard]);
  useEffect(() => { refresh(); }, [refresh]);
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      const expiresAt = expiry ? new Date(expiry).toISOString() : null;
      if (editing) {
        await updatePass(editing.id, note, displayName, expiresAt);
      } else {
        const result = await createPass(note, displayName, expiresAt);
        setFresh({ link: result.link });
      }
      setEditing(null); setNote(""); setDisplayName(""); setExpiry(""); setShowForm(false); await refresh();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Unable to save pass. Try again."); }
    finally { setBusy(false); }
  };
  const beginEdit = (pass: GuestPass) => { setEditing(pass); setNote(pass.note); setDisplayName(pass.display_name || ""); setExpiry(pass.expires_at ? localDateTime(pass.expires_at) : ""); setShowForm(true); };
  const act = async (action: () => Promise<unknown>) => {
    if (busy) return;
    setBusy(true);
    setError("");
    try { await action(); await refresh(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Pass action failed. Try again."); }
    finally { setBusy(false); }
  };
  return <section className="view-stack passes-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    <div className="view-title">
      <h1>Guest passes</h1>
      <p>Share access with an optional expiry.</p>
      {!showForm && <button className="gold-button" onClick={() => { setEditing(null); setNote(""); setDisplayName(""); setExpiry(""); setShowForm(true); }}>Create a pass</button>}
    </div>
    {fresh && <div className="setup-secret"><code>{window.location.origin + fresh.link}</code><CopyButton text={window.location.origin + fresh.link} label="Copy link" /></div>}
    {showForm && <form className="pass-form" onSubmit={submit}>
      <label><span>Who is this for?</span><textarea value={note} onChange={(e) => setNote(e.target.value)} maxLength={300} placeholder="Dave from climbing" required /></label>
      <label><span>Name (optional)</span><input value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={48} placeholder="Dave" /></label>
      <label><span>Expiry (optional)</span><input type="datetime-local" value={expiry} onChange={(e) => setExpiry(e.target.value)} /></label>
      <div className="join-actions"><button className="gold-button" disabled={busy}>{busy ? "Saving…" : editing ? "Save changes" : "Create pass"}</button><button type="button" className="quiet-button" onClick={() => { setShowForm(false); setEditing(null); }}>Cancel</button></div>
    </form>}
    <div className="pass-list" role="list" aria-label="Guest passes">
      {loading ? <div className="empty-state" role="status">Loading passes…</div> : passes.length === 0 ? (!showForm && <div className="empty-state">{error ? <button className="quiet-button" onClick={refresh}>Retry loading passes</button> : "No passes yet."}</div>) : passes.map((pass) => <article className="pass-row" role="listitem" key={pass.id}>
        <div className="passport-avatar small">{pass.avatar_url ? <img src={pass.avatar_url} alt="" /> : <span>{(pass.display_name || "G").slice(0, 2).toUpperCase()}</span>}</div>
        <div className="pass-identity"><strong>{pass.note}</strong><span>{pass.display_name || pass.id.slice(0, 8)}</span><small>{pass.expires_at ? `Expires ${new Date(pass.expires_at).toLocaleDateString()}` : "No expiry"} · {pass.clients} client{pass.clients === 1 ? "" : "s"}</small></div>
        <span className={classNames("pass-status", pass.status !== "active" && "inactive")}>{pass.status !== "active" ? pass.status : ""}</span>
        <div className="row-actions"><CopyButton text={window.location.origin + pass.link} label="Copy link" /><button disabled={busy || loading} onClick={() => beginEdit(pass)}>Edit</button><button disabled={busy || loading} onClick={() => window.confirm(`Replace the invitation link for ${pass.note}? The old link will stop working.`) && act(async () => { const result = await rotatePassLink(pass.id); setFresh({ link: result.link }); })}>Replace link</button>{pass.status === "active" ? <button className="danger-action" disabled={busy || loading} onClick={() => window.confirm(`Revoke access for ${pass.note}? Their ${pass.clients} client${pass.clients === 1 ? "" : "s"} will lose pool access.`) && act(() => revokePass(pass.id))}>Revoke</button> : <button disabled={busy || loading} onClick={() => act(() => restorePass(pass.id))}>Restore</button>}</div>
      </article>)}
    </div>
  </section>;
}
