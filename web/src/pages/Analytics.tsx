import { Area, AreaChart, Bar, BarChart, Grid, Legend, Line, LineChart, Sparkline, Tooltip, XAxis, YAxis, type ChartConfig, type DitherColor } from "../charts";
import { accountFlow, capacityForecasts, dailyDemandSeries, demandSummary, modelMix, originConcentration, peakHeatmap, type AccountFlow, type CapacityForecast } from "../insights";
import { type AccountStats, type HourlyUsage, type ModelDailyUsage, type ModelQuotaEfficiency, type OriginWeeklyUsage, type PoolStats, type Provider, type QuotaCapacityPoint, type ResetObservation, type SignalAnalytics } from "../types";
import { INSIGHT_MODES, queryValue, type InsightMode, updateURL } from "../navigation";
import { EmptyChart, Instrument, PROVIDERS, SignalPanel, SignalSkeleton, accountThroughput, classNames, compact, formatReset, formatTokens, money, originHandle, poolSurplus, preciseMoney, providerDisplay, tokenThroughput } from "../ui";
import { type CSSProperties, useEffect, useRef, useState } from "react";

export function Pulse({ stats, signal, onAccounts }: { stats: PoolStats | null; signal: SignalAnalytics | null; onAccounts: () => void }) {
  if (!stats || !signal) return <SignalSkeleton />;
  const surplus = poolSurplus(stats.aggregate);
  const burn = burnSummary(signal.hourly);
  const intervention = stats.accounts.filter((account) => account.status !== "healthy" || account.secondary_window_used_pct >= 80);

  const generated = new Date(stats.generated_at);
  const healthCopy = intervention.length === 0
    ? "All accounts are within their reported windows."
    : `${intervention.length} ${intervention.length === 1 ? "account needs" : "accounts need"} attention.`;

  return (
    <div className="signal-view pulse-view">
      <header className="pulse-heading">
        <div>
          <h1>Pool status</h1>
          <p>{healthCopy} Data updated {generated.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}.</p>
        </div>
      </header>

      {intervention.length > 0 && (
        <button className="intervention-strip" onClick={onAccounts}>
          <span><strong>{intervention.length}</strong> {intervention.length === 1 ? "account needs attention" : "accounts need attention"}</span>
          <span className="intervention-items">
            {intervention.slice(0, 4).map((account) => {
              const provider = providerDisplay(account.type);
              const state = account.status === "dead" ? "offline" : account.status === "verification_required" ? "revalidation" : account.secondary_window_used_pct >= 80 ? `${account.secondary_window_used_pct.toFixed(0)}% weekly used` : account.status;
              return <b key={account.id} style={{ color: provider.color }}>{provider.label}: {state}</b>;
            })}
          </span>
          <i>Review accounts</i>
        </button>
      )}

      <section className="inline-instruments" aria-label="Pool economics and demand">
        <Instrument label="API-equivalent value" value={money.format(stats.aggregate.total_api_cost)} note={`${stats.aggregate.overall_roi.toFixed(2)}× subscription cost`} accent />
        <Instrument label="Subscription spend" value={money.format(stats.aggregate.total_subscription_cost)} note={`${money.format(stats.aggregate.total_subscription_monthly)} per month`} />
        <Instrument label="Net surplus" value={money.format(surplus)} note="API value less subscription spend" accent={surplus >= 0} danger={surplus < 0} />
        <Instrument label="Tokens in 24 hours" value={formatTokens(burn.current24)} note={`${burn.delta >= 0 ? "+" : ""}${burn.delta.toFixed(1)}% from prior day`} danger={burn.delta > 25} />
      </section>

      <SignalPanel title="Provider health and capacity">
        <ProviderLanes accounts={stats.accounts} />
      </SignalPanel>

      <section className="primary-signal-grid">
        <SignalPanel title="Value produced over subscription cost" className="value-panel">
          <ValueGapChart data={signal.economics} />
          <div className="chart-corner-readout">
            <strong>{stats.aggregate.overall_roi.toFixed(2)}×</strong>
            <span>return on cost</span>
            <small>{money.format(stats.aggregate.total_subscription_monthly)} / month</small>
          </div>
        </SignalPanel>
        <SignalPanel title="Token demand by provider" className="burn-panel">
          <BurnChart hourly={signal.hourly} />
          <div className="burn-readout"><span>Now {formatTokens(burn.latestHour)}/h</span><span>14d avg {formatTokens(burn.averageHour)}/h</span></div>
        </SignalPanel>
      </section>

      <section className="lower-signal-grid">
        <SignalPanel title="Token mix, 14 days">
          <TokenComposition hourly={signal.hourly} />
        </SignalPanel>
        <SignalPanel title="Heavy users this week">
          <OriginDrain rows={signal.origin_weekly} />
        </SignalPanel>
      </section>
    </div>
  );
}


function ValueGapChart({ data }: { data: SignalAnalytics["economics"] }) {
  const chartData = data.map((point) => ({ date: point.date, value: point.cumulative_api_value, spend: point.cumulative_subscription_spend }));
  if (chartData.length < 2) return <EmptyChart label="Waiting for enough cost history to draw this chart." />;
  const config: ChartConfig = {
    value: { label: "API-equivalent value", color: "gold" },
    spend: { label: "Subscription spend", color: "grey" },
  };
  return (
    <div className="chart-stage large">
      <AreaChart data={chartData} config={config} margins={{ left: 54, bottom: 28 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        <Area dataKey="value" variant="hatched" isClickable />
        <Area dataKey="spend" variant="solid" strokeVariant="dashed" isClickable />
        <XAxis dataKey="date" tickFormatter={(value) => String(value).slice(5)} maxTicks={7} />
        <YAxis tickFormatter={(value) => `$${compact.format(value)}`} />
        <Legend isClickable />
        <Tooltip labelKey="date" valueFormatter={(value) => preciseMoney.format(value)} />
      </AreaChart>
    </div>
  );
}


function aggregateHourly(hourly: HourlyUsage[]) {
  const rows = new Map<string, Record<string, string | number>>();
  for (const item of hourly) {
    const row = rows.get(item.hour) ?? { hour: item.hour };
    row[item.account_type] = Number(row[item.account_type] ?? 0) + tokenThroughput(item);
    row.input = Number(row.input ?? 0) + item.input_tokens;
    row.cached = Number(row.cached ?? 0) + item.cached_tokens;
    row.output = Number(row.output ?? 0) + item.output_tokens;
    row.reasoning = Number(row.reasoning ?? 0) + item.reasoning_tokens;
    rows.set(item.hour, row);
  }
  return [...rows.values()].sort((a, b) => String(a.hour).localeCompare(String(b.hour)));
}


function BurnChart({ hourly }: { hourly: HourlyUsage[] }) {
  const data = aggregateHourly(hourly);
  const providers = Object.keys(PROVIDERS).filter((provider) => data.some((row) => Number(row[provider]) > 0)) as Provider[];
  if (data.length < 2 || providers.length === 0) return <EmptyChart label="Waiting for enough provider usage history." />;
  const config = Object.fromEntries(providers.map((provider) => [provider, { label: PROVIDERS[provider].label, color: PROVIDERS[provider].dither }])) as ChartConfig;
  return (
    <div className="chart-stage large">
      <LineChart data={data} config={config} margins={{ left: 48, bottom: 28 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        {providers.map((provider) => <Line key={provider} dataKey={provider} isClickable />)}
        <XAxis dataKey="hour" tickFormatter={(value) => String(value).slice(5).replace("T", " ")} maxTicks={6} />
        <YAxis tickFormatter={formatTokens} />
        <Legend isClickable />
        <Tooltip labelKey="hour" valueFormatter={(value) => `${formatTokens(value)} tok`} />
      </LineChart>
    </div>
  );
}


function burnSummary(hourly: HourlyUsage[]) {
  const data = aggregateHourly(hourly);
  const totals = data.map((row) => Object.keys(PROVIDERS).reduce((sum, provider) => sum + Number(row[provider] ?? 0), 0));
  const current = totals.slice(-24).reduce((sum, value) => sum + value, 0);
  const previous = totals.slice(-48, -24).reduce((sum, value) => sum + value, 0);
  return {
    current24: current,
    latestHour: totals.at(-1) ?? 0,
    averageHour: totals.length ? totals.reduce((sum, value) => sum + value, 0) / totals.length : 0,
    delta: previous ? ((current - previous) / previous) * 100 : 0,
  };
}


function ProviderLanes({ accounts }: { accounts: AccountStats[] }) {
  const groups = Object.keys(PROVIDERS).map((provider) => {
    const rows = accounts.filter((account) => account.type === provider);
    return { provider: provider as Provider, rows };
  }).filter((group) => group.rows.length);
  return (
    <div className="provider-lanes">
      <div className="provider-head" aria-hidden="true">
        <span>Provider</span><span>Weekly capacity</span><span>API value</span><span>Spend</span><span>Return</span><span>Trend</span><span>State</span>
      </div>
      {groups.map(({ provider, rows }) => {
        const used = rows.filter((row) => row.secondary_window_available).reduce((sum, row) => sum + row.secondary_window_used_pct, 0) / Math.max(1, rows.filter((row) => row.secondary_window_available).length);
        const value = rows.reduce((sum, row) => sum + row.api_cost_estimate, 0);
        const spend = rows.reduce((sum, row) => sum + row.subscription_spend, 0);
        const roi = spend ? value / spend : 0;
        const spark = rows.map(accountThroughput);
        const status = rows.some((row) => row.status === "dead") ? "cooked" : rows.some((row) => row.status === "verification_required" || row.health_blocked) ? "revalidation" : used > 80 ? "leaning hard" : roi > 2 ? "carrying" : roi < 0.5 && spend ? "paid for" : "live";
        return (
          <div className="provider-lane" key={provider} style={{ "--provider": PROVIDERS[provider].color } as CSSProperties}>
            <div className="provider-name"><span className="provider-mark" aria-hidden="true" /><b>{PROVIDERS[provider].label}</b><small>{rows.length} account{rows.length === 1 ? "" : "s"}</small></div>
            <div className="quota-field"><i style={{ width: `${Math.min(100, used)}%` }} /><span>{used ? `${used.toFixed(0)}% week` : "window n/a"}</span></div>
            <div className="lane-stat"><span>API VALUE</span><b>{money.format(value)}</b></div>
            <div className="lane-stat"><span>SPEND</span><b>{money.format(spend)}</b></div>
            <div className="lane-stat"><span>ROI</span><b>{roi ? `${roi.toFixed(2)}×` : "—"}</b></div>
            <div className="lane-spark"><Sparkline data={spark.length > 1 ? spark : [0, ...spark]} color={PROVIDERS[provider].dither} /></div>
            <div className="lane-status">{status}</div>
          </div>
        );
      })}
    </div>
  );
}


function TokenComposition({ hourly }: { hourly: HourlyUsage[] }) {
  const data = aggregateHourly(hourly);
  if (data.length < 2) return <EmptyChart label="Waiting for enough token history." />;
  const config: ChartConfig = {
    input: { label: "Input", color: "blue" },
    cached: { label: "Cached", color: "green" },
    output: { label: "Output", color: "orange" },
    reasoning: { label: "Reasoning", color: "purple" },
  };
  return (
    <div className="chart-stage medium">
      <AreaChart data={data} config={config} stackType="stacked" margins={{ left: 46, bottom: 28 }}>
        <Grid horizontal />
        <Area dataKey="input" variant="dotted" isClickable />
        <Area dataKey="cached" variant="hatched" isClickable />
        <Area dataKey="output" variant="dotted" isClickable />
        <Area dataKey="reasoning" variant="hatched" isClickable />
        <XAxis dataKey="hour" tickFormatter={(value) => String(value).slice(5, 10)} maxTicks={5} />
        <YAxis tickFormatter={formatTokens} />
        <Legend isClickable />
        <Tooltip labelKey="hour" valueFormatter={(value) => `${formatTokens(value)} tok`} />
      </AreaChart>
    </div>
  );
}


function OriginDrain({ rows }: { rows: OriginWeeklyUsage[] }) {
  const latestWeek = rows.reduce((latest, row) => row.week_start > latest ? row.week_start : latest, "");
  const originMap = new Map<string, { id: string; total: number; requests: number; providers: Partial<Record<Provider, number>> }>();
  for (const row of rows.filter((item) => item.week_start === latestWeek)) {
    const aggregate = originMap.get(row.origin_id) ?? { id: row.origin_id, total: 0, requests: 0, providers: {} };
    const throughput = tokenThroughput(row);
    aggregate.total += throughput;
    aggregate.requests += row.request_count;
    if (row.account_type in PROVIDERS) {
      const provider = row.account_type as Provider;
      aggregate.providers[provider] = (aggregate.providers[provider] ?? 0) + throughput;
    }
    originMap.set(row.origin_id, aggregate);
  }
  const origins = [...originMap.values()].sort((a, b) => b.total - a.total).slice(0, 10);
  const poolTotal = origins.reduce((sum, origin) => sum + origin.total, 0);
  return (
    <div className="origin-drain">
      <div className="origin-head"><span>HASHED IP</span><span>TOKENS</span><span>SHARE</span><span>ACCOUNT FOOTPRINT</span></div>
      {origins.length === 0 && <div className="empty-signal">No origin activity has been recorded for this period.</div>}
      {origins.map((origin, index) => (
        <div className="origin-row" key={origin.id}>
          <span><i>{String(index + 1).padStart(2, "0")}</i>{originHandle(origin.id)}</span>
          <b>{formatTokens(origin.total)}</b>
          <span>{poolTotal ? `${((origin.total / poolTotal) * 100).toFixed(1)}%` : "0%"}</span>
          <div className="footprint" aria-label="Provider footprint">
            {Object.entries(origin.providers).map(([provider, value]) => {
              const display = providerDisplay(provider);
              return <i key={provider} title={`${display.label}: ${formatTokens(value ?? 0)}`} style={{ background: display.color, flex: value }} />;
            })}
          </div>
        </div>
      ))}
      <footer>{latestWeek ? `Week of ${latestWeek}` : "No weekly data"} · origin IDs are hashed at ingest · {origins.length} active</footer>
    </div>
  );
}


function ProviderCapitalChart({ accounts }: { accounts: AccountStats[] }) {
  const data = Object.keys(PROVIDERS).map((provider) => {
    const rows = accounts.filter((account) => account.type === provider);
    return {
      provider,
      value: rows.reduce((sum, account) => sum + account.api_cost_estimate, 0),
      spend: rows.reduce((sum, account) => sum + account.subscription_spend, 0),
    };
  }).filter((row) => row.value > 0 || row.spend > 0);
  if (data.length === 0) return <EmptyChart label="No subscription or API-value history has been recorded yet." />;
  const config: ChartConfig = {
    value: { label: "API-equivalent value", color: "gold" },
    spend: { label: "Matched subscription spend", color: "grey" },
  };
  return (
    <div className="chart-stage medium">
      <BarChart data={data} config={config} margins={{ left: 52, bottom: 30 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        <Bar dataKey="value" variant="hatched" isClickable />
        <Bar dataKey="spend" variant="dotted" isClickable />
        <XAxis dataKey="provider" tickFormatter={(value) => String(value).toUpperCase()} maxTicks={8} />
        <YAxis tickFormatter={(value) => `$${compact.format(value)}`} />
        <Legend isClickable />
        <Tooltip labelKey="provider" valueFormatter={(value) => preciseMoney.format(value)} />
      </BarChart>
    </div>
  );
}


function OriginWeeklyChart({ rows }: { rows: OriginWeeklyUsage[] }) {
  const totals = new Map<string, number>();
  for (const row of rows) totals.set(row.origin_id, (totals.get(row.origin_id) ?? 0) + tokenThroughput(row));
  const origins = [...totals.entries()].sort((a, b) => b[1] - a[1]).slice(0, 6).map(([id]) => id);
  const weeks = [...new Set(rows.map((row) => row.week_start))].sort();
  const data = weeks.map((week) => {
    const point: Record<string, string | number> = { week };
    for (const origin of origins) point[origin] = 0;
    for (const row of rows) {
      if (row.week_start === week && origins.includes(row.origin_id)) {
        point[row.origin_id] = Number(point[row.origin_id] ?? 0) + tokenThroughput(row);
      }
    }
    return point;
  });
  if (data.length === 0 || origins.length === 0) return <EmptyChart label="No origin history has been recorded yet." />;
  const colors: DitherColor[] = ["gold", "orange", "purple", "cyan", "green", "blue"];
  const config = Object.fromEntries(origins.map((origin, index) => [origin, { label: originHandle(origin), color: colors[index] }])) as ChartConfig;
  return (
    <div className="chart-stage medium">
      <BarChart data={data} config={config} stackType="stacked" margins={{ left: 48, bottom: 30 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        {origins.map((origin, index) => <Bar key={origin} dataKey={origin} variant={index % 2 ? "dotted" : "hatched"} isClickable />)}
        <XAxis dataKey="week" tickFormatter={(value) => String(value).slice(5)} maxTicks={6} />
        <YAxis tickFormatter={formatTokens} />
        <Legend isClickable />
        <Tooltip labelKey="week" valueFormatter={(value) => `${formatTokens(value)} tok`} />
      </BarChart>
    </div>
  );
}


function DemandTrendChart({ hourly }: { hourly: HourlyUsage[] }) {
  const data = dailyDemandSeries(hourly);
  if (data.length < 2) return <EmptyChart label="Waiting for two complete days of demand history." />;
  const config: ChartConfig = {
    demand: { label: "Daily demand", color: "gold" },
    trend: { label: "3-day trend", color: "cyan" },
  };
  return (
    <div className="chart-stage large">
      <LineChart data={data} config={config} margins={{ left: 52, bottom: 28 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        <Line dataKey="demand" isClickable />
        <Line dataKey="trend" isClickable />
        <XAxis dataKey="date" tickFormatter={(value) => String(value).slice(5)} maxTicks={7} />
        <YAxis tickFormatter={formatTokens} />
        <Legend isClickable />
        <Tooltip labelKey="date" valueFormatter={(value) => `${formatTokens(value)} tok`} />
      </LineChart>
    </div>
  );
}


function CapacityForecastTable({ forecasts }: { forecasts: CapacityForecast[] }) {
  return (
    <div className="capacity-table" role="table" aria-label="Provider capacity forecast">
      <div className="capacity-row capacity-head" role="row"><span>PROVIDER</span><span>LOAD</span><span>SUPPLY</span><span>MIN</span><span>+20%</span><span>ACTION</span></div>
      {forecasts.map((forecast) => {
        const state = forecast.minimumToAdd > 0 ? "gap" : forecast.bufferedToAdd > 0 ? "buffer" : "covered";
        const display = providerDisplay(forecast.provider);
        return (
          <div className={classNames("capacity-row", state)} role="row" key={forecast.provider} style={{ "--provider": display.color } as CSSProperties}>
            <span className="capacity-provider"><i className="provider-mark" aria-hidden="true" /><b>{display.label}</b><small>{forecast.measuredAccounts} measured</small></span>
            <span><b>{forecast.loadEquivalents.toFixed(1)}</b><small>ACCOUNT LOAD</small></span>
            <span><b>{forecast.activeAccounts}</b><small>ACTIVE</small></span>
            <span><b>{forecast.baselineAccounts}</b><small>BASELINE</small></span>
            <span><b>{forecast.bufferedAccounts}</b><small>TARGET</small></span>
            <span className="capacity-action"><b>{forecast.minimumToAdd > 0 ? `+${forecast.minimumToAdd} NOW` : forecast.bufferedToAdd > 0 ? `+${forecast.bufferedToAdd} BUFFER` : "COVERED"}</b><small>{forecast.earliestFullMinutes !== null ? `FULL IN ${formatReset(Math.floor(forecast.earliestFullMinutes))}` : "LASTS TO RESET"}</small></span>
          </div>
        );
      })}
    </div>
  );
}


export function Insights({ stats, signal, onAccounts }: { stats: PoolStats | null; signal: SignalAnalytics | null; onAccounts: () => void }) {
  const [mode, setMode] = useState<InsightMode>(() => {
    const candidate = queryValue("insight") as InsightMode | null;
    return candidate && INSIGHT_MODES.includes(candidate) ? candidate : "overview";
  });
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const tabs: Array<[InsightMode, string, string]> = [
    ["overview", "Overview", "Risk and recommended actions"],
    ["capacity", "Capacity", "Measured limits, resets, and scenarios"],
    ["flow", "Flow", "Unused supply and routing balance"],
    ["demand", "Demand", "Peaks, models, and concentration"],
  ];

  useEffect(() => updateURL({ insight: mode }, "replace"), [mode]);
  useEffect(() => {
    const restoreMode = () => {
      const candidate = queryValue("insight") as InsightMode | null;
      setMode(candidate && INSIGHT_MODES.includes(candidate) ? candidate : "overview");
    };
    window.addEventListener("popstate", restoreMode);
    return () => window.removeEventListener("popstate", restoreMode);
  }, []);

  const chooseMode = (nextMode: InsightMode) => {
    if (nextMode === mode) return;
    updateURL({ insight: nextMode }, "push");
    setMode(nextMode);
  };

  const handleTabKey = (event: ReactKeyboardEvent<HTMLButtonElement>, index: number) => {
    let next = index;
    if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
    else if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else return;

    event.preventDefault();
    chooseMode(tabs[next][0]);
    tabRefs.current[next]?.focus();
  };

  if (!stats || !signal) return <SignalSkeleton />;

  return (
    <div className="signal-view insights-view">
      <div className="view-title"><h1>Capacity planning</h1><p>Forecast demand, find unused capacity, and decide when accounts need to be added.</p></div>
      <nav className="insight-tabs" aria-label="Insights dashboards" role="tablist">
        {tabs.map(([id, label, description], index) => (
          <button
            key={id}
            ref={(node) => { tabRefs.current[index] = node; }}
            id={`insight-tab-${id}`}
            className={mode === id ? "active" : ""}
            onClick={() => chooseMode(id)}
            onKeyDown={(event) => handleTabKey(event, index)}
            role="tab"
            aria-controls={`insight-panel-${id}`}
            aria-selected={mode === id}
            tabIndex={mode === id ? 0 : -1}
          >
            <b>{label}</b><small>{description}</small>
          </button>
        ))}
      </nav>
      <div id={`insight-panel-${mode}`} role="tabpanel" aria-labelledby={`insight-tab-${mode}`} tabIndex={0}>
        {mode === "overview" && <InsightsOverview stats={stats} signal={signal} onAccounts={onAccounts} />}
        {mode === "capacity" && <CapacityDashboard stats={stats} signal={signal} onAccounts={onAccounts} />}
        {mode === "flow" && <FlowDashboard stats={stats} signal={signal} onAccounts={onAccounts} />}
        {mode === "demand" && <DemandDashboard stats={stats} signal={signal} />}
      </div>
    </div>
  );
}


function CapacityHistoryChart({ rows }: { rows: QuotaCapacityPoint[] }) {
  const eligible = rows.filter((row) => row.estimated_weekly_tokens > 0 && row.account_type in PROVIDERS);
  const series = [...new Set(eligible.map((row) => `${row.account_type}|${row.plan_type}`))];
  const weeks = [...new Set(eligible.map((row) => row.week_start))].sort();
  const data = weeks.map((week) => {
    const point: Record<string, string | number> = { week };
    for (const row of eligible.filter((item) => item.week_start === week)) point[`${row.account_type}|${row.plan_type}`] = row.estimated_weekly_tokens;
    return point;
  });
  if (data.length === 0) return <EmptyChart label="A weekly quota change is required before capacity can be estimated." />;
  const config = Object.fromEntries(series.map((key) => {
    const [provider, plan] = key.split("|");
    const display = providerDisplay(provider);
    return [key, { label: `${display.label} ${plan}`, color: display.dither }];
  })) as ChartConfig;
  return (
    <div className="chart-stage large">
      <LineChart data={data} config={config} margins={{ left: 52, bottom: 28 }} bloom="low" bloomOnHover>
        <Grid horizontal />
        {series.map((key) => <Line key={key} dataKey={key} isClickable />)}
        <XAxis dataKey="week" tickFormatter={(value) => String(value).slice(5)} maxTicks={6} />
        <YAxis tickFormatter={formatTokens} />
        <Legend isClickable />
        <Tooltip labelKey="week" valueFormatter={(value) => `${formatTokens(value)} tok/week`} />
      </LineChart>
    </div>
  );
}


function CapacityEvidenceTable({ rows }: { rows: QuotaCapacityPoint[] }) {
  const latestWeek = rows.reduce((latest, row) => row.week_start > latest ? row.week_start : latest, "");
  const latest = rows.filter((row) => row.week_start === latestWeek).sort((a, b) => b.estimated_weekly_tokens - a.estimated_weekly_tokens);
  return (
    <div className="evidence-table" role="table" aria-label="Empirical token capacity estimates">
      <div className="evidence-row evidence-head" role="row"><span>PLAN</span><span>WEEKLY TOKENS</span><span>RANGE</span><span>OBSERVED</span><span>CONFIDENCE</span></div>
      {latest.length === 0 && <div className="empty-signal">NO QUOTA MOVEMENT SAMPLES YET</div>}
      {latest.map((row) => {
        const provider = providerDisplay(row.account_type);
        return (
          <div className="evidence-row" role="row" key={`${row.account_type}-${row.plan_type}`} style={{ "--provider": provider.color } as CSSProperties}>
            <span className="evidence-plan"><i className="provider-mark" aria-hidden="true" /><b>{provider.label}</b><small>{row.plan_type}</small></span>
            <span><b>{formatTokens(row.estimated_weekly_tokens)}</b><small>OBSERVED MIX</small></span>
            <span><b>{formatTokens(row.low_estimate_tokens)}–{formatTokens(row.high_estimate_tokens)}</b><small>MIDDLE 50% RANGE</small></span>
            <span><b>{row.observed_quota_pct.toFixed(1)}%</b><small>{row.interval_count} TICKS</small></span>
            <span className={`confidence ${row.confidence}`}><b>{row.confidence}</b><small>{row.request_count} REQUESTS</small></span>
          </div>
        );
      })}
      <footer>Estimated from complete intervals between weekly quota changes. The range is observed, not provider-published.</footer>
    </div>
  );
}


type CalendarEvent = { id: string; at: Date; provider: Provider | "unknown"; kind: string; detail: string; tone: "future" | "credit" | "risk" };


function ResetCalendar({ stats, forecasts, observations }: { stats: PoolStats; forecasts: CapacityForecast[]; observations: ResetObservation[] }) {
  const generated = new Date(stats.generated_at).valueOf();
  const events: CalendarEvent[] = [];
  for (const account of stats.accounts) {
    if (account.secondary_window_available && account.secondary_reset_minutes >= 0) {
      events.push({ id: `${account.id}-weekly`, at: new Date(generated + account.secondary_reset_minutes * 60000), provider: account.type, kind: "WEEKLY RESET", detail: `${account.secondary_window_used_pct.toFixed(0)}% used · ${account.id.slice(-5)}`, tone: "future" });
    }
    for (const [index, expiration] of (account.reset_credit_expirations ?? []).entries()) {
      const at = new Date(expiration);
      if (!Number.isNaN(at.valueOf())) events.push({ id: `${account.id}-credit-${index}`, at, provider: account.type, kind: "BANKED RESET EXPIRES", detail: `${account.id.slice(-5)} · redeemable capacity`, tone: "credit" });
    }
  }
  for (const forecast of forecasts) {
    if (forecast.earliestFullMinutes !== null) {
      events.push({ id: `${forecast.provider}-full`, at: new Date(generated + forecast.earliestFullMinutes * 60000), provider: forecast.provider, kind: "PROJECTED EXHAUSTION", detail: `${forecast.loadEquivalents.toFixed(1)} account-eq load`, tone: "risk" });
    }
  }
  events.sort((a, b) => a.at.valueOf() - b.at.valueOf());
  return (
    <div className="calendar-board">
      <div className="calendar-list">
        {events.slice(0, 14).map((event) => {
          const provider = providerDisplay(event.provider);
          return (
            <div className={`calendar-event ${event.tone}`} key={event.id}>
              <time>{event.at.toLocaleDateString([], { month: "short", day: "numeric" })}<b>{event.at.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })}</b></time>
              <i className="provider-mark" style={{ "--provider": provider.color } as CSSProperties} aria-hidden="true" />
              <span><b>{event.kind}</b><small>{provider.label} · {event.detail}</small></span>
            </div>
          );
        })}
        {events.length === 0 && <div className="empty-signal">NO UPCOMING RESET EVENTS REPORTED</div>}
      </div>
      <div className="reset-behavior">
        <header><b>OBSERVED RESET BEHAVIOR</b><span>{observations.length} EVENTS / 30D</span></header>
        {observations.slice(0, 8).map((event) => {
          const provider = providerDisplay(event.account_type);
          const deviation = event.deviation_minutes;
          return (
            <div className={`reset-observation ${event.timing}`} key={`${event.account_id}-${event.observed_at}`}>
              <i className="provider-mark" style={{ "--provider": provider.color } as CSSProperties} aria-hidden="true" />
              <span><b>{provider.label} {event.timing.replace("_", " ").toUpperCase()}</b><small>{event.from_used_pct.toFixed(0)}% → {event.to_used_pct.toFixed(0)}% · {new Date(event.observed_at).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}</small></span>
              <strong>{deviation === undefined ? "SCHEDULE UNKNOWN" : `${deviation > 0 ? "+" : ""}${Math.round(deviation / 60)}H`}</strong>
            </div>
          );
        })}
        {observations.length === 0 && <div className="empty-signal compact-empty">RESET TIMING BASELINE STARTS WITH THIS DEPLOY</div>}
      </div>
    </div>
  );
}


function ScenarioPlanner({ forecasts, onAccounts }: { forecasts: CapacityForecast[]; onAccounts: () => void }) {
  const [demandPct, setDemandPct] = useState(100);
  const [reservePct, setReservePct] = useState(20);
  const rows = forecasts.map((forecast) => {
    const load = forecast.loadEquivalents * demandPct / 100;
    const required = Math.ceil(load * (1 + reservePct / 100));
    return { ...forecast, scenarioLoad: load, required, add: Math.max(0, required - forecast.activeAccounts) };
  });
  const totalAdds = rows.reduce((sum, row) => sum + row.add, 0);
  return (
    <div className="scenario-planner">
      <div className="scenario-controls">
        <label><span>DEMAND</span><b>{demandPct}%</b><input type="range" min="50" max="200" step="5" value={demandPct} onChange={(event) => setDemandPct(Number(event.target.value))} /></label>
        <label><span>RESERVE</span><b>{reservePct}%</b><input type="range" min="0" max="50" step="5" value={reservePct} onChange={(event) => setReservePct(Number(event.target.value))} /></label>
        <div className={classNames("scenario-outcome", totalAdds > 0 && "risk")}><span>POOL ACTION</span><strong>{totalAdds ? `ADD ${totalAdds}` : "CAPACITY HOLDS"}</strong><button onClick={onAccounts}>OPEN ACCOUNTS →</button></div>
      </div>
      <div className="scenario-rows">
        {rows.map((row) => {
          const display = providerDisplay(row.provider);
          return <div key={row.provider} style={{ "--provider": display.color } as CSSProperties}><b><i className="provider-mark" aria-hidden="true" /> {display.label}</b><span>{row.scenarioLoad.toFixed(1)} account-equivalent demand</span><span>{row.activeAccounts} active</span><strong>{row.add ? `+${row.add} REQUIRED` : `${row.required} REQUIRED`}</strong></div>;
        })}
      </div>
      <footer>SCENARIO SCALES THE CURRENT OBSERVED QUOTA DRAIN; IT DOES NOT ASSUME TOKENS ARE INTERCHANGEABLE BETWEEN PROVIDERS.</footer>
    </div>
  );
}


function ModelSubsidyTable({ rows }: { rows: ModelQuotaEfficiency[] }) {
  const eligible = rows.filter((row) => row.api_value > 0 && row.observed_quota_pct > 0).slice(0, 12);
  return (
    <div className="subsidy-table">
      <div className="subsidy-row subsidy-head"><span>MODEL</span><span>QUOTA</span><span>API VALUE</span><span>VALUE / 1%</span><span>VS PROVIDER</span><span>CONF.</span></div>
      {eligible.map((row) => {
        const provider = providerDisplay(row.account_type);
        return <div className="subsidy-row" key={`${row.account_type}-${row.model}`} style={{ "--provider": provider.color } as CSSProperties}>
          <span><i className="provider-mark" aria-hidden="true" /><b>{row.model}</b><small>{provider.label}</small></span>
          <span><b>{row.observed_quota_pct.toFixed(1)}%</b><small>{row.interval_count} intervals</small></span>
          <span><b>{preciseMoney.format(row.api_value)}</b><small>{formatTokens(row.tokens)} tok</small></span>
          <span><b>{preciseMoney.format(row.api_value_per_quota_pct)}</b><small>API-EQUIV</small></span>
          <span className={row.relative_subsidy >= 1 ? "favorable" : "costly"}><b>{row.relative_subsidy.toFixed(2)}×</b><small>{row.relative_subsidy >= 1 ? "MORE SUBSIDIZED" : "LESS SUBSIDIZED"}</small></span>
          <span className={`confidence ${row.confidence}`}><b>{row.confidence}</b></span>
        </div>;
      })}
      {eligible.length === 0 && <div className="empty-signal">Model efficiency needs priced requests observed across a quota change.</div>}
      <footer>SUBSIDY INDEX COMPARES API-EQUIVALENT VALUE PER OBSERVED QUOTA POINT WITH OTHER MODELS ON THE SAME PROVIDER. 1.00× IS PROVIDER AVERAGE.</footer>
    </div>
  );
}


function CapacityDashboard({ stats, signal, onAccounts }: { stats: PoolStats; signal: SignalAnalytics; onAccounts: () => void }) {
  const forecasts = capacityForecasts(stats.accounts);
  const latestCapacity = signal.quota_capacity.filter((row) => row.week_start === signal.quota_capacity.reduce((latest, row) => row.week_start > latest ? row.week_start : latest, ""));
  const measuredWeekly = latestCapacity.reduce((sum, row) => sum + row.estimated_weekly_tokens, 0);
  const highConfidence = latestCapacity.filter((row) => row.confidence === "high").length;
  const surpriseCount = signal.reset_observations.filter((event) => event.timing === "early" || event.timing === "late").length;
  return (
    <div className="insight-dashboard">
      <section className="inline-instruments insights-instruments">
        <Instrument label="MEASURED / WEEK" value={measuredWeekly ? formatTokens(measuredWeekly) : "acquiring"} accent />
        <Instrument label="PLANS MEASURED" value={String(latestCapacity.length)} />
        <Instrument label="HIGH CONFIDENCE" value={String(highConfidence)} accent />
        <Instrument label="QUOTA INTERVALS" value={String(latestCapacity.reduce((sum, row) => sum + row.interval_count, 0))} />
        <Instrument label="RESET SURPRISES / 30D" value={String(surpriseCount)} danger={surpriseCount > 0} />
        <Instrument label="MODEL REFRESH" value={signal.quota_generated_at ? new Date(signal.quota_generated_at).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }) : "acquiring"} />
      </section>
      <section className="capacity-history-grid">
        <SignalPanel title="Observed weekly token capacity"><CapacityHistoryChart rows={signal.quota_capacity} /></SignalPanel>
        <SignalPanel title="Latest capacity evidence"><CapacityEvidenceTable rows={signal.quota_capacity} /></SignalPanel>
      </section>
      <SignalPanel title="Capacity calendar"><ResetCalendar stats={stats} forecasts={forecasts} observations={signal.reset_observations} /></SignalPanel>
      <section className="capacity-lower-grid">
        <SignalPanel title="Demand and reserve planner"><ScenarioPlanner forecasts={forecasts} onAccounts={onAccounts} /></SignalPanel>
        <SignalPanel title="API value per quota point"><ModelSubsidyTable rows={signal.model_efficiency} /></SignalPanel>
      </section>
    </div>
  );
}


function AccountFlowTable({ rows }: { rows: AccountFlow[] }) {
  return (
    <div className="flow-table">
      <div className="flow-row flow-head"><span>ACCOUNT</span><span>USED</span><span>PROJECTED AT RESET</span><span>UNUSED</span><span>ROUTING CALL</span></div>
      {rows.map((row) => {
        const provider = providerDisplay(row.provider);
        const canReceiveShift = row.state === "stranded" && rows.some((candidate) => candidate.provider === row.provider && candidate.id !== row.id && (candidate.state === "exhausts" || candidate.state === "tight"));
        return <div className={`flow-row ${row.state}`} key={row.id} style={{ "--provider": provider.color } as CSSProperties}>
          <span><i className="provider-mark" aria-hidden="true" /><b>{provider.label}</b><small>{row.id.slice(-7)}</small></span>
          <span><b>{row.usedPct.toFixed(0)}%</b><small>NOW</small></span>
          <span><div className="flow-meter"><i style={{ width: `${Math.min(100, row.projectedFinalPct)}%` }} /></div><b>{row.projectedFinalPct.toFixed(0)}%</b></span>
          <span><b>{row.strandedPct.toFixed(0)}%</b><small>FORECAST</small></span>
          <span><b>{row.state === "exhausts" ? "ROUTE AWAY" : canReceiveShift ? "ROUTE HERE" : row.state === "stranded" ? "SURPLUS" : row.state === "tight" ? "WATCH" : "BALANCED"}</b><small>RESETS IN {formatReset(row.resetMinutes)}</small></span>
        </div>;
      })}
    </div>
  );
}


function FlowDashboard({ stats, signal, onAccounts }: { stats: PoolStats; signal: SignalAnalytics; onAccounts: () => void }) {
  const flows = accountFlow(stats.accounts);
  const stranded = flows.reduce((sum, row) => sum + row.strandedPct / 100, 0);
  const exhausting = flows.filter((row) => row.state === "exhausts");
  const providers = [...new Set(flows.map((row) => row.provider))];
  const routingCalls = providers.map((provider) => {
    const rows = flows.filter((row) => row.provider === provider);
    const hot = rows[0];
    const cold = [...rows].sort((a, b) => a.projectedFinalPct - b.projectedFinalPct)[0];
    const spread = hot && cold ? hot.projectedFinalPct - cold.projectedFinalPct : 0;
    return { provider, hot, cold, spread, score: Math.max(0, 100 - spread) };
  }).sort((a, b) => a.score - b.score);
  const unhealthy = stats.accounts.filter((account) => account.status !== "healthy").length;
  const cyberFailures = stats.cyber_policy?.counters?.swap_no_candidate ?? 0;
  return (
    <div className="insight-dashboard">
      <section className="inline-instruments insights-instruments">
        <Instrument label="STRANDED FORECAST" value={`${stranded.toFixed(1)} acct-eq`} accent />
        <Instrument label="EXHAUSTING EARLY" value={String(exhausting.length)} danger={exhausting.length > 0} />
        <Instrument label="BALANCED" value={String(flows.filter((row) => row.state === "balanced").length)} />
        <Instrument label="ROUTING SPREAD" value={`${(routingCalls[0]?.spread ?? 0).toFixed(0)}pt`} danger={(routingCalls[0]?.spread ?? 0) > 40} />
        <Instrument label="UNHEALTHY ACCOUNTS" value={String(unhealthy)} danger={unhealthy > 0} />
        <Instrument label="UNSERVED POLICY SWAPS" value={String(cyberFailures)} danger={cyberFailures > 0} />
      </section>
      <section className="flow-grid">
        <SignalPanel title="Projected account utilization"><AccountFlowTable rows={flows} /></SignalPanel>
        <SignalPanel title="Routing balance by provider">
          <div className="routing-calls">
            {routingCalls.map((call) => {
              const display = providerDisplay(call.provider);
              return <div className={call.score < 60 ? "risk" : ""} key={call.provider} style={{ "--provider": display.color } as CSSProperties}>
                <span><i className="provider-mark" aria-hidden="true" /><b>{display.label}</b><small>{call.score.toFixed(0)}/100 balance</small></span>
              <p>{call.hot && call.cold && call.hot.id !== call.cold.id && call.spread > 20 ? <>Shift new traffic from <b>{call.hot.id.slice(-5)}</b> toward <b>{call.cold.id.slice(-5)}</b>; projected utilization differs by {call.spread.toFixed(0)} points.</> : <>Current accounts are draining within a reasonable range.</>}</p>
            </div>;
            })}
          </div>
        </SignalPanel>
      </section>
      <section className="flow-lower-grid">
        <SignalPanel title="Capacity likely to reset unused">
          <div className="stranded-list">
            {flows.filter((row) => row.strandedPct >= 20).slice(0, 10).map((row) => {
              const display = providerDisplay(row.provider);
              return <div key={row.id}><span className="provider-mark" style={{ "--provider": display.color } as CSSProperties} aria-hidden="true" /><b>{display.label} {row.id.slice(-5)}</b><div><i style={{ width: `${row.strandedPct}%` }} /></div><strong>{row.strandedPct.toFixed(0)}% UNUSED</strong></div>;
            })}
            {flows.every((row) => row.strandedPct < 20) && <div className="empty-signal">NO MATERIAL STRANDED WEEKLY CAPACITY</div>}
          </div>
        </SignalPanel>
        <SignalPanel title="Current routing health">
          <div className="health-board">
            <div><span>HEALTHY</span><b>{stats.accounts.filter((account) => account.status === "healthy").length}</b><small>routing normally</small></div>
            <div><span>DEGRADED</span><b>{stats.accounts.filter((account) => account.status === "degraded").length}</b><small>penalty elevated</small></div>
            <div><span>COOLDOWN</span><b>{stats.accounts.filter((account) => account.status === "cooldown").length}</b><small>temporarily unavailable</small></div>
            <div><span>REVALIDATION</span><b>{stats.accounts.filter((account) => account.status === "verification_required").length}</b><small>account action required</small></div>
            <div><span>DEAD</span><b>{stats.accounts.filter((account) => account.status === "dead").length}</b><small>not in supply</small></div>
            <footer>RELIABILITY COUNTERS ARE PROCESS-LIFETIME SIGNALS TODAY. DURABLE LATENCY AND FAILURE HISTORY IS NOT YET RECORDED, SO THIS PANEL DOES NOT CLAIM A LONG-TERM SLA.</footer>
            <button onClick={onAccounts}>INSPECT ACCOUNTS →</button>
          </div>
        </SignalPanel>
      </section>
    </div>
  );
}


function PeakDemandHeatmap({ hourly }: { hourly: HourlyUsage[] }) {
  const cells = peakHeatmap(hourly);
  const maximum = Math.max(1, ...cells.map((cell) => cell.averageTokens));
  const days = ["SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"];
  return (
    <div className="peak-heatmap">
      <div className="heatmap-hours">{Array.from({ length: 24 }, (_, hour) => <span key={hour}>{hour % 3 === 0 ? String(hour).padStart(2, "0") : ""}</span>)}</div>
      {days.map((day, dayIndex) => <div className="heatmap-row" key={day}><b>{day}</b><div>{cells.filter((cell) => cell.day === dayIndex).map((cell) => {
        const intensity = cell.averageTokens / maximum;
        return <i key={cell.hour} title={`${day} ${String(cell.hour).padStart(2, "0")}:00 UTC · ${formatTokens(cell.averageTokens)} tokens · ${cell.averageRequests.toFixed(1)} requests`} style={{ opacity: 0.12 + intensity * 0.88 }} />;
      })}</div></div>)}
      <footer><span>QUIET</span><i /><i /><i /><i /><i /><span>PEAK</span><b>UTC · 14D HOURLY AVERAGE</b></footer>
    </div>
  );
}


function ModelDemandTable({ rows }: { rows: ModelDailyUsage[] }) {
  const models = modelMix(rows);
  const [metric, setMetric] = useState<"tokens" | "requests" | "apiValue">("tokens");
  const ranked = [...models].sort((a, b) => b[metric] - a[metric]);
  const total = ranked.reduce((sum, row) => sum + row[metric], 0);
  return (
    <div className="model-demand-table">
      <div className="model-demand-controls">
        <span>ALL {models.length} MODELS · RANK BY</span>
        {(["tokens", "requests", "apiValue"] as const).map((value) => <button className={metric === value ? "active" : ""} key={value} onClick={() => setMetric(value)}>{value === "apiValue" ? "API VALUE" : value.toUpperCase()}</button>)}
      </div>
      {ranked.map((row, index) => {
        const provider = providerDisplay(row.provider);
        const share = total ? row[metric] / total * 100 : 0;
        return <div key={`${row.provider}-${row.model}`} style={{ "--provider": provider.color } as CSSProperties}>
          <span><i>{String(index + 1).padStart(2, "0")}</i><b>{row.model}</b><small>{provider.label}</small></span>
          <div><i style={{ width: `${Math.max(1, share)}%` }} /></div>
          <strong>{share.toFixed(1)}%</strong><span><b>{metric === "tokens" ? formatTokens(row.tokens) : metric === "requests" ? `${compact.format(row.requests)} req` : preciseMoney.format(row.apiValue)}</b><small>{formatTokens(row.requests ? row.tokens / row.requests : 0)}/req · {preciseMoney.format(row.apiValue)}</small></span>
        </div>;
      })}
      {models.length === 0 && <div className="empty-signal">No model demand history has been recorded yet.</div>}
    </div>
  );
}


function ConservationBoard({ stats, signal }: { stats: PoolStats; signal: SignalAnalytics }) {
  const concentration = originConcentration(signal.origin_weekly);
  const models = modelMix(signal.model_daily);
  const totalTokens = models.reduce((sum, row) => sum + row.tokens, 0);
  const totalRequests = models.reduce((sum, row) => sum + row.requests, 0);
  const cacheShare = stats.aggregate.total_input_tokens ? stats.aggregate.total_cached_tokens / stats.aggregate.total_input_tokens * 100 : 0;
  const reasoningShare = stats.aggregate.total_billable_tokens ? stats.aggregate.total_reasoning_tokens / stats.aggregate.total_billable_tokens * 100 : 0;
  const calls = [
    { label: "CACHE REUSE", value: `${cacheShare.toFixed(1)}%`, state: cacheShare < 20 ? "review" : "good", copy: cacheShare < 20 ? "Low observed cache share. Repeated large contexts are the first conservation target." : "Cache reuse is materially reducing repeated input work." },
    { label: "REASONING LOAD", value: `${reasoningShare.toFixed(1)}%`, state: reasoningShare > 30 ? "review" : "good", copy: reasoningShare > 30 ? "Reasoning is a large share of billable work. Check whether every origin needs the current effort level." : "Reasoning share is within the current operating band." },
    { label: "AVG REQUEST", value: formatTokens(totalRequests ? totalTokens / totalRequests : 0), state: "neutral", copy: "Use the model and origin tables to investigate workloads far above this pool-wide baseline." },
    { label: "TOP ORIGIN", value: `${concentration.topOriginShare.toFixed(1)}%`, state: concentration.topOriginShare > 40 ? "review" : "good", copy: concentration.topOriginShare > 40 ? "One origin drives a large share of this week’s drain. Review it before adding broad capacity." : "Demand is not dominated by a single origin." },
  ];
  return <div className="conservation-board">{calls.map((call) => <div className={call.state} key={call.label}><span>{call.label}</span><b>{call.value}</b><p>{call.copy}</p></div>)}</div>;
}


function DemandDashboard({ stats, signal }: { stats: PoolStats; signal: SignalAnalytics }) {
  const concentration = originConcentration(signal.origin_weekly);
  const models = modelMix(signal.model_daily);
  const topModel = models[0];
  const demand = demandSummary(signal.hourly);
  return (
    <div className="insight-dashboard">
      <section className="inline-instruments insights-instruments">
        <Instrument label="24H DEMAND" value={formatTokens(demand.current24)} accent />
        <Instrument label="P95 BURST" value={`${demand.peakFactor.toFixed(1)}×`} danger={demand.peakFactor > 2} />
        <Instrument label="ACTIVE ORIGINS" value={String(concentration.origins)} />
        <Instrument label="TOP ORIGIN SHARE" value={`${concentration.topOriginShare.toFixed(1)}%`} danger={concentration.topOriginShare > 40} />
        <Instrument label="TOP 3 SHARE" value={`${concentration.topThreeShare.toFixed(1)}%`} />
        <Instrument label="TOP MODEL" value={topModel?.model ?? "acquiring"} accent />
      </section>
      <section className="demand-grid">
        <SignalPanel title="Peak demand by hour"><PeakDemandHeatmap hourly={signal.hourly} /></SignalPanel>
        <SignalPanel title="Model mix over 14 days"><ModelDemandTable rows={signal.model_daily} /></SignalPanel>
      </section>
      <section className="demand-lower-grid">
        <SignalPanel title="Usage concentration this week">
          <div className="concentration-board">
            <div className="concentration-gauge" style={{ "--share": `${concentration.topOriginShare}%` } as CSSProperties}><strong>{concentration.topOriginShare.toFixed(1)}%</strong><span>TOP ORIGIN</span></div>
            <div><span>ACTIVE ORIGINS</span><b>{concentration.origins}</b></div><div><span>TOP THREE</span><b>{concentration.topThreeShare.toFixed(1)}%</b></div><div><span>GINI</span><b>{concentration.gini.toFixed(2)}</b></div>
            <p>{concentration.topOriginShare > 40 ? "Demand is concentrated enough that one workload can materially change account requirements." : "Demand is distributed; broad pool growth matters more than a single origin."}</p>
          </div>
        </SignalPanel>
        <SignalPanel title="Opportunities to conserve capacity"><ConservationBoard stats={stats} signal={signal} /></SignalPanel>
      </section>
    </div>
  );
}


function InsightsOverview({ stats, signal, onAccounts }: { stats: PoolStats; signal: SignalAnalytics; onAccounts: () => void }) {
  const demand = demandSummary(signal.hourly);
  const forecasts = capacityForecasts(stats.accounts);
  const minimumAdds = forecasts.reduce((sum, forecast) => sum + forecast.minimumToAdd, 0);
  const bufferedAdds = forecasts.reduce((sum, forecast) => sum + forecast.bufferedToAdd, 0);
  const required = forecasts.filter((forecast) => forecast.minimumToAdd > 0);
  const reserves = forecasts.filter((forecast) => forecast.bufferedToAdd > 0);
  const directiveForecasts = minimumAdds > 0 ? required : reserves;
  const directiveBreakdown = directiveForecasts.map((forecast) => {
    const count = minimumAdds > 0 ? forecast.minimumToAdd : forecast.bufferedToAdd;
    return `${providerDisplay(forecast.provider).label.toUpperCase()} ${count}`;
  }).join(" · ");
  const directiveTitle = minimumAdds > 0
    ? `ADD ${minimumAdds} ACCOUNT${minimumAdds === 1 ? "" : "S"} NOW`
    : bufferedAdds > 0
      ? `ADD ${bufferedAdds} ACCOUNT${bufferedAdds === 1 ? "" : "S"} FOR RESERVE`
      : forecasts.length > 0 ? "CURRENT CAPACITY HOLDS" : "WAITING FOR WEEKLY QUOTA HISTORY";
  const directiveDetail = minimumAdds > 0
    ? `${directiveBreakdown}. The 20% reserve target is +${bufferedAdds} total.`
    : bufferedAdds > 0
      ? `${directiveBreakdown}. Baseline demand is covered; these additions establish the 20% reserve.`
      : forecasts.length > 0
        ? "Observed weekly drain is covered with the 20% operating reserve intact."
        : "No provider is reporting enough weekly-window history yet.";
  const modeledProviders = new Set(forecasts.map((forecast) => forecast.provider));
  const unmodeled = [...new Set(stats.accounts.map((account) => account.type))].filter((provider) => !modeledProviders.has(provider));
  const sampleDays = forecasts.reduce((sum, forecast) => sum + forecast.sampleAccountDays, 0);
  const demandDirection = demand.deltaPct >= 0 ? `+${demand.deltaPct.toFixed(1)}%` : `${demand.deltaPct.toFixed(1)}%`;

  return (
    <>
      <section className="inline-instruments insights-instruments" aria-label="Capacity planning summary">
        <Instrument label="DEMAND / 24H" value={formatTokens(demand.current24)} accent />
        <Instrument label="DAY / DAY" value={demandDirection} danger={demand.deltaPct > 20} />
        <Instrument label="7D DAILY AVG" value={formatTokens(demand.averageDay7d)} />
        <Instrument label="P95 BURST" value={`${demand.peakFactor.toFixed(1)}×`} danger={demand.peakFactor > 2} />
        <Instrument label="MINIMUM ADDS" value={`+${minimumAdds}`} danger={minimumAdds > 0} />
        <Instrument label="20% BUFFER ADDS" value={`+${bufferedAdds}`} accent />
      </section>

      <section className={classNames("capacity-directive", minimumAdds > 0 ? "urgent" : bufferedAdds > 0 ? "advisory" : "clear")}>
        <span>Recommended action</span>
        <strong>{directiveTitle}</strong>
        <p>{directiveDetail}</p>
        <button onClick={onAccounts}>OPEN ACCOUNTS →</button>
      </section>

      <section className="insights-grid">
        <SignalPanel title="Daily demand and three-day trend"><DemandTrendChart hourly={signal.hourly} /></SignalPanel>
        <SignalPanel title="Account capacity at the current pace"><CapacityForecastTable forecasts={forecasts} /></SignalPanel>
      </section>

      <section className="insight-method">
        <b>HOW THE ACCOUNT NUMBER WORKS</b>
        <span>For each provider: sum <code>weekly used % ÷ expected used % by now</code>, then round up. “+20%” adds an operating reserve. Recommendations use {sampleDays.toFixed(1)} observed account-days and update every 30 seconds.</span>
        {unmodeled.length > 0 && <span>Unmodeled: {unmodeled.map((provider) => providerDisplay(provider).label).join(" · ")}. Their tokens appear in demand trends, but they do not report a weekly limit.</span>}
      </section>
    </>
  );
}

import type { KeyboardEvent as ReactKeyboardEvent } from "react";
