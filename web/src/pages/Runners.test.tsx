// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Runners, isArmedRunnerAction, pairingCountdown } from "./Runners";
import { AgentsApiError, decidePairing, listPairings, listRunners, revokeRunner, type PairingReview, type Runner } from "../agents-api";

vi.mock("../agents-api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../agents-api")>()),
  listRunners: vi.fn(),
  listPairings: vi.fn(),
  decidePairing: vi.fn(),
  revokeRunner: vi.fn(),
}));

const runner: Runner = { id: "r1", name: "desk", online: true, agents: ["codex"], os: "linux", arch: "amd64", revoked: false, created_at: "2026-11-01T00:00:00Z" };
const pairing: PairingReview = { id: "p1", name: "laptop", os: "linux", arch: "amd64", expires_at: new Date(Date.now() + 300_000).toISOString() };

beforeEach(() => {
  vi.mocked(listRunners).mockResolvedValue([runner]);
  vi.mocked(listPairings).mockResolvedValue([pairing]);
  vi.mocked(decidePairing).mockResolvedValue(undefined);
  vi.mocked(revokeRunner).mockResolvedValue(undefined);
});
afterEach(() => { cleanup(); vi.resetAllMocks(); });

function codeInput() {
  return screen.getByLabelText("Machine code") as HTMLInputElement;
}
function button(name: string) {
  return screen.getByRole("button", { name }) as HTMLButtonElement;
}

it("scopes the armed confirmation to both the resource and the action", () => {
  const armed = { id: "r1", kind: "revoke" as const };
  expect(isArmedRunnerAction(armed, "r1", "revoke")).toBe(true);
  expect(isArmedRunnerAction(armed, "r2", "revoke")).toBe(false);
  expect(isArmedRunnerAction(armed, "r1", "deny")).toBe(false);
  expect(isArmedRunnerAction(null, "r1", "revoke")).toBe(false);
});

it("formats the pairing countdown and flags expiry", () => {
  const expires = "2026-11-01T00:05:00Z";
  const base = new Date("2026-11-01T00:00:00Z").getTime();
  expect(pairingCountdown(expires, base)).toBe("Expires in 5:00");
  expect(pairingCountdown(expires, base + 299_000)).toBe("Expires in 0:01");
  expect(pairingCountdown(expires, base + 300_000)).toBe("Expired");
  expect(pairingCountdown("not-a-date", base)).toBe("Expired");
});

it("requires the machine code before approving a pairing", async () => {
  render(<Runners />);
  await screen.findByRole("button", { name: "Approve" });
  expect(button("Approve").disabled).toBe(true);
  fireEvent.change(codeInput(), { target: { value: "ABC-123" } });
  expect(button("Approve").disabled).toBe(false);
  fireEvent.click(button("Approve"));
  await waitFor(() => expect(decidePairing).toHaveBeenCalledWith("p1", "approve", "ABC-123"));
  expect((await screen.findByText(/Approved laptop/)).textContent).toContain("Approved laptop");
});

it("keeps the code input and shows an alert when the code is wrong", async () => {
  vi.mocked(decidePairing).mockRejectedValueOnce(new AgentsApiError(403, "machine code does not match"));
  render(<Runners />);
  await screen.findByRole("button", { name: "Approve" });
  fireEvent.change(codeInput(), { target: { value: "WRONG" } });
  fireEvent.click(button("Approve"));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("machine code does not match"));
  expect(codeInput().value).toBe("WRONG");
  expect(button("Approve").disabled).toBe(false);
});

it("denies a pairing only after a second confirming click", async () => {
  render(<Runners />);
  fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
  expect(decidePairing).not.toHaveBeenCalled();
  fireEvent.click(button("Confirm deny"));
  await waitFor(() => expect(decidePairing).toHaveBeenCalledWith("p1", "deny", ""));
  expect((await screen.findByText(/Denied the pairing request from laptop/)).textContent).toContain("Denied");
});

it("refreshes the list and explains an expired pairing", async () => {
  vi.mocked(decidePairing).mockRejectedValueOnce(new AgentsApiError(410, "pairing expired"));
  render(<Runners />);
  await screen.findByRole("button", { name: "Approve" });
  fireEvent.change(codeInput(), { target: { value: "ABC-123" } });
  fireEvent.click(button("Approve"));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("pairing expired"));
  await waitFor(() => expect(listPairings).toHaveBeenCalledTimes(2));
});

it("revokes a runner only after confirming, warning about active sessions", async () => {
  render(<Runners />);
  fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
  expect(revokeRunner).not.toHaveBeenCalled();
  expect(screen.getByText("Active sessions on this machine will close.")).toBeTruthy();
  fireEvent.click(button("Confirm revoke"));
  await waitFor(() => expect(revokeRunner).toHaveBeenCalledWith("r1"));
  expect((await screen.findByText(/Revoked desk/)).textContent).toContain("Revoked desk");
});

it("shows empty states when nothing is paired or pending", async () => {
  vi.mocked(listRunners).mockResolvedValue([]);
  vi.mocked(listPairings).mockResolvedValue([]);
  render(<Runners />);
  expect(await screen.findByText("No pending pairings.")).toBeTruthy();
  expect(screen.getByText("No runners paired yet.")).toBeTruthy();
});

it("surfaces load failures and retries on demand", async () => {
  vi.mocked(listRunners).mockRejectedValueOnce(new Error("session service unavailable"));
  render(<Runners />);
  await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("session service unavailable"));
  fireEvent.click(button("Retry loading runners"));
  expect(await screen.findByText("desk")).toBeTruthy();
});
