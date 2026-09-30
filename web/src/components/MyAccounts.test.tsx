// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, cleanup } from "@testing-library/react";
import { MyAccounts } from "./MyAccounts";
import { contributeAPIKey, loadMyAccounts, withdrawMyAccount } from "../api";
import type { MyAccount, PassportPrincipal } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  loadMyAccounts: vi.fn(), withdrawMyAccount: vi.fn(), contributeAPIKey: vi.fn(),
}));
const principal: PassportPrincipal = { id: "alice", kind: "member", status: "active", can_contribute: true };
const account: MyAccount = { id: "private-account", provider: "kimi", status: "active", state: "ready", revision: 2 };
beforeEach(() => { vi.mocked(loadMyAccounts).mockResolvedValue([account]); });
afterEach(() => { cleanup(); vi.resetAllMocks(); });

it("lists owned accounts and requires confirmation before withdrawing", async () => {
  vi.mocked(withdrawMyAccount).mockResolvedValue();
  render(<MyAccounts principal={principal} />);
  expect(await screen.findByText(account.id)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Withdraw" }));
  expect(withdrawMyAccount).not.toHaveBeenCalled();
  vi.mocked(loadMyAccounts).mockResolvedValue([{ ...account, status: "withdrawn", state: "withdrawn", revision: 3 }]);
  fireEvent.click(screen.getByRole("button", { name: "Confirm withdrawal" }));
  expect(await screen.findByText("withdrawn")).toBeTruthy();
  expect(withdrawMyAccount).toHaveBeenCalledWith(account);
  expect(screen.queryByRole("button", { name: "Withdraw" })).toBeNull();
});

it("retains a failed withdrawal and exposes a revision conflict", async () => {
  vi.mocked(withdrawMyAccount).mockRejectedValue(new Error("Account changed; reload before withdrawing"));
  render(<MyAccounts principal={principal} />);
  await screen.findByText(account.id);
  fireEvent.click(screen.getByRole("button", { name: "Withdraw" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm withdrawal" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Account changed");
  expect(screen.getByText(account.id)).toBeTruthy();
});

it("keeps credentials after a failed contribution and reloads after success", async () => {
  vi.mocked(contributeAPIKey).mockRejectedValueOnce(new Error("Provider unavailable")).mockResolvedValueOnce({ success: true, account_id: account.id });
  render(<MyAccounts principal={principal} />);
  await screen.findByText(account.id);
  fireEvent.click(screen.getByRole("button", { name: "Add account" }));
  fireEvent.click(screen.getByRole("button", { name: "Kimi" }));
  const input = screen.getByLabelText("Kimi API key") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "synthetic-key" } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Provider unavailable");
  expect(input.value).toBe("synthetic-key");
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  await waitFor(() => expect(screen.queryByLabelText("Kimi API key")).toBeNull());
  expect(loadMyAccounts).toHaveBeenCalledTimes(2);
});

it("hides contribution without permission and displays an explicit load failure", async () => {
  vi.mocked(loadMyAccounts).mockRejectedValue(new Error("Account authority unavailable"));
  render(<MyAccounts principal={{ ...principal, can_contribute: false }} />);
  expect((await screen.findByRole("alert")).textContent).toContain("Account authority unavailable");
  expect(screen.queryByRole("button", { name: "Add account" })).toBeNull();
  expect(screen.queryByText("No provider accounts yet.")).toBeNull();
});

it("does not load upstream accounts for guests", () => {
  render(<MyAccounts principal={{ ...principal, kind: "guest" }} />);
  expect(loadMyAccounts).not.toHaveBeenCalled();
  expect(screen.queryByText("My accounts")).toBeNull();
});
