import { useEffect, useState } from "react";
import { loadQwenTokenPlan, refreshQwenTokenPlan, saveQwenTokenPlan, type QwenTokenPlanConfig } from "../api";
import type { TokenPlanCredits } from "../types";
import { Instrument } from "../ui";

export interface QwenTokenPlanDraft {
  mode: "none" | "personal_manual" | "team_api" | "personal_cloud";
  plan: string;
  total: string;
  remaining: string;
  cycleStart: string;
  cycleEnd: string;
  accessKeyID: string;
  accessKeySecret: string;
  securityToken: string;
  seatID: string;
}

export const emptyQwenTokenPlan = (): QwenTokenPlanDraft => ({ mode: "none", plan: "", total: "", remaining: "", cycleStart: "", cycleEnd: "", accessKeyID: "", accessKeySecret: "", securityToken: "", seatID: "" });

export function qwenTokenPlanConfig(draft: QwenTokenPlanDraft): QwenTokenPlanConfig | null {
  if (draft.mode === "none") return null;
  if (draft.mode === "team_api" || draft.mode === "personal_cloud") {
    if (!draft.accessKeyID.trim() || !draft.accessKeySecret.trim() || draft.mode === "team_api" && !draft.seatID.trim()) throw new Error(draft.mode === "team_api" ? "AccessKey ID, AccessKey secret and seat ID are required." : "Alibaba Cloud AccessKey ID and AccessKey secret are required.");
    return { mode: draft.mode, access_key_id: draft.accessKeyID.trim(), access_key_secret: draft.accessKeySecret.trim(), security_token: draft.securityToken.trim(), ...(draft.mode === "team_api" ? { seat_id: draft.seatID.trim() } : {}) };
  }
  const total = Number(draft.total), remaining = Number(draft.remaining);
  const start = new Date(draft.cycleStart), end = new Date(draft.cycleEnd);
  if (!draft.total.trim() || !draft.remaining.trim() || !Number.isFinite(total) || !Number.isFinite(remaining) || total <= 0 || remaining < 0 || remaining > total) throw new Error("Enter a positive credit allowance and a remaining balance between zero and the allowance.");
  if (!Number.isFinite(start.valueOf()) || !Number.isFinite(end.valueOf()) || start.valueOf() > Date.now() || end.valueOf() <= Date.now() || end <= start) throw new Error("Enter the current subscription cycle start and end.");
  return { mode: draft.mode, plan: draft.plan.trim(), total_credits: total, remaining_credits: remaining, cycle_start: start.toISOString(), cycle_end: end.toISOString() };
}

export function QwenTokenPlanFields({ draft, onChange }: { draft: QwenTokenPlanDraft; onChange: (draft: QwenTokenPlanDraft) => void }) {
  const field = (key: keyof QwenTokenPlanDraft, value: string) => onChange({ ...draft, [key]: value });
  return <fieldset className="qwen-token-plan-fields"><legend>Token Plan credit tracking</legend>
    <p>Personal and Team keys share the same prefix. The model API key does not reveal your edition or credit balance.</p>
    <label className="contribution-field"><span>Credit source</span><select value={draft.mode} onChange={event => onChange({ ...emptyQwenTokenPlan(), mode: event.target.value as QwenTokenPlanDraft["mode"] })}>
      <option value="none">Not configured</option><option value="personal_manual">Personal — manual balance</option><option value="personal_cloud">Personal — Alibaba Cloud account</option><option value="team_api">Team — automatic balance</option>
    </select></label>
    {draft.mode === "personal_manual" && <>
      <p>Copy your current monthly allowance and remaining credits from the Alibaba Cloud console. These values stay manual until you update them. Add-on bundles are separate from this subscription allowance.</p>
      <label className="contribution-field"><span>Plan name</span><input value={draft.plan} onChange={event => field("plan", event.target.value)} /></label>
      <label className="contribution-field"><span>Cycle credit allowance</span><input type="number" min="0" step="any" required value={draft.total} onChange={event => field("total", event.target.value)} /></label>
      <label className="contribution-field"><span>Credits remaining</span><input type="number" min="0" step="any" required value={draft.remaining} onChange={event => field("remaining", event.target.value)} /></label>
      <label className="contribution-field"><span>Subscription cycle start</span><input type="datetime-local" required value={draft.cycleStart} onChange={event => field("cycleStart", event.target.value)} /></label>
      <label className="contribution-field"><span>Subscription cycle end</span><input type="datetime-local" required value={draft.cycleEnd} onChange={event => field("cycleEnd", event.target.value)} /></label>
    </>}
    {(draft.mode === "team_api" || draft.mode === "personal_cloud") && <>
      <p>{draft.mode === "team_api" ? "Connect an Alibaba Cloud AccessKey with permission to read this seat. Shared add-on packages are separate from this seat allowance." : "Connect your Alibaba Cloud account using an AccessKey with Token Plan read access and permission to generate a CLI access token. The plan, monthly allowance and usage are read automatically."} Credentials are stored in the encrypted account vault.</p>
      <a href="https://ram.console.alibabacloud.com/profile/access-keys" target="_blank" rel="noreferrer">Open Alibaba Cloud AccessKey management</a>
      <label className="contribution-field"><span>Alibaba Cloud AccessKey ID</span><input autoComplete="off" required value={draft.accessKeyID} onChange={event => field("accessKeyID", event.target.value)} /></label>
      <label className="contribution-field"><span>Alibaba Cloud AccessKey secret</span><input type="password" autoComplete="off" required value={draft.accessKeySecret} onChange={event => field("accessKeySecret", event.target.value)} /></label>
      <label className="contribution-field"><span>Security token (STS only)</span><input type="password" autoComplete="off" value={draft.securityToken} onChange={event => field("securityToken", event.target.value)} /></label>
      {draft.mode === "team_api" && <label className="contribution-field"><span>Token Plan seat ID</span><input required value={draft.seatID} onChange={event => field("seatID", event.target.value)} /></label>}
    </>}
  </fieldset>;
}

const creditNumber = new Intl.NumberFormat(undefined, { maximumFractionDigits: 4 });

export function QwenCreditBalance({ credits, error }: { credits?: TokenPlanCredits | null; error?: string }) {
  if (!credits) return <section aria-label="Token Plan credits"><p>{error || "Token Plan credit balance is unavailable."}</p></section>;
  if (Date.parse(credits.cycle_end) <= Date.now()) return <section aria-label="Token Plan credits"><p>This credit snapshot belongs to an expired cycle. Update your Token Plan balance.</p></section>;
  return <section aria-label="Token Plan credits">
    <p>Token Plan {credits.edition === "personal" ? "Personal" : "Team"}{credits.plan ? ` · ${credits.plan}` : ""} · {credits.source === "manual" ? "Manual balance" : "Automatic balance"}</p>
    <div className="inspector-metrics">
      <Instrument label="Credits used" value={creditNumber.format(credits.used)} />
      <Instrument label="Credits remaining" value={creditNumber.format(credits.remaining)} accent />
      <Instrument label="Cycle allowance" value={creditNumber.format(credits.total)} />
      <Instrument label="Subscription used" value={`${(credits.used / credits.total * 100).toFixed(1)}%`} />
    </div>
    <p>Cycle: {new Date(credits.cycle_start).toLocaleString()} – {new Date(credits.cycle_end).toLocaleString()}</p>
    <small>Updated {new Date(credits.updated_at).toLocaleString()}. {credits.source === "manual" ? "Update this balance from the console when usage changes." : "Checked automatically every 5 minutes."} Add-on credits are separate.</small>
  </section>;
}

function localDate(value?: string) {
  if (!value) return "";
  const date = new Date(value);
  return Number.isFinite(date.valueOf()) ? new Date(date.valueOf() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16) : "";
}

export function QwenCreditManager({ id, onChanged }: { id: string; onChanged?: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  return <section className="governance-panel"><button type="button" className="quiet-button" aria-expanded={open} onClick={() => setOpen(!open)}>Manage Token Plan credits</button>{open && <QwenCreditForm key={id} id={id} onChanged={onChanged} />}</section>;
}

function QwenCreditForm({ id, onChanged }: { id: string; onChanged?: () => Promise<void> }) {
  const [draft, setDraft] = useState(emptyQwenTokenPlan);
  const [credits, setCredits] = useState<TokenPlanCredits | null>(null);
  const [balanceError, setBalanceError] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [savedSource, setSavedSource] = useState<QwenTokenPlanDraft["mode"]>("none");
  useEffect(() => {
    let active = true;
    loadQwenTokenPlan(id).then(view => {
      if (!active) return;
      const plan = view.token_plan;
      setDraft({ ...emptyQwenTokenPlan(), mode: plan?.mode ?? "none", plan: plan?.plan ?? "", total: plan?.mode === "personal_manual" ? String(plan.total_credits) : "", remaining: plan?.mode === "personal_manual" ? String(plan.remaining_credits ?? 0) : "", cycleStart: localDate(plan?.cycle_start), cycleEnd: localDate(plan?.cycle_end), seatID: plan?.seat_id ?? "" });
      setCredits(view.credits); setBalanceError(view.error); setSavedSource(plan?.mode ?? "none"); setLoaded(true);
    }).catch(cause => { if (active) setError(cause instanceof Error ? cause.message : "Could not load Token Plan credits"); }).finally(() => { if (active) setBusy(false); });
    return () => { active = false; };
  }, [id]);
  const submit = async (refresh = false) => {
    setBusy(true); setError("");
    try {
      const view = refresh ? await refreshQwenTokenPlan(id) : await saveQwenTokenPlan(id, qwenTokenPlanConfig(draft));
      setCredits(view.credits); setBalanceError(view.error); setSavedSource(view.token_plan?.mode ?? "none");
      setDraft(current => ({ ...current, accessKeyID: "", accessKeySecret: "", securityToken: "" }));
      await onChanged?.();
    } catch (cause) { setError(cause instanceof Error ? cause.message : "Could not update Token Plan credits"); if (refresh) { setCredits(null); setBalanceError("Automatic credit refresh failed."); } }
    finally { setBusy(false); }
  };
  return <form onSubmit={event => { event.preventDefault(); void submit(); }}>
    <QwenCreditBalance credits={credits} error={balanceError} />
    {loaded && <QwenTokenPlanFields draft={draft} onChange={setDraft} />}
    {error && <p role="alert">{error}</p>}
    {loaded && <div className="row-actions"><button type="submit" disabled={busy}>Save credit tracking</button>{(savedSource === "team_api" || savedSource === "personal_cloud") && <button type="button" disabled={busy} onClick={() => void submit(true)}>{savedSource === "personal_cloud" ? "Refresh Cloud balance" : "Refresh Team balance"}</button>}</div>}
  </form>;
}
