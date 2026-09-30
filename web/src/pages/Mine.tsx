import { MyAccounts } from "../components/MyAccounts";
import { beginPasskeyRegistration, createMyClient, finishPasskeyRegistration, loadMyClients, loadMyUsage, loadPasskeys, loadPassportMe, removePasskey, revokeMyClient, rotateMyClient, setupLinkMyClient, updateMyProfile, uploadMyAvatar } from "../api";
import { Bar, BarChart, Grid, Tooltip, XAxis, YAxis } from "../components/dither-kit";
import { ResponseVersion } from "../response-version";
import { type ClientCredential, type PasskeyCredential, type PassportPrincipal, type PassportUsagePoint } from "../types";
import { CopyButton, Instrument, SignalPanel, classNames, compact, formatTokens } from "../ui";
import { browserSupportsWebAuthn, startRegistration } from "@simplewebauthn/browser";
import { type FormEvent, useCallback, useEffect, useState } from "react";

export function PassportMine({ principal, onPrincipal }: { principal: PassportPrincipal; onPrincipal: (principal: PassportPrincipal) => void }) {
  const [clients, setClients] = useState<ClientCredential[]>([]);
  const [passkeys, setPasskeys] = useState<PasskeyCredential[]>([]);
  const [usage, setUsage] = useState<PassportUsagePoint[]>([]);
  const [clientsLoading, setClientsLoading] = useState(true);
  const [usageLoading, setUsageLoading] = useState(true);
  const [passkeysLoading, setPasskeysLoading] = useState(principal.kind !== "guest");
  const [label, setLabel] = useState("");
  const [nickname, setNickname] = useState(principal.display_name || "");
  const [passkeyPassword, setPasskeyPassword] = useState("");
  const [passkeyLabel, setPasskeyLabel] = useState("");
  const [setupFor, setSetupFor] = useState<string | null>(null);
  const [setupLinks, setSetupLinks] = useState<{ urls: Record<string, string>; expires: Date } | null>(null);
  const [setupPlatform, setSetupPlatform] = useState("codex");
  const [showMint, setShowMint] = useState(false);
  const [showProfile, setShowProfile] = useState(false);
  const [busy, setBusy] = useState("");
  const [notice, setNotice] = useState("");
  const [errors, setErrors] = useState({ clients: "", usage: "", passkeys: "", profile: "" });
  // One guard per resource: mutations reload them concurrently, and a slow
  // older response must not overwrite the list a newer reload produced.
  const [clientsGuard] = useState(() => new ResponseVersion());
  const [usageGuard] = useState(() => new ResponseVersion());
  const [passkeysGuard] = useState(() => new ResponseVersion());

  const loadClientsData = useCallback(async () => {
    const version = clientsGuard.begin();
    setClientsLoading(true);
    try {
      const next = await loadMyClients();
      if (!clientsGuard.isCurrent(version)) return;
      setClients(next);
      setErrors((current) => ({ ...current, clients: "" }));
    } catch (cause) {
      if (!clientsGuard.isCurrent(version)) return;
      setErrors((current) => ({ ...current, clients: cause instanceof Error ? cause.message : "Unable to load clients" }));
    } finally {
      if (clientsGuard.isCurrent(version)) setClientsLoading(false);
    }
  }, [clientsGuard]);

  const loadUsageData = useCallback(async () => {
    const version = usageGuard.begin();
    setUsageLoading(true);
    try {
      const result = await loadMyUsage();
      if (!usageGuard.isCurrent(version)) return;
      setUsage(result.hourly);
      setErrors((current) => ({ ...current, usage: "" }));
    } catch (cause) {
      if (!usageGuard.isCurrent(version)) return;
      setErrors((current) => ({ ...current, usage: cause instanceof Error ? cause.message : "Unable to load usage" }));
    } finally {
      if (usageGuard.isCurrent(version)) setUsageLoading(false);
    }
  }, [usageGuard]);

  const loadPasskeysData = useCallback(async () => {
    if (principal.kind === "guest") {
      setPasskeys([]);
      setPasskeysLoading(false);
      return;
    }

    const version = passkeysGuard.begin();
    setPasskeysLoading(true);
    try {
      const next = await loadPasskeys();
      if (!passkeysGuard.isCurrent(version)) return;
      setPasskeys(next);
      setErrors((current) => ({ ...current, passkeys: "" }));
    } catch (cause) {
      if (!passkeysGuard.isCurrent(version)) return;
      setErrors((current) => ({ ...current, passkeys: cause instanceof Error ? cause.message : "Unable to load passkeys" }));
    } finally {
      if (passkeysGuard.isCurrent(version)) setPasskeysLoading(false);
    }
  }, [principal.kind, passkeysGuard]);

  useEffect(() => {
    void loadClientsData();
    void loadUsageData();
    void loadPasskeysData();
  }, [loadClientsData, loadPasskeysData, loadUsageData]);

  const total = usage.reduce((sum, row) => sum + row.billable_tokens, 0);
  const cost = usage.reduce((sum, row) => sum + row.api_equivalent_cost_usd, 0);
  const chartData = Array.from(usage.reduce((hours, row) => {
    const key = row.hour;
    const existing = hours.get(key) ?? { hour: key, tokens: 0 };
    existing.tokens += row.billable_tokens;
    hours.set(key, existing);
    return hours;
  }, new Map<string, { hour: string; tokens: number }>()).values()).sort((a, b) => a.hour.localeCompare(b.hour));
  const base = window.location.origin;
  const setupCommands = (urls: Record<string, string>): Record<string, string> => ({
    codex: `curl -sL "${base}/setup/codex/${(urls.codex ?? "").split("/").pop()}" | bash`,
    claude: `source <(curl -sL "${base}/setup/claude/${(urls.claude ?? "").split("/").pop()}")`,
    gemini: `curl -sL "${base}/setup/gemini/${(urls.gemini ?? "").split("/").pop()}" | bash`,
    antigravity: `curl -sL "${base}/setup/antigravity/${(urls.antigravity ?? "").split("/").pop()}" | bash`,
    grok: `curl -sL "${base}/setup/grok/${(urls.grok ?? "").split("/").pop()}" | bash`,
    "cute-code": `curl -sL "${base}/setup/cute-code/${(urls["cute-code"] ?? "").split("/").pop()}" | bash`,
    pi: `curl -sL "${base}/setup/pi/${(urls.pi ?? "").split("/").pop()}" | bash`,
  });
  const platforms = setupLinks ? setupCommands(setupLinks.urls) : {};

  const openSetup = async (clientID: string) => {
    setBusy(`reveal:${clientID}`);
    try {
      const result = await setupLinkMyClient(clientID);
      setSetupFor(clientID);
      setSetupLinks({ urls: result.setup_urls, expires: new Date(result.nonce_expires_at) });
      setErrors((current) => ({ ...current, clients: "" }));
    } catch (cause) {
      setErrors((current) => ({ ...current, clients: cause instanceof Error ? cause.message : "Unable to generate setup links" }));
    } finally {
      setBusy("");
    }
  };

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("create-client");
    try {
      await createMyClient(label);
      setSetupFor(null);
      setSetupLinks(null);
      setLabel("");
      setShowMint(false);
      await loadClientsData();
    } catch (cause) {
      setErrors((current) => ({ ...current, clients: cause instanceof Error ? cause.message : "Unable to create client" }));
    } finally {
      setBusy("");
    }
  };

  const rotate = async (client: ClientCredential) => {
    if (!window.confirm(client.status === "active"
      ? `Replace the key for ${client.label}? Apps using its current key will stop working. This cannot be undone.`
      : `Restore ${client.label} with a new key? You will need to run setup again.`)) return;
    setBusy(`rotate:${client.id}`);
    try {
      await rotateMyClient(client.id);
      setSetupFor(null);
      setSetupLinks(null);
      setNotice(`${client.label} has a new key. Run setup again on this device.`);
      await loadClientsData();
    } catch (cause) {
      setErrors((current) => ({ ...current, clients: cause instanceof Error ? cause.message : "Unable to rotate client" }));
    } finally {
      setBusy("");
    }
  };

  const revoke = async (client: ClientCredential) => {
    if (!window.confirm(`Revoke ${client.label}? Apps using this key will lose access. This cannot be undone.`)) return;

    setBusy(`revoke:${client.id}`);
    try {
      await revokeMyClient(client.id);
      if (setupFor === client.id) {
        setSetupFor(null);
        setSetupLinks(null);
      }
      await loadClientsData();
    } catch (cause) {
      setErrors((current) => ({ ...current, clients: cause instanceof Error ? cause.message : "Unable to revoke client" }));
    } finally {
      setBusy("");
    }
  };

  const saveProfile = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("profile");
    try {
      onPrincipal(await updateMyProfile(nickname));
      setErrors((current) => ({ ...current, profile: "" }));
    } catch (cause) {
      setErrors((current) => ({ ...current, profile: cause instanceof Error ? cause.message : "Unable to save profile" }));
    } finally {
      setBusy("");
    }
  };

  const uploadAvatar = async (file?: File) => {
    if (!file) return;

    setBusy("avatar");
    try {
      await uploadMyAvatar(file);
      onPrincipal(await loadPassportMe());
      setErrors((current) => ({ ...current, profile: "" }));
    } catch (cause) {
      setErrors((current) => ({ ...current, profile: cause instanceof Error ? cause.message : "Unable to upload avatar" }));
    } finally {
      setBusy("");
    }
  };

  const registerPasskey = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("passkey");
    try {
      const begin = await beginPasskeyRegistration(passkeyPassword, passkeyLabel || "Passkey");
      const credential = await startRegistration({ optionsJSON: begin.options });
      await finishPasskeyRegistration(begin.challenge_id, credential);
      setPasskeyPassword("");
      setPasskeyLabel("");
      await loadPasskeysData();
    } catch (cause) {
      setErrors((current) => ({ ...current, passkeys: cause instanceof Error ? cause.message : "Unable to add passkey" }));
    } finally {
      setBusy("");
    }
  };

  const deletePasskey = async (passkey: PasskeyCredential) => {
    if (!window.confirm(`Remove ${passkey.label}? You will no longer be able to sign in with this passkey.`)) return;

    setBusy(`passkey:${passkey.id}`);
    try {
      await removePasskey(passkey.id);
      await loadPasskeysData();
    } catch (cause) {
      setErrors((current) => ({ ...current, passkeys: cause instanceof Error ? cause.message : "Unable to remove passkey" }));
    } finally {
      setBusy("");
    }
  };

  const error = Object.values(errors).filter(Boolean).join(" · ");

  return <section className="view-stack mine-page">
    {error && <div className="signal-error" role="alert">{error}</div>}
    {notice && <div className="signal-success" role="status">{notice}</div>}
    <div className="view-title"><h1>Your usage</h1></div>
    <div className="identity-strip">
      <div className="passport-avatar">{principal.avatar_url ? <img src={principal.avatar_url} alt="" /> : <span>{(principal.display_name || principal.email || "G").slice(0, 2).toUpperCase()}</span>}</div>
      <div><strong>{principal.display_name || principal.email || `Guest ${principal.id.slice(0, 8)}`}</strong><small>{principal.kind}</small></div>
      <button className="quiet-button" onClick={() => setShowProfile(!showProfile)}>{showProfile ? "Close profile" : "Edit profile"}</button>
    </div>

    {showProfile && <SignalPanel title="Profile and sign-in security">
      <div className="profile-settings">
        <form className="access-form profile-form" onSubmit={saveProfile}>
          <label><span>Display name</span><input value={nickname} onChange={(event) => setNickname(event.target.value)} maxLength={48} placeholder="How you appear" /></label>
          <button className="gold-button" disabled={busy === "profile"}>{busy === "profile" ? "Saving…" : "Save profile"}</button>
        </form>
        <label className="avatar-upload"><span>Avatar</span><input type="file" accept="image/png,image/jpeg" disabled={busy === "avatar"} onChange={(event) => uploadAvatar(event.target.files?.[0])} /></label>
        {principal.kind !== "guest" && browserSupportsWebAuthn() && <section className="passkey-settings" aria-labelledby="passkey-heading">
          <h3 id="passkey-heading">Passkeys</h3>
          {passkeysLoading ? <div className="empty-state">Loading passkeys…</div> : passkeys.length > 0 && <div className="passkey-list">{passkeys.map((passkey) => <div key={passkey.id}><span><strong>{passkey.label}</strong><small>{passkey.last_used_at ? `Used ${new Date(passkey.last_used_at).toLocaleDateString()}` : `Added ${new Date(passkey.created_at).toLocaleDateString()}`}</small></span><button className="danger-action" disabled={busy === `passkey:${passkey.id}`} onClick={() => deletePasskey(passkey)}>{busy === `passkey:${passkey.id}` ? "Removing…" : "Remove"}</button></div>)}</div>}
          <form className="passkey-form" onSubmit={registerPasskey}>
            <label><span>Passkey label</span><input value={passkeyLabel} onChange={(event) => setPasskeyLabel(event.target.value)} placeholder="MacBook Touch ID" maxLength={80} required /></label>
            <label><span>Password</span><input type="password" value={passkeyPassword} onChange={(event) => setPasskeyPassword(event.target.value)} autoComplete="current-password" required /></label>
            <button className="quiet-button" disabled={busy === "passkey"}>{busy === "passkey" ? "Adding…" : "Add passkey"}</button>
          </form>
        </section>}
      </div>
    </SignalPanel>}

    <MyAccounts key={principal.id} principal={principal} />

    <section className="mine-clients" aria-labelledby="client-heading">
      <header className="section-heading">
        <div><h2 id="client-heading">Clients</h2><p>Use a separate client for each device.</p></div>
        {!showMint && clients.length > 0 && <button className="quiet-button" onClick={() => setShowMint(true)}>Add client</button>}
      </header>
      {showMint && <form className="access-form client-create" onSubmit={create}>
        <label><span>Label</span><input value={label} onChange={(event) => setLabel(event.target.value)} placeholder="MacBook, server, work laptop…" maxLength={80} required autoFocus /></label>
        <button className="gold-button" disabled={Boolean(busy)}>{busy === "create-client" ? "Creating…" : "Create client"}</button>
        {clients.length > 0 && <button type="button" className="quiet-button" onClick={() => setShowMint(false)}>Cancel</button>}
      </form>}
      {clientsLoading ? <div className="empty-state">Loading clients…</div> : clients.length === 0 && !showMint ? <div className="empty-state client-empty"><p>No client credentials yet.</p><button className="gold-button" onClick={() => setShowMint(true)}>Create first client</button></div> : clients.map((client) => <article key={client.id} className="client-card">
        <div className="client-header">
          <span><strong>{client.label}</strong>{client.status !== "active" && <small>{client.status}</small>}</span>
          <div className="row-actions">
            <button disabled={Boolean(busy) || client.status !== "active"} onClick={() => setupFor === client.id ? (setSetupFor(null), setSetupLinks(null)) : openSetup(client.id)}>{busy === `reveal:${client.id}` ? "Loading…" : setupFor === client.id ? "Hide setup" : "Show setup"}</button>
            <button disabled={Boolean(busy)} onClick={() => rotate(client)}>{busy === `rotate:${client.id}` ? "Updating…" : client.status === "active" ? "Replace key" : "Restore"}</button>
            {client.status === "active" && <button className="danger-action" disabled={Boolean(busy)} onClick={() => revoke(client)}>{busy === `revoke:${client.id}` ? "Revoking…" : "Revoke"}</button>}
          </div>
        </div>
        {setupFor === client.id && setupLinks && <div className="setup-secret" role="status">
          <p className="setup-note">One-time links, expire {setupLinks.expires.toLocaleTimeString()}. Reopen "Show setup" for fresh ones.</p>
          <div className="tabs client-platform-tabs">
            {Object.keys(platforms).map((platform) => <button key={platform} className={classNames("tab", setupPlatform === platform && "active")} onClick={() => setSetupPlatform(platform)}>{platform}</button>)}
          </div>
          <code>{platforms[setupPlatform]}</code>
          <CopyButton text={platforms[setupPlatform]} />
        </div>}
      </article>)}
    </section>

    <div className="instrument-grid mine-instruments">
      <Instrument label="7-day billable" value={usageLoading ? "…" : formatTokens(total)} accent />
      <Instrument label="API-equivalent value" value={usageLoading ? "…" : `$${cost.toFixed(2)}`} />
      <Instrument label="Active clients" value={clientsLoading ? "…" : String(clients.filter((client) => client.status === "active").length)} />
    </div>
    <SignalPanel title="Usage · last 7 days">
      {usageLoading ? <div className="empty-state">Loading usage…</div> : chartData.length ? <div className="chart-stage medium"><BarChart data={chartData} config={{ tokens: { label: "Tokens", color: "orange" } }} margins={{ left: 52, bottom: 34 }}><Grid horizontal /><Bar dataKey="tokens" variant="hatched" isClickable /><XAxis dataKey="hour" tickFormatter={(value) => String(value).slice(11, 16)} maxTicks={8} /><YAxis tickFormatter={(value) => compact.format(Number(value))} /><Tooltip /></BarChart></div> : <div className="empty-state">No usage in the last 7 days.</div>}
    </SignalPanel>

  </section>;
}
