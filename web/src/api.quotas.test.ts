import { afterEach, expect, it, vi } from "vitest";
import { loadMemberQuotas } from "./api";

afterEach(() => vi.unstubAllGlobals());
it.each([undefined, -1, 101, "75", Number.NaN])("rejects an invalid quota percentage: %s", async value => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ generated_at: new Date().toISOString(), models: [{ model: "claude-opus-5-5", name: "Opus", provider: "claude", available_now: true, supporting_accounts: 1, primary: { remaining_percent: value, reported_accounts: 1, updated_at: new Date().toISOString() }, secondary: null }] }))));
  await expect(loadMemberQuotas()).rejects.toThrow("Invalid quota response");
});
it("does not replace a failed quota request with a successful empty result", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "Policy unavailable" }), { status: 503 })));
  await expect(loadMemberQuotas()).rejects.toThrow("Policy unavailable");
});
