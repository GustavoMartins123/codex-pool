// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { exchangeClaudeRelogin, startClaudeRelogin } from "../api";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  startClaudeRelogin: vi.fn(), exchangeClaudeRelogin: vi.fn(),
}));
vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const account: AccountStats = {
  id: "public-claude", type: "claude", plan_type: "max", status: "needs_login", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: false, secondary_window_available: false,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 200, subscription_spend: 200, subscription_billing_cycles: 1, subscription_label: "Max", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
};
const stats: PoolStats = {
  total_accounts: 1, active_accounts: 1, accounts: [account], generated_at: new Date().toISOString(),
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
};
const admin = (canManage: boolean) => ({
  can_manage: canManage, id: "pool-claude", public_id: account.id, type: "claude", plan_type: "max", needs_login: true,
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "needs_login", state_routable: false, usage: {}, totals: {},
}) as unknown as AdminAccount;

beforeEach(() => {
  window.history.replaceState({}, "", "/?account=pool-claude");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
  vi.spyOn(window, "open").mockReturnValue(null);
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

it("lets the owner relogin to a Claude account and refreshes its displayed state", async () => {
  vi.mocked(startClaudeRelogin).mockResolvedValue({ oauth_url: "https://claude.com/authorize", verifier: "relogin-verifier", state: "relogin-state" });
  vi.mocked(exchangeClaudeRelogin).mockResolvedValue({ success: true, account_id: "pool-claude" });
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);
  fireEvent.click(screen.getByRole("button", { name: "Relogin account" }));
  fireEvent.click(screen.getByRole("button", { name: "Sign in to Claude" }));
  fireEvent.change(await screen.findByLabelText("Claude authentication code"), { target: { value: "fresh-code#relogin-state" } });
  fireEvent.click(screen.getByRole("button", { name: "Save credentials" }));
  await waitFor(() => expect(onAccountsChanged).toHaveBeenCalledOnce());
  expect(startClaudeRelogin).toHaveBeenCalledWith("pool-claude");
  expect(exchangeClaudeRelogin).toHaveBeenCalledWith("fresh-code#relogin-state", "relogin-verifier");
  await waitFor(() => expect(screen.queryByRole("dialog", { name: "Relogin account" })).toBeNull());
});

it("hides relogin when the user has use access without management permission", () => {
  render(<Accounts stats={stats} adminAccounts={[admin(false)]} onAccountsChanged={vi.fn()} />);
  expect(screen.queryByRole("button", { name: "Relogin account" })).toBeNull();
});
