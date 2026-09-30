import { type DitherColor } from "./components/dither-kit";
import { quotaEstimate } from "./insights";
import { type AccountStats, type PoolStats, type Provider, type ResetWindowKind } from "./types";
import { type ReactNode, useState } from "react";

export const PROVIDERS: Record<Provider, { label: string; color: string; dither: DitherColor; glyph: string }> = {
  codex: { label: "Codex", color: "#39e75f", dither: "green", glyph: "◎" },
  claude: { label: "Claude", color: "#a678ff", dither: "purple", glyph: "◉" },
  gemini: { label: "Gemini", color: "#27d8d1", dither: "cyan", glyph: "✦" },
	  antigravity: { label: "Antigravity", color: "#70d6ff", dither: "cyan", glyph: "✧" },
  kimi: { label: "Kimi", color: "#3f8cff", dither: "blue", glyph: "◈" },
  minimax: { label: "MiniMax", color: "#ffb23f", dither: "orange", glyph: "◇" },
  zai: { label: "Z.ai", color: "#ff5454", dither: "red", glyph: "◆" },
  xiaomi: { label: "Xiaomi", color: "#ff7b2d", dither: "orange", glyph: "◫" },
  grok: { label: "Grok", color: "#86efff", dither: "cyan", glyph: "⌁" },
  adverserial: { label: "Adverserial", color: "#ff5454", dither: "red", glyph: "◬" },
  opencode_go: { label: "OpenCode Go", color: "#ffd23f", dither: "gold", glyph: "⬢" },
  pool: { label: "Pool", color: "#d5a638", dither: "gold", glyph: "⊛" },
};


export const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });

export const money = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 0 });

export const preciseMoney = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 2 });


export function poolSurplus(aggregate: Pick<PoolStats["aggregate"], "total_api_cost" | "total_subscription_cost">) {
  return aggregate.total_api_cost - aggregate.total_subscription_cost;
}


export function formatAPIValue(value: number) {
  return preciseMoney.format(value || 0);
}


export function formatTokens(value: number) {
  return compact.format(value || 0).replace("T", "T");
}

// Burn is account throughput, not API-price-equivalent tokens. Codex-style
// usage includes cache reads inside input_tokens; Anthropic reports them as a
// separate field, so only Claude needs the cached count added explicitly.

export function tokenThroughput(row: { account_type: Provider | "unknown"; input_tokens: number; cached_tokens: number; output_tokens: number }) {
  return row.input_tokens + row.output_tokens + (row.account_type === "claude" ? row.cached_tokens : 0);
}


export function accountThroughput(account: AccountStats) {
  return account.last_24h_tokens ?? 0;
}


export function formatReset(minutes: number) {
  if (!minutes) return "now";
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor((minutes % 1440) / 60);
  return days ? `${days}d ${hours}h` : `${hours}h ${minutes % 60}m`;
}


export function originHandle(originID: string) {
  return originID.replace(/^ip_/, "").slice(0, 4).toUpperCase();
}


export function formatAdmission(value?: string) {
  if (!value) return "UNKNOWN";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? "UNKNOWN" : date.toLocaleDateString([], { year: "numeric", month: "short", day: "numeric" });
}


function paceLabel(paceRatio?: number) {
  if (!paceRatio || paceRatio <= 0) return "Pace unavailable";
  return paceRatio >= 1.1 ? `${paceRatio.toFixed(1)}× over pace` : `${paceRatio.toFixed(1)}× within pace`;
}


export function WeeklyPace({ account }: { account: AccountStats }) {
  const { primary, secondary } = account.reset_windows;
  const hasQuotaWindow = secondary === "weekly" || primary === "daily" || secondary === "secondary";
  const usePrimary = primary === "daily" || (secondary === "secondary" && !account.secondary_window_available && account.primary_window_available);
  const available = usePrimary ? account.primary_window_available : account.secondary_window_available;
  if (!hasQuotaWindow || !available) {
    return <span className="quota-limit unavailable">N/A</span>;
  }
  const usedPct = usePrimary ? account.primary_window_used_pct : account.secondary_window_used_pct;
  const resetMinutes = usePrimary ? account.primary_reset_minutes : account.secondary_reset_minutes;
  const rawWindow = usePrimary ? account.primary_window_minutes : account.secondary_window_minutes;
  const windowMinutes = rawWindow > 0 ? rawWindow : (usePrimary ? 1440 : 7 * 1440);
  const windowDays = windowMinutes / 1440;
  const budgetPerDay = 100 / windowDays;
  const estimate = quotaEstimate(usedPct, resetMinutes, windowMinutes);
  if (!estimate || usedPct <= 0) {
    return (
      <span className="quota-limit acquiring" aria-label={`${windowDays >= 7 ? "Weekly" : "Daily"} budget ${budgetPerDay.toFixed(1)} percent per day; not enough history to forecast`}>
        <b>—</b><small>{budgetPerDay.toFixed(1)}% daily budget</small><em>Not enough history</em>
      </span>
    );
  }
  const exhaustsEarly = estimate.fullInMinutes < resetMinutes;
  const forecast = exhaustsEarly ? `FULL IN ${formatReset(Math.max(1, Math.floor(estimate.fullInMinutes)))}` : "LASTS TO RESET";
  return (
    <span className={classNames("quota-limit", exhaustsEarly ? "fast" : "safe")} aria-label={`Burning ${estimate.burnPerDay.toFixed(1)} percent per day against a ${budgetPerDay.toFixed(1)} percent daily budget. ${forecast.toLowerCase()}.`}>
      <b>{estimate.burnPerDay.toFixed(1)}% per day</b><small>{budgetPerDay.toFixed(1)}% daily budget</small><em>{forecast === "LASTS TO RESET" ? "Lasts to reset" : forecast.toLowerCase()}</em>
    </span>
  );
}


function ResetWindow({ label, available, used, resetMinutes, paceRatio, showPace = false, compact = false }: { label: string; available: boolean; used: number; resetMinutes: number; paceRatio?: number; showPace?: boolean; compact?: boolean }) {
  if (!available) return <span className={classNames("reset-window unavailable", compact && "compact")}><b>{label}</b><small>NOT REPORTED</small></span>;
  if (compact) return <span className="reset-window compact" aria-label={`${label} ${used.toFixed(0)}%, resets in ${formatReset(resetMinutes)}`}><b>{label}</b><strong>{used.toFixed(0)}%</strong><small>{formatReset(resetMinutes)}</small></span>;
  return <span className="reset-window"><b>{label} {used.toFixed(0)}%</b><small>Resets in {formatReset(resetMinutes)}{showPace ? ` · ${paceLabel(paceRatio)}` : ""}</small></span>;
}


const RESET_WINDOW_LABELS: Record<ResetWindowKind, string> = {
  none: "",
  five_hour: "5 hour",
  daily: "Daily",
  weekly: "Weekly",
  tokens: "Tokens",
  requests: "Requests",
  primary: "Primary",
  secondary: "Secondary",
};


export function AccountResetWindows({ account, compact = false }: { account: AccountStats; compact?: boolean }) {
  const windows = [
    { kind: account.reset_windows.primary, available: account.primary_usage_reported ?? account.primary_window_available, used: account.primary_window_used_pct, resetMinutes: account.primary_reset_minutes, paceRatio: account.primary_pace_ratio },
    { kind: account.reset_windows.secondary, available: account.secondary_usage_reported ?? account.secondary_window_available, used: account.secondary_window_used_pct, resetMinutes: account.secondary_reset_minutes, paceRatio: account.secondary_pace_ratio },
  ];
  return <>{windows.filter((window) => window.kind !== "none").map((window) => (
    <ResetWindow key={window.kind} label={`${RESET_WINDOW_LABELS[window.kind]}${compact ? "" : " window"}`} available={window.available} used={window.used} resetMinutes={window.resetMinutes} paceRatio={window.paceRatio} showPace={!compact} compact={compact} />
  ))}</>;
}


function formatResetCreditExpiry(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.valueOf())) return "UNKNOWN EXPIRATION";
  const absolute = new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit",
    timeZoneName: "short",
  }).format(date);
  const remainingMinutes = Math.floor((date.valueOf() - Date.now()) / 60_000);
  if (remainingMinutes <= 0) return `${absolute} · expired`;
  const days = Math.floor(remainingMinutes / 1440);
  const hours = Math.floor((remainingMinutes % 1440) / 60);
  const minutes = remainingMinutes % 60;
  const relative = days > 0 ? `${days}D ${hours}H` : hours > 0 ? `${hours}H ${minutes}M` : `${minutes}M`;
  return `${absolute} · in ${relative.toLowerCase()}`;
}


export function ResetCreditExpirations({ account }: { account: AccountStats }) {
  const expirations = account.reset_credit_expirations ?? [];
  const count = account.reset_credits_available ?? expirations.length;
  const missing = Math.max(0, count - expirations.length);
  return (
    <>
      {expirations.map((expiry, index) => <span key={`${expiry}-${index}`}>{formatResetCreditExpiry(expiry)}</span>)}
      {missing > 0 && <span>{missing} EXPIRATION {missing === 1 ? "IS" : "ARE"} NOT REPORTED</span>}
      {count === 0 && <span>NO BANKED RESETS</span>}
    </>
  );
}


function ResetCreditBadge({ account }: { account: AccountStats }) {
  if (account.type !== "codex" || !account.reset_credits_known) return <span className="reset-credit unknown">—</span>;
  const count = account.reset_credits_available ?? 0;
  return (
    <span className="reset-credit" aria-label={`${count} banked usage reset${count === 1 ? "" : "s"}; select account for expiration details`}>
      <b aria-hidden="true">↻</b><strong>{count}</strong>
      <span className="reset-credit-popover" role="tooltip">
        <em>{count} BANKED USAGE RESET{count === 1 ? "" : "S"}</em>
        <ResetCreditExpirations account={account} />
      </span>
    </span>
  );
}


export function classNames(...values: Array<string | false | null | undefined>) {
  return values.filter(Boolean).join(" ");
}


export function CopyButton({ text, label = "Copy", className, disabled = false }: { text: string; label?: string; className?: string; disabled?: boolean }) {
  const [copiedText, setCopiedText] = useState("");
  const [failedText, setFailedText] = useState("");
  const copy = async () => {
    if (disabled) return;
    setCopiedText("");
    setFailedText("");
    try {
      await navigator.clipboard.writeText(text);
      setCopiedText(text);
    } catch {
      setFailedText(text);
    }
  };
  return <>
    <button type="button" className={className} onClick={copy} onBlur={() => setCopiedText("")} disabled={disabled} title={disabled ? "Generate a link first" : undefined}>{copiedText === text ? "Copied" : label}</button>
    {failedText === text && <span className="access-error" role="alert">Copy failed. Select the text and copy it manually.</span>}
  </>;
}


export function Instrument({ label, value, note, accent, danger }: { label: string; value: string; note?: string; accent?: boolean; danger?: boolean }) {
  return <div className={classNames("instrument", accent && "accent", danger && "danger")}><span>{label}</span><strong>{value}</strong>{note && <small>{note}</small>}</div>;
}


export function SignalPanel({ title, className, children }: { title: string; className?: string; children: ReactNode }) {
  return (
    <section className={classNames("signal-panel", className)}>
      <header><h2>{title}</h2></header>
      <div className="panel-body">{children}</div>
    </section>
  );
}


export function providerDisplay(provider: string) {
  return provider in PROVIDERS
    ? PROVIDERS[provider as Provider]
    : { label: "Unknown", color: "#9c967f", dither: "grey" as DitherColor, glyph: "·" };
}


export function EmptyChart({ label }: { label: string }) {
  return <div className="empty-chart"><span>{label}</span><i aria-hidden="true" /></div>;
}


export function SignalSkeleton() {
  return <div className="signal-skeleton" aria-label="Loading signal data"><i /><i /><i /><i /></div>;
}
