import { describe, expect, it } from "vitest";
import { ResponseVersion } from "./response-version";

describe("ResponseVersion", () => {
  it("drops a response superseded by a newer call", () => {
    const guard = new ResponseVersion();
    const first = guard.begin();
    const second = guard.begin();

    expect(guard.isCurrent(first)).toBe(false);
    expect(guard.isCurrent(second)).toBe(true);
  });

  it("keeps the latest call current while it is the only one", () => {
    const guard = new ResponseVersion();
    const only = guard.begin();

    expect(guard.isCurrent(only)).toBe(true);
  });

  it("invalidates every in-flight call without starting a new one", () => {
    const guard = new ResponseVersion();
    const inFlight = guard.begin();
    guard.invalidate();

    expect(guard.isCurrent(inFlight)).toBe(false);
    expect(guard.isCurrent(guard.begin())).toBe(true);
  });
});
