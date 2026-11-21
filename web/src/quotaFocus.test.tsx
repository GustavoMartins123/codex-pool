// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { accountFlow, capacityForecasts } from "./insights";
import { focusedQuotaAccount } from "./quotaFocus";
import { Pulse } from "./pages/Analytics";
import type { AccountStats, PoolStats, SignalAnalytics } from "./types";

vi.stubGlobal("IntersectionObserver", class {
  observe = vi.fn();
  disconnect = vi.fn();
  unobserve = vi.fn();
});
afterEach(cleanup);

// The blended account windows carry the most consumed bucket (Claude/GPT at
// 100%), while the Gemini family has barely been used.
const antigravity = {
  id: "ag", type: "antigravity", plan_type: "g1-pro-tier", status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 100, secondary_window_used_pct: 100, primary_window_available: true, secondary_window_available: true,
  primary_usage_reported: true, secondary_usage_reported: true,
  primary_reset_minutes: 180, secondary_reset_minutes: 7800, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 0, subscription_spend: 0, subscription_billing_cycles: 0, subscription_label: "", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
  quota_families: [
    { id: "gemini", label: "Gemini", weekly: { used_pct: 5, reset_minutes: 9000, window_minutes: 10080 } },
    { id: "argon", label: "Gemini 4 Argon", weekly: { used_pct: 81, reset_minutes: 9000, window_minutes: 10080 } },
    { id: "3p", label: "Claude / GPT", weekly: { used_pct: 100, reset_minutes: 7800, window_minutes: 10080 } },
  ],
} as AccountStats;

it("computes the weekly flow from the Gemini quota instead of the blended bucket", () => {
  const [flow] = accountFlow([antigravity]);
  expect(flow.usedPct).toBe(5);
  expect(flow.resetMinutes).toBe(9000);
  expect(flow.state).toBe("stranded");
  // The blended Claude/GPT bucket alone would have projected the account as exhausted.
  expect(accountFlow([{ ...antigravity, quota_families: undefined } as AccountStats])[0].state).toBe("exhausts");
});

it("forecasts capacity from the Gemini quota", () => {
  const blended = capacityForecasts([{ ...antigravity, quota_families: undefined } as AccountStats]);
  const focused = capacityForecasts([antigravity]);
  expect(blended).toHaveLength(1);
  expect(focused).toHaveLength(1);
  expect(focused[0].baselineAccounts).toBeLessThan(blended[0].baselineAccounts);
});

it("does not flag an account for attention when only Claude/GPT or Argon is nearly spent", () => {
  const stats: PoolStats = {
    generated_at: "2026-11-21T12:00:00Z", total_accounts: 1, active_accounts: 1, accounts: [antigravity],
    aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
  };
  const signal: SignalAnalytics = { generated_at: stats.generated_at, origin_data_since: "2026-11-01T00:00:00Z", economics: [], hourly: [], origin_weekly: [], model_daily: [], quota_capacity: [], model_efficiency: [], reset_observations: [] };
  const { container } = render(<Pulse stats={stats} signal={signal} recentErrors={[]} onAccounts={() => {}} />);
  expect(container.querySelector(".intervention-strip")).toBeNull();
});

it("leaves accounts that report no Gemini family untouched", () => {
  const plain = { ...antigravity, quota_families: undefined } as AccountStats;
  expect(focusedQuotaAccount(plain)).toBe(plain);
});
