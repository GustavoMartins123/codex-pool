import { useCallback, useEffect, useState } from "react";
import { loadMyAccounts, withdrawMyAccount } from "../api";
import { ResponseVersion } from "../response-version";
import type { MyAccount, PassportPrincipal } from "../types";
import { providerDisplay } from "../ui";
import { AccountContribution } from "./AccountContribution";
import { AccountGovernance } from "./AccountGovernance";

export function MyAccounts({ principal }: { principal: PassportPrincipal }) {
  const [accounts, setAccounts] = useState<MyAccount[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [armed, setArmed] = useState("");
  const [adding, setAdding] = useState(false);
  const [guard] = useState(() => new ResponseVersion());

  const load = useCallback(async () => {
    const version = guard.begin();
    setLoading(true);
    try {
      const result = await loadMyAccounts();
      if (!guard.isCurrent(version)) return;
      setAccounts(result);
      setError("");
    } catch (cause) {
      if (!guard.isCurrent(version)) return;
      setAccounts([]);
      setError(cause instanceof Error ? cause.message : "Unable to load accounts");
    } finally {
      if (guard.isCurrent(version)) setLoading(false);
    }
  }, [guard]);

  useEffect(() => {
    if (principal.kind === "guest") return;
    void load();
    return () => guard.invalidate();
  }, [guard, load, principal.kind]);

  const added = useCallback(async () => {
    setAdding(false);
    await load();
  }, [load]);

  const withdraw = async (account: MyAccount) => {
    setBusy(account.id);
    setError("");
    try {
      await withdrawMyAccount(account);
      setArmed("");
      await load();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Unable to withdraw account");
    } finally { setBusy(""); }
  };

  if (principal.kind === "guest") return null;
  return <section className="mine-accounts" aria-labelledby="my-account-heading">
    <header className="section-heading">
      <div><h2 id="my-account-heading">My accounts</h2><p>Provider accounts you add are private to you.</p></div>
      <div className="row-actions">
        <button className="quiet-button" disabled={loading || Boolean(busy)} onClick={() => void load()}>Refresh accounts</button>
        {(principal.kind === "operator" || principal.can_contribute) && <button className="gold-button" disabled={Boolean(busy)} onClick={() => setAdding(true)}>Add account</button>}
      </div>
    </header>
    {error && <div className="access-error" role="alert">{error}</div>}
    {loading ? <div className="empty-state">Loading accounts…</div> : accounts.length === 0 && !error ? <div className="empty-state">No provider accounts yet.</div> : accounts.map((account) => <article className="client-card" key={`${account.provider}:${account.id}`}>
      <div className="client-header">
        <span><strong>{providerDisplay(account.provider).label}</strong><small>{account.id}</small><small>{account.state.replaceAll("_", " ")}</small></span>
        {account.status !== "withdrawn" && <div className="row-actions">
          {armed === account.id ? <><button className="danger-action" disabled={Boolean(busy)} onClick={() => void withdraw(account)}>{busy === account.id ? "Withdrawing…" : "Confirm withdrawal"}</button><button disabled={Boolean(busy)} onClick={() => setArmed("")}>Cancel withdrawal</button></> : <button className="danger-action" disabled={Boolean(busy)} onClick={() => setArmed(account.id)}>Withdraw</button>}
        </div>}
      </div>
      {armed === account.id && <p>New requests will stop using this account. Withdrawal is permanent.</p>}
      {account.status === "active" && <AccountGovernance provider={account.provider} id={account.id} onChanged={load} />}
    </article>)}
    {adding && <AccountContribution onClose={() => setAdding(false)} onAdded={added} />}
  </section>;
}
