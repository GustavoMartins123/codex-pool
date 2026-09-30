import { type ModelDescriptor } from "../types";
import { Instrument, classNames, formatTokens, providerDisplay } from "../ui";
import { type CSSProperties, useState } from "react";

export function Models({ models }: { models: ModelDescriptor[] }) {
	const [query, setQuery] = useState("");
	const [provider, setProvider] = useState<string>("all");
	const [copied, setCopied] = useState("");
	const providers = [...new Set(models.map((model) => model.provider))].sort();
	const normalizedQuery = query.trim().toLowerCase();
	const filtered = models.filter((model) => {
		if (provider !== "all" && model.provider !== provider) return false;
		if (!normalizedQuery) return true;
		return [model.id, model.name, model.upstream_id, ...(model.aliases ?? [])]
			.filter(Boolean)
			.some((value) => String(value).toLowerCase().includes(normalizedQuery));
	});
	const available = models.filter((model) => model.available_now).length;
	const copyID = async (id: string) => {
		try {
			await navigator.clipboard.writeText(id);
			setCopied(id);
			window.setTimeout(() => setCopied(""), 1400);
		} catch {
			setCopied("");
		}
	};
	return (
		<div className="signal-view models-view">
			<div className="view-title"><h1>Supported models</h1><p>Search the routing names accepted by the pool and see whether capacity is available now.</p></div>
			<div className="model-summary" aria-label="Model catalog summary">
				<Instrument label="Routing names" value={String(models.length)} note="canonical IDs and aliases" accent />
				<Instrument label="Available now" value={String(available)} note={`${models.length - available} waiting for capacity`} />
				<Instrument label="Providers" value={String(providers.length)} note="routing automatically" />
			</div>
			<div className="model-controls">
				<label><span>SEARCH</span><input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="model name or alias" /></label>
				<div className="model-provider-filter" aria-label="Filter by provider">
					<button className={provider === "all" ? "active" : ""} onClick={() => setProvider("all")}>ALL</button>
					{providers.map((id) => <button key={id} className={provider === id ? "active" : ""} onClick={() => setProvider(id)}>{providerDisplay(id).label}</button>)}
				</div>
				<span>{filtered.length} MATCHES</span>
			</div>
			<div className="model-table" role="table" aria-label="Supported model routing names">
				<div className="model-row model-head" role="row"><span>ROUTING ID / ALIASES</span><span>PROVIDER</span><span>STATUS</span><span>PROTOCOLS</span><span>CONTEXT</span><span>OUTPUT</span><span>ACCOUNTS</span></div>
				{filtered.map((model) => {
					const reset = model.next_reset_at ? new Date(model.next_reset_at) : null;
					const hasReset = Boolean(reset && !Number.isNaN(reset.valueOf()) && reset.getUTCFullYear() > 2000);
					const display = providerDisplay(model.provider);
					return <div className="model-row" role="row" key={`${model.provider}:${model.id}`} style={{ "--provider": display.color } as CSSProperties}>
						<span className="model-route"><button onClick={() => copyID(model.id)}>{copied === model.id ? "COPIED" : model.id}</button><small>{model.name && model.name !== model.id ? model.name : "canonical"}{model.aliases?.length ? ` · ${model.aliases.join(" · ")}` : ""}</small></span>
						<span className="model-provider">{display.label}</span>
						<span className={classNames("model-status", model.available_now ? "available" : "unavailable")}>{model.available_now ? "AVAILABLE" : hasReset && reset ? `RESET ${reset.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}` : "UNAVAILABLE"}{model.stale ? " / STALE" : ""}</span>
						<span>{(model.protocols?.length ? model.protocols : [model.protocol]).join(" / ")}</span>
						<span>{model.contextWindow ? formatTokens(model.contextWindow) : "n/a"}</span>
						<span>{model.max_output_tokens ? formatTokens(model.max_output_tokens) : "n/a"}</span>
						<span>{model.available_accounts ?? 0}/{model.supporting_accounts ?? 0}</span>
					</div>;
				})}
				{filtered.length === 0 && <div className="model-empty">NO ROUTING NAMES MATCH THIS FILTER</div>}
			</div>
		</div>
	);
}
