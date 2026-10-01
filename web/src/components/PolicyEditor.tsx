import { useEffect, useRef, useState } from "react";
import { emptyPolicy, loadPolicies, previewPolicy, savePolicy, type Policy, type PolicyDraft, type PolicyView } from "../governance-api";
import { PolicyFields, validLimits } from "./PolicyFields";

export function PolicyEditor({ principalID }: { principalID: string }) {
  const [open, setOpen] = useState(false);
  return <section className="governance-panel"><button className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>Effective policy</button>{open && <PolicyEditorForm key={principalID} principalID={principalID} />}</section>;
}
export function PolicyEditorForm({ principalID }: { principalID: string }) {
  const [view, setView] = useState<PolicyView | null>(null);
  const [draft, setDraft] = useState<Policy>(emptyPolicy);
  const [client, setClient] = useState("");
  const [target, setTarget] = useState<"principal" | "client">("principal");
  const [model, setModel] = useState("");
  const [provider, setProvider] = useState("");
  const [account, setAccount] = useState("");
  const [tokens, setTokens] = useState(0);
  const [preview, setPreview] = useState<PolicyView | null>(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const version = useRef(0);
  const invalidate = () => { version.current++; setPreview(null); };
  const load = async (credential = client) => {
    const epoch = ++version.current;
    setBusy("load"); setPreview(null); setError(""); setView(null);
    try {
      const result = await loadPolicies(principalID, credential);
      if (epoch !== version.current) return;
      setView(result);
      setDraft(target === "client" ? result.clients.find((item) => item.id === credential)!.policy : result.principal_policy);
    } catch (cause) { if (epoch === version.current) setError(cause instanceof Error ? cause.message : "Policy load failed"); }
    finally { if (epoch === version.current) setBusy(""); }
  };
  useEffect(() => { void load(""); return () => { version.current++; }; }, [principalID]);
  const payload = (): PolicyDraft => ({ revision: view!.revision, target, client_id: client, policy: draft, model, provider, account_id: account, tokens });
  const valid = view !== null && validLimits(draft.limits) && Number.isSafeInteger(tokens) && tokens >= 0 && Number.isSafeInteger(draft.priority ?? 0) && (draft.priority ?? 0) >= 0 && (draft.priority ?? 0) <= 100 && (target !== "client" || client !== "");
  const run = async (save: boolean) => {
    if (!valid || (save && !preview)) return;
    const epoch = ++version.current;
    setBusy(save ? "save" : "preview"); setError("");
    try {
      const result = await (save ? savePolicy : previewPolicy)(principalID, payload());
      if (epoch !== version.current) return;
      if (save) { setView(result); setPreview(null); setDraft(target === "principal" ? result.principal_policy : result.clients.find((item) => item.id === client)!.policy); }
      else setPreview(result);
    } catch (cause) { if (epoch === version.current) { setPreview(null); setError(cause instanceof Error ? cause.message : "Policy request failed"); } }
    finally { if (epoch === version.current) setBusy(""); }
  };
  const change = (policy: Policy) => { invalidate(); setDraft(policy); if (busy === "preview") setBusy(""); };
  const shown = preview ?? view;
  return <div className="governance-form">
    {error && <p role="alert" className="access-error">{error}</p>}
    <button className="quiet-button" disabled={Boolean(busy)} onClick={() => void load()}>Reload policy</button>
    {!view ? <p>{busy === "load" ? "Loading policy..." : "Policy unavailable."}</p> : <>
      <p>Revision {view.revision}. Restrictions from every source apply together.</p>
      <div className="governance-fields"><label>Edit scope<select disabled={Boolean(busy)} value={target} onChange={(event) => { invalidate(); const value = event.target.value as "principal" | "client"; setTarget(value); setDraft(value === "principal" ? view.principal_policy : view.clients.find((item) => item.id === client)?.policy ?? emptyPolicy()); }}><option value="principal">Principal</option><option value="client" disabled={!client}>Credential</option></select></label><label>Credential for preview<select value={client} disabled={Boolean(busy)} onChange={(event) => { setClient(event.target.value); void load(event.target.value); }}><option value="">Principal only</option>{view.clients.map((item) => <option key={item.id} value={item.id}>{item.label} ({item.status})</option>)}</select></label></div>
      <PolicyFields value={draft} onChange={change} disabled={busy === "save" || busy === "load"} />
      <label>Sample account ID (optional)<input disabled={busy === "save"} value={account} onChange={(event) => { invalidate(); setAccount(event.target.value); if (busy === "preview") setBusy(""); }} /></label>
      {!valid && <p role="alert">Use nonnegative integers. Token budgets require a reservation.</p>}
      <fieldset disabled={busy === "save"}><legend>Preview a request</legend><div className="governance-fields"><label>Sample model<input value={model} onChange={(event) => { invalidate(); setModel(event.target.value); if (busy === "preview") setBusy(""); }} /></label><label>Sample provider<input value={provider} onChange={(event) => { invalidate(); setProvider(event.target.value); if (busy === "preview") setBusy(""); }} /></label><label>Estimated tokens<input type="number" min="0" step="1" value={tokens} onChange={(event) => { invalidate(); setTokens(event.target.valueAsNumber); if (busy === "preview") setBusy(""); }} /></label></div></fieldset>
      <div className="row-actions"><button disabled={!valid || Boolean(busy)} onClick={() => void run(false)}>Preview policy</button><button className="gold-button" disabled={!valid || !preview || Boolean(busy)} onClick={() => void run(true)}>Save policy</button></div>
      {preview && <p role="status">{preview.allowed ? "Sample request allowed" : "Sample request denied"}{preview.reasons.length > 0 && `: ${preview.reasons.join("; ")}`}</p>}
      {shown && <><details><summary>Effective restrictions</summary><pre>{JSON.stringify(shown.effective, null, 2)}</pre></details><details><summary>Policy sources</summary>{shown.sources.map((source) => <div key={source.source}><strong>{source.source}</strong><pre>{JSON.stringify(source.policy, null, 2)}</pre></div>)}</details><div aria-label="Policy budget usage">{shown.usage.map((usage) => <p key={usage.scope}>{usage.scope}: {usage.inflight} in flight; today {usage.day.requests} requests, {usage.day.tokens} tokens, {usage.day.reserved_tokens ?? 0} reserved; month {usage.month.requests} requests, {usage.month.tokens} tokens, {usage.month.reserved_tokens ?? 0} reserved.</p>)}</div></>}
    </>}
  </div>;
}
