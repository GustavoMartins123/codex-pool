import type { Limits, Policy } from "../governance-api";
import { useEffect, useState } from "react";

export const limitLabels: [keyof Limits, string][] = [["requests_per_minute", "Requests per minute"], ["concurrent_requests", "Concurrent requests"], ["daily_requests", "Daily requests"], ["monthly_requests", "Monthly requests"], ["daily_tokens", "Daily tokens"], ["monthly_tokens", "Monthly tokens"], ["token_reservation", "Token reservation"]];
export function selectors(value: string): string[] { return value.split(",").map((item) => item.trim().toLowerCase()).filter(Boolean); }
function SelectorField({ label, values, onChange }: { label: string; values: string[]; onChange: (values: string[]) => void }) {
  const [text, setText] = useState(values.join(", "));
  useEffect(() => { if (selectors(text).join(",") !== values.join(",")) setText(values.join(", ")); }, [values]);
  return <label>{label}<input value={text} placeholder="Comma-separated identifiers" onChange={(event) => { setText(event.target.value); onChange(selectors(event.target.value)); }} /></label>;
}
export function BudgetFields({ value, onChange, disabled }: { value: Limits; onChange: (value: Limits) => void; disabled?: boolean }) {
  return <div className="governance-fields">{limitLabels.map(([key, label]) => <label key={key}>{label}<input type="number" min="0" max={Number.MAX_SAFE_INTEGER} step="1" value={value[key] ?? 0} disabled={disabled} onChange={(event) => onChange({ ...value, [key]: event.target.valueAsNumber })} /></label>)}<small>Zero leaves a limit unset. Token budgets require a positive reservation.</small></div>;
}
export function PolicyFields({ value, onChange, disabled }: { value: Policy; onChange: (value: Policy) => void; disabled?: boolean }) {
  return <fieldset disabled={disabled}><legend>Policy restrictions</legend><div className="governance-fields">{(["models", "providers"] as const).flatMap((kind) => (["allow", "deny"] as const).map((selector) => <SelectorField key={`${kind}:${selector}`} label={`${selector === "allow" ? "Allowed" : "Denied"} ${kind}`} values={value[kind][selector] ?? []} onChange={(values) => onChange({ ...value, [kind]: { ...value[kind], [selector]: values } })} />))}<label>Routing profile<input value={value.routing.profile ?? ""} onChange={(event) => onChange({ ...value, routing: { profile: event.target.value } })} /></label><label>Priority<input type="number" min="0" max="100" step="1" value={value.priority ?? 0} onChange={(event) => onChange({ ...value, priority: event.target.valueAsNumber })} /></label></div><BudgetFields value={value.limits} onChange={(limits) => onChange({ ...value, limits })} /></fieldset>;
}
export function validLimits(limits: Limits): boolean {
  return Object.values(limits).every((value) => Number.isSafeInteger(value) && value >= 0) && (!(Number(limits.daily_tokens) > 0 || Number(limits.monthly_tokens) > 0) || Number(limits.token_reservation) > 0);
}
