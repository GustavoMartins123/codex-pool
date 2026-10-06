// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, act, within } from "@testing-library/react";
import { AccountGovernance, AccountGovernanceForm } from "./AccountGovernance";
import { PolicyEditorForm } from "./PolicyEditor";
import * as api from "../governance-api";
import { loadModelCatalog } from "../api";
import type { ModelDescriptor } from "../types";

vi.mock("../governance-api", async (original) => ({ ...await original<typeof import("../governance-api")>(), loadControls: vi.fn(), saveControls: vi.fn(), loadSharing: vi.fn(), setDelegation: vi.fn(), createGrant: vi.fn(), revokeGrant: vi.fn(), loadPolicies: vi.fn(), previewPolicy: vi.fn(), savePolicy: vi.fn() }));
vi.mock("../api", async (original) => ({ ...await original<typeof import("../api")>(), loadModelCatalog: vi.fn() }));
const models: ModelDescriptor[] = [
  { id: "gpt-5.5", name: "GPT 5.5", provider: "codex", protocol: "responses", available_now: true, aliases: ["gpt-latest"] },
  { id: "gpt-6-astra", provider: "codex", protocol: "responses", available_now: false },
  { id: "claude-opus-5", provider: "claude", protocol: "messages", available_now: true },
];
const controls: api.ControlView = { provider: "codex", id: "private", revision: 3, inflight: 1, controls: { state: "enabled", max_concurrent: 2 } };
const sharing: api.SharingView = { provider: "codex", id: "private", revision: 3, owner: true, operator_may_delegate: false, grants: [{ id: "grant-one", recipient_id: "bob", models: ["gpt-5.5"], budget: { daily_requests: 5 }, expires_at: "2099-01-01T00:00:00Z", revision: 1, reason: "Team access" }] };
const view: api.PolicyView = { principal_id: "bob", revision: 2, principal_policy: api.emptyPolicy(), clients: [{ id: "client", label: "Laptop", status: "active", policy: api.emptyPolicy() }], sources: [{ source: "global", policy: api.emptyPolicy() }], effective: api.emptyPolicy(), allowed: true, reasons: [], usage: [{ scope: "principal:bob", limits: {}, minute: { requests: 1, tokens: 2 }, day: { requests: 5, tokens: 10, reserved_tokens: 20 }, month: { requests: 5, tokens: 10 }, inflight: 1 }] };
beforeEach(() => { vi.mocked(api.loadControls).mockResolvedValue(controls); vi.mocked(api.loadSharing).mockResolvedValue(sharing); vi.mocked(api.loadPolicies).mockResolvedValue(view); vi.mocked(api.previewPolicy).mockResolvedValue(view); vi.mocked(loadModelCatalog).mockResolvedValue({ models }); });
afterEach(() => { cleanup(); vi.resetAllMocks(); });
const change = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const enabled = (name: string) => !(screen.getByRole("button", { name }) as HTMLButtonElement).disabled;
const checkModel = (id: string, group = "Allowed models") => fireEvent.click(within(screen.getByRole("group", { name: group })).getByRole("checkbox", { name: id }));

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
  render(<PolicyEditorForm principalID="bob" />); await screen.findByRole("group", { name: "Allowed models" });
  expect(enabled("Save policy")).toBe(true); checkModel("gpt-5.5"); checkModel("gpt-6-astra");
  let complete!: (value: api.PolicyView) => void; vi.mocked(api.previewPolicy).mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }));
  fireEvent.click(screen.getByRole("button", { name: "Preview policy" }));
  checkModel("gpt-6-astra");
  await act(async () => complete(view)); expect(enabled("Save policy")).toBe(true); expect(screen.queryByText("Sample request allowed")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Preview policy" })); await screen.findByText("Sample request allowed"); expect(enabled("Save policy")).toBe(true);
  change("Sample model", "gpt-6-astra"); expect(enabled("Save policy")).toBe(true);
});

it("displays sources and usage and never claims success after rejected save", async () => {
  vi.mocked(api.previewPolicy).mockResolvedValue({ ...view, allowed: false, reasons: ["daily request budget exhausted"] });
  vi.mocked(api.savePolicy).mockRejectedValue(new Error("Policy changed; reload"));
  render(<PolicyEditorForm principalID="bob" />); await screen.findByRole("group", { name: "Allowed models" });
  expect(screen.getByText("global")).toBeTruthy(); expect(screen.getByLabelText("Policy budget usage").textContent).toContain("20 reserved");
  change("Daily requests", "5"); fireEvent.click(screen.getByRole("button", { name: "Preview policy" })); await screen.findByText(/Sample request denied/);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" })); expect((await screen.findByRole("alert")).textContent).toContain("Policy changed"); expect(enabled("Save policy")).toBe(true); expect(screen.queryByText("Policy saved")).toBeNull();
});

it("saves the selected principal directly without sending optional preview inputs", async () => {
  const policy = { ...api.emptyPolicy(), models: { allow: ["gpt-5.5"] } };
  vi.mocked(api.savePolicy).mockResolvedValue({ ...view, revision: 3, principal_policy: policy });
  render(<PolicyEditorForm principalID="bob" />); await screen.findByRole("group", { name: "Allowed models" });
  checkModel("gpt-5.5"); change("Sample account ID (optional)", "preview-only-account"); change("Estimated tokens", "-1");
  expect(enabled("Preview policy")).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await screen.findByText("Policy saved");
  expect(api.previewPolicy).not.toHaveBeenCalled();
  expect(api.savePolicy).toHaveBeenCalledWith("bob", { revision: 2, target: "principal", client_id: "", policy, model: "", provider: "", account_id: "", tokens: 0 });
  expect(screen.getByText(/Revision 3/)).toBeTruthy();
  checkModel("gpt-6-astra"); expect(screen.queryByText("Policy saved")).toBeNull();
});

it("saves credential restrictions and returns to principal scope when the credential is cleared", async () => {
  const policy = { ...api.emptyPolicy(), limits: { daily_requests: 8 } };
  vi.mocked(api.savePolicy).mockResolvedValue({ ...view, revision: 3, clients: [{ ...view.clients[0], policy }] });
  render(<PolicyEditorForm principalID="bob" />); await screen.findByRole("group", { name: "Allowed models" });
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

it("selects catalog models and aliases using checkboxes, keeping selections through search and provider filters", async () => {
  vi.mocked(api.savePolicy).mockImplementation(async (_id, draft) => ({ ...view, revision: 3, principal_policy: draft.policy }));
  render(<PolicyEditorForm principalID="bob" />);
  const allowed = within(await screen.findByRole("group", { name: "Allowed models" }));
  expect(loadModelCatalog).toHaveBeenCalledOnce();
  checkModel("gpt-6-astra"); // A temporarily unavailable model is still a policy option.
  change("Search allowed models", "GPT 5.5");
  expect(allowed.queryByRole("checkbox", { name: "claude-opus-5" })).toBeNull();
  checkModel("gpt-latest");
  change("Search allowed models", ""); change("Filter allowed models by provider", "claude");
  expect(allowed.queryByRole("checkbox", { name: "gpt-6-astra" })).toBeNull();
  checkModel("claude-opus-5"); checkModel("gpt-5.5", "Denied models");
  change("Filter allowed models by provider", "");
  expect((allowed.getByRole("checkbox", { name: "gpt-6-astra" }) as HTMLInputElement).checked).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" })); await screen.findByText("Policy saved");
  expect(api.savePolicy).toHaveBeenCalledWith("bob", expect.objectContaining({ policy: expect.objectContaining({ models: { allow: ["gpt-6-astra", "gpt-latest", "claude-opus-5"], deny: ["gpt-5.5"] } }) }));
  fireEvent.click(screen.getByRole("button", { name: "Clear allowed models" }));
  expect(allowed.getByText(/All models are allowed by this policy/)).toBeTruthy();
});

it("preserves existing selectors on a catalog failure and offers a retry", async () => {
  const policy = { ...api.emptyPolicy(), models: { allow: ["retired-model", "claude-*"], deny: ["blocked-model"] } };
  vi.mocked(api.loadPolicies).mockResolvedValue({ ...view, principal_policy: policy });
  vi.mocked(loadModelCatalog).mockRejectedValueOnce(new Error("Catalog unavailable"));
  vi.mocked(api.savePolicy).mockResolvedValue({ ...view, revision: 3, principal_policy: policy });
  render(<PolicyEditorForm principalID="bob" />);
  expect((await screen.findByRole("alert")).textContent).toContain("Saved selections are preserved");
  const allowed = within(screen.getByRole("group", { name: "Allowed models" }));
  expect((allowed.getByRole("checkbox", { name: "retired-model" }) as HTMLInputElement).checked).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Save policy" })); await screen.findByText("Policy saved");
  expect(api.savePolicy).toHaveBeenCalledWith("bob", expect.objectContaining({ policy }));
  fireEvent.click(screen.getByRole("button", { name: "Retry models" }));
  await waitFor(() => expect(allowed.getByRole("checkbox", { name: "gpt-5.5" })).toBeTruthy());
  expect(screen.queryByRole("alert")).toBeNull();
  expect((allowed.getByRole("checkbox", { name: "claude-*" }) as HTMLInputElement).checked).toBe(true);
  checkModel("retired-model");
  expect(allowed.queryByRole("checkbox", { name: "retired-model" })).toBeNull();
});

it("resets model restrictions when a different principal is selected and drops the previous pending save", async () => {
  let complete!: (value: api.PolicyView) => void;
  vi.mocked(api.savePolicy).mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }));
  const { rerender } = render(<PolicyEditorForm key="bob" principalID="bob" />);
  await screen.findByRole("group", { name: "Allowed models" }); checkModel("gpt-5.5");
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  vi.mocked(api.loadPolicies).mockResolvedValue({ ...view, principal_id: "alice" });
  rerender(<PolicyEditorForm key="alice" principalID="alice" />);
  const allowed = within(await screen.findByRole("group", { name: "Allowed models" }));
  await act(async () => complete({ ...view, revision: 3 }));
  expect(screen.queryByText("Policy saved")).toBeNull();
  expect((allowed.getByRole("checkbox", { name: "gpt-5.5" }) as HTMLInputElement).checked).toBe(false);
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
