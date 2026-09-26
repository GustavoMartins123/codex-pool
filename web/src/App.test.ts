import { describe, expect, it, vi, afterEach } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { operatorBootstrap } from "./api";
import { AccountResetWindows, formatAPIValue, isArmedAccountAction, Models, poolSurplus, providerDisplay, shouldShowPassFormOnLoad, viewFromSearch } from "./App";
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

  it("never sends a fabricated admin token header", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ principal: { id: "p1", kind: "operator", status: "active" } }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    await operatorBootstrap("root", "root@local", "correct-horse-battery");

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    const headers = init.headers as Record<string, string>;
    expect(headers["X-Admin-Token"]).toBeUndefined();
    expect((init.credentials as string) || "").not.toBe("");
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
