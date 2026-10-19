import type { Policy } from "../governance-api";

export const accessProviders = ["codex", "claude", "gemini", "antigravity", "kimi", "minimax", "zai", "xiaomi", "grok", "adverserial", "opencode_go"];

export function ProviderAccess({ value, onChange }: { value: Policy["providers"]; onChange: (value: Policy["providers"]) => void }) {
  const denied = value.deny ?? [];
  const allowed = value.allow ?? [];
  const all = !denied.includes("*") && (allowed.length === 0 || allowed.includes("*"));
  const selected = (provider: string) => !denied.includes("*") && !denied.includes(provider) && (all || allowed.includes(provider));
  return <fieldset className="provider-access"><legend>Provider access</legend>
    <p>Choose which providers this user can use. Account owners must also share the accounts and models. Other policy restrictions still apply.</p>
    <div className="row-actions">
      <button type="button" className="quiet-button" onClick={() => onChange({ allow: ["*"], deny: [] })}>Allow all providers</button>
      <button type="button" className="quiet-button" onClick={() => onChange({ allow: [], deny: ["*"] })}>Block all providers</button>
    </div>
    <div className="provider-access-options">{accessProviders.map(provider => <label key={provider}>
      <input type="checkbox" checked={selected(provider)} onChange={() => {
        const next = accessProviders.filter(item => item === provider ? !selected(item) : selected(item));
        onChange(next.length ? { allow: next, deny: denied.filter(item => item !== "*" && item !== provider) } : { allow: [], deny: ["*"] });
      }} />{provider === "codex" ? "Codex" : provider === "claude" ? "Claude" : provider.replaceAll("_", " ")}
    </label>)}</div>
    <small>{denied.includes("*") ? "No providers allowed." : all && denied.length === 0 ? "All providers allowed." : "Only checked providers are allowed."} Save the policy to apply changes.</small>
  </fieldset>;
}
