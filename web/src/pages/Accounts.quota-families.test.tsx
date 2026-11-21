// @vitest-environment jsdom
import { cleanup, fireEvent, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { focusedQuotaAccount } from "../ui";
import type { AccountQuotaFamily, AccountStats, PoolStats } from "../types";

vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const families: AccountQuotaFamily[] = [
  { id: "gemini", label: "Gemini", five_hour: { used_pct: 6, reset_minutes: 297, window_minutes: 300 }, weekly: { used_pct: 0, reset_minutes: 8820, window_minutes: 10080 } },
  { id: "argon", label: "Gemini 4 Argon", weekly: { used_pct: 29.5, reset_minutes: 10042, window_minutes: 10080 } },
  { id: "3p", label: "Claude / GPT", five_hour: { used_pct: 0, reset_minutes: 297, window_minutes: 300 }, weekly: { used_pct: 100, reset_minutes: 7920, window_minutes: 10080 } },
];

// The blended account windows carry the most consumed bucket: Claude/GPT at 100%.
const antigravity = {
  id: "ag", type: "antigravity", plan_type: "g1-pro-tier", status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 6, secondary_window_used_pct: 100, primary_window_available: true, secondary_window_available: true,
  primary_usage_reported: true, secondary_usage_reported: true,
  primary_reset_minutes: 297, secondary_reset_minutes: 7920, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 0, subscription_spend: 0, subscription_billing_cycles: 0, subscription_label: "", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
  quota_families: families,
} as AccountStats;
const stats = (accounts: AccountStats[]): PoolStats => ({
  total_accounts: accounts.length, active_accounts: accounts.length, generated_at: new Date().toISOString(), accounts,
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
});

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState({}, "", "/");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("leads with the Gemini windows instead of the blended most consumed bucket", () => {
  const focused = focusedQuotaAccount(antigravity);
  expect(focused.secondary_window_used_pct).toBe(0);
  expect(focused.secondary_reset_minutes).toBe(8820);
  expect(focused.primary_window_used_pct).toBe(6);
  expect(focused.secondary_window_minutes).toBe(10080);
  // The original account is not mutated.
  expect(antigravity.secondary_window_used_pct).toBe(100);
});

it("leaves accounts without a Gemini family untouched", () => {
  const withoutFamilies = { ...antigravity, quota_families: undefined } as AccountStats;
  expect(focusedQuotaAccount(withoutFamilies)).toBe(withoutFamilies);
  const thirdPartyOnly = { ...antigravity, quota_families: [families[1]] } as AccountStats;
  expect(focusedQuotaAccount(thirdPartyOnly)).toBe(thirdPartyOnly);
  const claude = { ...antigravity, type: "claude" } as AccountStats;
  expect(focusedQuotaAccount(claude)).toBe(claude);
});

it("shows the Gemini weekly quota in the row and both families in the hover breakdown", () => {
  const { container } = render(<Accounts stats={stats([antigravity])} adminAccounts={[]} onAccountsChanged={vi.fn().mockResolvedValue(undefined)} />);
  const windows = container.querySelector(".account-windows")!;
  const compactWindows = [...windows.querySelectorAll(".reset-window.compact")].map((node) => node.textContent);
  expect(compactWindows).toEqual(["5 hour6%4h 57m", "Weekly0%6d 3h"]);

  const popover = windows.querySelector(".quota-family-popover")!;
  expect(popover.textContent).toContain("Gemini");
  expect(popover.textContent).toContain("Claude / GPT");
  expect(popover.textContent).toContain("100%");

  // Argon reports a weekly limit only, so no empty 5-hour row is listed for it.
  const argon = [...popover.querySelectorAll(".quota-family")].find((node) => node.textContent?.includes("Gemini 4 Argon"))!;
  expect(argon.querySelectorAll(".quota-family-window")).toHaveLength(1);
  expect(argon.textContent).toContain("30%");
  expect(popover.textContent).not.toContain("NOT REPORTED");
});

it("lists every family in the inspector", () => {
  const { container } = render(<Accounts stats={stats([antigravity])} adminAccounts={[]} onAccountsChanged={vi.fn().mockResolvedValue(undefined)} />);
  fireEvent.click(container.querySelector("button.account-row")!);
  const section = container.querySelector(".inspector-quota-families")!;
  expect(section.textContent).toContain("Gemini");
  expect(section.textContent).toContain("Claude / GPT");
});

it("does not flag an account for attention when only the Claude quota is exhausted", () => {
  window.history.replaceState({}, "", "/?accounts=attention");
  const { container } = render(<Accounts stats={stats([antigravity])} adminAccounts={[]} onAccountsChanged={vi.fn().mockResolvedValue(undefined)} />);
  expect(container.querySelector("button.account-row")).toBeNull();
});
