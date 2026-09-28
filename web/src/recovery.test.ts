import { describe, expect, it } from "vitest";
import { recoveryDeadlineMs, recoveryReducer, type RecoveryLinkState } from "./recovery";

const ready: RecoveryLinkState = { phase: "ready", expiresAt: new Date("2030-01-01T00:00:00Z") };

describe("recovery link gating", () => {
  it("shows the form only after the backend validates the token", () => {
    const state = recoveryReducer({ phase: "checking" }, { type: "status", valid: true, expiresAt: "2030-01-01T00:00:00.000Z" });
    expect(state.phase).toBe("ready");
    expect((state as { expiresAt: Date }).expiresAt).toEqual(new Date("2030-01-01T00:00:00.000Z"));
  });

  it("collapses expired, used, replaced, and random tokens into the same unavailable state", () => {
    const events = [
      { type: "status" as const, valid: false },
      { type: "status" as const, valid: true, expiresAt: null },
      { type: "status" as const, valid: true, expiresAt: undefined },
      { type: "status" as const, valid: true, expiresAt: "not a date" },
      { type: "recheck" as const, valid: false, expiresAt: "2030-01-01T00:00:00.000Z" },
    ];
    for (const event of events) {
      expect(recoveryReducer(ready, event)).toEqual({ phase: "unavailable" });
      expect(recoveryReducer({ phase: "checking" }, event)).toEqual({ phase: "unavailable" });
    }
  });

  it("hides the form when a focus recheck finds the token consumed elsewhere", () => {
    expect(recoveryReducer(ready, { type: "recheck", valid: false }).phase).toBe("unavailable");
  });

  it("keeps a valid recheck answer ready", () => {
    const state = recoveryReducer(ready, { type: "recheck", valid: true, expiresAt: "2030-01-01T00:00:00.000Z" });
    expect(state.phase).toBe("ready");
  });

  it("treats a reached local deadline as expired", () => {
    const deadline = new Date("2030-01-01T00:00:00Z").getTime();
    expect(recoveryDeadlineMs(new Date(deadline), deadline)).toBe(0);
    expect(recoveryDeadlineMs(new Date(deadline), deadline - 1)).toBe(1);
    expect(recoveryDeadlineMs(new Date(deadline), deadline + 5000)).toBe(0);
  });
});
