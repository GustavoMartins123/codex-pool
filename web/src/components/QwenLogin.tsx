import { useEffect, useRef, useState } from "react";
import { authorizeQwenLogin, completeQwenLogin, startQwenLogin } from "../api";

export function QwenLogin({ accountID, onLinked }: { accountID?: string; onLinked: () => Promise<void> }) {
  const [session, setSession] = useState<{ id: string; url: string } | null>(null);
  const [awaitingKey, setAwaitingKey] = useState(false);
  const [callbackURL, setCallbackURL] = useState("");
  const [linked, setLinked] = useState(false);
  const [apiKey, setAPIKey] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const mounted = useRef(true);
  const completed = useRef(false);
  useEffect(() => { mounted.current = true; return () => { mounted.current = false; }; }, []);

  const start = async () => {
    const authorizationWindow = window.open("about:blank", "_blank");
    if (authorizationWindow) authorizationWindow.opener = null;
    setBusy(true); setError("");
    try {
      const result = await startQwenLogin(accountID);
      if (!mounted.current) { authorizationWindow?.close(); return; }
      if (!result.oauth_url || !result.session_id) throw new Error("Qwen did not return a sign-in session");
      completed.current = false;
      setLinked(false); setAwaitingKey(false); setAPIKey(""); setCallbackURL("");
      setSession({ id: result.session_id, url: result.oauth_url });
      authorizationWindow?.location.replace(result.oauth_url);
    } catch (cause) {
      authorizationWindow?.close();
      if (mounted.current) setError(cause instanceof Error ? cause.message : "Could not start Alibaba Cloud sign-in");
    } finally { if (mounted.current) setBusy(false); }
  };

  const authorize = async () => {
    if (!session || !callbackURL.trim() || busy || completed.current) return;
    setBusy(true); setError("");
    try {
      const result = await authorizeQwenLogin(session.id, callbackURL.trim());
      if (!mounted.current) return;
      if (result.status === "awaiting_key" && !accountID) {
        setAwaitingKey(true); setCallbackURL("");
      } else if (result.status === "complete") {
        completed.current = true;
        setLinked(true); setSession(null); setCallbackURL("");
        await onLinked();
      } else {
        setSession(null);
        throw new Error(result.error || "Alibaba Cloud authorization failed");
      }
    } catch (cause) {
      if (mounted.current) setError(cause instanceof Error ? cause.message : "Could not authorize Alibaba Cloud");
    } finally { if (mounted.current) setBusy(false); }
  };

  const finish = async () => {
    if (!session || !apiKey.trim() || busy || completed.current) return;
    setBusy(true); setError("");
    try {
      const result = await completeQwenLogin(session.id, apiKey.trim());
      if (!mounted.current) return;
      if (result.status !== "complete") {
        setSession(null); setAwaitingKey(false);
        throw new Error(result.error || "Could not link Qwen account");
      }
      completed.current = true;
      setAPIKey(""); setLinked(true); setSession(null); setAwaitingKey(false);
      await onLinked();
    } catch (cause) {
      if (mounted.current) setError(cause instanceof Error ? cause.message : "Could not link Qwen account");
    } finally { if (mounted.current) setBusy(false); }
  };

  return <div className="contribution-oauth">
    {linked && <p role="status">Alibaba Cloud account linked.</p>}
    {!session ? <button type="button" className="oauth-launch" disabled={busy} onClick={() => void start()}>{busy ? "Opening…" : accountID ? linked ? "Reconnect Alibaba Cloud" : "Sign in to Alibaba Cloud" : "Sign in to Qwen"}</button> : <>
      <a href={session.url} target="_blank" rel="noreferrer">Open Alibaba Cloud sign-in page</a>
      {awaitingKey ? <>
        <p>Alibaba Cloud is authorized. Enter your existing Token Plan API key to enable models. Alibaba only shows existing keys masked.</p>
        <label className="contribution-field"><span>Qwen Token Plan API key</span><input type="password" value={apiKey} onChange={event => setAPIKey(event.target.value)} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); void finish(); } }} autoComplete="off" autoFocus /></label>
        <button type="button" className="gold-button" disabled={busy || !apiKey.trim()} onClick={() => void finish()}>{busy ? "Adding…" : "Add to pool"}</button>
      </> : <>
        <p id="qwen-callback-help">After authorizing Alibaba Cloud, copy the full URL from the address bar and paste it here. The local callback page may fail to load; its URL still contains your authorization code.</p>
        <label className="contribution-field"><span>Alibaba Cloud callback URL</span><input value={callbackURL} onChange={event => setCallbackURL(event.target.value)} onKeyDown={event => { if (event.key === "Enter") { event.preventDefault(); void authorize(); } }} aria-describedby="qwen-callback-help" autoComplete="off" spellCheck={false} autoFocus /></label>
        <button type="button" className="gold-button" disabled={busy || !callbackURL.trim()} onClick={() => void authorize()}>{busy ? "Authorizing…" : "Confirm Alibaba Cloud authorization"}</button>
      </>}
      <button type="button" disabled={busy} onClick={() => void start()}>Restart sign-in</button>
    </>}
    {error && <div className="access-error" role="alert">{error}</div>}
  </div>;
}
