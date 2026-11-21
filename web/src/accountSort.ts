import { type AccountStats } from "./types";
import { accountThroughput, providerDisplay, weeklyPaceBurnPerDay } from "./ui";

export type AccountSortKey = "provider" | "plan" | "account" | "state" | "pace" | "reset" | "burn" | "value";
export type SortDirection = "asc" | "desc";
export interface AccountSort { key: AccountSortKey; direction: SortDirection }

export const ACCOUNT_SORT_KEYS: AccountSortKey[] = ["provider", "plan", "account", "state", "pace", "reset", "burn", "value"];

export function parseAccountSort(key: string | null, direction: string | null): AccountSort | null {
  if (!key || !ACCOUNT_SORT_KEYS.includes(key as AccountSortKey)) return null;
  return { key: key as AccountSortKey, direction: direction === "desc" ? "desc" : "asc" };
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
    case "pace": return weeklyPaceBurnPerDay(account);
    case "reset": return soonestReset(account);
    case "burn": return accountThroughput(account);
    case "value": return account.api_cost_estimate;
  }
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

function compareValues(a: string | number, b: string | number) {
  return typeof a === "number" && typeof b === "number" ? a - b : collator.compare(String(a), String(b));
}

// Accounts without a value for the column stay at the bottom in both directions.
export function sortAccounts(accounts: AccountStats[], sort: AccountSort | null, displayID: (account: AccountStats) => string = (account) => account.id): AccountStats[] {
  if (!sort) return accounts;
  const sign = sort.direction === "asc" ? 1 : -1;
  return accounts
    .map((account) => ({ account, id: displayID(account), value: sortValue(account, sort.key, displayID(account)) }))
    .sort((left, right) => {
      if (left.value === null || right.value === null) {
        if (left.value === right.value) return collator.compare(left.id, right.id);
        return left.value === null ? 1 : -1;
      }
      const primary = compareValues(left.value, right.value) * sign;
      if (primary !== 0) return primary;
      return collator.compare(providerDisplay(left.account.type).label, providerDisplay(right.account.type).label) || collator.compare(left.id, right.id);
    })
    .map((entry) => entry.account);
}
