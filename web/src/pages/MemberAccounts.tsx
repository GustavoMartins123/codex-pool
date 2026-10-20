import { useCallback, useEffect, useRef, useState } from "react";
import { loadMemberQuotas, type MemberModelQuota, type MemberQuotaWindow } from "../api";
import { providerDisplay } from "../ui";
import type { Provider } from "../types";
import { ModelCompatibility } from "../components/ModelCompatibility";

function QuotaWindow({ label, window, total }: { label: string; window: MemberQuotaWindow | null; total: number }) {
  return <div className="member-quota-window"><span>{label}</span>{window ? <>
    <strong>{window.remaining_percent.toLocaleString(undefined, { maximumFractionDigits: 1 })}% <small>available</small></strong>
    <meter min={0} max={100} value={window.remaining_percent} aria-label={`${label} available quota`} />
    <small>{window.reported_accounts} of {total} shared accounts reporting · Updated {new Date(window.updated_at).toLocaleTimeString()}</small>
  </> : <p>Quota unavailable <small>No recent percentage reported by the provider.</small></p>}</div>;
}

export function MemberAccounts() {
  const [models, setModels] = useState<MemberModelQuota[] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [provider, setProvider] = useState("");
  const request = useRef<AbortController | null>(null);
  const version = useRef(0);
  const refresh = useCallback(async () => {
    const current = ++version.current;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    // Previously authorized percentages disappear while permissions are rechecked.
    setModels(null); setLoading(true); setError("");
    try {
      const result = await loadMemberQuotas(controller.signal);
      if (current === version.current) setModels(result.models);
    } catch (cause) {
      if (current === version.current) setError(cause instanceof Error ? cause.message : "Unable to load quotas");
    } finally {
      if (current === version.current) { setLoading(false); request.current = null; }
    }
  }, []);
  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => { if (document.visibilityState === "visible" && !request.current) void refresh(); }, 30_000);
    const focus = () => { void refresh(); };
    const visibility = () => {
      if (document.visibilityState === "visible") void refresh();
      else { version.current++; request.current?.abort(); request.current = null; setModels(null); }
    };
    window.addEventListener("focus", focus);
    document.addEventListener("visibilitychange", visibility);
    return () => { version.current++; request.current?.abort(); window.clearInterval(timer); window.removeEventListener("focus", focus); document.removeEventListener("visibilitychange", visibility); };
  }, [refresh]);
  const providers = [...new Set(models?.map(model => model.provider))];
  const shown = models?.filter(model => !provider || model.provider === provider);
  return <section className="view-stack member-accounts">
    <div className="view-title"><h1>Accounts</h1><button className="quiet-button" disabled={loading} onClick={() => void refresh()}>Refresh quotas</button></div>
    <p>Available quota for your permitted models. Percentages are averages across shared accounts that reported recent data. Models from the same provider may share quota; this is not a personal budget.</p>
    {error && <div className="signal-error" role="alert">{error}</div>}
    {loading && <p role="status">Checking your access and quotas…</p>}
    {models && models.length === 0 && <div className="empty-state"><h2>No models available</h2><p>Ask the operator to allow a provider and share an account with the models you need.</p></div>}
    {models && models.length > 0 && <>
      <label className="member-quota-filter">Provider<select value={provider} onChange={event => setProvider(event.target.value)}><option value="">All permitted providers</option>{providers.map(value => <option key={value} value={value}>{providerDisplay(value as Provider).label}</option>)}</select></label>
      {shown?.length === 0 && <p>No permitted models for this provider.</p>}
      <div className="member-quota-grid">{shown?.map(model => <article className="member-quota-card" key={`${model.provider}/${model.model}`}>
        <header><span>{providerDisplay(model.provider as Provider).label}</span><h2>{model.name || model.model}</h2><code>{model.model}</code><p>{model.available_now ? "Available now" : "Currently unavailable"}</p></header>
        <QuotaWindow label="Primary window" window={model.primary} total={model.supporting_accounts} />
        <QuotaWindow label="Secondary window" window={model.secondary} total={model.supporting_accounts} />
        <ModelCompatibility verification={model.compatibility} />
      </article>)}</div>
    </>}
  </section>;
}
