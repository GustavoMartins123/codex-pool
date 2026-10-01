import { mutateAccount, reloadAccounts } from "../api";
import { AccountContribution } from "../components/AccountContribution";
import { AccountGovernance } from "../components/AccountGovernance";
import { Sparkline } from "../charts";
import { type AccountStats, type AdminAccount, type PoolStats, type Provider } from "../types";
import { queryValue, updateURL } from "../navigation";
import { AccountResetWindows, Instrument, ResetCreditExpirations, SignalSkeleton, WeeklyPace, accountThroughput, classNames, compact, formatAPIValue, formatAdmission, formatTokens, money, providerDisplay } from "../ui";
import { type CSSProperties, useCallback, useEffect, useRef, useState } from "react";

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

      const focusable = [...inspectorRef.current.querySelectorAll<HTMLElement>('button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])')].filter(element => element.getClientRects().length > 0);
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!inspectorRef.current.contains(document.activeElement) || !event.shiftKey && document.activeElement === last) {
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

  useEffect(() => {
    if (!selected || mobileInspector) return;
    const frame = requestAnimationFrame(() => closeRef.current?.focus());
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") { event.preventDefault(); closeInspector(); }
    };
    document.addEventListener("keydown", closeOnEscape);
    return () => { cancelAnimationFrame(frame); document.removeEventListener("keydown", closeOnEscape); };
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
                    <AccountGovernance key={`${selectedAccount.type}:${selectedAdmin.id}`} provider={selectedAccount.type} id={selectedAdmin.id} onChanged={onAccountsChanged} />
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


function safeVerificationURL(raw: string | undefined) {
  if (!raw) return "";
  try {
    const parsed = new URL(raw);
    return parsed.protocol === "https:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}
