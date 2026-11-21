import type { AccountQuotaWindow, AccountStats } from "./types";

function windowPaceRatio(window: AccountQuotaWindow) {
  const elapsed = window.window_minutes - window.reset_minutes;
  if (window.window_minutes <= 0 || elapsed < window.window_minutes / 100) return 0;
  return window.used_pct / (100 * elapsed / window.window_minutes);
}


// The account-level Antigravity windows blend every model family into the most
// consumed bucket, so an exhausted Claude/GPT or Gemini 4 Argon bucket reads as
// a full Gemini quota. Every view that reads an account's quota leads with the
// Gemini family instead, and keeps the other families in the breakdown.
export function focusedQuotaAccount(account: AccountStats): AccountStats {
  const gemini = account.type === "antigravity" ? account.quota_families?.find((family) => family.id === "gemini") : undefined;
  if (!gemini || (!gemini.five_hour && !gemini.weekly)) return account;
  const { five_hour: fiveHour, weekly } = gemini;
  return {
    ...account,
    primary_window_available: Boolean(fiveHour),
    primary_usage_reported: Boolean(fiveHour),
    primary_window_used_pct: fiveHour?.used_pct ?? 0,
    primary_reset_minutes: fiveHour?.reset_minutes ?? 0,
    primary_window_minutes: fiveHour?.window_minutes ?? 300,
    primary_pace_ratio: fiveHour ? windowPaceRatio(fiveHour) : 0,
    secondary_window_available: Boolean(weekly),
    secondary_usage_reported: Boolean(weekly),
    secondary_window_used_pct: weekly?.used_pct ?? 0,
    secondary_reset_minutes: weekly?.reset_minutes ?? 0,
    secondary_window_minutes: weekly?.window_minutes ?? 7 * 1440,
    secondary_pace_ratio: weekly ? windowPaceRatio(weekly) : 0,
  };
}
