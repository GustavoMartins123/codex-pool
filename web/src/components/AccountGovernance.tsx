import { useEffect, useId, useRef, useState } from "react";
import { createGrant, updateGrant, loadControls, loadSharing, loadShareableAccounts, revokeGrant, saveControls, setDelegation, type Grant, type Controls, type ControlView, type Limits, type SharingView } from "../governance-api";
import { BudgetFields, validLimits } from "./PolicyFields";
import { loadModelCatalog } from "../api";
import type { ConsolePrincipal, ModelDescriptor } from "../types";
import { PolicyModelSelector } from "./PolicyModelSelector";

function localExpiry(value: string) {
  const date = new Date(value);
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
}
const activeGrant = (grant: Grant) => !grant.revoked_at && Date.parse(grant.expires_at) > Date.now();

type SharingRecipients = { recipients?: ConsolePrincipal[]; recipientID?: string };

export function AccountGovernance({ provider, id, sharingOnly = false, onChanged }: { provider: string; id: string; sharingOnly?: boolean; onChanged?: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  return <section className="governance-panel"><button className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>{sharingOnly ? "Manage sharing" : "Controls and sharing"}</button>{open && <AccountGovernanceForm key={`${provider}:${id}`} provider={provider} id={id} sharingOnly={sharingOnly} onChanged={onChanged} />}</section>;
}
export function AccountGovernanceForm({ provider, id, sharingOnly = false, onChanged, recipients, recipientID = "" }: { provider: string; id: string; sharingOnly?: boolean; onChanged?: () => Promise<void> } & SharingRecipients) {
  const [tab, setTab] = useState(sharingOnly ? "sharing" : "controls");
  const [control, setControl] = useState<ControlView | null>(null);
  const [draft, setDraft] = useState<Controls>({ state: "enabled", max_concurrent: 0 });
  const [sharing, setSharing] = useState<SharingView | null>(null);
  const [reason, setReason] = useState("");
  const [recipient, setRecipient] = useState(recipientID);
  const [editingID, setEditingID] = useState("");
  const [models, setModels] = useState<string[]>([]);
  const [catalog, setCatalog] = useState<ModelDescriptor[]>([]);
  const [catalogLoading, setCatalogLoading] = useState(false);
  const [catalogError, setCatalogError] = useState("");
  const [catalogAttempt, setCatalogAttempt] = useState(0);
  const [expires, setExpires] = useState("");
  const [budget, setBudget] = useState<Limits>({ daily_requests: 100 });
  const [grantReason, setGrantReason] = useState("");
  const [armed, setArmed] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState("");
  const version = useRef(0);
  const grantAttempt = useRef<{ payload: string; id: string } | null>(null);
  const grantHelpID = useId();
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
  useEffect(() => {
    if (tab !== "sharing") return;
    const controller = new AbortController();
    setCatalogLoading(true); setCatalogError(""); setCatalog([]);
    loadModelCatalog(controller.signal).then((value) => {
      if (!controller.signal.aborted) setCatalog(value.models.filter((model) => model.provider === provider));
    }).catch((cause) => {
      if (!controller.signal.aborted) setCatalogError(cause instanceof Error ? cause.message : "Could not load models");
    }).finally(() => { if (!controller.signal.aborted) setCatalogLoading(false); });
    return () => controller.abort();
  }, [tab, provider, id, catalogAttempt]);
  const recipientGrants = sharing?.grants.filter((grant) => grant.recipient_id === recipient.trim() && activeGrant(grant)) ?? [];
  const editingGrant = recipientGrants.find((grant) => grant.id === editingID) ?? recipientGrants[0];
  useEffect(() => {
    if (!sharing) return;
    const grant = editingGrant;
    setModels(grant ? [...grant.models] : []);
    setBudget(grant ? { ...grant.budget } : { daily_requests: 100 });
    setExpires(grant ? localExpiry(grant.expires_at) : "");
    setGrantReason(grant?.reason ?? "");
    grantAttempt.current = null;
  }, [sharing, recipient, editingGrant?.id]);
  const submitGrant = () => {
    if (!sharing || !grantValid) return;
    const value = { revision: sharing.revision, recipient_id: recipient.trim(), models, budget, expires_at: new Date(expires).toISOString(), reason: grantReason.trim() };
    if (editingGrant) {
      void mutate(async () => {
        setSharing(await updateGrant(provider, id, { ...value, id: editingGrant.id, grant_revision: editingGrant.revision }));
      }, "Account access updated");
      return;
    }
    const payload = JSON.stringify(value);
    if (grantAttempt.current?.payload !== payload) grantAttempt.current = { payload, id: crypto.randomUUID() };
    const grantID = grantAttempt.current.id;
    void mutate(async () => {
      setSharing(await createGrant(provider, id, { ...value, id: grantID }));
      grantAttempt.current = null; setRecipient(""); setModels([]); setExpires(""); setGrantReason("");
    }, "Account grant created");
  };
  const mutate = async (operation: () => Promise<void>, message: string) => {
    const epoch = ++version.current; setBusy(true); setError(""); setSuccess("");
    try { await operation(); if (epoch !== version.current) return; setSuccess(message); if (onChanged) await onChanged(); }
    catch (cause) { if (epoch === version.current) setError(cause instanceof Error ? cause.message : "Account update failed"); }
    finally { if (epoch === version.current) setBusy(false); }
  };
  const grantRequirements: string[] = [];
  if (!recipient.trim()) grantRequirements.push(recipients ? "Select a recipient user." : "Enter a recipient principal ID.");
  else if (recipients && !recipients.some((item) => item.id === recipient && item.status === "active" && (!item.expires_at || Date.parse(item.expires_at) > Date.now()))) grantRequirements.push("Select an active recipient user.");
  if (!models.length) grantRequirements.push("Select at least one granted model.");
  if (!Number.isFinite(Date.parse(expires)) || Date.parse(expires) <= Date.now()) grantRequirements.push("Set Expires at to a future date and time.");
  if (!grantReason.trim()) grantRequirements.push("Enter a grant reason.");
  if (!Object.values(budget).every((value) => Number.isSafeInteger(value) && value >= 0)) grantRequirements.push("Use nonnegative whole numbers for all budget limits.");
  if ((Number(budget.daily_tokens) > 0 || Number(budget.monthly_tokens) > 0) && !(Number(budget.token_reservation) > 0)) grantRequirements.push("Set a positive token reservation for the token budget.");
  if (![budget.daily_requests, budget.monthly_requests, budget.daily_tokens, budget.monthly_tokens].some((value) => Number(value) > 0)) grantRequirements.push("Set a positive daily or monthly request or token budget.");
  const grantValid = sharing !== null && grantRequirements.length === 0 && validLimits(budget);
  return <div className="governance-form">
    {!sharingOnly && <div className="row-actions"><button disabled={busy} aria-pressed={tab === "controls"} onClick={() => setTab("controls")}>Account controls</button><button disabled={busy} aria-pressed={tab === "sharing"} onClick={() => setTab("sharing")}>Sharing</button></div>}
    {error && <p role="alert" className="access-error">{error}</p>}{success && <p role="status">{success}</p>}
    <button className="quiet-button" disabled={busy} onClick={() => void load()}>Reload account settings</button>
    {busy && !control && !sharing && <p>Loading account settings...</p>}
    {tab === "controls" && control && <form onSubmit={(event) => { event.preventDefault(); void mutate(async () => { const result = await saveControls(provider, id, control.revision, draft, reason); setControl(result); setDraft(result.controls); setReason(""); }, "Account controls saved"); }}><p>Revision {control.revision}; {control.inflight} active requests.</p><fieldset disabled={busy}><legend>Persistent account controls</legend><div className="governance-fields"><label>Account mode<select value={draft.state} onChange={(event) => setDraft({ ...draft, state: event.target.value as Controls["state"] })}><option value="enabled">Enabled</option><option value="disabled">Disabled</option><option value="maintenance">Maintenance</option><option value="draining">Draining</option></select></label><label>Account concurrency<input type="number" min="0" max="10000" step="1" value={draft.max_concurrent} onChange={(event) => setDraft({ ...draft, max_concurrent: event.target.valueAsNumber })} /></label><label>Change reason<input required maxLength={240} value={reason} onChange={(event) => setReason(event.target.value)} /></label></div><p>Draining keeps existing conversation pins. Maintenance and disabled block new requests. Zero concurrency means unlimited.</p><button className="gold-button" disabled={!reason.trim() || !Number.isSafeInteger(draft.max_concurrent) || draft.max_concurrent < 0 || draft.max_concurrent > 10000}>Save account controls</button></fieldset></form>}
    {tab === "sharing" && sharing && <><p>Revision {sharing.revision}. Sharing never transfers credentials or ownership.</p>{sharing.owner && <label className="governance-consent"><input type="checkbox" checked={sharing.operator_may_delegate} disabled={busy} onChange={(event) => { const allowed = event.target.checked; void mutate(async () => setSharing(await setDelegation(provider, id, sharing.revision, allowed)), "Delegation consent updated"); }} />Allow operators to create grants for this account</label>}
      {editingGrant && <aside aria-label="Current account access"><p>Editing saved account access. Saving replaces this grant's models, limits and expiry; accumulated usage is preserved.</p><p>Current shared models: {editingGrant.models.join(", ")}. Expires {new Date(editingGrant.expires_at).toLocaleString()}.</p></aside>}
      {recipientGrants.length > 1 && <label>Existing account access<select disabled={busy} value={editingGrant?.id ?? ""} onChange={(event) => setEditingID(event.target.value)}>{recipientGrants.map((grant) => <option key={grant.id} value={grant.id}>{grant.models.join(", ")} · {grant.reason}</option>)}</select></label>}
      <form onSubmit={(event) => { event.preventDefault(); submitGrant(); }}><fieldset disabled={busy}><legend>{editingGrant ? "Edit account grant" : "Create account grant"}</legend><div className="governance-fields">{recipients ? <label>Recipient user<select required value={recipient} onChange={(event) => setRecipient(event.target.value)}><option value="">Select a user</option>{recipients.filter((item) => item.status === "active" && (!item.expires_at || Date.parse(item.expires_at) > Date.now())).map((item) => <option key={item.id} value={item.id}>{item.display_name || item.username || item.email || item.note || item.id} · {item.email || item.kind}</option>)}</select></label> : <label>Recipient principal ID<input required value={recipient} onChange={(event) => setRecipient(event.target.value)} /></label>}<label>Expires at<input type="datetime-local" step="1" required value={expires} onChange={(event) => setExpires(event.target.value)} /></label><label>Grant reason<input required maxLength={240} value={grantReason} onChange={(event) => setGrantReason(event.target.value)} /></label></div>{!recipients && <p>To select a user by name, use Members → Share provider accounts.</p>}{catalogError && <p role="alert">{catalogError} <button type="button" onClick={() => setCatalogAttempt((value) => value + 1)}>Retry models</button></p>}<PolicyModelSelector label="Granted models" values={models} models={catalog} loading={catalogLoading} includeAliases={false} onChange={setModels} /><BudgetFields value={budget} onChange={setBudget} /><div className="grant-save-actions">
        <div id={grantHelpID} className="grant-save-help" aria-live="polite">
          {grantRequirements.length > 0 ? <><p>To save account access:</p><ul>{grantRequirements.map((requirement) => <li key={requirement}>{requirement}</li>)}</ul></> : <p>Ready to save. This user will receive access to the selected models with these limits.</p>}
        </div>
        <button type="submit" className="gold-button" disabled={busy || !grantValid} aria-describedby={grantHelpID}>{busy ? "Saving account access…" : "Save account access"}</button>
      </div></fieldset></form>
      <div aria-label="Account grants">{sharing.grants.length === 0 ? <p>No grants.</p> : sharing.grants.map((grant) => <article className="client-card" key={grant.id}><strong>{recipients?.find((item) => item.id === grant.recipient_id)?.display_name || grant.recipient_id}</strong><p>{grant.models.join(", ")} · expires {new Date(grant.expires_at).toLocaleString()}</p><p>{grant.reason}</p><details><summary>Grant budget</summary><pre>{JSON.stringify(grant.budget, null, 2)}</pre></details>{grant.revoked_at ? <span>Revoked</span> : Date.parse(grant.expires_at) <= Date.now() ? <span>Expired</span> : armed === grant.id ? <div className="row-actions"><button className="danger-action" disabled={busy} onClick={() => void mutate(async () => { await revokeGrant(grant.id, grant.revision); setSharing(await loadSharing(provider, id)); setArmed(""); }, "Grant revoked; new requests are blocked")}>Confirm revoke</button><button disabled={busy} onClick={() => setArmed("")}>Cancel revoke</button></div> : <div className="row-actions"><button disabled={busy} onClick={() => { setRecipient(grant.recipient_id); setEditingID(grant.id); }}>Edit account access</button><button className="danger-action" disabled={busy} onClick={() => setArmed(grant.id)}>Revoke grant</button></div>}</article>)}</div>
    </>}
  </div>;
}
export function OperatorSharing({ recipients, recipientID }: SharingRecipients) {
  const [accounts, setAccounts] = useState<{ id: string; provider: string; owner_id: string }[]>([]);
  const [selected, setSelected] = useState("");
  const [open, setOpen] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => { if (!open) return; let active = true; loadShareableAccounts().then((value) => { if (active) { setAccounts(value); setError(""); } }).catch((cause) => { if (active) setError(cause instanceof Error ? cause.message : "Sharing unavailable"); }); return () => { active = false; }; }, [open]);
  const account = accounts.find((item) => `${item.provider}:${item.id}` === selected);
  return <section className="governance-panel"><button className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>Share provider accounts</button>{open && <><p>Choose an account, a user and the models to share. Setup tokens and policies do not grant account access.</p>{error && <p role="alert">{error}</p>}<label>Shareable account<select value={selected} onChange={(event) => setSelected(event.target.value)}><option value="">Select an account</option>{accounts.map((item) => <option key={`${item.provider}:${item.id}`} value={`${item.provider}:${item.id}`}>{item.provider} · {item.id} · {item.owner_id || "Operator managed"}</option>)}</select></label>{account && <AccountGovernanceForm key={`${selected}:${recipientID ?? ""}`} provider={account.provider} id={account.id} sharingOnly recipients={recipients} recipientID={recipientID} />}</>}</section>;
}
