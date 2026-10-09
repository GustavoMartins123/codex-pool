import { useEffect, useState } from "react";
import { loadPolicies, type PolicyView, type Selector } from "../governance-api";
import { PolicyEditorForm } from "./PolicyEditor";

function allows(selector: Selector, model: string) {
  const normalize = (value: string) => value.trim().toLowerCase().replace(/^antigravity\//, "");
  const matches = (values?: string[]) => values?.some((value) => normalize(value) === "*" || normalize(value) === normalize(model));
  return !matches(selector.deny) && (!selector.allow?.length || matches(selector.allow));
}
function sourceLabel(source: string) {
  if (source === "global") return "Global policy";
  if (source.startsWith("role:")) return `${source.slice(5)} role policy`;
  if (source.startsWith("principal:")) return "User policy";
  if (source.startsWith("credential:")) return "Credential policy";
  if (source.startsWith("configuration:")) return "Configured credential policy";
  return source;
}

export function SharingPolicyCheck({ principalID, models, disabled }: { principalID: string; models: string[]; disabled: boolean }) {
  const [views, setViews] = useState<{ label: string; client: string; view: PolicyView }[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [editor, setEditor] = useState<{ client: string; target: "principal" | "client" } | null>(null);
  useEffect(() => {
    let active = true;
    setLoading(true); setError(""); setViews([]);
    void (async () => {
      try {
        const principal = await loadPolicies(principalID);
        if (!active) return;
        const clients = principal.clients.filter((client) => client.status === "active");
        const credentials = await Promise.all(clients.map(async (client) => ({ label: client.label, client: client.id, view: await loadPolicies(principalID, client.id) })));
        if (active) setViews([{ label: "User", client: "", view: principal }, ...credentials]);
      } catch (cause) {
        if (active) setError(cause instanceof Error ? cause.message : "Could not check recipient policies");
      } finally { if (active) setLoading(false); }
    })();
    return () => { active = false; };
  }, [principalID, attempt]);
  const blockers = views.flatMap(({ label, client, view }) => view.sources.flatMap((source) => {
    // Principal restrictions are already displayed once under User.
    if (client && !source.source.startsWith("credential:") && !source.source.startsWith("configuration:")) return [];
    const denied = models.filter((model) => !allows(source.policy.models, model));
    if (!denied.length) return [];
    const target: "principal" | "client" | null = source.source.startsWith("principal:") ? "principal" : source.source.startsWith("credential:") ? "client" : null;
    return [{ label, client, source: source.source, denied, target }];
  }));
  return <section className="governance-panel" aria-label="Recipient model policies">
    <strong>Recipient model policies</strong>
    {loading ? <p role="status">Checking user and credential policies…</p> : error ? <p role="alert">Could not verify recipient access: {error}</p> : blockers.length ? <div role="alert">
      <p>Sharing these models does not authorize them through the policies below. Requests using a blocked model will return 403 until the blocking policy is updated.</p>
      <ul>{blockers.map((blocker) => <li key={`${blocker.client}:${blocker.source}`}><strong>{blocker.label} · {sourceLabel(blocker.source)}</strong>: {blocker.denied.join(", ")}. {blocker.target ? <button type="button" disabled={disabled} onClick={() => { if (blocker.target) setEditor({ client: blocker.target === "client" ? blocker.client : "", target: blocker.target }); }}>Edit {blocker.target === "client" ? `${blocker.label} credential policy` : "user policy"}</button> : <span>Update this restriction in the server policy configuration.</span>}</li>)}</ul>
    </div> : models.length ? <p>The current user and active credential model policies allow the selected models.</p> : <p>Select granted models to check for policy conflicts.</p>}
    <button type="button" className="quiet-button" disabled={loading || disabled} onClick={() => setAttempt((value) => value + 1)}>Recheck recipient policies</button>
    {editor && <><button type="button" disabled={disabled} onClick={() => setEditor(null)}>Close recipient policy editor</button><PolicyEditorForm key={`${principalID}:${editor.client}:${editor.target}`} principalID={principalID} initialClientID={editor.client} initialTarget={editor.target} onSaved={() => setAttempt((value) => value + 1)} /></>}
  </section>;
}
