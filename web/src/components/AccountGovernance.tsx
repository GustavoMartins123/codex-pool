import { useEffect, useRef, useState } from "react";
import { createGrant, loadControls, loadSharing, loadShareableAccounts, revokeGrant, saveControls, setDelegation, type Controls, type ControlView, type Limits, type SharingView } from "../governance-api";
import { BudgetFields, selectors, validLimits } from "./PolicyFields";

export function AccountGovernance({ provider, id, sharingOnly = false, onChanged }: { provider: string; id: string; sharingOnly?: boolean; onChanged?: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  return <section className="governance-panel"><button className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>{sharingOnly ? "Manage sharing" : "Controls and sharing"}</button>{open && <AccountGovernanceForm key={`${provider}:${id}`} provider={provider} id={id} sharingOnly={sharingOnly} onChanged={onChanged} />}</section>;
}
export function AccountGovernanceForm({ provider, id, sharingOnly = false, onChanged }: { provider: string; id: string; sharingOnly?: boolean; onChanged?: () => Promise<void> }) {
  const [tab, setTab] = useState(sharingOnly ? "sharing" : "controls");
  const [control, setControl] = useState<ControlView | null>(null);
  const [draft, setDraft] = useState<Controls>({ state: "enabled", max_concurrent: 0 });
  const [sharing, setSharing] = useState<SharingView | null>(null);
  const [reason, setReason] = useState("");
  const [recipient, setRecipient] = useState("");
  const [models, setModels] = useState("");
  const [expires, setExpires] = useState("");
  const [budget, setBudget] = useState<Limits>({ daily_requests: 100 });
  const [grantReason, setGrantReason] = useState("");
  const [armed, setArmed] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState("");
  const version = useRef(0);
  const grantAttempt = useRef<{ payload: string; id: string } | null>(null);
  const load = async () => {
    const epoch = ++version.current;
    setBusy(true); setError(""); setSuccess(""); setControl(null); setSharing(null);
    try {
      if (tab === "controls") { const result = await loadControls(provider, id); if (epoch === version.current) { setControl(result); setDraft(result.controls); } }
      else { const result = await loadSharing(provider, id); if (epoch === version.current) setSharing(result); }
    } catch (cause) { if (epoch === version.current) setError(cause instanceof Error ? cause.message : "Account load failed"); }
    finally { if (epoch === version.current) setBusy(false); }
  };
  useEffect(() => { void load(); return () => { version.current++; }; }, [provider, id, tab]);
  const mutate = async (operation: () => Promise<void>, message: string) => {
    const epoch = ++version.current; setBusy(true); setError(""); setSuccess("");
    try { await operation(); if (epoch !== version.current) return; setSuccess(message); if (onChanged) await onChanged(); }
    catch (cause) { if (epoch === version.current) setError(cause instanceof Error ? cause.message : "Account update failed"); }
    finally { if (epoch === version.current) setBusy(false); }
  };
  const grantValid = sharing !== null && recipient.trim() !== "" && selectors(models).length > 0 && Number.isFinite(Date.parse(expires)) && Date.parse(expires) > Date.now() && grantReason.trim() !== "" && validLimits(budget) && (Number(budget.daily_requests) > 0 || Number(budget.monthly_requests) > 0 || Number(budget.daily_tokens) > 0 || Number(budget.monthly_tokens) > 0);
  return <div className="governance-form">
    {!sharingOnly && <div className="row-actions"><button disabled={busy} aria-pressed={tab === "controls"} onClick={() => setTab("controls")}>Account controls</button><button disabled={busy} aria-pressed={tab === "sharing"} onClick={() => setTab("sharing")}>Sharing</button></div>}
    {error && <p role="alert" className="access-error">{error}</p>}{success && <p role="status">{success}</p>}
    <button className="quiet-button" disabled={busy} onClick={() => void load()}>Reload account settings</button>
    {busy && !control && !sharing && <p>Loading account settings...</p>}
    {tab === "controls" && control && <form onSubmit={(event) => { event.preventDefault(); void mutate(async () => { const result = await saveControls(provider, id, control.revision, draft, reason); setControl(result); setDraft(result.controls); setReason(""); }, "Account controls saved"); }}><p>Revision {control.revision}; {control.inflight} active requests.</p><fieldset disabled={busy}><legend>Persistent account controls</legend><div className="governance-fields"><label>Account mode<select value={draft.state} onChange={(event) => setDraft({ ...draft, state: event.target.value as Controls["state"] })}><option value="enabled">Enabled</option><option value="disabled">Disabled</option><option value="maintenance">Maintenance</option><option value="draining">Draining</option></select></label><label>Account concurrency<input type="number" min="0" max="10000" step="1" value={draft.max_concurrent} onChange={(event) => setDraft({ ...draft, max_concurrent: event.target.valueAsNumber })} /></label><label>Change reason<input required maxLength={240} value={reason} onChange={(event) => setReason(event.target.value)} /></label></div><p>Draining keeps existing conversation pins. Maintenance and disabled block new requests. Zero concurrency means unlimited.</p><button className="gold-button" disabled={!reason.trim() || !Number.isSafeInteger(draft.max_concurrent) || draft.max_concurrent < 0 || draft.max_concurrent > 10000}>Save account controls</button></fieldset></form>}
    {tab === "sharing" && sharing && <><p>Revision {sharing.revision}. Sharing never transfers credentials or ownership.</p>{sharing.owner && <label className="governance-consent"><input type="checkbox" checked={sharing.operator_may_delegate} disabled={busy} onChange={(event) => { const allowed = event.target.checked; void mutate(async () => setSharing(await setDelegation(provider, id, sharing.revision, allowed)), "Delegation consent updated"); }} />Allow operators to create grants for this account</label>}
      <form onSubmit={(event) => { event.preventDefault(); if (!grantValid) return; const value = { revision: sharing.revision, recipient_id: recipient.trim(), models: selectors(models), budget, expires_at: new Date(expires).toISOString(), reason: grantReason.trim() }; const payload = JSON.stringify(value); if (grantAttempt.current?.payload !== payload) grantAttempt.current = { payload, id: crypto.randomUUID() }; const grantID = grantAttempt.current.id; void mutate(async () => { setSharing(await createGrant(provider, id, { ...value, id: grantID })); grantAttempt.current = null; setRecipient(""); setModels(""); setExpires(""); setGrantReason(""); }, "Account grant created"); }}><fieldset disabled={busy}><legend>Create account grant</legend><div className="governance-fields"><label>Recipient principal ID<input required value={recipient} onChange={(event) => setRecipient(event.target.value)} /></label><label>Granted models<input required placeholder="Comma-separated canonical identifiers" value={models} onChange={(event) => setModels(event.target.value)} /></label><label>Expires at<input type="datetime-local" required value={expires} onChange={(event) => setExpires(event.target.value)} /></label><label>Grant reason<input required maxLength={240} value={grantReason} onChange={(event) => setGrantReason(event.target.value)} /></label></div><BudgetFields value={budget} onChange={setBudget} /><button className="gold-button" disabled={!grantValid}>Create grant</button></fieldset></form>
      <div aria-label="Account grants">{sharing.grants.length === 0 ? <p>No grants.</p> : sharing.grants.map((grant) => <article className="client-card" key={grant.id}><strong>{grant.recipient_id}</strong><p>{grant.models.join(", ")} · expires {new Date(grant.expires_at).toLocaleString()}</p><p>{grant.reason}</p><details><summary>Grant budget</summary><pre>{JSON.stringify(grant.budget, null, 2)}</pre></details>{grant.revoked_at ? <span>Revoked</span> : Date.parse(grant.expires_at) <= Date.now() ? <span>Expired</span> : armed === grant.id ? <div className="row-actions"><button className="danger-action" disabled={busy} onClick={() => void mutate(async () => { await revokeGrant(grant.id, grant.revision); setSharing(await loadSharing(provider, id)); setArmed(""); }, "Grant revoked; new requests are blocked")}>Confirm revoke</button><button disabled={busy} onClick={() => setArmed("")}>Cancel revoke</button></div> : <button className="danger-action" disabled={busy} onClick={() => setArmed(grant.id)}>Revoke grant</button>}</article>)}</div>
    </>}
  </div>;
}
export function OperatorSharing() {
  const [accounts, setAccounts] = useState<{ id: string; provider: string; owner_id: string }[]>([]);
  const [selected, setSelected] = useState("");
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => { if (!open) return; let active = true; loadShareableAccounts().then((value) => { if (active) { setAccounts(value); setError(""); } }).catch((cause) => { if (active) setError(cause instanceof Error ? cause.message : "Sharing unavailable"); }); return () => { active = false; }; }, [open]);
  const account = accounts.find((item) => `${item.provider}:${item.id}` === selected);
  return <section className="governance-panel"><button className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>Share provider accounts</button>{open && <>{error && <p role="alert">{error}</p>}<label>Shareable account<select value={selected} onChange={(event) => setSelected(event.target.value)}><option value="">Select an account</option>{accounts.map((item) => <option key={`${item.provider}:${item.id}`} value={`${item.provider}:${item.id}`}>{item.provider} · {item.id} · {item.owner_id || "Operator managed"}</option>)}</select></label>{account && <AccountGovernance key={selected} provider={account.provider} id={account.id} sharingOnly />}</>}</section>;
}
