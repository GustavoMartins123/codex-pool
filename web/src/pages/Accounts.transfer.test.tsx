// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { exportAccounts, importAccounts } from "../api";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  exportAccounts: vi.fn(), importAccounts: vi.fn(),
}));
vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const account: AccountStats = {
  id: "public-codex", type: "codex", plan_type: "pro", status: "needs_login", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: false, secondary_window_available: false,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 200, subscription_spend: 0, subscription_billing_cycles: 0, subscription_label: "Pro", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
};
const stats: PoolStats = {
  total_accounts: 1, active_accounts: 0, accounts: [account], generated_at: new Date().toISOString(),
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
};
const admin = (awaitingLogin: boolean) => ({
  can_manage: true, id: "pool-codex", public_id: account.id, type: "codex", plan_type: "pro", needs_login: true, awaiting_login: awaitingLogin,
  auth_error: awaitingLogin ? "imported without credentials: sign in to activate this account" : "refresh_token_invalidated",
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "needs_login", state_routable: false, usage: {}, totals: {},
}) as unknown as AdminAccount;

function button(name: string) {
  return screen.queryByRole("button", { name });
}
function status() {
  return screen.getByRole("status").textContent ?? "";
}

beforeEach(() => {
  window.history.replaceState({}, "", "/");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

it("offers the export and the import to operators only", () => {
  const props = { stats, adminAccounts: [admin(true)], onAccountsChanged: vi.fn().mockResolvedValue(undefined) };
  render(<Accounts {...props} operator />);
  expect(button("Export accounts")).not.toBeNull();
  expect(button("Import accounts")).not.toBeNull();
  cleanup();
  render(<Accounts {...props} />);
  expect(button("Export accounts")).toBeNull();
  expect(button("Import accounts")).toBeNull();
});

it("downloads the export and says what it holds", async () => {
  vi.mocked(exportAccounts).mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={vi.fn()} operator />);
  fireEvent.click(button("Export accounts")!);
  await waitFor(() => expect(status()).toContain("Account list exported"));
  expect(exportAccounts).toHaveBeenCalledOnce();
  expect(status()).toContain("no credential");
});

it("shows why an export was refused", async () => {
  vi.mocked(exportAccounts).mockRejectedValue(new Error("account export signing is not configured on this pool"));
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={vi.fn()} operator />);
  fireEvent.click(button("Export accounts")!);
  await waitFor(() => expect(status()).toContain("account export signing is not configured on this pool"));
});

it("imports an account list and refreshes the pool", async () => {
  const file = new File(["{\"format\":\"codex-pool-accounts\"}"], "codex-pool-accounts.json", { type: "application/json" });
  vi.mocked(importAccounts).mockResolvedValue({ imported: ["codex/codex_a", "antigravity/antigravity-b"], skipped: ["codex/codex_c"], invalid: [] });
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} operator />);

  fireEvent.change(screen.getByLabelText("Account list to import"), { target: { files: [file] } });
  await waitFor(() => expect(status()).toContain("2 imported, waiting for sign-in · 1 already present"));
  expect(importAccounts).toHaveBeenCalledWith(file);
  await waitFor(() => expect(onAccountsChanged).toHaveBeenCalledOnce());
  expect(button("Import accounts")).not.toBeNull();
});

it("reports rejected entries and a refused list", async () => {
  vi.mocked(importAccounts).mockResolvedValueOnce({ imported: ["codex/codex_a"], skipped: [], invalid: ["kimi/kimi_key", "codex/../escape"] });
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} operator />);
  const input = screen.getByLabelText("Account list to import");
  fireEvent.change(input, { target: { files: [new File(["{}"], "list.json")] } });
  await waitFor(() => expect(status()).toContain("1 imported, waiting for sign-in · 0 already present · 2 rejected"));
  expect(document.querySelector(".account-message.error")).not.toBeNull();

  vi.mocked(importAccounts).mockRejectedValueOnce(new Error("account export signature is invalid"));
  fireEvent.change(input, { target: { files: [new File(["{}"], "forged.json")] } });
  await waitFor(() => expect(status()).toContain("account export signature is invalid"));
});

it("asks for the first sign-in on an account imported without credentials", () => {
  window.history.replaceState({}, "", "/?account=pool-codex");
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={vi.fn()} operator />);
  expect(screen.getByRole("alert").textContent).toContain("Imported without credentials — sign in to activate");
  expect(screen.getByRole("alert").textContent).not.toContain("imported without credentials: sign in to activate this account");
  expect(button("Sign in")).not.toBeNull();
  expect(button("Relogin account")).toBeNull();
});

it("keeps the relogin wording for an account whose credentials were rejected", () => {
  window.history.replaceState({}, "", "/?account=pool-codex");
  render(<Accounts stats={stats} adminAccounts={[admin(false)]} onAccountsChanged={vi.fn()} operator />);
  const alert = screen.getByRole("alert").textContent ?? "";
  expect(alert).toContain("Credentials rejected — relogin required");
  expect(alert).toContain("refresh_token_invalidated");
  expect(button("Relogin account")).not.toBeNull();
  expect(button("Sign in")).toBeNull();
});
