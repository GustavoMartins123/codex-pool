import { PageBoundary } from "./PageBoundary";
import { AccessGate, BootScreen, JoinSwitch, JoinUnavailable, MemberRecovery } from "./access";
import { loadAdminAccounts, loadModelCatalog, loadPassportMe, loadPoolStats, loadSignalAnalytics, passportJoin, passportLogout } from "./api";
import { allowedViews, type View, updateURL, viewFromSearch } from "./navigation";
import { ResponseVersion } from "./response-version";
import { type AdminAccount, type ModelDescriptor, type PassportPrincipal, type PoolStats, type SignalAnalytics } from "./types";
import { classNames, formatTokens } from "./ui";
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";

const Pulse = lazy(() => import("./pages/Analytics").then(module => ({ default: module.Pulse })));
const Insights = lazy(() => import("./pages/Analytics").then(module => ({ default: module.Insights })));
const PassportMine = lazy(() => import("./pages/Mine").then(module => ({ default: module.PassportMine })));
const SetupPage = lazy(() => import("./pages/Setup").then(module => ({ default: module.SetupPage })));
const Passes = lazy(() => import("./pages/Passes").then(module => ({ default: module.Passes })));
const PassportConsole = lazy(() => import("./pages/Console").then(module => ({ default: module.PassportConsole })));
const Accounts = lazy(() => import("./pages/Accounts").then(module => ({ default: module.Accounts })));
const Models = lazy(() => import("./pages/Models").then(module => ({ default: module.Models })));

export function App() {
  const [passport, setPassport] = useState<PassportPrincipal | null>(null);
  const [booting, setBooting] = useState(true);
  const [view, setView] = useState<View>(() => viewFromSearch(window.location.search));
  const [pendingJoin, setPendingJoin] = useState<{ token: string; current: PassportPrincipal } | null>(null);
  const [joinBusy, setJoinBusy] = useState(false);
  const [recoveryToken, setRecoveryToken] = useState("");
  const [joinError, setJoinError] = useState("");
  const [stats, setStats] = useState<PoolStats | null>(null);
  const [signal, setSignal] = useState<SignalAnalytics | null>(null);
	const [models, setModels] = useState<ModelDescriptor[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [adminAccounts, setAdminAccounts] = useState<AdminAccount[]>([]);
  const adminLoadVersion = useRef(0);
  // Drops dashboard responses superseded by a newer refresh (poll vs manual
  // vs post-mutation) so a slow older payload cannot overwrite newer state.
  const [refreshGuard] = useState(() => new ResponseVersion());

  const goToView = useCallback((nextView: View, params: Record<string, string | null> = {}) => {
    const cleanup: Record<string, string | null> = { account: null, member: null };
    if (nextView !== "insights") cleanup.insight = null;
    if (nextView !== "accounts") cleanup.accounts = null;
    updateURL({ ...cleanup, view: nextView, ...params }, "push");
    setView(nextView);
  }, []);

  const refresh = useCallback(async () => {
    const version = refreshGuard.begin();
    setLoading(true);
    try {
	  const [nextStats, nextSignal, nextCatalog] = await Promise.all([loadPoolStats(), loadSignalAnalytics(), loadModelCatalog()]);
      if (!refreshGuard.isCurrent(version)) return;
      setStats(nextStats);
      setSignal(nextSignal);
	  setModels(nextCatalog.models);
      setError("");
    } catch (cause) {
      if (!refreshGuard.isCurrent(version)) return;
      setError(cause instanceof Error ? cause.message : "Unable to refresh pool data. Try again.");
    } finally {
      if (refreshGuard.isCurrent(version)) setLoading(false);
    }
  }, [refreshGuard]);

  useEffect(() => {
    const boot = async () => {
      sessionStorage.removeItem("operatorToken");
      const memberToken = window.location.pathname === "/recover" ? decodeURIComponent(window.location.hash.replace(/^#/, "")) : "";
      if (memberToken) {
        window.history.replaceState(null, "", "/recover");
        setRecoveryToken(memberToken);
        return;
      }
      const joinToken = window.location.pathname === "/join" ? decodeURIComponent(window.location.hash.replace(/^#/, "")) : "";
      if (joinToken) {
        window.history.replaceState(null, "", "/");
        try {
          const result = await passportJoin(joinToken);
          if (result.switch_required && result.current) {
            setPendingJoin({ token: joinToken, current: result.current });
            return;
          }
          if (result.principal) {
            setPassport(result.principal);
            setView("mine");
            if (result.principal.kind === "operator") await refresh();
            return;
          }
        } catch (cause) {
          setJoinError(cause instanceof Error ? cause.message : "This pass is unavailable");
          return;
        }
      }
      try {
        const principal = await loadPassportMe();
        setPassport(principal);
        if (principal.kind === "guest") setView("mine");
        else if (principal.kind === "operator") await refresh();
      } catch {
        setPassport(null);
      }
    };
    boot().finally(() => setBooting(false));
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    const restoreView = () => setView(viewFromSearch(window.location.search));
    window.addEventListener("popstate", restoreView);
    return () => window.removeEventListener("popstate", restoreView);
  }, []);

  useEffect(() => {
    updateURL({ view }, "replace");
  }, [view]);

  useEffect(() => {
    if (!passport) return;
    const permitted = allowedViews(passport);
    if (permitted.includes(view)) return;
    const fallback = passport.kind === "operator" ? "pulse" : "mine";
    setView(fallback);
    updateURL({ view: fallback, insight: null, account: null, accounts: null, member: null }, "replace");
  }, [passport, view]);

  useEffect(() => {
    if (passport?.kind !== "operator") return;
    refresh();
    const timer = window.setInterval(refresh, 30_000);
    return () => window.clearInterval(timer);
  }, [passport, refresh]);

  useEffect(() => {
    const version = ++adminLoadVersion.current;
    if (passport?.kind !== "operator") {
      setAdminAccounts([]);
      return;
    }
    loadAdminAccounts()
      .then((accounts) => {
        if (version === adminLoadVersion.current) setAdminAccounts(accounts);
      })
      .catch((cause) => {
        if (version !== adminLoadVersion.current) return;
        setAdminAccounts([]);
        setError(cause instanceof Error ? cause.message : "Unable to load operator accounts");
      });
    return () => { adminLoadVersion.current++; };
  }, [passport]);

  if (booting) return <BootScreen />;
  if (recoveryToken && !passport) return <MemberRecovery token={recoveryToken} onAccess={(next) => { setRecoveryToken(""); setPassport(next); setView("mine"); if (next.kind === "operator") refresh(); }} />;
  if (pendingJoin) {
    return <JoinSwitch current={pendingJoin.current} busy={joinBusy} onCancel={() => { setPassport(pendingJoin.current); setView("mine"); setPendingJoin(null); }} onConfirm={async () => {
      if (joinBusy) return;
      setJoinBusy(true);
      try {
        const result = await passportJoin(pendingJoin.token, true);
        if (result.principal) {
          setPassport(result.principal);
          setPendingJoin(null);
          setView("mine");
        }
      } catch (cause) {
        setPendingJoin(null);
        setJoinError(cause instanceof Error ? cause.message : "This pass is unavailable");
      } finally {
        setJoinBusy(false);
      }
    }} />;
  }
  if (!passport) {
    return joinError ? <JoinUnavailable /> : <AccessGate onAccess={(next) => { setPassport(next); setView(next.kind === "operator" ? "pulse" : "mine"); if (next.kind === "operator") refresh(); }} />;
  }

  const signOut = async () => {
    if (passport) await passportLogout().catch(() => undefined);
    adminLoadVersion.current++;
    refreshGuard.invalidate();
    setAdminAccounts([]);
    setPassport(null);
    setStats(null);
    setSignal(null);
    setModels([]);
    updateURL({ view: null, insight: null, account: null, accounts: null, member: null }, "replace");
  };

  return (
    <div className="signal-app">
      <Header
        stats={stats}
        loading={loading}
        operator={passport?.kind === "operator"}
        onRefresh={passport?.kind === "operator" ? refresh : undefined}
      />
      <div className="app-grid">
        <Navigation view={view} principal={passport} onChange={goToView} onSignOut={signOut} />
        <main className="signal-main" id="main-content">
          {error && <div className="signal-error" role="alert"> {error}</div>}
          {allowedViews(passport).includes(view) && <PageBoundary key={view}>
            <Suspense fallback={<p role="status">Loading page…</p>}>
              {view === "pulse" && <Pulse stats={stats} signal={signal} onAccounts={() => goToView("accounts", { accounts: "attention" })} />}
              {view === "insights" && <Insights stats={stats} signal={signal} onAccounts={() => goToView("accounts", { accounts: null })} />}
              {view === "mine" && <PassportMine principal={passport} onPrincipal={setPassport} />}
              {view === "passes" && passport && passport.kind !== "guest" && <Passes />}
              {view === "console" && passport && passport.kind === "operator" && <PassportConsole principal={passport} />}
              {view === "accounts" && passport?.kind === "operator" && (
                <Accounts
                  stats={stats}
                  adminAccounts={adminAccounts}
                  onAccountsChanged={async () => {
                    const version = ++adminLoadVersion.current;
                    const [accounts] = await Promise.all([loadAdminAccounts(), refresh()]);
                    if (version === adminLoadVersion.current) setAdminAccounts(accounts);
                  }}
                />
              )}
              {view === "models" && <Models models={models} />}
              {view === "setup" && <SetupPage />}
            </Suspense>
          </PageBoundary>}
        </main>
      </div>
    </div>
  );
}


function Header({ stats, loading, operator, onRefresh }: {
  stats: PoolStats | null;
  loading: boolean;
  operator: boolean;
  onRefresh?: () => void;
}) {
  const generated = stats ? new Date(stats.generated_at) : null;
  return (
    <header className="command-rail">
      <a href="#main-content" className="skip-link">Skip to content</a>
      <div className="command-brand">
        <span className="command-mark" aria-hidden="true" />
        <div className="command-brand-copy">
          <span>Codex Pool</span>
          <em>Shared model access</em>
        </div>
      </div>
      <div className="rail-readouts">
        {stats && <span>{stats.active_accounts} of {stats.total_accounts} accounts live</span>}
        {stats && <span>{formatTokens(stats.last_24h_tokens ?? 0)} tokens today</span>}
        {generated && <span>Updated {generated.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</span>}
        {onRefresh && <button onClick={onRefresh} disabled={loading}>{loading ? "Refreshing…" : "Refresh"}</button>}
        {operator && <span className="operator-live">Operator</span>}
      </div>
    </header>
  );
}


export function Navigation({ view, principal, onChange, onSignOut }: { view: View; principal: PassportPrincipal | null; onChange: (view: View) => void; onSignOut: () => void | Promise<void> }) {
  const [signingOut, setSigningOut] = useState(false);
  const signOut = async () => {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await onSignOut();
    } finally {
      setSigningOut(false);
    }
  };
  const groups: NavGroup[] = principal?.kind === "guest"
    ? [{ label: "Your access", items: [["mine", "Usage"], ["setup", "Setup"]] }]
    : principal?.kind === "operator"
    ? [
        { label: "Operate", items: [["pulse", "Pool status"], ["insights", "Insights"], ["models", "Models"]] },
        { label: "Manage", items: [["accounts", "Accounts"], ["passes", "Guest passes"], ["console", "Members"]] },
        { label: "Personal", items: [["mine", "Your usage"], ["setup", "Setup"]] },
      ]
    : [
        { label: "Personal", items: [["mine", "Your usage"], ["setup", "Setup"]] },
        { label: "Sharing", items: [["passes", "Guest passes"]] },
      ];
  return (
    <nav className="signal-nav" aria-label="Codex Pool">
      <div className="mobile-nav">
        <label>
          <span>Workspace</span>
          <select value={view} onChange={(event) => onChange(event.target.value as View)}>
            {groups.map((group) => <optgroup label={group.label} key={group.label}>{group.items.map(([id, label]) => <option key={id} value={id}>{label}</option>)}</optgroup>)}
          </select>
        </label>
        <button onClick={signOut} disabled={signingOut}>{signingOut ? "Signing out…" : "Sign out"}</button>
      </div>
      <div className="nav-groups">
        {groups.map((group) => (
          <section className="nav-group" key={group.label} aria-label={group.label}>
            <h2>{group.label}</h2>
            {group.items.map(([id, label]) => (
              <button key={id} className={classNames("nav-item", view === id && "active")} onClick={() => onChange(id)} aria-current={view === id ? "page" : undefined}>
                {label}
              </button>
            ))}
          </section>
        ))}
      </div>
      <div className="nav-account">
        <span>{principal?.display_name || principal?.username || principal?.email || "Pool member"}</span>
        <small>{principal?.kind ?? "member"}</small>
        <button onClick={signOut} disabled={signingOut}>{signingOut ? "Signing out…" : "Sign out"}</button>
      </div>
    </nav>
  );
}


type NavGroup = { label: string; items: Array<[View, string]> };
