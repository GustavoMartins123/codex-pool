// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { consumeResetCredit } from "../api";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../api", () => ({ consumeResetCredit: vi.fn(), mutateAccount: vi.fn(), reloadAccounts: vi.fn() }));
vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));
vi.mock("../components/AccountContribution", () => ({ AccountContribution: () => null }));

const codexStats = (overrides: Partial<AccountStats> = {}): AccountStats => ({
  id: "public-1", type: "codex", plan_type: "pro", reset_windows: { tier: "pro_or_higher", primary: "five_hour", secondary: "weekly" },
  status: "healthy", penalty: 0, primary_window_used_pct: 10, secondary_window_used_pct: 20, primary_window_available: true, secondary_window_available: true,
  primary_reset_minutes: 120, secondary_reset_minutes: 3000, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 1, secondary_pace_ratio: 1,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 200, subscription_spend: 200, subscription_billing_cycles: 1, subscription_label: "Pro", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
  reset_credits_known: true, reset_credits_available: 1, reset_credit_expirations: [new Date(Date.now() + 3_600_000).toISOString()],
  ...overrides,
});
const stats = (account: AccountStats): PoolStats => ({
  total_accounts: 1, active_accounts: 1, accounts: [account], generated_at: new Date().toISOString(),
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
});
const admin = (overrides: Partial<AdminAccount> = {}) => ({
  can_manage: true, can_consume_reset: true, id: "pool-codex", public_id: "public-1", type: "codex", plan_type: "pro",
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "healthy", state_routable: true, usage: {}, totals: {},
  ...overrides,
}) as unknown as AdminAccount;

function renderAccounts(account: AccountStats, adminAccount: AdminAccount, onAccountsChanged = vi.fn().mockResolvedValue(undefined)) {
  render(<Accounts stats={stats(account)} adminAccounts={[adminAccount]} onAccountsChanged={onAccountsChanged} />);
  return onAccountsChanged;
}

beforeEach(() => {
  window.history.replaceState({}, "", "/?account=pool-codex");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { cleanup(); vi.resetAllMocks(); });

it("hides the consume button unless the server grants it for this account", () => {
  renderAccounts(codexStats(), admin({ can_consume_reset: false }));
  expect(screen.getByText("Banked usage resets")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /consume banked reset/i })).toBeNull();
});

it("hides the consume button when the server omits the permission", () => {
  renderAccounts(codexStats(), admin({ can_consume_reset: undefined }));
  expect(screen.queryByRole("button", { name: /consume banked reset/i })).toBeNull();
});

it("needs a second click before spending the reset, then refreshes the accounts", async () => {
  vi.mocked(consumeResetCredit).mockResolvedValue({ credit_id: "credit-1", code: "reset", windows_reset: 2 });
  const changed = renderAccounts(codexStats(), admin());

  fireEvent.click(screen.getByRole("button", { name: "Consume banked reset" }));
  expect(consumeResetCredit).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Confirm consume" }));

  await waitFor(() => expect(consumeResetCredit).toHaveBeenCalledWith("pool-codex"));
  expect(consumeResetCredit).toHaveBeenCalledTimes(1);
  expect((await screen.findByRole("status")).textContent).toContain("pool-codex banked reset consumed · 2 windows reset");
  expect(changed).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("button", { name: "Consume banked reset" })).toBeTruthy();
});

it("disables the button when no banked reset is known to be available", () => {
  renderAccounts(codexStats({ reset_credits_available: 0, reset_credit_expirations: [] }), admin());
  expect((screen.getByRole("button", { name: "Consume banked reset" }) as HTMLButtonElement).disabled).toBe(true);
});

it("shows the server error and disarms the confirmation when the consume fails", async () => {
  vi.mocked(consumeResetCredit).mockRejectedValue(new Error("no banked reset is available"));
  const changed = renderAccounts(codexStats(), admin());

  fireEvent.click(screen.getByRole("button", { name: "Consume banked reset" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm consume" }));

  expect((await screen.findByRole("status")).textContent).toContain("no banked reset is available");
  expect(changed).not.toHaveBeenCalled();
  // A retry must be confirmed again.
  expect(screen.getByRole("button", { name: "Consume banked reset" })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Consume banked reset" }));
  expect(consumeResetCredit).toHaveBeenCalledTimes(1);
});

it("never reports a spent reset as failed when only the list refresh fails", async () => {
  vi.mocked(consumeResetCredit).mockResolvedValue({ credit_id: "credit-1", code: "reset", windows_reset: 1 });
  renderAccounts(codexStats(), admin(), vi.fn().mockRejectedValue(new Error("Unable to refresh pool accounts")));

  fireEvent.click(screen.getByRole("button", { name: "Consume banked reset" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm consume" }));

  const message = await screen.findByText(/banked reset consumed/);
  expect(message.textContent).toContain("1 window reset");
  expect(message.textContent).toContain("reload the pool");
  expect(message.className).toContain("success");
});
