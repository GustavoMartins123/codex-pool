// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { accountEmailLabel } from "../ui";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const account: AccountStats = {
  id: "public-codex", type: "codex", plan_type: "team", status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: false, secondary_window_available: false,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 30, subscription_spend: 30, subscription_billing_cycles: 1, subscription_label: "Team", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
};
const stats: PoolStats = {
  total_accounts: 1, active_accounts: 1, accounts: [account], generated_at: new Date().toISOString(),
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
};
const admin = {
  can_manage: true, id: "codex_8f31c5d0549ab3c80121bc61", public_id: account.id, type: "codex", plan_type: "team", email: "member@example.com",
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "healthy", state_routable: true, usage: {}, totals: {},
} as unknown as AdminAccount;

beforeEach(() => {
  window.history.replaceState({}, "", `/?account=${admin.id}`);
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("shows which ChatGPT account a hashed Codex account belongs to", () => {
  render(<Accounts stats={stats} adminAccounts={[admin]} onAccountsChanged={vi.fn()} />);
  expect(screen.getByText("ChatGPT account: member@example.com")).toBeTruthy();
  expect(screen.queryByText(/Google account/)).toBeNull();
});

it("labels account emails by the provider that issued them", () => {
  expect(accountEmailLabel("codex")).toBe("ChatGPT account");
  expect(accountEmailLabel("antigravity")).toBe("Google account");
  expect(accountEmailLabel("zai")).toBe("Account email");
});
