import { useState } from "react";
import type { ModelDescriptor } from "../types";
import { providerDisplay } from "../ui";

export function PolicyModelSelector({ label, values, models, loading, includeAliases = true, onChange }: {
  label: "Allowed models" | "Denied models" | "Granted models";
  values: string[];
  models: ModelDescriptor[];
  loading: boolean;
  includeAliases?: boolean;
  onChange: (values: string[]) => void;
}) {
  const [query, setQuery] = useState("");
  const [provider, setProvider] = useState("");
  const options = new Map<string, { id: string; providers: string[]; terms: string[]; saved?: boolean }>();
  for (const model of models) {
    for (const id of [model.id, ...(includeAliases ? model.aliases ?? [] : [])]) {
      const option = options.get(id) ?? { id, providers: [], terms: [] };
      if (!option.providers.includes(model.provider)) option.providers.push(model.provider);
      option.terms.push(model.name ?? "", model.upstream_id ?? "", ...(model.aliases ?? []));
      options.set(id, option);
    }
  }
  for (const id of values) {
    if (!options.has(id)) options.set(id, { id, providers: [], terms: [], saved: true });
  }
  const providers = [...new Set(models.map((model) => model.provider))].sort();
  const search = query.trim().toLowerCase();
  const filtered = [...options.values()].filter((option) => (
    (!provider || option.providers.includes(provider) || option.saved)
    && [option.id, ...option.terms].some((term) => term.toLowerCase().includes(search))
  )).sort((a, b) => a.id.localeCompare(b.id));

  return <fieldset className="policy-model-selector" aria-label={label}>
    <legend>{label}</legend>
    <p>{values.length ? `${values.length} selected` : label === "Granted models" ? "Select the models this user may use on this account." : label === "Allowed models" ? "No model allowlist. All models are allowed by this policy." : "No models denied by this policy."}</p>
    <div className="policy-model-filters">
      <label>Search {label.toLowerCase()}<input type="search" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Model name or alias" /></label>
      <label>Filter {label.toLowerCase()} by provider<select value={provider} onChange={(event) => setProvider(event.target.value)}><option value="">All providers</option>{providers.map((id) => <option key={id} value={id}>{providerDisplay(id).label}</option>)}</select></label>
    </div>
    {loading && <p role="status">Loading models…</p>}
    <div className="policy-model-options">
      {filtered.map((option) => <label key={option.id} className="policy-model-option">
        <input type="checkbox" aria-label={option.id} checked={values.includes(option.id)} onChange={(event) => onChange(event.target.checked ? [...values, option.id] : values.filter((id) => id !== option.id))} />
        <span><strong>{option.id}</strong><small>{option.saved ? "Saved selector" : option.providers.map((id) => providerDisplay(id).label).join(" · ")}</small></span>
      </label>)}
      {!loading && !filtered.length && <p>No models match this filter.</p>}
    </div>
    <button type="button" className="quiet-button" disabled={!values.length} onClick={() => onChange([])}>Clear {label.toLowerCase()}</button>
  </fieldset>;
}
