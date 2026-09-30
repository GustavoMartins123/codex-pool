import { antigravityOAuthStatus, contributeAPIKey, contributeGrok, exchangeAccountOAuth, exchangeAntigravityOAuth, exchangeAntigravityRelogin, exchangeCodexRelogin, mutateAccount, reloadAccounts, startAccountOAuth, startAntigravityOAuth, startAntigravityRelogin, startCodexRelogin, startZAILogin, zaiLoginStatus } from "../api";
import { Sparkline } from "../components/dither-kit";
import { type AccountStats, type AdminAccount, type PoolStats, type Provider } from "../types";
import { queryValue, updateURL } from "../navigation";
import { AccountResetWindows, Instrument, ResetCreditExpirations, SignalSkeleton, WeeklyPace, accountThroughput, classNames, compact, formatAPIValue, formatAdmission, formatTokens, money, providerDisplay } from "../ui";
import { type CSSProperties, type FormEvent, useCallback, useEffect, useRef, useState } from "react";

type AccountAction = "enable" | "disable" | "resurrect" | "refresh";

type ArmedAccountAction = { accountID: string; kind: AccountAction } | null;


export function isArmedAccountAction(action: ArmedAccountAction, accountID: string, kind: AccountAction) {
  return action?.accountID === accountID && action.kind === kind;
}


export function Accounts({ stats, adminAccounts, onAccountsChanged }: {
  stats: PoolStats | null;
  adminAccounts: AdminAccount[];
  onAccountsChanged: () => Promise<void>;
}) {
  const [selected, setSelected] = useState<string | null>(() => queryValue("account"));
  const [query, setQuery] = useState("");
  const [attentionOnly, setAttentionOnly] = useState(() => queryValue("accounts") === "attention");
  const [mobileInspector, setMobileInspector] = useState(() => window.matchMedia("(max-width: 760px)").matches);
  const [contributing, setContributing] = useState(false);
  const [reloginAccountID, setReloginAccountID] = useState<string | null>(null);
  const [reloginProvider, setReloginProvider] = useState<"codex" | "antigravity">("codex");
  const [action, setAction] = useState<ArmedAccountAction>(null);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ tone: "success" | "error"; text: string } | null>(null);
  const inspectorRef = useRef<HTMLElement | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const accountTriggerRef = useRef<HTMLButtonElement | null>(null);

  const closeInspector = useCallback(() => {
    updateURL({ account: null }, "replace");
    setSelected(null);
    window.requestAnimationFrame(() => accountTriggerRef.current?.focus());
  }, []);

  useEffect(() => { setAction(null); setMessage(null); }, [selected]);
  useEffect(() => updateURL({ account: selected }, "replace"), [selected]);
  useEffect(() => updateURL({ accounts: attentionOnly ? "attention" : null }, "replace"), [attentionOnly]);
  useEffect(() => {
    const restoreAccountState = () => {
      setSelected(queryValue("account"));
      setAttentionOnly(queryValue("accounts") === "attention");
    };
    window.addEventListener("popstate", restoreAccountState);
    return () => window.removeEventListener("popstate", restoreAccountState);
  }, []);
  useEffect(() => {
    const media = window.matchMedia("(max-width: 760px)");
    const update = () => setMobileInspector(media.matches);
    media.addEventListener("change", update);
    return () => media.removeEventListener("change", update);
  }, []);
  useEffect(() => {
    if (!selected || !mobileInspector) return;

    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    window.requestAnimationFrame(() => closeRef.current?.focus());

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        closeInspector();
        return;
      }
      if (event.key !== "Tab" || !inspectorRef.current) return;

      const focusable = [...inspectorRef.current.querySelectorAll<HTMLElement>('button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')];
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };

    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.body.style.overflow = previousOverflow;
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [closeInspector, mobileInspector, selected]);

  if (!stats) return <SignalSkeleton />;

  const needsAttention = (account: AccountStats) => account.status !== "healthy" || (account.secondary_window_available && account.secondary_window_used_pct >= 80);
  const normalizedQuery = query.trim().toLowerCase();
  const filteredAccounts = stats.accounts.filter((account) => {
    if (attentionOnly && !needsAttention(account)) return false;
    if (!normalizedQuery) return true;
    const provider = providerDisplay(account.type);
    return [provider.label, account.type, account.plan_type, account.id].some((value) => value?.toLowerCase().includes(normalizedQuery));
  });
  const attentionCount = stats.accounts.filter(needsAttention).length;
  const selectedAdmin = adminAccounts.find((account) => account.id === selected) ?? null;
  const selectedAccount = stats.accounts.find((account) => {
    const adminMatch = adminAccounts.find((candidate) => candidate.public_id === account.id);
    return (adminMatch?.id ?? account.id) === selected;
  }) ?? null;
  const selectedVerificationURL = selectedAdmin
    ? safeVerificationURL(selectedAdmin.account_verification_url) || safeVerificationURL(selectedAdmin.verification_url)
    : "";
  const toggleAction: AccountAction | null = selectedAdmin ? selectedAdmin.disabled ? "enable" : "disable" : null;

  const perform = async (nextAction: AccountAction) => {
    if (!selectedAdmin) return;
    if (!isArmedAccountAction(action, selectedAdmin.id, nextAction)) {
      setAction({ accountID: selectedAdmin.id, kind: nextAction });
      return;
    }
    setBusy(true);
    try {
      await mutateAccount(selectedAdmin.id, nextAction);
      setMessage({ tone: "success", text: `${selectedAdmin.id} ${nextAction} complete` });
      setAction(null);
      await onAccountsChanged();
    } catch (cause) {
      setMessage({ tone: "error", text: cause instanceof Error ? cause.message : "Action failed" });
    } finally {
      setBusy(false);
    }
  };

  const reloadPool = async () => {
    setBusy(true);
    setMessage(null);
    try {
      await reloadAccounts();
      await onAccountsChanged();
      setMessage({ tone: "success", text: "Pool accounts reloaded" });
    } catch (cause) {
      setMessage({ tone: "error", text: cause instanceof Error ? cause.message : "Unable to reload pool accounts" });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="signal-view accounts-view">
      <div className="view-title account-title">
        <h1>Accounts</h1>
        <div className="account-title-actions">
          <button className="contribute-button" onClick={() => { setReloginAccountID(null); setReloginProvider("codex"); setContributing(true); }}>Add pool account</button>
          <button className="operator-badge" disabled={busy} onClick={reloadPool}>{busy ? "Reloading…" : "Reload pool"}</button>
        </div>
      </div>
      {message && <div className={classNames("account-message", message.tone)} role="status" aria-live="polite">{message.text}</div>}
      <section className="account-filters" aria-label="Filter accounts">
        <label><span>Search accounts</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Provider, plan, or account ID" /></label>
        <div>
          <button className={!attentionOnly ? "active" : ""} aria-pressed={!attentionOnly} onClick={() => { updateURL({ accounts: null }, "push"); setAttentionOnly(false); }}>All <b>{stats.accounts.length}</b></button>
          <button className={attentionOnly ? "active" : ""} aria-pressed={attentionOnly} onClick={() => { updateURL({ accounts: "attention" }, "push"); setAttentionOnly(true); }}>Needs attention <b>{attentionCount}</b></button>
        </div>
      </section>
      <div className={classNames("accounts-layout", selected && "inspecting")}>
        <div className="account-table" role="list" aria-label="Provider accounts" aria-hidden={mobileInspector && Boolean(selected) ? true : undefined}>
          <div className="account-row account-head" aria-hidden="true">
            <span>Provider / plan / account</span><span>State</span><span>Weekly pace</span><span>Reset windows</span><span>24h burn</span><span>API value</span><span>Trend</span>
          </div>
          {filteredAccounts.length === 0 && <div className="empty-state">{stats.accounts.length === 0 ? "No provider accounts are connected." : "No accounts match this filter."}</div>}
          {filteredAccounts.map((account) => {
            const adminMatch = adminAccounts.find((candidate) => candidate.public_id === account.id);
            const rowID = adminMatch?.id ?? account.id;
            const provider = providerDisplay(account.type);
            return (
              <button
                className={classNames("account-row", selected === rowID && "selected")}
                key={account.id}
                onClick={(event) => { accountTriggerRef.current = event.currentTarget; updateURL({ account: rowID }, "push"); setSelected(rowID); }}
                style={{ "--provider": provider.color } as CSSProperties}
                aria-label={`Open ${provider.label} ${account.plan_type || "account"} details`}
              >
                <span className="account-identity"><i className="provider-mark" aria-hidden="true" /><b>{provider.label}</b><small><em>{account.plan_type || "unknown plan"}</em><span>{adminMatch?.id ?? account.id}</span></small></span>
                <span className={`state ${account.status}`} data-label="State">{account.status === "dead" ? "offline" : account.status === "verification_required" ? "revalidation" : account.status}</span>
                <span className="account-pace" data-label="Weekly pace"><WeeklyPace account={account} /></span>
                <span className="account-windows" data-label="Reset windows">
                  <AccountResetWindows account={account} compact />
                </span>
                <span data-label="24h burn">{formatTokens(accountThroughput(account))}</span>
                <strong data-label="API value">{formatAPIValue(account.api_cost_estimate)}</strong>
                <span className="account-spark" aria-hidden="true"><Sparkline data={[0, account.total_input_tokens, accountThroughput(account), account.total_output_tokens]} color={provider.dither} /></span>
              </button>
            );
          })}
        </div>
        {selected && (
          <aside
            ref={inspectorRef}
            className="account-inspector"
            role={mobileInspector ? "dialog" : "complementary"}
            aria-modal={mobileInspector ? true : undefined}
            aria-label={selectedAccount ? `Account details for ${selectedAccount.id}` : "Account details"}
          >
            <button ref={closeRef} className="inspector-close" onClick={closeInspector} aria-label="Close account details">Close</button>
            {selectedAccount ? (
              <>
                <span className="inspector-code">Account details</span>
                <h2 id="account-inspector-title">{selectedAccount.id}</h2>
                <div className="inspector-provider" style={{ color: providerDisplay(selectedAccount.type).color }}>{providerDisplay(selectedAccount.type).label} · {selectedAccount.plan_type}</div>
                <div className="account-admission">Added {formatAdmission(selectedAccount.account_added_at)} · Spend {money.format(selectedAccount.subscription_spend)}</div>
                <div className="inspector-windows" aria-label="Account usage reset windows">
                  <AccountResetWindows account={selectedAccount} />
                </div>
                {selectedAccount.type === "codex" && (
                  <section className="inspector-reset-credits" aria-label="Banked usage resets">
                    <header><span>Banked usage resets</span><strong>{selectedAccount.reset_credits_known ? selectedAccount.reset_credits_available ?? 0 : "—"}</strong></header>
                    <div>{selectedAccount.reset_credits_known ? <ResetCreditExpirations account={selectedAccount} /> : <span>Reset credit data is not reported.</span>}</div>
                    <small>Expiration times use your local timezone.</small>
                  </section>
                )}
                <div className="inspector-metrics">
                  <Instrument label="24h burn" value={formatTokens(accountThroughput(selectedAccount))} accent />
                  <Instrument label="Cache hit rate" value={`${selectedAccount.cache_hit_rate_pct.toFixed(1)}%`} />
                  <Instrument label="API-equivalent value" value={money.format(selectedAccount.api_cost_estimate)} />
                  <Instrument label="Return on cost" value={selectedAccount.subscription_spend ? `${selectedAccount.roi.toFixed(2)}×` : "—"} />
                </div>
                {selectedAdmin ? (
                  <>
                    <span className="inspector-code operator-section">Operator controls · {selectedAdmin.id}</span>
                    <div className="inspector-metrics operator-metrics">
                      <Instrument label="Score" value={selectedAdmin.score.toFixed(2)} accent />
                      <Instrument label="Penalty" value={selectedAdmin.penalty.toFixed(1)} danger={selectedAdmin.penalty > 2} />
                      <Instrument label="In flight" value={String(selectedAdmin.inflight)} />
                      <Instrument label="Primary" value={selectedAdmin.is_primary ? "Yes" : "No"} />
                    </div>
                    <pre className="score-trace">{selectedAdmin.score_tooltip || "No score detail is available."}</pre>
                    {selectedAdmin.email && <p className="account-identity-hint">Google account: {selectedAdmin.email}{selectedAccount.type === "antigravity" && selectedVerificationURL ? " · finish the phone verification on this account, then relogin" : ""}</p>}
                    <div className="operator-actions">
                      {toggleAction && <button disabled={busy} className={isArmedAccountAction(action, selectedAdmin.id, toggleAction) ? "confirm" : ""} onClick={() => perform(toggleAction)}>{isArmedAccountAction(action, selectedAdmin.id, toggleAction) ? `Confirm ${selectedAdmin.disabled ? "enable" : "disable"}` : selectedAdmin.disabled ? "Enable account" : "Disable account"}</button>}
                      <button disabled={busy || !selectedAdmin.dead} className={isArmedAccountAction(action, selectedAdmin.id, "resurrect") ? "confirm" : ""} onClick={() => perform("resurrect")}>{isArmedAccountAction(action, selectedAdmin.id, "resurrect") ? "Confirm restore" : "Restore offline account"}</button>
                      <button disabled={busy} className={isArmedAccountAction(action, selectedAdmin.id, "refresh") ? "confirm" : ""} onClick={() => perform("refresh")}>{isArmedAccountAction(action, selectedAdmin.id, "refresh") ? "Confirm refresh" : "Refresh credentials"}</button>
                      {selectedAccount.type === "antigravity" && selectedVerificationURL && <a className="verification-link" href={selectedVerificationURL} target="_blank" rel="noreferrer" title={selectedAdmin.email ? `Google verification pinned to ${selectedAdmin.email}. If that account is not signed in here, Google will ask for its password instead of reusing another account in the browser.` : "Open the Google verification flow."}>{selectedAdmin.email ? `Verify ${selectedAdmin.email}` : "Open Google verification"}</a>}
                      {(selectedAccount.type === "codex" || selectedAccount.type === "antigravity") && <button disabled={busy} onClick={() => { setReloginAccountID(selectedAdmin.id); setReloginProvider(selectedAccount.type === "antigravity" ? "antigravity" : "codex"); setContributing(true); }}>{selectedAccount.type === "antigravity" ? "Relogin / revalidate" : "Relogin account"}</button>}
                    </div>

                  </>
                ) : null}
              </>
            ) : null}
          </aside>
        )}
      </div>
      {contributing && <AccountContribution reloginAccountID={reloginAccountID ?? undefined} reloginProvider={reloginProvider} onClose={() => setContributing(false)} onAdded={async () => { await onAccountsChanged(); setContributing(false); }} />}
    </div>
  );
}


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


function oauthCode(value: string) {
  const trimmed = value.trim();
  if (!trimmed) return "";
  try {
    const parsed = new URL(trimmed);
    return parsed.searchParams.get("code") ?? trimmed;
  } catch {
    const match = trimmed.match(/(?:^|[?&])code=([^&]+)/);
    return match ? decodeURIComponent(match[1]) : trimmed;
  }
}


function safeVerificationURL(raw: string | undefined) {
  if (!raw) return "";
  try {
    const parsed = new URL(raw);
    return parsed.protocol === "https:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}


function AccountContribution({ onClose, onAdded, reloginAccountID, reloginProvider = "codex" }: { onClose: () => void; onAdded: () => Promise<void>; reloginAccountID?: string; reloginProvider?: "codex" | "antigravity" }) {
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
	    const onMessage = (event: MessageEvent) => {
	      if (event.origin !== window.location.origin || event.data?.type !== "codex-pool-antigravity-oauth" || event.data?.session_id !== oauth.sessionID) return;
	      if (event.data.status === "complete") void complete();
	      if (event.data.status === "error") setError(event.data.error || "Google sign-in failed");
	    };
	    window.addEventListener("message", onMessage);
	    let polling = false;
	    const timer = window.setInterval(async () => {
	      if (polling) return;
	      polling = true;
	      try {
	        const status = await antigravityOAuthStatus(oauth.sessionID!);
	        if (stopped) return;
	        if (status.status === "complete") { window.clearInterval(timer); await complete(); }
	        if (status.status === "error") { window.clearInterval(timer); setError(status.error || "Google sign-in failed"); }
	      } catch { /* polling is only a fallback for a missed popup message */ }
	      finally { polling = false; }
	    }, 1200);
	    return () => { stopped = true; window.clearInterval(timer); window.removeEventListener("message", onMessage); };
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
	      if (!result.oauth_url || ((provider === "antigravity" || provider === "zai") ? !result.session_id : !result.verifier)) throw new Error("Provider did not return an OAuth session");
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
	            await exchangeAntigravityRelogin(oauth.sessionID, credential, oauth.state || "");
	          } else {
	            await exchangeAntigravityOAuth(oauth.sessionID, credential, oauth.state || "");
	          }
	        } else {
	          const code = oauthCode(credential);
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
          : "Other pool members will be able to use this account."}</p>
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
