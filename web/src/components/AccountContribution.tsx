import { antigravityOAuthStatus, contributeAPIKey, contributeGrok, exchangeAccountOAuth, exchangeAntigravityOAuth, exchangeAntigravityRelogin, exchangeCodexRelogin, startAccountOAuth, startAntigravityOAuth, startAntigravityRelogin, startCodexRelogin, startZAILogin, zaiLoginStatus } from "../api";
import { type FormEvent, useEffect, useRef, useState } from "react";

type ContributableProvider = "codex" | "claude" | "antigravity" | "kimi" | "minimax" | "zai" | "zai_key" | "xiaomi" | "grok" | "opencode_go";


const CONTRIBUTION_PROVIDERS: Array<{ id: ContributableProvider; label: string; mode: "oauth" | "key" | "json" }> = [
  { id: "codex", label: "Codex", mode: "oauth" },
  { id: "claude", label: "Claude", mode: "oauth" },
	  { id: "antigravity", label: "Google Antigravity", mode: "oauth" },
  { id: "kimi", label: "Kimi", mode: "key" },
  { id: "minimax", label: "MiniMax", mode: "key" },
  { id: "zai", label: "Z.ai", mode: "oauth" },
  { id: "zai_key", label: "Z.ai API key", mode: "key" },
  { id: "xiaomi", label: "Xiaomi", mode: "key" },
  { id: "grok", label: "Grok", mode: "json" },
  { id: "opencode_go", label: "OpenCode Go", mode: "key" },
];


function oauthCode(value: string, expectedState: string | undefined) {
  const text = value.trim();
  if (!text) throw new Error("Authorization code is required");
  if (/^https?:/.test(text)) {
    const callback = new URL(text);
    const code = callback.searchParams.get("code");
    if (!code || !expectedState || callback.searchParams.get("state") !== expectedState) throw new Error("Callback does not match this sign-in session");
    return code;
  }
  return text;
}

export function AccountContribution({ onClose, onAdded, reloginAccountID, reloginProvider = "codex" }: { onClose: () => void; onAdded: () => Promise<void>; reloginAccountID?: string; reloginProvider?: "codex" | "antigravity" }) {
  const [provider, setProvider] = useState<ContributableProvider>(reloginAccountID ? reloginProvider : "codex");
  const [credential, setCredential] = useState("");
	  const [oauth, setOAuth] = useState<{ verifier?: string; sessionID?: string; state?: string; url: string } | null>(null);
	  const oauthCompleted = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const selected = CONTRIBUTION_PROVIDERS.find((candidate) => candidate.id === provider)!;

	  useEffect(() => {
	    if (provider !== "antigravity" || !oauth?.sessionID) return;
	    let stopped = false;
	    const complete = async () => {
	      if (stopped || oauthCompleted.current) return;
	      oauthCompleted.current = true;
	      await onAdded();
	    };
	    let polling = false;
	    const timer = window.setInterval(async () => {
	      if (polling) return;
	      polling = true;
	      try {
	        const status = await antigravityOAuthStatus(oauth.sessionID!);
	        if (stopped) return;
	        if (status.status === "complete") { window.clearInterval(timer); await complete(); }
	        if (status.status === "error") { window.clearInterval(timer); setError(status.error || "Google sign-in failed"); }
	      } catch (cause) { if (!stopped) { window.clearInterval(timer); setError(cause instanceof Error ? cause.message : "Google sign-in status failed"); } }
	      finally { polling = false; }
	    }, 1200);
	    return () => { stopped = true; window.clearInterval(timer);  };
	  }, [oauth?.sessionID, onAdded, provider]);

  useEffect(() => {
    if (provider !== "zai" || !oauth?.sessionID) return;
    let stopped = false;
    let polling = false;
    const timer = window.setInterval(async () => {
      if (polling) return;
      polling = true;
      try {
        const result = await zaiLoginStatus(oauth.sessionID!);
        if (stopped) return;
        if (result.status === "complete") {
          window.clearInterval(timer);
          await onAdded();
        } else if (result.status === "error") {
          window.clearInterval(timer);
          setError(result.error || "Z.ai login failed");
        }
      } catch (cause) {
        if (!stopped) {
          window.clearInterval(timer);
          setError(cause instanceof Error ? cause.message : "Z.ai login status failed");
        }
      } finally {
        polling = false;
      }
    }, 2000);
    return () => { stopped = true; window.clearInterval(timer); };
  }, [oauth?.sessionID, onAdded, provider]);

	  const choose = (next: ContributableProvider) => {
	    oauthCompleted.current = false;
    setProvider(next);
    setCredential("");
    setOAuth(null);
    setError("");
  };

  const startOAuth = async () => {
    // Reserve the tab while the click is still a trusted user gesture. Opening
    // it after the network response is commonly blocked as a popup.
    const authorizationWindow = window.open("about:blank", "_blank");
	    if (authorizationWindow && provider !== "antigravity") authorizationWindow.opener = null;
    setBusy(true);
    setError("");
    try {
	      const result = provider === "zai"
	        ? await startZAILogin()
	        : provider === "antigravity"
	        ? reloginAccountID
	          ? await startAntigravityRelogin(reloginAccountID)
	          : await startAntigravityOAuth()
	        : reloginAccountID
	          ? await startCodexRelogin(reloginAccountID)
	          : await startAccountOAuth(provider as "codex" | "claude");
	      if (!result.oauth_url || (provider !== "zai" && !result.state) || ((provider === "antigravity" || provider === "zai") ? !result.session_id : !result.verifier)) throw new Error("Provider did not return an OAuth session");
	      oauthCompleted.current = false;
	      setOAuth({ verifier: result.verifier, sessionID: result.session_id, state: result.state, url: result.oauth_url });
      authorizationWindow?.location.replace(result.oauth_url);
    } catch (cause) {
      authorizationWindow?.close();
      setError(cause instanceof Error ? cause.message : "Could not start OAuth");
    } finally {
      setBusy(false);
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (selected.mode === "oauth") {
        if (!oauth) {
          await startOAuth();
          return;
        }
	        if (provider === "zai") {
	          throw new Error("Finish Z.ai sign-in in the opened page");
	        } else if (provider === "antigravity") {
	          if (!oauth.sessionID || !credential.trim()) throw new Error("Paste the authorization code or callback URL");
	          if (reloginAccountID) {
	            await exchangeAntigravityRelogin(oauth.sessionID, credential, oauth.state!);
	          } else {
	            await exchangeAntigravityOAuth(oauth.sessionID, credential, oauth.state!);
	          }
	        } else {
	          const code = oauthCode(credential, oauth.state);
	          if (!code || !oauth.verifier) throw new Error("Paste the authorization code or callback URL");
	          if (reloginAccountID) {
	            await exchangeCodexRelogin(code, oauth.verifier);
	          } else {
	            await exchangeAccountOAuth(provider as "codex" | "claude", code, oauth.verifier);
	          }
	        }
      } else if (selected.mode === "json") {
        await contributeGrok(credential);
      } else {
        await contributeAPIKey(provider === "zai_key" ? "zai" : provider as "kimi" | "minimax" | "xiaomi" | "opencode_go", credential);
      }
      setCredential("");
      await onAdded();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Account contribution failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="operator-backdrop" role="presentation" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <form className="operator-dialog contribution-dialog" onSubmit={submit} role="dialog" aria-modal="true" aria-labelledby="contribution-title">
        <h2 id="contribution-title">{reloginAccountID ? reloginProvider === "antigravity" ? "Relogin / revalidate account" : "Relogin account" : "Add a pool account"}</h2>
        <p>{reloginAccountID
          ? "Sign in with the same upstream account to replace its credentials. The pool account stays the same."
          : "This account is private and can only be used by you."}</p>
        {!reloginAccountID && <div className="contribution-providers" aria-label="Provider">
          {CONTRIBUTION_PROVIDERS.map((candidate) => <button type="button" key={candidate.id} className={provider === candidate.id ? "active" : ""} disabled={busy} aria-pressed={provider === candidate.id} onClick={() => choose(candidate.id)}>{candidate.label}</button>)}
        </div>}
        {selected.mode === "oauth" ? (
          <div className="contribution-oauth">
            {!oauth ? (
              <button type="button" className="oauth-launch" disabled={busy} onClick={startOAuth}>{busy ? "Opening…" : `Sign in to ${selected.label}`}</button>
            ) : (
              <>
                <a href={oauth.url} target="_blank" rel="noreferrer">Open sign-in page</a>
                {provider === "zai" ? <p>Finish signing in on the Z.ai page. Your Coding Plan will be checked automatically.</p> : <label className="contribution-field"><span>Authorization code or callback URL</span><input value={credential} onChange={(event) => setCredential(event.target.value)} autoFocus autoComplete="off" required /></label>}
              </>
            )}
          </div>
        ) : selected.mode === "json" ? (
          <label className="contribution-field"><span>Grok auth JSON</span><textarea value={credential} onChange={(event) => setCredential(event.target.value)} autoFocus spellCheck={false} required /></label>
        ) : (
          <label className="contribution-field"><span>{provider === "zai_key" ? "Z.ai API key" : `${selected.label} API key`}</span><input type="password" value={credential} onChange={(event) => setCredential(event.target.value)} autoFocus autoComplete="off" required /></label>
        )}
        {error && <div className="access-error" role="alert">{error}</div>}
        <div><button type="button" onClick={onClose}>Cancel</button>{(selected.mode !== "oauth" || oauth) && provider !== "zai" && <button className="gold-button" disabled={busy}>{busy ? "Adding…" : "Add to pool"}</button>}</div>
      </form>
    </div>
  );
}
