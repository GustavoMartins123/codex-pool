// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { loadAnalyticsHealth, loadConsoleAudit, loadConsolePrincipalUsage, loadConsolePrincipals, loadPasses, revokePass } from "./api";
import { Navigation } from "./App";
import { Passes } from "./pages/Passes";
import { PassportConsole } from "./pages/Console";
import type { ConsolePrincipal, GuestPass, PassportPrincipal } from "./types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    loadConsolePrincipals: vi.fn(),
    loadConsoleAudit: vi.fn(async () => []),
    loadConsolePrincipalUsage: vi.fn(async () => ({ hourly: [] })),
    loadAnalyticsHealth: vi.fn(async () => ({ health: { state: "CURRENT", outbox_depth: 0 }, accounting_gaps: [] })),
    loadPasses: vi.fn(),
    revokePass: vi.fn(),
  };
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function principalFixture(overrides: Partial<ConsolePrincipal> = {}): ConsolePrincipal {
  return {
    id: "member-fixture",
    kind: "member",
    status: "active",
    note: "",
    created_at: "2026-09-01T00:00:00Z",
    billable_tokens: 0,
    request_count: 0,
    api_equivalent_cost_usd: 0,
    ...overrides,
  };
}

const operator: PassportPrincipal = { id: "op", kind: "operator", status: "active" };

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("console stale-response guard", () => {
  it("drops an older window response that lands after a newer one", async () => {
    const first = deferred<{ principals: ConsolePrincipal[]; hours: number; excludes_passthrough: boolean }>();
    const second = deferred<{ principals: ConsolePrincipal[]; hours: number; excludes_passthrough: boolean }>();
    vi.mocked(loadConsolePrincipals)
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise);

    render(createElement(PassportConsole, { principal: operator }));

    // Switch the window while the first (7-day default) load is in flight.
    const windowSelect = await screen.findByLabelText("Window");
    fireEvent.change(windowSelect, { target: { value: "24" } });

    // The newer 24-hour response lands first…
    await act(async () => {
      second.resolve({ principals: [principalFixture({ id: "latest", note: "latest-window-member" })], hours: 24, excludes_passthrough: false });
    });
    await waitFor(() => {
      expect(screen.getAllByText("latest-window-member").length).toBeGreaterThan(0);
    });

    // …then the stale 7-day response lands last and must be dropped: it
    // must not paint its member anywhere (roster or detail heading).
    await act(async () => {
      first.resolve({ principals: [principalFixture({ id: "stale", note: "stale-window-member" })], hours: 168, excludes_passthrough: false });
    });
    expect(screen.queryByText("stale-window-member")).toBeNull();
    expect(screen.getAllByText("latest-window-member").length).toBeGreaterThan(0);
  });
});

describe("sign-out double submit", () => {
  it("fires the logout once for repeated clicks", async () => {
    const hanging = deferred<void>();
    const onSignOut = vi.fn(() => hanging.promise);

    render(createElement(Navigation, { view: "pulse", principal: operator, onChange: () => undefined, onSignOut }));

    const button = screen.getAllByRole("button", { name: "Sign out" })[0];
    fireEvent.click(button);
    fireEvent.click(button);

    await waitFor(() => {
      expect(onSignOut).toHaveBeenCalledTimes(1);
    });
    expect((button as HTMLButtonElement).disabled).toBe(true);
  });
});

describe("pass action double submit", () => {
  const activePass: GuestPass = {
    id: "pass-1",
    note: "Dave from climbing",
    status: "active",
    created_at: "2026-09-01T00:00:00Z",
    created_by: "op",
    link: "/join/p1",
    clients: 1,
  };

  it("revokes only once while the first revocation is in flight", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const hanging = deferred<void>();
    vi.mocked(loadPasses).mockReturnValueOnce(Promise.resolve([activePass]));
    vi.mocked(revokePass).mockReturnValueOnce(hanging.promise);

    render(createElement(Passes));
    const revoke = await screen.findByRole("button", { name: "Revoke" });

    fireEvent.click(revoke);
    fireEvent.click(revoke);

    await waitFor(() => {
      expect(revokePass).toHaveBeenCalledTimes(1);
    });
    confirmSpy.mockRestore();
  });

  it("drops the slower retry response when two retries overlap", async () => {
    // mockImplementationOnce (not mockReturnValueOnce) so the slow promise —
    // and its timer — are created at call time, after the clicks.
    vi.mocked(loadPasses)
      .mockImplementationOnce(() => Promise.reject(new Error("boom")))
      .mockImplementationOnce(() => new Promise<GuestPass[]>((resolve) => {
        // The first retry's list stays in flight while the second retry
        // below completes faster.
        setTimeout(() => resolve([{ ...activePass, note: "stale-retry-list" }]), 60);
      }))
      .mockImplementationOnce(() => Promise.resolve([{ ...activePass, note: "fresh-retry-list" }]));

    render(createElement(Passes));
    const retry = await screen.findByRole("button", { name: "Retry loading passes" });

    // The retry button fires refresh directly, so two clicks overlap: the
    // first reload is slow, the second is fast.
    fireEvent.click(retry);
    fireEvent.click(retry);

    await waitFor(() => {
      expect(screen.getAllByText("fresh-retry-list").length).toBeGreaterThan(0);
    });
    // Let the slower, older snapshot land last: the version guard must keep
    // it from overwriting the newer list.
    await new Promise((done) => setTimeout(done, 150));
    expect(screen.queryByText("stale-retry-list")).toBeNull();
    expect(screen.getAllByText("fresh-retry-list").length).toBeGreaterThan(0);
  });
});
