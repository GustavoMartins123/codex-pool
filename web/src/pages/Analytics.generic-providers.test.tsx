// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { AccountStats, HourlyUsage, OriginWeeklyUsage, PoolStats, SignalAnalytics } from "../types";
import { Pulse } from "./Analytics";

vi.stubGlobal("IntersectionObserver", class {
  observe = vi.fn();
  disconnect = vi.fn();
  unobserve = vi.fn();
});

afterEach(cleanup);

function account(type: string, overrides: Partial<AccountStats> = {}): AccountStats {
  return {
    id: `configured-${type}`,
    type,
    plan_type: "configured",
    reset_windows: { tier: "unknown", primary: "none", secondary: "none" },
    status: "healthy",
    penalty: 0,
    primary_window_used_pct: 0,
    secondary_window_used_pct: 0,
    primary_window_available: false,
    secondary_window_available: false,
    primary_reset_minutes: 0,
    secondary_reset_minutes: 0,
    primary_window_minutes: 0,
    secondary_window_minutes: 0,
    primary_pace_ratio: 0,
    secondary_pace_ratio: 0,
    total_input_tokens: 0,
    total_cached_tokens: 0,
    total_output_tokens: 0,
    total_reasoning_tokens: 0,
    total_billable_tokens: 0,
    last_24h_tokens: 4000,
    cache_hit_rate_pct: 0,
    score: 0,
    is_primary: false,
    subscription_cost_monthly: 0,
    subscription_spend: 0,
    subscription_billing_cycles: 0,
    subscription_label: "configured",
    api_cost_estimate: 12,
    api_cost_last_30d: 12,
    roi: 0,
    ...overrides,
  };
}

function hourly(accountType: string, hour: string, tokens: number): HourlyUsage {
  return { hour, account_type: accountType, input_tokens: tokens, cached_tokens: 0, output_tokens: 0, reasoning_tokens: 0, billable_tokens: tokens, request_count: 1 };
}

function poolStats(accounts: AccountStats[]): PoolStats {
  return {
    generated_at: "2026-11-16T12:00:00Z",
    total_accounts: accounts.length,
    active_accounts: accounts.length,
    accounts,
    aggregate: {
      total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0,
      total_billable_tokens: 0, overall_cache_hit_rate_pct: 0,
      total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0,
    },
  };
}

function signalWith(overrides: Partial<SignalAnalytics> = {}): SignalAnalytics {
  return {
    generated_at: "2026-11-16T12:00:00Z",
    origin_data_since: "2026-11-01T00:00:00Z",
    economics: [], hourly: [], origin_weekly: [], model_daily: [], quota_capacity: [], model_efficiency: [], reset_observations: [],
    ...overrides,
  };
}

function renderPulse(accounts: AccountStats[], signal: SignalAnalytics) {
  return render(<Pulse stats={poolStats(accounts)} signal={signal} recentErrors={[]} onAccounts={() => {}} />);
}

it("gives a configured provider its own lane instead of dropping it", () => {
  const view = renderPulse([account("mock"), account("codex", { id: "codex-1" })], signalWith());
  const lanes = [...view.container.querySelectorAll(".provider-lane")];
  expect(lanes.map((lane) => lane.querySelector(".provider-name b")?.textContent)).toEqual(["Mock", "Codex"]);
  expect(lanes[0].textContent).toContain("1 account");
});

it("counts configured provider tokens in the burn chart and the demand instruments", () => {
  const hourlyRows = [
    hourly("mock", "2026-11-16T10:00:00Z", 1000),
    hourly("mock", "2026-11-16T11:00:00Z", 2000),
    hourly("codex", "2026-11-16T11:00:00Z", 500),
  ];
  renderPulse([account("mock")], signalWith({ hourly: hourlyRows }));
  expect(screen.getByRole("columnheader", { name: "Mock" })).toBeTruthy();
  expect(screen.getByText("Tokens in 24 hours").parentElement?.textContent).toContain("3.5K");
});

it("keeps configured providers in the origin footprint", () => {
  const rows: OriginWeeklyUsage[] = [{
    week_start: "2026-11-16", origin_id: "ip_abcd1234", account_id: "configured-mock", account_type: "mock",
    input_tokens: 900, cached_tokens: 0, output_tokens: 100, reasoning_tokens: 0, billable_tokens: 1000, request_count: 3,
  }];
  const view = renderPulse([account("mock")], signalWith({ origin_weekly: rows }));
  const footprint = view.container.querySelector(".footprint")!;
  expect(within(footprint as HTMLElement).getByTitle("Mock: 1K")).toBeTruthy();
});
