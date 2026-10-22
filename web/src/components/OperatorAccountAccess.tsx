import { useEffect, useRef, useState, type FormEvent } from "react";
import { loadOperatorAccounts, loadOperatorAccess, setOperatorAccountAccess, setContributionPermission, type OperatorAccount, type OperatorAccessEntry } from "../governance-api";

export function MemberContributionPermission({ principalID, allowed, onChanged }: { principalID: string; allowed: boolean; onChanged: () => Promise<void> }) {
  const [busy, setBusy] = useState(false);
  const [enabled, setEnabled] = useState(allowed);
  const [error, setError] = useState("");
  useEffect(() => setEnabled(allowed), [allowed]);
  const change = async (next: boolean) => {
    setBusy(true); setError("");
    try { setEnabled(await setContributionPermission(principalID, next)); await onChanged(); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Unable to update account registration permission"); }
    finally { setBusy(false); }
  };
  return <section className="mine-accounts operator-account-access"><label className="governance-consent"><input type="checkbox" checked={enabled} disabled={busy} onChange={event => void change(event.target.checked)} />Allow this member to add private provider accounts</label><p>Registration permission does not share the member's accounts with anyone.</p>{error && <p role="alert">{error}</p>}</section>;
}

function OperatorAccountRow({ account, onSaved }: { account: OperatorAccount; onSaved: (value: OperatorAccount) => void }) {
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (event: FormEvent) => {
    event.preventDefault(); setBusy(true); setError("");
    try { onSaved(await setOperatorAccountAccess(account, !account.enabled, reason.trim())); setReason(""); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Unable to update your account access"); }
    finally { setBusy(false); }
  };
  return <article className="client-card"><strong>{account.provider}</strong><code>{account.id}</code><p>{account.enabled ? "Your activation is enabled" : "Your activation is disabled"}</p>{account.updated_at && <p>Last changed {new Date(account.updated_at).toLocaleString()}: {account.reason}</p>}<form onSubmit={submit}><label className="contribution-field"><span>Access change reason</span><input required maxLength={240} value={reason} disabled={busy} onChange={event => setReason(event.target.value)} /></label><button className="quiet-button" disabled={busy || !reason.trim()}>{busy ? "Saving accessâ€¦" : account.enabled ? "Deactivate my access" : "Activate my access"}</button></form>{error && <p role="alert">{error}</p>}</article>;
}

export function OperatorAccounts({ ownerID }: { ownerID: string }) {
  const [open, setOpen] = useState(false);
  const [accounts, setAccounts] = useState<OperatorAccount[] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const version = useRef(0);
  useEffect(() => () => { version.current++; }, []);
  const load = async () => {
    const current = ++version.current;
    setLoading(true); setAccounts(null); setError("");
    try { const value = await loadOperatorAccounts(ownerID); if (current === version.current) setAccounts(value); }
    catch (cause) { if (current === version.current) setError(cause instanceof Error ? cause.message : "Unable to load private accounts"); }
    finally { if (current === version.current) setLoading(false); }
  };
  return <section className="mine-accounts operator-account-access"><button className="quiet-button" aria-expanded={open} onClick={() => { setOpen(!open); if (!open) void load(); }}>My access to private accounts</button>{open && <><p>Access starts disabled for each account and operator. Activating allows you and your clients to use this account through the pool. It does not transfer ownership, account management or sharing permissions. Changes are recorded and visible to the owner.</p><p>Disabling blocks new requests and WebSocket messages through this activation. Requests already admitted may finish. Existing owner grants are separate permissions.</p><button className="quiet-button" disabled={loading} onClick={() => void load()}>Reload private accounts</button>{error && <p role="alert">{error}</p>}{loading && <p role="status">Loading private accountsâ€¦</p>}{accounts?.length === 0 && <p>No private accounts available for this user.</p>}{accounts?.map(account => <OperatorAccountRow key={`${account.provider}/${account.id}/${account.revision}`} account={account} onSaved={value => setAccounts(current => current?.map(item => item.provider === value.provider && item.id === value.id ? value : item) ?? null)} />)}</>}</section>;
}

export function OwnedAccountOperatorAccess({ provider, id }: { provider: string; id: string }) {
  const [open, setOpen] = useState(false);
  const [entries, setEntries] = useState<OperatorAccessEntry[] | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const version = useRef(0);
  useEffect(() => () => { version.current++; }, []);
  const load = async () => {
    const current = ++version.current;
    setEntries(null); setLoading(true); setError("");
    try { const value = await loadOperatorAccess(provider, id); if (current === version.current) setEntries(value); }
    catch (cause) { if (current === version.current) setError(cause instanceof Error ? cause.message : "Unable to load operator access"); }
    finally { if (current === version.current) setLoading(false); }
  };
  return <section><button className="quiet-button" aria-expanded={open} onClick={() => { setOpen(!open); if (!open) void load(); }}>Operator access</button>{open && <><p>Latest activation change per operator. Operators activate their own access; this does not transfer your credentials or account ownership.</p><button className="quiet-button" disabled={loading} onClick={() => void load()}>Refresh operator access</button>{loading && <p role="status">Loading operator accessâ€¦</p>}{error && <p role="alert">{error}</p>}{entries?.length === 0 && <p>No operator has activated access.</p>}{entries?.map(entry => <article className="client-card" key={entry.operator_id}><strong>Operator {entry.operator_id}</strong><p>{entry.enabled ? "Activation enabled" : "Activation disabled"} Â· {new Date(entry.updated_at).toLocaleString()}</p><p>{entry.reason}</p></article>)}</>}</section>;
}
