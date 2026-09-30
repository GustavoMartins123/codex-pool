// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { loadAdminAccounts, loadModelCatalog, loadPoolStats, loadSignalAnalytics } from "./api";
import type { View } from "./navigation";
import type { ModelDescriptor, PassportPrincipal, PoolStats, SignalAnalytics } from "./types";
import { usePageData } from "./usePageData";

vi.mock("./api", () => ({
  loadAdminAccounts: vi.fn(), loadModelCatalog: vi.fn(), loadPoolStats: vi.fn(), loadSignalAnalytics: vi.fn(),
}));

const operator: PassportPrincipal = { id: "alice", kind: "operator", status: "active" };
const stats = { generated_at: "2026-09-29T00:00:00Z" } as PoolStats;
const analytics = { generated_at: "2026-09-29T00:00:00Z" } as SignalAnalytics;
const model = (id: string) => ({ id } as ModelDescriptor);
const endpoints = [loadPoolStats, loadSignalAnalytics, loadAdminAccounts, loadModelCatalog];

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

beforeEach(() => {
  vi.resetAllMocks();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  vi.mocked(loadPoolStats).mockResolvedValue(stats);
  vi.mocked(loadSignalAnalytics).mockResolvedValue(analytics);
  vi.mocked(loadAdminAccounts).mockResolvedValue([]);
  vi.mocked(loadModelCatalog).mockResolvedValue({ models: [model("current")] });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

async function setVisibility(state: DocumentVisibilityState) {
  vi.spyOn(document, "visibilityState", "get").mockReturnValue(state);
  await act(async () => { document.dispatchEvent(new Event("visibilitychange")); });
}

describe("page data requests", () => {
  it.each([
    ["pulse", [1, 1, 0, 0]], ["insights", [1, 1, 0, 0]],
    ["accounts", [1, 0, 1, 0]], ["models", [0, 0, 0, 1]],
    ["mine", [0, 0, 0, 0]], ["passes", [0, 0, 0, 0]],
    ["console", [0, 0, 0, 0]], ["setup", [0, 0, 0, 0]],
  ] as const)("loads only the endpoints needed by %s", async (view, calls) => {
    const { result } = renderHook(() => usePageData(view, operator, true));
    await waitFor(() => expect(result.current.loading).toBe(false));
    endpoints.forEach((endpoint, index) => expect(endpoint).toHaveBeenCalledTimes(calls[index]));
  });

  it.each([
    [null, true], [{ ...operator, kind: "member" }, true],
    [{ ...operator, kind: "guest" }, true], [{ ...operator, status: "suspended" }, true],
    [operator, false],
  ] as const)("does not load privileged data without an enabled operator session", async (principal, enabled) => {
    const { result } = renderHook(() => usePageData("pulse", principal, enabled));
    await waitFor(() => expect(result.current.loading).toBe(false));
    endpoints.forEach(endpoint => expect(endpoint).not.toHaveBeenCalled());
    expect(result.current.data).toBeNull();
  });

  it("polls the active endpoints and stops polling on a page with its own data", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const { result, rerender } = renderHook(({ view }: { view: View }) => usePageData(view, operator, true), { initialProps: { view: "pulse" } });
    await act(async () => { await Promise.resolve(); });
    expect(result.current.loading).toBe(false);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(loadPoolStats).toHaveBeenCalledTimes(2);
    expect(loadSignalAnalytics).toHaveBeenCalledTimes(2);
    rerender({ view: "models" });
    await act(async () => { await Promise.resolve(); });
    expect(result.current.loading).toBe(false);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(loadModelCatalog).toHaveBeenCalledTimes(2);
    expect(loadPoolStats).toHaveBeenCalledTimes(2);
    expect(loadSignalAnalytics).toHaveBeenCalledTimes(2);
    rerender({ view: "setup" });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(loadModelCatalog).toHaveBeenCalledTimes(2);
    expect(loadAdminAccounts).not.toHaveBeenCalled();
    expect(result.current.data).toBeNull();
  });

  it("pauses hidden polling and resumes immediately with one timer", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const { result } = renderHook(() => usePageData("models", operator, true));
    await act(async () => { await Promise.resolve(); });
    await setVisibility("hidden");
    await act(async () => { await vi.advanceTimersByTimeAsync(90_000); });
    expect(loadModelCatalog).toHaveBeenCalledOnce();
    expect(result.current.data?.view).toBe("models");
    expect(vi.getTimerCount()).toBe(0);
    await setVisibility("visible");
    expect(loadModelCatalog).toHaveBeenCalledTimes(2);
    await setVisibility("visible");
    expect(vi.getTimerCount()).toBe(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(loadModelCatalog).toHaveBeenCalledTimes(3);
  });

  it("defers hidden initial loading and resumes only the current page", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    await setVisibility("hidden");
    const { result, rerender } = renderHook(({ view }: { view: View }) => usePageData(view, operator, true), { initialProps: { view: "pulse" } });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    endpoints.forEach(endpoint => expect(endpoint).not.toHaveBeenCalled());
    rerender({ view: "accounts" });
    await setVisibility("visible");
    expect(result.current.data?.view).toBe("accounts");
    expect(loadPoolStats).toHaveBeenCalledOnce();
    expect(loadAdminAccounts).toHaveBeenCalledOnce();
    expect(loadSignalAnalytics).not.toHaveBeenCalled();
    expect(loadModelCatalog).not.toHaveBeenCalled();
  });

  it("allows hidden manual refresh and preserves it through visibility changes and polling", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const { result } = renderHook(() => usePageData("models", operator, true));
    await act(async () => { await Promise.resolve(); });
    await setVisibility("hidden");
    const pending = deferred<{ models: ModelDescriptor[] }>();
    vi.mocked(loadModelCatalog).mockReturnValueOnce(pending.promise);
    let refresh!: Promise<boolean>;
    act(() => { refresh = result.current.refresh(); });
    const signal = vi.mocked(loadModelCatalog).mock.calls[1][0];
    await setVisibility("visible");
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    await setVisibility("hidden");
    expect(signal?.aborted).toBe(false);
    expect(loadModelCatalog).toHaveBeenCalledTimes(2);
    await act(async () => {
      pending.resolve({ models: [model("updated")] });
      expect(await refresh).toBe(true);
    });
    expect(result.current.data).toEqual({ view: "models", models: [model("updated")] });
    await setVisibility("visible");
    expect(loadModelCatalog).toHaveBeenCalledTimes(3);
  });

  it("removes visibility listeners and polling on unmount", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    const pending = deferred<{ models: ModelDescriptor[] }>();
    vi.mocked(loadModelCatalog).mockReturnValueOnce(pending.promise);
    const { unmount } = renderHook(() => usePageData("models", operator, true));
    const signal = vi.mocked(loadModelCatalog).mock.calls[0][0];
    unmount();
    expect(signal?.aborted).toBe(true);
    await setVisibility("hidden");
    await setVisibility("visible");
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(vi.getTimerCount()).toBe(0);
    expect(loadModelCatalog).toHaveBeenCalledOnce();
    await act(async () => { pending.resolve({ models: [] }); });
  });

  it("aborts a page request and ignores its late answer after navigation", async () => {
    const old = deferred<PoolStats>();
    vi.mocked(loadPoolStats).mockReturnValueOnce(old.promise);
    const { result, rerender } = renderHook(({ view }: { view: View }) => usePageData(view, operator, true), { initialProps: { view: "pulse" } });
    const signal = vi.mocked(loadPoolStats).mock.calls[0][0];
    const oldRefresh = result.current.refresh;
    rerender({ view: "models" });
    expect(signal?.aborted).toBe(true);
    await waitFor(() => expect(result.current.data?.view).toBe("models"));
    await act(async () => { expect(await oldRefresh()).toBe(false); });
    expect(loadPoolStats).toHaveBeenCalledOnce();
    await act(async () => { old.resolve(stats); });
    expect(result.current.data?.view).toBe("models");
    expect(result.current.error).toBe("");
  });

  it("keeps the newer refresh when the aborted older response arrives", async () => {
    const old = deferred<{ models: ModelDescriptor[] }>();
    vi.mocked(loadModelCatalog).mockReturnValueOnce(old.promise);
    const { result } = renderHook(() => usePageData("models", operator, true));
    const signal = vi.mocked(loadModelCatalog).mock.calls[0][0];
    await act(async () => { expect(await result.current.refresh()).toBe(true); });
    expect(signal?.aborted).toBe(true);
    await act(async () => { old.resolve({ models: [model("obsolete")] }); });
    expect(result.current.data).toEqual({ view: "models", models: [model("current")] });
  });

  it("invalidates requests across sign-out and principal changes", async () => {
    const old = deferred<{ models: ModelDescriptor[] }>();
    vi.mocked(loadModelCatalog).mockReturnValueOnce(old.promise);
    const { result, rerender } = renderHook(({ principal }: { principal: PassportPrincipal | null }) => usePageData("models", principal, true), { initialProps: { principal: operator as PassportPrincipal | null } });
    const signal = vi.mocked(loadModelCatalog).mock.calls[0][0];
    rerender({ principal: null });
    expect(signal?.aborted).toBe(true);
    expect(result.current.data).toBeNull();
    rerender({ principal: { ...operator, id: "bob" } });
    await waitFor(() => expect(result.current.data?.view).toBe("models"));
    await act(async () => { old.resolve({ models: [model("alice")] }); });
    expect(result.current.data).toEqual({ view: "models", models: [model("current")] });
  });

  it("clears stale data on failure and retries only the current page endpoints", async () => {
    const { result } = renderHook(() => usePageData("accounts", operator, true));
    await waitFor(() => expect(result.current.loading).toBe(false));
    vi.mocked(loadAdminAccounts).mockRejectedValueOnce(new Error("account service unavailable"));
    await act(async () => { expect(await result.current.refresh()).toBe(false); });
    expect(result.current.data).toBeNull();
    expect(result.current.error).toBe("account service unavailable");
    await act(async () => { expect(await result.current.refresh()).toBe(true); });
    expect(result.current.data?.view).toBe("accounts");
    expect(result.current.error).toBe("");
    expect(loadSignalAnalytics).not.toHaveBeenCalled();
    expect(loadModelCatalog).not.toHaveBeenCalled();
  });
});
