// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Accounts } from "./Accounts";
import type { AccountStats, AdminAccount, PoolStats } from "../types";

vi.mock("../components/AccountGovernance", () => ({ AccountGovernance: () => null }));

const make = (id: string, type: string, plan: string): AccountStats => ({
  id, type, plan_type: plan, status: "healthy", penalty: 0,
  reset_windows: { tier: "unknown", primary: "five_hour", secondary: "weekly" },
  primary_window_used_pct: 0, secondary_window_used_pct: 0, primary_window_available: true, secondary_window_available: true,
  primary_reset_minutes: 0, secondary_reset_minutes: 0, primary_window_minutes: 300, secondary_window_minutes: 10080, primary_pace_ratio: 0, secondary_pace_ratio: 0,
  total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_reasoning_tokens: 0, total_billable_tokens: 0, cache_hit_rate_pct: 0,
  score: 1, is_primary: false, subscription_cost_monthly: 0, subscription_spend: 0, subscription_billing_cycles: 0, subscription_label: "", api_cost_estimate: 0, api_cost_last_30d: 0, roi: 0,
}) as AccountStats;
const poolStats = (): PoolStats => ({
  total_accounts: 2, active_accounts: 2, generated_at: new Date().toISOString(),
  accounts: [make("zeta", "codex", "pro"), make("alpha", "claude", "max")],
  aggregate: { total_input_tokens: 0, total_cached_tokens: 0, total_output_tokens: 0, total_billable_tokens: 0, total_reasoning_tokens: 0, overall_cache_hit_rate_pct: 0, total_api_cost: 0, total_subscription_cost: 0, total_subscription_monthly: 0, overall_roi: 0 },
});
const rowIDs = (container: HTMLElement) => [...container.querySelectorAll("button.account-row")].map((row) => row.querySelector(".account-identity small span")?.textContent);
const noop = vi.fn().mockResolvedValue(undefined);

beforeEach(() => {
  window.localStorage.clear();
  window.history.replaceState({}, "", "/");
  window.matchMedia = vi.fn().mockReturnValue({ matches: false, addEventListener() {}, removeEventListener() {} }) as unknown as typeof window.matchMedia;
  window.requestAnimationFrame = ((callback: FrameRequestCallback) => { callback(0); return 0; }) as typeof window.requestAnimationFrame;
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("remembers the chosen sort across reloads, preferring an explicit URL sort", () => {
  const first = render(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  expect(rowIDs(first.container)).toEqual(["zeta", "alpha"]);
  fireEvent.click(screen.getByRole("button", { name: "Sort by account" }));
  expect(rowIDs(first.container)).toEqual(["alpha", "zeta"]);
  expect(JSON.parse(window.localStorage.getItem("codex-pool.accounts.sort")!)).toEqual({ key: "account", direction: "asc" });
  first.unmount();

  window.history.replaceState({}, "", "/");
  const reloaded = render(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  expect(rowIDs(reloaded.container)).toEqual(["alpha", "zeta"]);
  reloaded.unmount();

  window.history.replaceState({}, "", "/?sort=account&dir=desc");
  const fromURL = render(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  expect(rowIDs(fromURL.container)).toEqual(["zeta", "alpha"]);
});

it("clears the stored sort when sorting is switched off and tolerates broken storage", () => {
  window.localStorage.setItem("codex-pool.accounts.sort", "{not json");
  const { container } = render(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  expect(rowIDs(container)).toEqual(["zeta", "alpha"]);
  const button = screen.getByRole("button", { name: "Sort by account" });
  fireEvent.click(button);
  fireEvent.click(button);
  fireEvent.click(button);
  expect(window.localStorage.getItem("codex-pool.accounts.sort")).toBeNull();
});

it("keeps the inspector scroll across refreshes and restores it per account", () => {
  const { container, rerender } = render(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  const rows = () => [...container.querySelectorAll<HTMLButtonElement>("button.account-row")];
  const inspector = () => container.querySelector<HTMLElement>(".account-inspector")!;

  fireEvent.click(rows()[0]);
  inspector().scrollTop = 240;
  fireEvent.scroll(inspector());

  rerender(<Accounts stats={poolStats()} adminAccounts={[]} onAccountsChanged={noop} />);
  expect(inspector().scrollTop).toBe(240);

  fireEvent.click(rows()[1]);
  expect(inspector().scrollTop).toBe(0);
  inspector().scrollTop = 90;
  fireEvent.scroll(inspector());

  fireEvent.click(rows()[0]);
  expect(inspector().scrollTop).toBe(240);
  fireEvent.click(rows()[1]);
  expect(inspector().scrollTop).toBe(90);
});
