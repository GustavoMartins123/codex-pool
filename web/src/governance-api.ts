export type Limits = Partial<Record<"token_reservation" | "requests_per_minute" | "concurrent_requests" | "daily_requests" | "monthly_requests" | "daily_tokens" | "monthly_tokens", number>>;
export type Selector = { allow?: string[]; deny?: string[] };
export type Policy = { models: Selector; providers: Selector; limits: Limits; routing: { profile?: string }; priority?: number };
export type Controls = { state: "enabled" | "disabled" | "maintenance" | "draining"; max_concurrent: number };
export type ControlView = { provider: string; id: string; revision: number; controls: Controls; inflight: number };
export type Grant = { id: string; recipient_id: string; models: string[]; budget: Limits; expires_at: string; revision: number; revoked_at?: string; reason: string };
export type SharingView = { provider: string; id: string; revision: number; owner: boolean; operator_may_delegate: boolean; grants: Grant[] };
type Counter = { requests: number; tokens: number; reserved_tokens?: number };
export type PolicyView = { principal_id: string; revision: number; principal_policy: Policy; clients: { id: string; label: string; policy: Policy; status: string }[]; sources: { source: string; policy: Policy }[]; effective: Policy; allowed: boolean; reasons: string[]; usage: { scope: string; limits: Limits; minute: Counter; day: Counter; month: Counter; inflight: number }[] };
export type PolicyDraft = { revision: number; target: "principal" | "client"; client_id: string; policy: Policy; model: string; provider: string; account_id: string; tokens: number };

export function emptyPolicy(): Policy { return { models: {}, providers: {}, limits: {}, routing: {} }; }

async function request(path: string, method = "GET", body?: unknown): Promise<unknown> {
  const csrf = document.cookie.split("; ").find((part) => part.startsWith("pool_csrf="))?.slice("pool_csrf=".length) ?? "";
  const response = await fetch(path, { method, credentials: "same-origin", cache: "no-store", headers: { "Content-Type": "application/json", ...(method === "GET" ? {} : { "X-CSRF-Token": csrf }) }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) });
  const payload: unknown = await response.json();
  if (!response.ok) {
    const value = payload as { error?: unknown; code?: string };
    const message = typeof value?.error === "string" ? value.error : object(value?.error) && typeof value.error.message === "string" ? value.error.message : `Request failed (${response.status})`;
    throw new Error(response.status === 409 ? `${message}. Reload before trying again.` : message);
  }
  return payload;
}
function object(value: unknown): value is Record<string, unknown> { return value !== null && typeof value === "object" && !Array.isArray(value); }
function policy(value: unknown): value is Policy {
  if (!object(value) || !object(value.models) || !object(value.providers) || !object(value.limits) || !object(value.routing)) return false;
  for (const selector of [value.models, value.providers]) for (const key of ["allow", "deny"]) if (selector[key] !== undefined && (!Array.isArray(selector[key]) || !selector[key].every((item: unknown) => typeof item === "string"))) return false;
  return Object.values(value.limits).every((item) => typeof item === "number" && Number.isSafeInteger(item) && item >= 0);
}
function controls(value: unknown): ControlView {
  if (!object(value) || !Number.isSafeInteger(value.revision) || !object(value.controls) || !["enabled", "disabled", "maintenance", "draining"].includes(String(value.controls.state)) || !Number.isSafeInteger(value.controls.max_concurrent) || !Number.isSafeInteger(value.inflight)) throw new Error("Invalid account controls response");
  return value as ControlView;
}
function sharing(value: unknown): SharingView {
  if (!object(value) || !Number.isSafeInteger(value.revision) || typeof value.owner !== "boolean" || typeof value.operator_may_delegate !== "boolean" || !Array.isArray(value.grants) || !value.grants.every((grant) => object(grant) && typeof grant.id === "string" && typeof grant.recipient_id === "string" && Array.isArray(grant.models) && object(grant.budget) && typeof grant.expires_at === "string" && Number.isSafeInteger(grant.revision))) throw new Error("Invalid account sharing response");
  return value as SharingView;
}
function policies(value: unknown): PolicyView {
  if (!object(value) || !Number.isSafeInteger(value.revision) || !policy(value.principal_policy) || !policy(value.effective) || typeof value.allowed !== "boolean" || !Array.isArray(value.clients) || !value.clients.every((c) => object(c) && typeof c.id === "string" && policy(c.policy)) || !Array.isArray(value.sources) || !value.sources.every((s) => object(s) && typeof s.source === "string" && policy(s.policy)) || !Array.isArray(value.reasons) || !Array.isArray(value.usage)) throw new Error("Invalid policy response");
  return value as PolicyView;
}
const accountPath = (provider: string, id: string) => `/api/accounts/${encodeURIComponent(provider)}/${encodeURIComponent(id)}`;
const policyPath = (id: string) => `/api/console/policies/${encodeURIComponent(id)}`;
export async function loadControls(provider: string, id: string) { return controls(await request(`${accountPath(provider, id)}/controls`)); }
export async function saveControls(provider: string, id: string, revision: number, controlsValue: Controls, reason: string) { return controls(await request(`${accountPath(provider, id)}/controls`, "PUT", { revision, controls: controlsValue, reason })); }
export async function loadSharing(provider: string, id: string) { return sharing(await request(`${accountPath(provider, id)}/grants`)); }
export async function setDelegation(provider: string, id: string, revision: number, allowed: boolean) { return sharing(await request(`${accountPath(provider, id)}/delegation`, "PUT", { revision, allowed })); }
export async function createGrant(provider: string, id: string, value: { revision: number; id: string; recipient_id: string; models: string[]; budget: Limits; expires_at: string; reason: string }) { return sharing(await request(`${accountPath(provider, id)}/grants`, "POST", value)); }
export async function updateGrant(provider: string, id: string, value: { revision: number; grant_revision: number; id: string; recipient_id: string; models: string[]; budget: Limits; expires_at: string; reason: string }) { return sharing(await request(`${accountPath(provider, id)}/grants`, "PUT", value)); }
export async function revokeGrant(id: string, revision: number) { await request(`/api/account-grants/${encodeURIComponent(id)}`, "DELETE", { revision }); }
export async function loadPolicies(id: string, client = "") { return policies(await request(`${policyPath(id)}?client_id=${encodeURIComponent(client)}`)); }
export async function previewPolicy(id: string, draft: PolicyDraft) { return policies(await request(`${policyPath(id)}/preview`, "POST", draft)); }
export async function savePolicy(id: string, draft: PolicyDraft) { return policies(await request(policyPath(id), "PUT", draft)); }
export async function loadShareableAccounts(): Promise<{ id: string; provider: string; owner_id: string }[]> {
  const value = await request("/api/console/shareable-accounts");
  if (!Array.isArray(value) || !value.every((item) => object(item) && typeof item.id === "string" && typeof item.provider === "string" && typeof item.owner_id === "string")) throw new Error("Invalid shareable accounts response");
  return value;
}
