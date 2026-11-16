// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { PoolStats, RecentError, SignalAnalytics } from "../types";
import { Pulse } from "./Analytics";

vi.stubGlobal("IntersectionObserver", class {
  observe = vi.fn();
  disconnect = vi.fn();
  unobserve = vi.fn();
});

const stats: PoolStats = {
  generated_at: "2026-11-16T12:00:00Z",
  total_accounts: 0,
  active_accounts: 0,
  accounts: [],
  aggregate: {
    total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0,
    total_billable_tokens: 0, overall_cache_hit_rate_pct: 0,
    total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0,
  },
};
const signal: SignalAnalytics = {
  generated_at: "2026-11-16T12:00:00Z",
  origin_data_since: "2026-11-01T00:00:00Z",
  economics: [], hourly: [], origin_weekly: [], model_daily: [], quota_capacity: [], model_efficiency: [], reset_observations: [],
};

afterEach(cleanup);

function renderPulse(recentErrors: RecentError[]) {
  return render(<Pulse stats={stats} signal={signal} recentErrors={recentErrors} onAccounts={() => {}} />);
}

it("lists recent provider errors with their timestamps", () => {
  renderPulse([
    { at: "2026-11-16T12:00:00Z", message: "codex upstream returned 502" },
    { at: "2026-11-16T11:30:00Z", message: "claude stream copy failed" },
  ]);
  const log = screen.getByRole("log", { name: "Recent provider errors" });
  const rows = [...log.querySelectorAll("time")].map(time => time.parentElement!);
  expect(rows).toHaveLength(2);
  expect(rows[0].textContent).toContain("codex upstream returned 502");
  expect(rows[0].querySelector("time")?.getAttribute("datetime")).toBe("2026-11-16T12:00:00Z");
  expect(rows[1].textContent).toContain("claude stream copy failed");
});

it("caps the list at the twenty most recent errors", () => {
  const errors = Array.from({ length: 25 }, (_, index) => ({ at: `2026-11-16T12:${String(index).padStart(2, "0")}:00Z`, message: `failure ${index}` }));
  renderPulse(errors);
  const log = screen.getByRole("log", { name: "Recent provider errors" });
  expect(log.querySelectorAll("time")).toHaveLength(20);
  expect(log.textContent).toContain("failure 0");
  expect(log.textContent).not.toContain("failure 24");
});

it("shows an empty state when the pool has not recorded errors", () => {
  renderPulse([]);
  expect(screen.queryByRole("log", { name: "Recent provider errors" })).toBeNull();
  expect(screen.getByText("No provider errors recorded since the pool started.")).toBeTruthy();
});
