import { useEffect, useRef, useState } from "react";
import { completeQwenLogin, qwenLoginStatus, startQwenLogin } from "../api";

export function QwenLogin({ accountID, onLinked }: { accountID?: string; onLinked: () => Promise<void> }) {
  const [session, setSession] = useState<{ id: string; url: string } | null>(null);
  const [awaitingKey, setAwaitingKey] = useState(false);
  const [linked, setLinked] = useState(false);
  const [apiKey, setAPIKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const completed = useRef(false);

  useEffect(() => {
    if (!session || awaitingKey) return;
    let stopped = false;
    let polling = false;
    const timer = window.setInterval(async () => {
      if (polling || completed.current) return;
      polling = true;
      try {
        const result = await qwenLoginStatus(session.id);
        if (stopped) return;
        if (result.status === "complete") {
          window.clearInterval(timer);
          completed.current = true;
          setLinked(true); setSession(null);
          await onLinked();
        } else if (result.status === "awaiting_key") {
          window.clearInterval(timer);
          setAwaitingKey(true);
        } else if (result.status === "error") {
          window.clearInterval(timer);
          setSession(null);
          setError(result.error || "Alibaba Cloud sign-in failed");
        }
      } catch (cause) {
        if (!stopped) {
          window.clearInterval(timer);
          setSession(null);
          setError(cause instanceof Error ? cause.message : "Could not check Alibaba Cloud sign-in");
        }
      } finally {
        polling = false;
      }
    }, 2000);
    return () => { stopped = true; window.clearInterval(timer); };
  }, [awaitingKey, onLinked, session]);

  const start = async () => {
    const authorizationWindow = window.open("about:blank", "_blank");
    if (authorizationWindow) authorizationWindow.opener = null;
    setBusy(true); setError("");
    try {
      const result = await startQwenLogin(accountID);
      if (!result.oauth_url || !result.session_id) throw new Error("Qwen did not return a sign-in session");
      completed.current = false;
      setLinked(false);
      setAwaitingKey(false);
      setAPIKey("");
      setSession({ id: result.session_id, url: result.oauth_url });
      authorizationWindow?.location.replace(result.oauth_url);
    } catch (cause) {
      authorizationWindow?.close();
      setError(cause instanceof Error ? cause.message : "Could not start Alibaba Cloud sign-in");
    } finally { setBusy(false); }
  };

  const finish = async () => {
    if (!session || !apiKey.trim() || busy || completed.current) return;
    setBusy(true); setError("");
    try {
      const result = await completeQwenLogin(session.id, apiKey.trim());
      if (result.status !== "complete") {
        setSession(null); setAwaitingKey(false);
        throw new Error(result.error || "Could not link Qwen account");
      }
      completed.current = true;
      setAPIKey(""); setLinked(true); setSession(null); setAwaitingKey(false);
      await onLinked();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not link Qwen account");
    } finally { setBusy(false); }
  };

  return <div className="contribution-oauth">
    {linked && <p role="status">Alibaba Cloud account linked.</p>}
    {!session ? <button type="button" className="oauth-launch" disabled={busy} onClick={() => void start()}>{busy ? "Opening…" : accountID ? linked ? "Reconnect Alibaba Cloud" : "Sign in to Alibaba Cloud" : "Sign in to Qwen"}</button> : <>
      <a href={session.url} target="_blank" rel="noreferrer">Open Alibaba Cloud sign-in page</a>
      {awaitingKey ? <>
        <p>Alibaba Cloud is authorized. Enter your existing Token Plan API key to enable models. Alibaba only shows existing keys masked.</p>
        <label className="contribution-field"><span>Qwen Token Plan API key</span><input type="password" value={apiKey} onChange={event => setAPIKey(event.target.value)} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); void finish(); } }} autoComplete="off" autoFocus /></label>
        <button type="button" className="gold-button" disabled={busy || !apiKey.trim()} onClick={() => void finish()}>{busy ? "Adding…" : "Add to pool"}</button>
      </> : <p>Finish signing in and authorize access on the Alibaba Cloud page. Token Plan credits will be checked automatically.</p>}
    </>}
    {error && <div className="access-error" role="alert">{error}</div>}
  </div>;
}
