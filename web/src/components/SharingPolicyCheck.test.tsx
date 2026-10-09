// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { SharingPolicyCheck } from "./SharingPolicyCheck";
import * as api from "../governance-api";
import { loadModelCatalog } from "../api";

vi.mock("../governance-api", async (original) => ({ ...await original<typeof import("../governance-api")>(), loadPolicies: vi.fn(), savePolicy: vi.fn() }));
vi.mock("../api", () => ({ loadModelCatalog: vi.fn() }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

// This model arrives in the remote catalog and is absent from the static catalog.
const haiku = "claude-haiku-5-5";
const sonnet = "claude-sonnet-5-5";
const restricted: api.Policy = { ...api.emptyPolicy(), models: { allow: [sonnet] }, limits: { daily_requests: 100 } };
const base: api.PolicyView = { principal_id: "member", revision: 2, principal_policy: restricted, clients: [{ id: "laptop", label: "Claude Code", status: "active", policy: api.emptyPolicy() }], sources: [{ source: "principal:member", policy: restricted }], effective: restricted, allowed: true, reasons: [], usage: [] };
function catalog() {
  vi.mocked(loadModelCatalog).mockResolvedValue({ models: [{ id: sonnet, provider: "claude", protocol: "messages", available_now: true }, { id: haiku, provider: "claude", protocol: "messages", available_now: true }] });
}

it("identifies the stale user allowlist for a model added through remote discovery and rechecks after saving", async () => {
  catalog();
  let current = base;
  vi.mocked(api.loadPolicies).mockImplementation(async (_id, client) => ({ ...current, sources: [...current.sources, ...(client ? [{ source: `credential:${client}`, policy: api.emptyPolicy() }] : [])] }));
  vi.mocked(api.savePolicy).mockImplementation(async (_id, draft) => {
    current = { ...base, revision: 3, principal_policy: draft.policy, sources: [{ source: "principal:member", policy: draft.policy }] };
    return current;
  });
  render(<SharingPolicyCheck principalID="member" models={[sonnet, haiku]} disabled={false} />);
  expect((await screen.findByRole("alert")).textContent).toContain(`User · User policy: ${haiku}`);
  expect(screen.getByRole("alert").textContent).toContain("return 403");
  fireEvent.click(screen.getByRole("button", { name: "Edit user policy" }));
  const choices = within(await screen.findByRole("group", { name: "Allowed models" }));
  fireEvent.click(await choices.findByRole("checkbox", { name: haiku }));
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await screen.findByText("The current user and active credential model policies allow the selected models.");
  expect(api.savePolicy).toHaveBeenCalledWith("member", expect.objectContaining({ target: "principal", revision: 2, client_id: "", policy: { ...restricted, models: { allow: [sonnet, haiku] } } }));
  expect(api.loadPolicies).toHaveBeenCalledWith("member", "laptop");
  expect(screen.queryByRole("alert")).toBeNull();
});

it("opens the exact credential blocking a granted model and preserves its other restrictions", async () => {
  catalog();
  const principal = { ...base, principal_policy: api.emptyPolicy(), sources: [], clients: [{ ...base.clients[0], policy: restricted }] };
  const credential = { ...principal, sources: [{ source: "credential:laptop", policy: restricted }] };
  vi.mocked(api.loadPolicies).mockImplementation(async (_id, client) => client ? credential : principal);
  vi.mocked(api.savePolicy).mockImplementation(async (_id, draft) => {
    const saved = { ...credential, revision: 3, clients: [{ ...credential.clients[0], policy: draft.policy }], sources: [{ source: "credential:laptop", policy: draft.policy }] };
    vi.mocked(api.loadPolicies).mockImplementation(async (_id, client) => client ? saved : { ...principal, clients: saved.clients });
    return saved;
  });
  render(<SharingPolicyCheck principalID="member" models={[haiku]} disabled={false} />);
  fireEvent.click(await screen.findByRole("button", { name: "Edit Claude Code credential policy" }));
  const choices = within(await screen.findByRole("group", { name: "Allowed models" }));
  expect((screen.getByLabelText("Edit scope") as HTMLSelectElement).value).toBe("client");
  expect((screen.getByLabelText("Credential to edit") as HTMLSelectElement).value).toBe("laptop");
  expect((screen.getByLabelText("Daily requests") as HTMLInputElement).value).toBe("100");
  fireEvent.click(await choices.findByRole("checkbox", { name: haiku }));
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await screen.findByText("The current user and active credential model policies allow the selected models.");
  expect(api.savePolicy).toHaveBeenCalledWith("member", expect.objectContaining({ target: "client", client_id: "laptop", policy: { ...restricted, models: { allow: [sonnet, haiku] } } }));
});

it("reports configuration restrictions and explicit denies without automatically overriding them", async () => {
  vi.mocked(api.loadPolicies).mockImplementation(async (_id, client) => ({ ...base, principal_policy: api.emptyPolicy(), sources: client ? [{ source: "configuration:laptop", policy: { ...api.emptyPolicy(), models: { deny: [haiku] } } }] : [{ source: "global", policy: restricted }, { source: "role:member", policy: restricted }] }));
  render(<SharingPolicyCheck principalID="member" models={[haiku]} disabled={false} />);
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("Global policy");
  expect(alert.textContent).toContain("member role policy");
  expect(alert.textContent).toContain("Claude Code · Configured credential policy");
  expect(screen.queryByRole("button", { name: /Edit .*policy/ })).toBeNull();
  expect(api.savePolicy).not.toHaveBeenCalled();
});

it("retains policy conflicts when saving fails", async () => {
  catalog();
  vi.mocked(api.loadPolicies).mockResolvedValue({ ...base, clients: [] });
  vi.mocked(api.savePolicy).mockRejectedValue(new Error("Policy revision conflict"));
  render(<SharingPolicyCheck principalID="member" models={[haiku]} disabled={false} />);
  fireEvent.click(await screen.findByRole("button", { name: "Edit user policy" }));
  const choices = within(await screen.findByRole("group", { name: "Allowed models" }));
  fireEvent.click(await choices.findByRole("checkbox", { name: haiku }));
  fireEvent.click(screen.getByRole("button", { name: "Save policy" }));
  await screen.findByText("Policy revision conflict");
  expect(screen.getByRole("button", { name: "Edit user policy" })).toBeTruthy();
  expect(screen.queryByText("The current user and active credential model policies allow the selected models.")).toBeNull();
});

it("does not report access as verified when policy loading fails and offers a retry", async () => {
  vi.mocked(api.loadPolicies).mockRejectedValueOnce(new Error("Unavailable"));
  render(<SharingPolicyCheck principalID="member" models={[haiku]} disabled={false} />);
  expect((await screen.findByRole("alert")).textContent).toContain("Could not verify recipient access");
  vi.mocked(api.loadPolicies).mockResolvedValue({ ...base, principal_policy: api.emptyPolicy(), sources: [], clients: [] });
  fireEvent.click(screen.getByRole("button", { name: "Recheck recipient policies" }));
  await screen.findByText("The current user and active credential model policies allow the selected models.");
});

it("discards a slow response when another recipient is selected", async () => {
  let complete!: (value: api.PolicyView) => void;
  vi.mocked(api.loadPolicies).mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }));
  const { rerender } = render(<SharingPolicyCheck principalID="first" models={[haiku]} disabled={false} />);
  vi.mocked(api.loadPolicies).mockResolvedValue({ ...base, principal_id: "second", sources: [], clients: [] });
  rerender(<SharingPolicyCheck principalID="second" models={[haiku]} disabled={false} />);
  await screen.findByText("The current user and active credential model policies allow the selected models.");
  complete(base);
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
});
