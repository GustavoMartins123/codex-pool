// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { MemberAccounts } from "./MemberAccounts";
import { loadMemberQuotas, type MemberModelQuota } from "../api";

vi.mock("../api", () => ({ loadMemberQuotas: vi.fn() }));
const row: MemberModelQuota = { model: "claude-opus-5-5", name: "Claude Opus 5.5", provider: "claude", supporting_accounts: 1, available_now: true, primary: { remaining_percent: 75, reported_accounts: 1, updated_at: new Date().toISOString() }, secondary: null };
const result = (models: MemberModelQuota[]) => ({ models, generated_at: new Date().toISOString() });
beforeEach(() => vi.mocked(loadMemberQuotas).mockResolvedValue(result([row])));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

it("shows authorized percentages and an explicit unknown window", async () => {
  render(<MemberAccounts />);
  await screen.findByText("Claude Opus 5.5");
  expect(screen.getByRole("meter").getAttribute("value")).toBe("75");
  expect(screen.getByText("No recent percentage reported by the provider.")).toBeTruthy();
  expect(screen.queryByText("Enable")).toBeNull();
});

it("clears previously allowed quotas on refresh and stays closed on error", async () => {
  render(<MemberAccounts />); await screen.findByText("Claude Opus 5.5");
  let reject!: (reason: Error) => void;
  vi.mocked(loadMemberQuotas).mockImplementationOnce(() => new Promise((_, fail) => { reject = fail; }));
  fireEvent.click(screen.getByRole("button", { name: "Refresh quotas" }));
  expect(screen.queryByRole("meter")).toBeNull();
  await act(async () => reject(new Error("Access revoked")));
  expect((await screen.findByRole("alert")).textContent).toContain("Access revoked");
  expect(screen.queryByText("Claude Opus 5.5")).toBeNull();
});

it("discards an older response after a focus recheck revokes access", async () => {
  let resolve!: (value: ReturnType<typeof result>) => void;
  vi.mocked(loadMemberQuotas).mockImplementationOnce(() => new Promise(done => { resolve = done; }));
  render(<MemberAccounts />);
  vi.mocked(loadMemberQuotas).mockResolvedValueOnce(result([]));
  fireEvent(window, new Event("focus"));
  await screen.findByText("No models available");
  await act(async () => resolve(result([row])));
  expect(screen.queryByRole("meter")).toBeNull();
  expect(screen.getByText("No models available")).toBeTruthy();
});

it("shows zero remaining quota instead of treating it as unknown", async () => {
  vi.mocked(loadMemberQuotas).mockResolvedValue(result([{ ...row, primary: { ...row.primary!, remaining_percent: 0 }, available_now: false }]));
  render(<MemberAccounts />);
  expect((await screen.findByRole("meter")).getAttribute("value")).toBe("0");
  expect(screen.getByText("Currently unavailable")).toBeTruthy();
});

it("aborts and removes hidden quota data then rechecks visibility", async () => {
  render(<MemberAccounts />); await screen.findByRole("meter");
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
  fireEvent(document, new Event("visibilitychange"));
  expect(screen.queryByRole("meter")).toBeNull();
  vi.mocked(loadMemberQuotas).mockResolvedValueOnce(result([]));
  visibility.mockReturnValue("visible");
  fireEvent(document, new Event("visibilitychange"));
  await waitFor(() => expect(screen.getByText("No models available")).toBeTruthy());
  visibility.mockRestore();
});
