import { expect, it } from "vitest";
import { nextAccountSort, parseAccountSort, sortAccounts } from "./accountSort";
import type { AccountStats } from "./types";

const base = {
  id: "a", type: "codex", plan_type: "pro", status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: true, secondary_window_available: true,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 0, subscription_spend: 0, subscription_billing_cycles: 0, subscription_label: "", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
} as AccountStats;
const make = (overrides: Partial<AccountStats>) => ({ ...base, ...overrides }) as AccountStats;
const ids = (accounts: AccountStats[]) => accounts.map((account) => account.id);

const accounts = [
  make({ id: "b", type: "claude", plan_type: "pro", subscription_cost_monthly: 20, api_cost_estimate: 14 }),
  make({ id: "c", type: "antigravity", plan_type: "free-tier", api_cost_estimate: 35 }),
  make({ id: "a", type: "antigravity", plan_type: "g1-pro-tier", api_cost_estimate: 0.4 }),
  make({ id: "d", type: "aistudio", plan_type: "", api_cost_estimate: 0 }),
];

it("keeps the pool order when no sort is active", () => {
  expect(sortAccounts(accounts, null)).toBe(accounts);
});

it("sorts by provider, breaking ties by account id", () => {
  expect(ids(sortAccounts(accounts, { key: "provider", direction: "asc" }))).toEqual(["d", "a", "c", "b"]);
  expect(ids(sortAccounts(accounts, { key: "provider", direction: "desc" }))).toEqual(["b", "a", "c", "d"]);
});

it("sorts by plan price, most expensive first, and keeps accounts without a plan last in both directions", () => {
  expect(ids(sortAccounts(accounts, { key: "plan", direction: "asc" }))).toEqual(["b", "a", "c", "d"]);
  expect(ids(sortAccounts(accounts, { key: "plan", direction: "desc" }))).toEqual(["c", "a", "b", "d"]);
});

it("lists each group's plans from the most expensive down to free in both directions", () => {
  const pool = [
    make({ id: "codex_1f", type: "codex", plan_type: "free", subscription_cost_monthly: 0 }),
    make({ id: "codex_2c", type: "codex", plan_type: "plus", subscription_cost_monthly: 20 }),
    make({ id: "codex_3a", type: "codex", plan_type: "pro", subscription_cost_monthly: 200 }),
    make({ id: "codex_4e", type: "codex", plan_type: "team", subscription_cost_monthly: 25 }),
    make({ id: "codex_5b", type: "codex", plan_type: "prolite", subscription_cost_monthly: 100 }),
    make({ id: "ag_1", type: "antigravity", plan_type: "free-tier" }),
    make({ id: "ag_2", type: "antigravity", plan_type: "g1-pro-tier" }),
    make({ id: "ag_3", type: "antigravity", plan_type: "g1-ultra-tier" }),
  ];
  const codex = ["codex_3a", "codex_5b", "codex_4e", "codex_2c", "codex_1f"];
  const antigravity = ["ag_3", "ag_2", "ag_1"];
  expect(ids(sortAccounts(pool, { key: "provider", direction: "asc" }))).toEqual([...antigravity, ...codex]);
  expect(ids(sortAccounts(pool, { key: "provider", direction: "desc" }))).toEqual([...codex, ...antigravity]);
});

it("breaks ties in other columns by provider, then by the most expensive plan", () => {
  const pool = [
    make({ id: "x", type: "codex", plan_type: "plus", subscription_cost_monthly: 20, status: "healthy" }),
    make({ id: "y", type: "codex", plan_type: "pro", subscription_cost_monthly: 200, status: "healthy" }),
    make({ id: "z", type: "codex", plan_type: "free", status: "cooldown" }),
  ];
  expect(ids(sortAccounts(pool, { key: "state", direction: "desc" }))).toEqual(["y", "x", "z"]);
});

it("sorts by account id and by API value numerically", () => {
  expect(ids(sortAccounts(accounts, { key: "account", direction: "asc" }))).toEqual(["a", "b", "c", "d"]);
  expect(ids(sortAccounts(accounts, { key: "value", direction: "desc" }))).toEqual(["c", "b", "a", "d"]);
});

it("uses the display id supplied by the caller", () => {
  const reversed: Record<string, string> = { a: "pool-4", b: "pool-3", c: "pool-2", d: "pool-1" };
  expect(ids(sortAccounts(accounts, { key: "account", direction: "asc" }, (account) => reversed[account.id]))).toEqual(["d", "c", "b", "a"]);
});

it("cycles ascending, descending, then off, and resets when the column changes", () => {
  const asc = nextAccountSort(null, "plan");
  expect(asc).toEqual({ key: "plan", direction: "asc" });
  const desc = nextAccountSort(asc, "plan");
  expect(desc).toEqual({ key: "plan", direction: "desc" });
  expect(nextAccountSort(desc, "plan")).toBeNull();
  expect(nextAccountSort(desc, "provider")).toEqual({ key: "provider", direction: "asc" });
});

it("rejects unknown sort keys from the URL", () => {
  expect(parseAccountSort("bogus", "asc")).toBeNull();
  expect(parseAccountSort("state", "desc")).toEqual({ key: "state", direction: "desc" });
  expect(parseAccountSort("state", null)).toEqual({ key: "state", direction: "asc" });
});
