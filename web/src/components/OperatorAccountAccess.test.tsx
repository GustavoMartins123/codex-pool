// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemberContributionPermission, OperatorAccounts, OwnedAccountOperatorAccess } from "./OperatorAccountAccess";
import * as api from "../governance-api";

vi.mock("../governance-api", () => ({ loadOperatorAccounts: vi.fn(), loadOperatorAccess: vi.fn(), setOperatorAccountAccess: vi.fn(), setContributionPermission: vi.fn() }));
const account: api.OperatorAccount = { provider: "kimi", id: "private-kimi", owner_id: "alice", revision: 2, enabled: false };
beforeEach(() => { vi.mocked(api.loadOperatorAccounts).mockResolvedValue([account]); });
afterEach(() => { cleanup(); vi.resetAllMocks(); });

it("loads private account metadata only when opened and requires a reason", async () => {
  render(<OperatorAccounts ownerID="alice" />);
  expect(api.loadOperatorAccounts).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "My access to private accounts" }));
  await screen.findByText(account.id);
  expect(api.loadOperatorAccounts).toHaveBeenCalledWith("alice");
  const activate = screen.getByRole("button", { name: "Activate my access" }) as HTMLButtonElement;
  expect(activate.disabled).toBe(true);
  vi.mocked(api.setOperatorAccountAccess).mockRejectedValueOnce(new Error("Revision conflict. Reload before trying again."));
  const reason = screen.getByLabelText("Access change reason") as HTMLInputElement;
  fireEvent.change(reason, { target: { value: "Support" } });
  fireEvent.click(activate);
  expect((await screen.findByRole("alert")).textContent).toContain("Revision conflict");
  expect(reason.value).toBe("Support");
  expect(screen.queryByRole("button", { name: "Deactivate my access" })).toBeNull();
  vi.mocked(api.setOperatorAccountAccess).mockResolvedValue({ ...account, enabled: true, revision: 3 });
  fireEvent.click(activate);
  await screen.findByRole("button", { name: "Deactivate my access" });
  expect(api.setOperatorAccountAccess).toHaveBeenLastCalledWith(account, true, "Support");
});

it("disables an activation using the current revision", async () => {
  vi.mocked(api.loadOperatorAccounts).mockResolvedValue([{ ...account, enabled: true, revision: 3 }]);
  vi.mocked(api.setOperatorAccountAccess).mockResolvedValue({ ...account, revision: 4 });
  render(<OperatorAccounts ownerID="alice" />);
  fireEvent.click(screen.getByRole("button", { name: "My access to private accounts" }));
  await screen.findByText(account.id);
  fireEvent.change(screen.getByLabelText("Access change reason"), { target: { value: "Finished" } });
  fireEvent.click(screen.getByRole("button", { name: "Deactivate my access" }));
  await screen.findByRole("button", { name: "Activate my access" });
  expect(api.setOperatorAccountAccess).toHaveBeenCalledWith({ ...account, enabled: true, revision: 3 }, false, "Finished");
});

it("clears previously visible metadata if reloading fails", async () => {
  render(<OperatorAccounts ownerID="alice" />);
  fireEvent.click(screen.getByRole("button", { name: "My access to private accounts" }));
  await screen.findByText(account.id);
  vi.mocked(api.loadOperatorAccounts).mockRejectedValue(new Error("Authority unavailable"));
  fireEvent.click(screen.getByRole("button", { name: "Reload private accounts" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Authority unavailable");
  expect(screen.queryByText(account.id)).toBeNull();
});

it("does not paint an old user's metadata after the selection changes", async () => {
  let resolve!: (value: api.OperatorAccount[]) => void;
  vi.mocked(api.loadOperatorAccounts).mockReturnValueOnce(new Promise(done => { resolve = done; }));
  const view = render(<OperatorAccounts key="alice" ownerID="alice" />);
  fireEvent.click(screen.getByRole("button", { name: "My access to private accounts" }));
  view.rerender(<OperatorAccounts key="bob" ownerID="bob" />);
  await act(async () => { resolve([account]); });
  expect(screen.queryByText(account.id)).toBeNull();
});

it("shows owners the latest operator activations and explicit loading errors", async () => {
  vi.mocked(api.loadOperatorAccess).mockResolvedValue([{ operator_id: "bob", enabled: true, updated_at: "2026-10-21T12:00:00Z", reason: "Support" }]);
  render(<OwnedAccountOperatorAccess provider="kimi" id={account.id} />);
  fireEvent.click(screen.getByRole("button", { name: "Operator access" }));
  await screen.findByText("Operator bob");
  expect(screen.getByText("Support")).toBeTruthy();
  vi.mocked(api.loadOperatorAccess).mockRejectedValue(new Error("Access denied"));
  fireEvent.click(screen.getByRole("button", { name: "Refresh operator access" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Access denied");
  expect(screen.queryByText("Operator bob")).toBeNull();
});

it("retains member registration permissions after a failed save", async () => {
  const changed = vi.fn(async () => {});
  vi.mocked(api.setContributionPermission).mockRejectedValueOnce(new Error("Audit unavailable")).mockResolvedValueOnce(true);
  render(<MemberContributionPermission principalID="alice" allowed={false} onChanged={changed} />);
  const checkbox = screen.getByRole("checkbox") as HTMLInputElement;
  fireEvent.click(checkbox);
  await screen.findByRole("alert");
  expect(checkbox.checked).toBe(false);
  expect(changed).not.toHaveBeenCalled();
  fireEvent.click(checkbox);
  await waitFor(() => expect(checkbox.checked).toBe(true));
  expect(api.setContributionPermission).toHaveBeenLastCalledWith("alice", true);
  expect(changed).toHaveBeenCalledTimes(1);
});
