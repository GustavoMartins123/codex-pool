import { type AccountStats } from "./types";
import { accountThroughput, focusedQuotaAccount, providerDisplay, weeklyPaceBurnPerDay } from "./ui";

export type AccountSortKey = "provider" | "plan" | "account" | "state" | "pace" | "reset" | "burn" | "value";
export type SortDirection = "asc" | "desc";
export interface AccountSort { key: AccountSortKey; direction: SortDirection }

export const ACCOUNT_SORT_KEYS: AccountSortKey[] = ["provider", "plan", "account", "state", "pace", "reset", "burn", "value"];

export function parseAccountSort(key: string | null, direction: string | null): AccountSort | null {
  if (!key || !ACCOUNT_SORT_KEYS.includes(key as AccountSortKey)) return null;
  return { key: key as AccountSortKey, direction: direction === "desc" ? "desc" : "asc" };
}

const SORT_STORAGE_KEY = "codex-pool.accounts.sort";

// Storage can be missing or throw (private windows, blocked site data); the table then simply starts unsorted.
export function loadStoredAccountSort(): AccountSort | null {
  try {
    const raw = window.localStorage.getItem(SORT_STORAGE_KEY);
    if (!raw) return null;
    const stored = JSON.parse(raw) as { key?: string; direction?: string };
    return parseAccountSort(stored.key ?? null, stored.direction ?? null);
  } catch {
    return null;
  }
}

export function storeAccountSort(sort: AccountSort | null) {
  try {
    if (sort) window.localStorage.setItem(SORT_STORAGE_KEY, JSON.stringify(sort));
    else window.localStorage.removeItem(SORT_STORAGE_KEY);
  } catch {
    // The preference just will not survive a reload.
  }
}

// Each click cycles ascending, descending, then back to the pool's own order.
export function nextAccountSort(current: AccountSort | null, key: AccountSortKey): AccountSort | null {
  if (current?.key !== key) return { key, direction: "asc" };
  return current.direction === "asc" ? { key, direction: "desc" } : null;
}

function soonestReset(account: AccountStats): number | null {
  const resets: number[] = [];
  if (account.reset_windows.primary !== "none" && (account.primary_usage_reported ?? account.primary_window_available)) resets.push(account.primary_reset_minutes);
  if (account.reset_windows.secondary !== "none" && (account.secondary_usage_reported ?? account.secondary_window_available)) resets.push(account.secondary_reset_minutes);
  return resets.length ? Math.min(...resets) : null;
}

function sortValue(account: AccountStats, key: AccountSortKey, displayID: string): string | number | null {
  switch (key) {
    case "provider": return providerDisplay(account.type).label;
    case "plan": return account.plan_type || null;
    case "account": return displayID;
    case "state": return account.status;
    case "pace": return weeklyPaceBurnPerDay(focusedQuotaAccount(account));
    case "reset": return soonestReset(focusedQuotaAccount(account));
    case "burn": return accountThroughput(account);
    case "value": return account.api_cost_estimate;
  }
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

function compareValues(a: string | number, b: string | number) {
  return typeof a === "number" && typeof b === "number" ? a - b : collator.compare(String(a), String(b));
}

// Plan names rank tiers the price table cannot tell apart: Antigravity reports
// every tier at $0, and unknown plans have no price at all.
const PLAN_TIERS: [RegExp, number][] = [[/ultra/, 5], [/max|20x|enterprise/, 4], [/pro/, 3], [/team|business/, 2], [/plus|standard|lite/, 1]];

function planPrice(account: AccountStats): [number, number, number] {
  const plan = (account.plan_type || "").toLowerCase();
  if (!plan) return [0, 0, 0];
  if (plan.includes("free")) return [1, 0, 0];
  return [2, account.subscription_cost_monthly || 0, PLAN_TIERS.find(([pattern]) => pattern.test(plan))?.[1] ?? 0];
}

// byPlanPriceDesc orders paid plans from the most expensive down, then free
// plans, then accounts that report no plan.
function byPlanPriceDesc(left: AccountStats, right: AccountStats) {
  const a = planPrice(left);
  const b = planPrice(right);
  return b[0] - a[0] || b[1] - a[1] || b[2] - a[2];
}

// Accounts without a value for the column stay at the bottom in both directions.
// Ties keep providers together and list each group's most expensive plan first.
export function sortAccounts(accounts: AccountStats[], sort: AccountSort | null, displayID: (account: AccountStats) => string = (account) => account.id): AccountStats[] {
  if (!sort) return accounts;
  const sign = sort.direction === "asc" ? 1 : -1;
  return accounts
    .map((account) => ({ account, id: displayID(account), value: sortValue(account, sort.key, displayID(account)) }))
    .sort((left, right) => {
      const tie = () => collator.compare(providerDisplay(left.account.type).label, providerDisplay(right.account.type).label) || byPlanPriceDesc(left.account, right.account) || collator.compare(left.id, right.id);
      if (left.value === null || right.value === null) {
        if (left.value === right.value) return tie();
        return left.value === null ? 1 : -1;
      }
      const primary = (sort.key === "plan" ? byPlanPriceDesc(left.account, right.account) : compareValues(left.value, right.value)) * sign;
      return primary || tie();
    })
    .map((entry) => entry.account);
}
