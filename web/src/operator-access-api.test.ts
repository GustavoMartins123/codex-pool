// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { loadAdminAccounts, loadConsolePrincipals } from "./api";
import { loadOperatorAccounts, loadOperatorAccess, setOperatorAccountAccess, setContributionPermission } from "./governance-api";

afterEach(() => vi.unstubAllGlobals());

it("sends an explicit activation, revision and CSRF without accepting an operator identity", async () => {
  document.cookie = "pool_csrf=operator-test";
  const account = { provider: "kimi", id: "private", owner_id: "alice", revision: 2, enabled: false };
  const fetchMock = vi.fn(async () => new Response(JSON.stringify({ ...account, revision: 3, enabled: true })));
  vi.stubGlobal("fetch", fetchMock);
  await setOperatorAccountAccess(account, true, "Support");
  const [path, options] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
  expect(path).toBe("/api/accounts/kimi/private/operator-access");
  expect(options.credentials).toBe("same-origin");
  expect(options.headers).toMatchObject({ "X-CSRF-Token": "operator-test" });
  expect(JSON.parse(String(options.body))).toEqual({ revision: 2, enabled: true, reason: "Support" });
});

it("rejects incomplete permissions and corrupt history", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify([{ provider: "kimi", id: "private", owner_id: "alice", revision: 2 }]))));
  await expect(loadOperatorAccounts("alice")).rejects.toThrow("Invalid operator account response");
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify([{ operator_id: "bob", enabled: true, reason: "Support", updated_at: "invalid" }]))));
  await expect(loadOperatorAccess("kimi", "private")).rejects.toThrow("Invalid operator access response");
});

it("rejects a contribution permission response for another user", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ principal_id: "bob", can_contribute: true }))));
  await expect(setContributionPermission("alice", true)).rejects.toThrow("Invalid contribution permission response");
});

it("rejects a console response without explicit registration permissions", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ principals: [{ id: "alice", kind: "member" }] }))));
  await expect(loadConsolePrincipals()).rejects.toThrow("Invalid member registration permissions response");
});

it("rejects accounts without explicit management permissions", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify([{ id: "private", type: "kimi" }]))));
  await expect(loadAdminAccounts()).rejects.toThrow("Invalid account management permissions response");
});
