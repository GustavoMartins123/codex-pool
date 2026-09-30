import { afterEach, expect, it, vi } from "vitest";
import { loadAdminAccounts, loadModelCatalog, loadPoolStats, loadSignalAnalytics } from "./api";

afterEach(() => vi.unstubAllGlobals());

it("passes cancellation to page data requests", async () => {
  const controller = new AbortController();
  const fetchMock = vi.fn(async () => new Response(JSON.stringify({
    models: [], economics: [], hourly: [], origin_weekly: [], model_daily: [],
    quota_capacity: [], model_efficiency: [], reset_observations: [],
  }), { status: 200 }));
  vi.stubGlobal("fetch", fetchMock);
  await Promise.all([loadPoolStats(controller.signal), loadSignalAnalytics(controller.signal),
    loadModelCatalog(controller.signal), loadAdminAccounts(controller.signal)]);
  for (const [, init] of fetchMock.mock.calls as unknown as Array<[string, RequestInit]>) {
    expect(init.signal).toBe(controller.signal);
    expect(init.credentials).toBe("same-origin");
  }
});

it("rejects an incomplete analytics response", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 200 })));
  await expect(loadSignalAnalytics()).rejects.toThrow("economics must be an array");
});

it("rejects a malformed catalog instead of presenting an empty catalog", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response('{"models":null}', { status: 200 })));
  await expect(loadModelCatalog()).rejects.toThrow("models must be an array");
});
