import { AgentsApiError, decidePairing, listPairings, listRunners, revokeRunner, type PairingReview, type Runner } from "../agents-api";
import { ResponseVersion } from "../response-version";
import { classNames } from "../ui";
import { useCallback, useEffect, useState } from "react";

type RunnerAction = "deny" | "revoke";
type ArmedRunnerAction = { id: string; kind: RunnerAction } | null;

export function isArmedRunnerAction(action: ArmedRunnerAction, id: string, kind: RunnerAction) {
  return action?.id === id && action.kind === kind;
}

export function pairingCountdown(expiresAt: string, now: number) {
  const remaining = Math.floor((new Date(expiresAt).getTime() - now) / 1000);
  if (!Number.isFinite(remaining) || remaining <= 0) return "Expired";
  const minutes = Math.floor(remaining / 60);
  const seconds = remaining % 60;
  return `Expires in ${minutes}:${String(seconds).padStart(2, "0")}`;
}

export function Runners() {
  const [runners, setRunners] = useState<Runner[]>([]);
  const [pairings, setPairings] = useState<PairingReview[]>([]);
  const [codes, setCodes] = useState<Record<string, string>>({});
  const [codeError, setCodeError] = useState<{ id: string; text: string } | null>(null);
  const [armed, setArmed] = useState<ArmedRunnerAction>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [message, setMessage] = useState<{ tone: "success" | "error"; text: string } | null>(null);
  const [now, setNow] = useState(() => Date.now());
  const [refreshGuard] = useState(() => new ResponseVersion());
  const refresh = useCallback(async () => {
    const version = refreshGuard.begin();
    try {
      const [runnerItems, pairingItems] = await Promise.all([listRunners(), listPairings()]);
      if (!refreshGuard.isCurrent(version)) return;
      setRunners(runnerItems);
      setPairings(pairingItems);
      setError("");
    } catch (cause) {
      if (!refreshGuard.isCurrent(version)) return;
      setError(cause instanceof Error ? cause.message : "Unable to load runners. Try again.");
    } finally {
      if (refreshGuard.isCurrent(version)) setLoading(false);
    }
  }, [refreshGuard]);
  useEffect(() => { refresh(); }, [refresh]);
  useEffect(() => {
    const tick = () => { if (document.visibilityState === "visible") void refresh(); };
    const timer = window.setInterval(tick, 15_000);
    document.addEventListener("visibilitychange", tick);
    return () => { window.clearInterval(timer); document.removeEventListener("visibilitychange", tick); };
  }, [refresh]);
  useEffect(() => {
    if (pairings.length === 0) return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [pairings.length]);

  const decide = async (pairing: PairingReview, decision: "approve" | "deny") => {
    if (busy) return;
    setBusy(true);
    setError("");
    setCodeError(null);
    setMessage(null);
    try {
      await decidePairing(pairing.id, decision, (codes[pairing.id] ?? "").trim());
      setArmed(null);
      setCodes((current) => { const next = { ...current }; delete next[pairing.id]; return next; });
      setMessage({ tone: "success", text: decision === "approve" ? `Approved ${pairing.name}. It can connect once runnerd finishes pairing.` : `Denied the pairing request from ${pairing.name}.` });
      await refresh();
    } catch (cause) {
      if (cause instanceof AgentsApiError && cause.status === 403) {
        setCodeError({ id: pairing.id, text: cause.message });
      } else if (cause instanceof AgentsApiError && (cause.status === 409 || cause.status === 410)) {
        setMessage({ tone: "error", text: cause.message });
        await refresh();
      } else {
        setError(cause instanceof Error ? cause.message : "Pairing action failed. Try again.");
      }
    } finally {
      setBusy(false);
    }
  };
  const revoke = async (runner: Runner) => {
    if (busy) return;
    setBusy(true);
    setError("");
    setMessage(null);
    try {
      await revokeRunner(runner.id);
      setArmed(null);
      setMessage({ tone: "success", text: `Revoked ${runner.name}. Its active sessions will close.` });
      await refresh();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to revoke the runner. Try again.");
    } finally {
      setBusy(false);
    }
  };
  const armOrRun = (id: string, kind: RunnerAction, run: () => Promise<void>) => {
    if (busy) return;
    if (!isArmedRunnerAction(armed, id, kind)) { setArmed({ id, kind }); return; }
    void run();
  };

  return <section className="view-stack runners-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    {message && <div className={classNames("account-message", message.tone === "error" && "error")} role={message.tone === "error" ? "alert" : "status"}>{message.text}</div>}
    <div className="view-title">
      <h1>Runners</h1>
      <p>Machines that execute agent sessions for your account. Start <code>runnerd pair</code> on a machine, then approve it here with the code it prints.</p>
    </div>
    <h2 className="panel-title">Pending pairings</h2>
    {loading ? <div className="empty-state" role="status">Loading runners…</div> : pairings.length === 0 ? <div className="empty-state">No pending pairings.</div> : <div className="pairing-list" role="list" aria-label="Pending pairings">
      {pairings.map((pairing) => <article className="pairing-card" role="listitem" key={pairing.id}>
        <div className="pairing-identity">
          <strong>{pairing.name}</strong>
          <small>{pairing.os}/{pairing.arch}{pairing.runner_id ? " · replaces a paired runner" : ""}</small>
          <small>{pairingCountdown(pairing.expires_at, now)}</small>
        </div>
        <label className="pairing-code"><span>Machine code</span><input value={codes[pairing.id] ?? ""} onChange={(event) => setCodes((current) => ({ ...current, [pairing.id]: event.target.value }))} placeholder="Code shown by runnerd" autoComplete="off" /></label>
        {codeError?.id === pairing.id && <div className="signal-error" role="alert">{codeError.text}</div>}
        <div className="row-actions">
          <button className="gold-button" disabled={busy || !(codes[pairing.id] ?? "").trim()} onClick={() => void decide(pairing, "approve")}>Approve</button>
          <button className={classNames("danger-action", isArmedRunnerAction(armed, pairing.id, "deny") && "confirm")} disabled={busy} onClick={() => armOrRun(pairing.id, "deny", () => decide(pairing, "deny"))}>{isArmedRunnerAction(armed, pairing.id, "deny") ? "Confirm deny" : "Deny"}</button>
        </div>
      </article>)}
    </div>}
    <h2 className="panel-title">Paired runners</h2>
    {loading ? null : runners.length === 0 ? <div className="empty-state">{error ? <button className="quiet-button" onClick={refresh}>Retry loading runners</button> : "No runners paired yet."}</div> : <div className="runner-list" role="list" aria-label="Runners">
      {runners.map((runner) => <article className={classNames("runner-row", runner.revoked && "inactive")} role="listitem" key={runner.id}>
        <div className="runner-identity">
          <strong>{runner.name}</strong>
          <small>{runner.os}/{runner.arch} · {(runner.agents ?? []).join(", ") || "no agents"} · paired {new Date(runner.created_at).toLocaleDateString()}</small>
        </div>
        <span className={classNames("state", runner.revoked ? "dead" : runner.online ? "online" : "offline")}>{runner.revoked ? "revoked" : runner.online ? "online" : "offline"}</span>
        <div className="row-actions">
          {!runner.revoked && <button className={classNames("danger-action", isArmedRunnerAction(armed, runner.id, "revoke") && "confirm")} disabled={busy} onClick={() => armOrRun(runner.id, "revoke", () => revoke(runner))}>{isArmedRunnerAction(armed, runner.id, "revoke") ? "Confirm revoke" : "Revoke"}</button>}
        </div>
        {isArmedRunnerAction(armed, runner.id, "revoke") && <small className="runner-warning">Active sessions on this machine will close.</small>}
      </article>)}
    </div>}
  </section>;
}
