// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, act } from "@testing-library/react";
import { AccountGovernance, AccountGovernanceForm } from "./AccountGovernance";
import { PolicyEditorForm } from "./PolicyEditor";
import * as api from "../governance-api";

vi.mock("../governance-api", async (original) => ({ ...await original<typeof import("../governance-api")>(), loadControls: vi.fn(), saveControls: vi.fn(), loadSharing: vi.fn(), setDelegation: vi.fn(), createGrant: vi.fn(), revokeGrant: vi.fn(), loadPolicies: vi.fn(), previewPolicy: vi.fn(), savePolicy: vi.fn() }));
const controls: api.ControlView = { provider: "codex", id: "private", revision: 3, inflight: 1, controls: { state: "enabled", max_concurrent: 2 } };
const sharing: api.SharingView = { provider: "codex", id: "private", revision: 3, owner: true, operator_may_delegate: false, grants: [{ id: "grant-one", recipient_id: "bob", models: ["gpt-5.5"], budget: { daily_requests: 5 }, expires_at: "2099-01-01T00:00:00Z", revision: 1, reason: "Team access" }] };
const view: api.PolicyView = { principal_id: "bob", revision: 2, principal_policy: api.emptyPolicy(), clients: [{ id: "client", label: "Laptop", status: "active", policy: api.emptyPolicy() }], sources: [{ source: "global", policy: api.emptyPolicy() }], effective: api.emptyPolicy(), allowed: true, reasons: [], usage: [{ scope: "principal:bob", limits: {}, minute: { requests: 1, tokens: 2 }, day: { requests: 5, tokens: 10, reserved_tokens: 20 }, month: { requests: 5, tokens: 10 }, inflight: 1 }] };
beforeEach(() => { vi.mocked(api.loadControls).mockResolvedValue(controls); vi.mocked(api.loadSharing).mockResolvedValue(sharing); vi.mocked(api.loadPolicies).mockResolvedValue(view); vi.mocked(api.previewPolicy).mockResolvedValue(view); });
afterEach(() => { cleanup(); vi.resetAllMocks(); });
const change = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const enabled = (name: string) => !(screen.getByRole("button", { name }) as HTMLButtonElement).disabled;

it("loads account controls lazily and retains a failed revision change", async () => {
  vi.mocked(api.saveControls).mockRejectedValue(new Error("Revision conflict. Reload before trying again."));
  render(<AccountGovernance provider="codex" id="private" />);
  expect(api.loadControls).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Controls and sharing" }));
  await screen.findByLabelText("Account mode");
  change("Account mode", "maintenance"); change("Change reason", "Database maintenance");
  fireEvent.click(screen.getByRole("button", { name: "Save account controls" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Revision conflict");
  expect(api.saveControls).toHaveBeenCalledWith("codex", "private", 3, { state: "maintenance", max_concurrent: 2 }, "Database maintenance");
  expect((screen.getByLabelText("Account mode") as HTMLSelectElement).value).toBe("maintenance");
  expect(screen.queryByText("Account controls saved")).toBeNull();
});

it("shows persisted controls only after a successful save", async () => {
  vi.mocked(api.saveControls).mockResolvedValue({ ...controls, revision: 4, controls: { state: "draining", max_concurrent: 1 } });
  render(<AccountGovernanceForm provider="codex" id="private" />);
  await screen.findByLabelText("Account mode"); change("Account mode", "draining"); change("Account concurrency", "1"); change("Change reason", "Finish active conversations");
  fireEvent.click(screen.getByRole("button", { name: "Save account controls" }));
  await screen.findByText("Account controls saved"); expect(screen.getByText("Revision 4; 1 active requests.")).toBeTruthy();
});

it("allows saving without preview and discards a slow preview after an edit", async () => {
  render(<PolicyEditorForm principalID="bob" />); await screen.findByLabelText("Allowed models");
  expect(enabled("Save policy")).toBe(true); change("Allowed models", "gpt-5.5,"); change("Allowed models", "gpt-5.5, gpt-6-astra");
  expect((screen.getByLabelText("Allowed models") as HTMLInputElement).value).toBe("gpt-5.5, gpt-6-astra");
  let complete!: (value: api.PolicyView) => void; vi.mocked(api.previewPolicy).mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }));
  fireEvent.click(screen.getByRole("button", { name: "Preview policy" }));
  change("Allowed models", "gpt-5.5");
  await act(async () => complete(view)); expect(enabled("Save policy")).toBe(true); expect(screen.queryByText("Sample request allowed")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Preview policy" })); await screen.findByText("Sample request allowed"); expect(enabled("Save policy")).toBe(true);
  change("Sample model", "gpt-6-astra"); expect(enabled("Save policy")).toBe(true);
});

it("displays sources and usage and never claims success after rejected save", async () => {
  vi.mocked(api.previewPolicy).mockResolvedValue({ ...view, allowed: false, reasons: ["daily request budget exhausted"] });
  vi.mocked(api.savePolicy).mockRejectedValue(new Error("Policy changed; reload"));
  render(<PolicyEditorForm principalID="bob" />); await screen.findByLabelText("Allowed models");
  expect(screen.getByText("global")).toBeTruthy(); expect(screen.getByLabelText("Policy budget usage").textContent).toContain("20 reserved");
  change("Daily requests", "5"); fireEvent.click(screen.getByRole("button", { name: "Preview policy" })); await screen.findByText(/Sample request denied/);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" })); expect((await screen.findByRole("alert")).textContent).toContain("Policy changed"); expect(enabled("Save policy")).toBe(true); expect(screen.queryByText("Policy saved")).toBeNull();
});

it("saves the selected principal directly without sending optional preview inputs", async () => {
  const policy = { ...api.emptyPolicy(), models: { allow: ["gpt-5.5"] } };
  vi.mocked(api.savePolicy).mockResolvedValue({ ...view, revision: 3, principal_policy: policy });
  render(<PolicyEditorForm principalID="bob" />); await screen.findByLabelText("Allowed models");
  change("Allowed models", "gpt-5.5"); change("Sample account ID (optional)", "preview-only-account"); change("Estimated tokens", "-1");
  expect(enabled("Preview policy")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await screen.findByText("Policy saved");
  expect(api.previewPolicy).not.toHaveBeenCalled();
  expect(api.savePolicy).toHaveBeenCalledWith("bob", { revision: 2, target: "principal", client_id: "", policy, model: "", provider: "", account_id: "", tokens: 0 });
  expect(screen.getByText(/Revision 3/)).toBeTruthy();
  change("Allowed models", "gpt-6-astra"); expect(screen.queryByText("Policy saved")).toBeNull();
});

it("saves credential restrictions and returns to principal scope when the credential is cleared", async () => {
  const policy = { ...api.emptyPolicy(), limits: { daily_requests: 8 } };
  vi.mocked(api.savePolicy).mockResolvedValue({ ...view, revision: 3, clients: [{ ...view.clients[0], policy }] });
  render(<PolicyEditorForm principalID="bob" />); await screen.findByLabelText("Allowed models");
  change("Credential for preview", "client"); await waitFor(() => expect(enabled("Save policy")).toBe(true));
  change("Edit scope", "client"); change("Daily requests", "8");
  fireEvent.click(screen.getByRole("button", { name: "Save policy" })); await screen.findByText("Policy saved");
  expect(api.savePolicy).toHaveBeenCalledWith("bob", expect.objectContaining({ target: "client", client_id: "client", policy }));
  change("Credential to edit", "");
  await screen.findByLabelText("Credential for preview"); await waitFor(() => expect(enabled("Save policy")).toBe(true));
  expect((screen.getByLabelText("Edit scope") as HTMLSelectElement).value).toBe("principal");
  expect((screen.getByLabelText("Daily requests") as HTMLInputElement).value).toBe("0");
  expect(screen.queryByRole("alert")).toBeNull();
});

it("rejects invalid token budgets before preview", async () => {
  render(<PolicyEditorForm principalID="bob" />); await screen.findByLabelText("Daily tokens"); change("Daily tokens", "1000");
  expect(enabled("Preview policy")).toBe(false); expect(api.previewPolicy).not.toHaveBeenCalled();
  change("Token reservation", "100"); expect(enabled("Preview policy")).toBe(true);
});

it("requires revoke confirmation and preserves the grant on failure", async () => {
  vi.mocked(api.revokeGrant).mockRejectedValue(new Error("Grant revision conflict"));
  render(<AccountGovernanceForm provider="codex" id="private" sharingOnly />);
  fireEvent.click(await screen.findByRole("button", { name: "Revoke grant" })); expect(api.revokeGrant).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Confirm revoke" })); expect((await screen.findByRole("alert")).textContent).toContain("Grant revision conflict");
  expect(screen.queryByText("Revoked")).toBeNull(); expect(screen.getByText("bob")).toBeTruthy();
});

it("uses explicit recipient, models, expiry and budget, preserving a retry ID", async () => {
  vi.mocked(api.createGrant).mockRejectedValue(new Error("Account unavailable"));
  render(<AccountGovernanceForm provider="codex" id="private" sharingOnly />); await screen.findByLabelText("Recipient principal ID");
  change("Recipient principal ID", "bob"); change("Granted models", "gpt-5.5"); change("Expires at", "2099-01-01T12:00"); change("Grant reason", "Team access");
  fireEvent.click(screen.getByRole("button", { name: "Create grant" })); await screen.findByText("Account unavailable");
  const first = vi.mocked(api.createGrant).mock.calls[0][2]; expect(first.recipient_id).toBe("bob"); expect(first.models).toEqual(["gpt-5.5"]); expect(first.budget.daily_requests).toBe(100);
  fireEvent.click(screen.getByRole("button", { name: "Create grant" })); await waitFor(() => expect(api.createGrant).toHaveBeenCalledTimes(2)); expect(vi.mocked(api.createGrant).mock.calls[1][2].id).toBe(first.id);
});

it("shows sharing denial without displaying grant creation", async () => {
  vi.mocked(api.loadSharing).mockRejectedValue(new Error("Account delegation denied")); render(<AccountGovernanceForm provider="codex" id="private" sharingOnly />);
  expect((await screen.findByRole("alert")).textContent).toContain("Account delegation denied"); expect(screen.queryByRole("button", { name: "Create grant" })).toBeNull();
});
