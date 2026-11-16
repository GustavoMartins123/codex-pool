// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { deleteAccount } from "../api";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  deleteAccount: vi.fn(),
}));
vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const account: AccountStats = {
  id: "public-codex", type: "codex", plan_type: "pro", status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: true, secondary_window_available: true,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 20, subscription_spend: 20, subscription_billing_cycles: 1, subscription_label: "Pro", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
};
const stats: PoolStats = {
  total_accounts: 1, active_accounts: 1, accounts: [account], generated_at: new Date().toISOString(),
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
};
const admin = (canManage: boolean) => ({
  can_manage: canManage, id: "pool-codex", public_id: account.id, type: "codex", plan_type: "pro", needs_login: false,
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "healthy", state_routable: true, usage: {}, totals: {},
}) as unknown as AdminAccount;

beforeEach(() => {
  window.history.replaceState({}, "", "/?account=pool-codex");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { vi.unstubAllGlobals(); cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

it("arms on the first click and only deletes on the confirm click", async () => {
  vi.mocked(deleteAccount).mockResolvedValue({ id: "pool-codex", status: "deleted" });
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);

  fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
  expect(deleteAccount).not.toHaveBeenCalled();
  expect(onAccountsChanged).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Confirm delete" }));
  await waitFor(() => expect(deleteAccount).toHaveBeenCalledWith("pool-codex"));
  await waitFor(() => expect(onAccountsChanged).toHaveBeenCalledOnce());
  expect(await screen.findByText("pool-codex deleted")).toBeTruthy();
  await waitFor(() => expect(screen.queryByRole("button", { name: "Confirm delete" })).toBeNull());
});

it("shows the server error and keeps the account when the delete fails", async () => {
  vi.mocked(deleteAccount).mockRejectedValue(new Error("account has in-flight requests; retry when idle"));
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);

  fireEvent.click(screen.getByRole("button", { name: "Delete account" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm delete" }));

  expect(await screen.findByText("account has in-flight requests; retry when idle")).toBeTruthy();
  expect(onAccountsChanged).not.toHaveBeenCalled();
  expect(screen.getByRole("button", { name: "Confirm delete" })).toBeTruthy();
});

it("hides delete when the user has use access without management permission", () => {
  render(<Accounts stats={stats} adminAccounts={[admin(false)]} onAccountsChanged={vi.fn()} />);
  expect(screen.queryByRole("button", { name: "Delete account" })).toBeNull();
});

it("posts the delete mutation with the operator CSRF token", async () => {
  document.cookie = "pool_csrf=operator-csrf";
  const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "pool-codex", status: "deleted" }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  const api = await vi.importActual<typeof import("../api")>("../api");

  const result = await api.deleteAccount("pool-codex");

  expect(result.status).toBe("deleted");
  const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
  expect(url).toBe("/admin/accounts/pool-codex/delete");
  expect(init.method).toBe("POST");
  expect(init.credentials).toBe("same-origin");
  expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("operator-csrf");
});
