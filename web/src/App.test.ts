import { describe, expect, it, vi, afterEach } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { loadAdminAccounts, mutateAccount, operatorBootstrap, reloadAccounts, recoverMemberStatus } from "./api";
import { AccountResetWindows, formatAPIValue, isArmedAccountAction, MemberRecovery, Models, poolSurplus, providerDisplay, RecoveryUnavailable, shouldShowPassFormOnLoad, viewFromSearch } from "./App";
import type { AccountStats, ResetWindowPolicy } from "./types";

function renderWindows(resetWindows: ResetWindowPolicy, overrides: Partial<AccountStats> = {}) {
  const account = {
    reset_windows: resetWindows,
    primary_window_available: true,
    secondary_window_available: true,
    primary_window_used_pct: 20,
    secondary_window_used_pct: 30,
    primary_reset_minutes: 120,
    secondary_reset_minutes: 9000,
    primary_pace_ratio: 0,
    secondary_pace_ratio: 0,
    ...overrides,
  } as AccountStats;
  return renderToStaticMarkup(createElement(AccountResetWindows, { account, compact: true }));
}

describe("account reset windows", () => {
  it("shows Codex Plus and Team Basic five-hour and weekly windows", () => {
    for (const tier of ["plus", "team_basic"] as const) {
      const markup = renderWindows({ tier, primary: "five_hour", secondary: "weekly" });
      expect(markup).toContain("5 hour");
      expect(markup).toContain("Weekly");
    }
  });

  it("shows only the weekly window for Codex Pro and labels Z.ai rate limits honestly", () => {
    const pro = renderWindows({ tier: "pro_or_higher", primary: "none", secondary: "weekly" });
    expect(pro).toContain("Weekly");
    expect(pro).not.toContain("5 hour");
    expect(pro).not.toContain("Primary");

    const zai = renderWindows({ tier: "unknown", primary: "requests", secondary: "tokens" });
    expect(zai).toContain("Requests");
    expect(zai).toContain("Tokens");
    expect(zai).not.toContain("Daily");
    expect(zai).not.toContain("Weekly");

    const unreported = renderWindows({ tier: "unknown", primary: "requests", secondary: "tokens" }, { primary_usage_reported: false, secondary_usage_reported: false });
    expect(unreported.match(/NOT REPORTED/g)).toHaveLength(2);
  });
});

describe("account action confirmation", () => {
  it("is scoped to both the account and action", () => {
    const armed = { accountID: "account-a", kind: "disable" as const };

    expect(isArmedAccountAction(armed, "account-a", "disable")).toBe(true);
    expect(isArmedAccountAction(armed, "account-b", "disable")).toBe(false);
    expect(isArmedAccountAction(armed, "account-a", "refresh")).toBe(false);
  });
});

describe("poolSurplus", () => {
  it("uses the same account totals shown beside it", () => {
    expect(poolSurplus({ total_api_cost: 5634, total_subscription_cost: 2064 })).toBe(3570);
  });
});

describe("formatAPIValue", () => {
  it("shows API-equivalent value even when the account has no subscription cost", () => {
    expect(formatAPIValue(0)).toBe("$0.00");
    expect(formatAPIValue(12.345)).toBe("$12.35");
  });
});

describe("guest pass form", () => {
  it("opens automatically only when the loaded pass list is empty", () => {
    expect(shouldShowPassFormOnLoad([])).toBe(true);
    expect(shouldShowPassFormOnLoad([{ id: "pass_1" } as never])).toBe(false);
  });
});

describe("viewFromSearch", () => {
  it("restores a valid workspace and rejects unknown values", () => {
    expect(viewFromSearch("?view=accounts")).toBe("accounts");
    expect(viewFromSearch("?view=unknown")).toBe("pulse");
    expect(viewFromSearch("")).toBe("pulse");
  });
});

describe("providerDisplay", () => {
  it("defines display metadata for adverserial accounts returned by the pool API", () => {
    expect(providerDisplay("adverserial")).toEqual({
      label: "Adverserial",
      color: "#ff5454",
      dither: "red",
      glyph: "◬",
    });
  });

  it("defines display metadata for opencode_go accounts returned by the pool API", () => {
    expect(providerDisplay("opencode_go")).toEqual({
      label: "OpenCode Go",
      color: "#ffd23f",
      dither: "gold",
      glyph: "⬢",
    });
  });

  it("defines display metadata for pool orchestration models", () => {
    expect(providerDisplay("pool")).toEqual({
      label: "Pool",
      color: "#d5a638",
      dither: "gold",
      glyph: "⊛",
    });
  });

  it("falls back safely when the API returns a provider newer than the frontend", () => {
    expect(providerDisplay("future-provider")).toEqual({
      label: "Unknown",
      color: "#9c967f",
      dither: "grey",
      glyph: "·",
    });
  });
});


describe("operator bootstrap", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("sends the configured admin token in the bootstrap header", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ principal: { id: "p1", kind: "operator", status: "active" } }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await operatorBootstrap("root", "root@local", "correct-horse-battery", "configured-admin-token");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Admin-Token"]).toBe("configured-admin-token");
    expect((init.credentials as string) || "").not.toBe("");
  });
});

describe("member recovery gating", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("asks the backend for link status before any form renders", () => {
    const markup = renderToStaticMarkup(createElement(MemberRecovery, { token: "tok", onAccess: () => undefined }));
    expect(markup).toContain("Checking your recovery link");
    expect(markup).not.toContain("Set password");
    expect(markup).not.toContain('type="password"');
  });

  it("renders the unavailable screen without any password field", () => {
    const markup = renderToStaticMarkup(createElement(RecoveryUnavailable));
    expect(markup).toContain("Recovery link unavailable");
    expect(markup).toContain("expired, was already used, or has been replaced");
    expect(markup).not.toContain('type="password"');
  });

  it("posts the token to the live status endpoint", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ valid: true, expires_at: "2030-01-01T00:00:00Z", expires_in_seconds: 900 }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const status = await recoverMemberStatus("tok-1");

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/auth/recover/status");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("same-origin");
    expect(JSON.parse(init.body as string)).toEqual({ token: "tok-1" });
    expect(status.valid).toBe(true);
    expect(status.expires_in_seconds).toBe(900);
  });
});

describe("operator account requests", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("uses the operator session and sends CSRF for mutations", async () => {
    vi.stubGlobal("document", { cookie: "pool_csrf=operator-csrf" });
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response("[]", { status: 200 }))
      .mockResolvedValueOnce(new Response("{}", { status: 200 }))
      .mockResolvedValueOnce(new Response("ok", { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await loadAdminAccounts();
    await mutateAccount("account-1", "disable");
    await reloadAccounts();

    const calls = fetchMock.mock.calls as Array<[string, RequestInit]>;
    expect(calls.map(([url]) => url)).toEqual([
      "/admin/accounts", "/admin/accounts/account-1/disable", "/admin/reload",
    ]);
    for (const [, init] of calls) {
      expect(init.credentials).toBe("same-origin");
      expect(init.headers ?? {}).not.toHaveProperty("X-Admin-Token");
    }
    for (const [, init] of calls.slice(1)) {
      expect((init.headers as Record<string, string>)["X-CSRF-Token"]).toBe("operator-csrf");
    }
  });
});


describe("Models component", () => {
  it("renders models including pool orchestration and unknown providers without error", () => {
    const models = [
      {
        id: "pool/auto",
        name: "Pool Auto",
        provider: "pool",
        protocol: "openai",
        available_now: true,
        supporting_accounts: 3,
        available_accounts: 3,
      },
      {
        id: "custom/future-model",
        name: "Future Model",
        provider: "unknown-future-provider",
        protocol: "openai",
        available_now: false,
        supporting_accounts: 1,
        available_accounts: 0,
      },
    ];
    const markup = renderToStaticMarkup(createElement(Models, { models }));
    expect(markup).toContain("Pool");
    expect(markup).toContain("pool/auto");
    expect(markup).toContain("Unknown");
    expect(markup).toContain("custom/future-model");
  });
});
