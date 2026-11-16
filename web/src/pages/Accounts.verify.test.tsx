// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import { verifyAccount, verifyAllAntigravity, type AntigravityVerifyResult } from "../api";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  verifyAccount: vi.fn(),
  verifyAllAntigravity: vi.fn(),
}));
vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const account: AccountStats = {
  id: "public-ag", type: "antigravity", plan_type: "pro", status: "healthy", penalty: 0,
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
const admin = (canManage: boolean, type: "antigravity" | "codex" = "antigravity") => ({
  can_manage: canManage, id: "pool-ag", public_id: account.id, type, plan_type: "pro", needs_login: false,
  disabled: false, dead: false, inflight: 0, penalty: 0, score: 1, is_primary: false, state: "healthy", state_routable: true, usage: {}, totals: {},
}) as unknown as AdminAccount;

const healthyResult: AntigravityVerifyResult = { account_id: "pool-ag", status: "healthy", checked_at: "2026-11-15T12:00:00Z" };
const bannedResult: AntigravityVerifyResult = {
  account_id: "pool-ag", status: "banned", detail: "403 PERMISSION_DENIED: TOS_VIOLATION",
  appeal_url: "https://forms.gle/appeal?continue=AbCdEf0123456789", checked_at: "2026-11-15T12:00:00Z",
};

beforeEach(() => {
  window.history.replaceState({}, "", "/?account=pool-ag");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
});
afterEach(() => { vi.unstubAllGlobals(); cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

it("verifies access on a single click and shows the healthy chip", async () => {
  vi.mocked(verifyAccount).mockResolvedValue(healthyResult);
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);

  fireEvent.click(screen.getByRole("button", { name: "Verify access" }));

  await waitFor(() => expect(verifyAccount).toHaveBeenCalledWith("pool-ag"));
  expect(await screen.findByText("Access healthy")).toBeTruthy();
  await waitFor(() => expect(onAccountsChanged).toHaveBeenCalledOnce());
});

it("shows the ban status with the sanitized appeal link and detail", async () => {
  vi.mocked(verifyAccount).mockResolvedValue(bannedResult);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={vi.fn().mockResolvedValue(undefined)} />);

  fireEvent.click(screen.getByRole("button", { name: "Verify access" }));

  expect(await screen.findByText("Banned — Terms of Service violation")).toBeTruthy();
  expect(screen.getByText("403 PERMISSION_DENIED: TOS_VIOLATION")).toBeTruthy();
  const link = screen.getByRole("link", { name: "Open appeal form" }) as HTMLAnchorElement;
  expect(link.href).toBe("https://forms.gle/appeal?continue=AbCdEf0123456789");
});

it("drops appeal and verification links outside the Google host allowlist", async () => {
  vi.mocked(verifyAccount).mockResolvedValue({
    ...bannedResult,
    appeal_url: "https://evil.example/appeal",
    verification_url: "http://accounts.google.com/verify",
  });
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={vi.fn().mockResolvedValue(undefined)} />);

  fireEvent.click(screen.getByRole("button", { name: "Verify access" }));

  expect(await screen.findByText("Banned — Terms of Service violation")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "Open appeal form" })).toBeNull();
  expect(screen.queryByRole("link", { name: "Open verification" })).toBeNull();
});

it("hides verify controls when the user has use access without management permission", () => {
  render(<Accounts stats={stats} adminAccounts={[admin(false)]} onAccountsChanged={vi.fn()} />);
  expect(screen.queryByRole("button", { name: "Verify access" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Verify all Antigravity" })).toBeNull();
});

it("hides verify controls for non-antigravity accounts", () => {
  const codexStats = { ...stats, accounts: [{ ...account, type: "codex" as const }] };
  render(<Accounts stats={codexStats} adminAccounts={[admin(true, "codex")]} onAccountsChanged={vi.fn()} />);
  expect(screen.queryByRole("button", { name: "Verify access" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Verify all Antigravity" })).toBeNull();
});

it("verifies every antigravity account in batch and summarizes the results", async () => {
  vi.mocked(verifyAllAntigravity).mockResolvedValue({ accounts: [bannedResult, { ...healthyResult, account_id: "pool-ag-2" }] });
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);

  fireEvent.click(screen.getByRole("button", { name: "Verify all Antigravity" }));

  await waitFor(() => expect(verifyAllAntigravity).toHaveBeenCalledOnce());
  expect(await screen.findByText("2 Antigravity accounts checked · 1 healthy · 1 banned")).toBeTruthy();
  expect(screen.getByText("Banned — Terms of Service violation")).toBeTruthy();
  await waitFor(() => expect(onAccountsChanged).toHaveBeenCalledOnce());
});

it("shows the server error when the verification fails", async () => {
  vi.mocked(verifyAccount).mockRejectedValue(new Error("Antigravity provider is not configured"));
  const onAccountsChanged = vi.fn().mockResolvedValue(undefined);
  render(<Accounts stats={stats} adminAccounts={[admin(true)]} onAccountsChanged={onAccountsChanged} />);

  fireEvent.click(screen.getByRole("button", { name: "Verify access" }));

  expect(await screen.findByText("Antigravity provider is not configured")).toBeTruthy();
  expect(onAccountsChanged).not.toHaveBeenCalled();
});

it("posts both verify mutations with the operator CSRF token", async () => {
  document.cookie = "pool_csrf=operator-csrf";
  const fetchMock = vi.fn().mockImplementation(async (url: string) => new Response(
    JSON.stringify(url === "/admin/antigravity/verify" ? { accounts: [healthyResult] } : healthyResult),
    { status: 200 },
  ));
  vi.stubGlobal("fetch", fetchMock);
  const api = await vi.importActual<typeof import("../api")>("../api");

  const single = await api.verifyAccount("pool-ag");
  const batch = await api.verifyAllAntigravity();

  expect(single.status).toBe("healthy");
  expect(batch.accounts).toHaveLength(1);
  const [singleURL, singleInit] = fetchMock.mock.calls[0] as [string, RequestInit];
  expect(singleURL).toBe("/admin/accounts/pool-ag/verify");
  expect(singleInit.method).toBe("POST");
  expect(singleInit.credentials).toBe("same-origin");
  expect((singleInit.headers as Record<string, string>)["X-CSRF-Token"]).toBe("operator-csrf");
  const [batchURL, batchInit] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(batchURL).toBe("/admin/antigravity/verify");
  expect(batchInit.method).toBe("POST");
  expect((batchInit.headers as Record<string, string>)["X-CSRF-Token"]).toBe("operator-csrf");
});
