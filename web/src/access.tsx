import { beginPasskeyLogin, finishPasskeyLogin, loadAuthConfig, operatorBootstrap, passportLogin, recoverMemberStatus, redeemMemberRecovery } from "./api";
import { recoveryDeadlineMs, recoveryReducer, type RecoveryEvent, type RecoveryLinkState } from "./recovery";
import { ResponseVersion } from "./response-version";
import { type PassportPrincipal } from "./types";
import { classNames } from "./ui";
import { browserSupportsWebAuthn, startAuthentication } from "@simplewebauthn/browser";
import { type FormEvent, type ReactNode, useCallback, useEffect, useState } from "react";

function Threshold({ title, lede, children }: { title: string; lede?: string; children?: ReactNode }) {
  return (
    <div className="threshold">
      <div className="threshold-crest" aria-hidden="true" />
      <div className="threshold-panel">
        <div className="threshold-body">
          <h1>{title}</h1>
          {lede && <p className="threshold-lede">{lede}</p>}
          {children}
        </div>
      </div>
    </div>
  );
}


export function BootScreen() {
  return (
    <div className="threshold booting">
      <div className="threshold-crest" aria-hidden="true" />
      <p className="threshold-booting" role="status">Restoring your session…</p>
    </div>
  );
}


export function JoinUnavailable() {
  return (
    <Threshold title="Pass unavailable" lede="This invitation has expired or been revoked. Ask whoever sent it to issue a new one.">
      <div className="threshold-actions">
        <button className="threshold-submit" onClick={() => { window.history.replaceState(null, "", "/"); window.location.reload(); }}>Go to sign in</button>
      </div>
    </Threshold>
  );
}


export function JoinSwitch({ current, busy, onConfirm, onCancel }: { current: PassportPrincipal; busy: boolean; onConfirm: () => void | Promise<void>; onCancel: () => void }) {
  const who = current.display_name || current.email || current.id.slice(0, 8);
  return (
    <Threshold title="Switch accounts?" lede={`You are signed in as ${who}. Accepting this pass replaces that session in this browser.`}>
      <div className="threshold-actions">
        <button className="threshold-submit" disabled={busy} onClick={onConfirm}>{busy ? "Accepting…" : "Accept the pass"}</button>
        <button type="button" className="threshold-alt" disabled={busy} onClick={onCancel}>Stay signed in as {who}</button>
      </div>
    </Threshold>
  );
}


export function RecoveryUnavailable() {
  return (
    <Threshold title="Recovery link unavailable" lede="This link has expired, was already used, or has been replaced.">
      <div className="threshold-actions">
        <button className="threshold-submit" onClick={() => { window.history.replaceState(null, "", "/"); window.location.reload(); }}>Go to sign in</button>
      </div>
    </Threshold>
  );
}


export function MemberRecovery({ token, onAccess }: { token: string; onAccess: (principal: PassportPrincipal) => void }) {
  const [state, setState] = useState<RecoveryLinkState>({ phase: "checking" });
  // Focus and visibility rechecks can overlap the initial validation; only
  // the most recently started answer may apply, so a slow "valid" response
  // can never resurrect a form after a newer "invalid" one.
  const [validationGuard] = useState(() => new ResponseVersion());
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [confirmationTouched, setConfirmationTouched] = useState(false);
  const mismatch = confirmationTouched && password !== confirmation;

  const applyStatus = useCallback(async (type: RecoveryEvent["type"]) => {
    const version = validationGuard.begin();
    let event: RecoveryEvent;
    try {
      const status = await recoverMemberStatus(token);
      event = { type, valid: status.valid, expiresAt: status.expiresAt };
    } catch {
      event = { type, valid: false };
    }
    if (!validationGuard.isCurrent(version)) return;
    setState((current) => recoveryReducer(current, event));
  }, [token, validationGuard]);
  const validate = useCallback(() => applyStatus("recheck"), [applyStatus]);
  useEffect(() => {
    void applyStatus("status");
    // In-flight answers die with the component or a token change.
    return () => validationGuard.invalidate();
  }, [applyStatus, validationGuard]);

  // Server-provided deadline: once reached locally the form disappears even
  // without a recheck. The countdown is a convenience, never the authority.
  useEffect(() => {
    if (state.phase !== "ready") return;
    const timer = window.setTimeout(() => setState({ phase: "unavailable" }), recoveryDeadlineMs(state.expiresAt));
    return () => window.clearTimeout(timer);
  }, [state]);

  // Tokens can be consumed in another tab: revalidate whenever this tab
  // regains focus or becomes visible again.
  useEffect(() => {
    if (state.phase !== "ready") return;
    const onVisibility = () => { if (document.visibilityState === "visible") void validate(); };
    window.addEventListener("focus", validate);
    document.addEventListener("visibilitychange", onVisibility);
    return () => { window.removeEventListener("focus", validate); document.removeEventListener("visibilitychange", onVisibility); };
  }, [state.phase, validate]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setConfirmationTouched(true);
    if (password !== confirmation) { setError("Passwords must match."); return; }
    setBusy(true); setError("");
    try { onAccess(await redeemMemberRecovery(token, password)); }
    catch (cause) {
      const message = cause instanceof Error ? cause.message : "This recovery link is unavailable.";
      setError(message);
      if (/unavailable/i.test(message)) void validate();
    }
    finally { setBusy(false); }
  };

  if (state.phase === "checking") {
    return (
      <Threshold title="Checking your recovery link…" lede="Verifying that this link is still valid.">
        <div className="threshold-actions"><button className="threshold-submit" disabled>Checking…</button></div>
      </Threshold>
    );
  }
  if (state.phase === "unavailable") return <RecoveryUnavailable />;
  return (
    <Threshold title="Set your password" lede="This link works once and expires 30 minutes after it was issued.">
      <form className="threshold-form" onSubmit={submit}>
        <label className="threshold-field">
          <span>New password<i>12 characters minimum</i></span>
          <input type="password" minLength={12} autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} required autoFocus />
        </label>
        <label className={classNames("threshold-field", mismatch && "invalid")}>
          <span>Confirm password</span>
          <input type="password" minLength={12} autoComplete="new-password" value={confirmation} onChange={(event) => setConfirmation(event.target.value)} onBlur={() => setConfirmationTouched(true)} aria-invalid={mismatch} aria-describedby={mismatch ? "password-match-hint" : undefined} required />
          {mismatch && <em id="password-match-hint" className="threshold-hint">Passwords must match.</em>}
        </label>
        {error && <p className="threshold-error" role="alert">{error}</p>}
        <button className="threshold-submit" disabled={busy}>{busy ? "Setting…" : "Set password"}</button>
      </form>
    </Threshold>
  );
}


export function AccessGate({ onAccess }: { onAccess: (principal: PassportPrincipal) => void }) {
  const [mode, setMode] = useState<"login" | "bootstrap">("login");
  const [email, setEmail] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [bootstrapToken, setBootstrapToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    loadAuthConfig().then((config) => {
      if (!config.operator_exists) setMode("bootstrap");
    }).catch(() => {});
  }, []);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (mode === "bootstrap") {
        const principal = await operatorBootstrap(username, email, password, bootstrapToken, displayName);
        onAccess(principal);
      } else {
        onAccess(await passportLogin(email.trim(), password));
      }
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Access denied");
    } finally {
      setBusy(false);
    }
  };
  const passkey = async () => {
    setBusy(true); setError("");
    try {
      const begin = await beginPasskeyLogin();
      const credential = await startAuthentication({ optionsJSON: begin.options });
      onAccess(await finishPasskeyLogin(begin.challenge_id, credential));
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Passkey sign-in failed");
    } finally { setBusy(false); }
  };

  const passkeyAvailable = mode === "login" && browserSupportsWebAuthn();

  if (mode === "bootstrap") {
    return (
      <Threshold title="Set up this pool" lede="Create the operator account to manage members and pool access.">
        <form onSubmit={submit} className="threshold-form">
          <label className="threshold-field">
            <span>Email</span>
            <input value={email} onChange={(event) => setEmail(event.target.value)} type="email" required autoFocus autoComplete="email" />
          </label>
          <label className="threshold-field">
            <span>Username<i>letters, numbers, . _ -</i></span>
            <input value={username} onChange={(event) => setUsername(event.target.value)} minLength={3} maxLength={32} pattern="[A-Za-z0-9._-]+" required autoComplete="username" />
          </label>
          <label className="threshold-field">
            <span>Display name<i>optional</i></span>
            <input value={displayName} onChange={(event) => setDisplayName(event.target.value)} maxLength={48} autoComplete="name" placeholder="How you appear to members" />
          </label>
          <label className="threshold-field">
            <span>Password<i>12 characters minimum</i></span>
            <input value={password} onChange={(event) => setPassword(event.target.value)} type="password" minLength={12} required autoComplete="new-password" />
          </label>
          <label className="threshold-field">
            <span>Admin token<i>from your server configuration</i></span>
            <input value={bootstrapToken} onChange={(event) => setBootstrapToken(event.target.value)} type="password" required autoComplete="off" />
          </label>
          {error && <p className="threshold-error" role="alert">{error}</p>}
          <button className="threshold-submit" disabled={busy}>{busy ? "Creating…" : "Create operator account"}</button>
        </form>
      </Threshold>
    );
  }

  return (
    <Threshold title="Sign in">
      <form onSubmit={submit} className="threshold-form">
        <label className="threshold-field">
          <span>Username or email</span>
          <input value={email} onChange={(event) => setEmail(event.target.value)} required autoFocus autoComplete="username" />
        </label>
        <label className="threshold-field">
          <span>Password</span>
          <input value={password} onChange={(event) => setPassword(event.target.value)} type="password" required autoComplete="current-password" />
        </label>
        {error && <p className="threshold-error" role="alert">{error}</p>}
        <button className="threshold-submit" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
        {passkeyAvailable && <button type="button" className="threshold-alt" disabled={busy} onClick={passkey}>Use a passkey instead</button>}
      </form>
      <div className="threshold-aside">
        {mode === "login" && <p>Locked out? Ask the operator for a recovery link.</p>}
      </div>
    </Threshold>
  );
}
