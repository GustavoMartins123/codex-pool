// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { exchangeAccountOAuth } from "./api";

afterEach(() => vi.unstubAllGlobals());

it.each([502, 503, 504])("explains a non-JSON gateway response (%s) without exposing its body", async (status) => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("<html>Gateway error with private diagnostic details</html>", { status })));
  await expect(exchangeAccountOAuth("claude", "synthetic-code#state", "synthetic-verifier"))
    .rejects.toThrow(`Pool service is temporarily unavailable (${status}). Please try again shortly.`);
});

it("preserves the pool's JSON error for account identification failures", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response('{"error":"could not identify Claude account"}', { status: 502 })));
  await expect(exchangeAccountOAuth("claude", "synthetic-code#state", "synthetic-verifier"))
    .rejects.toThrow("could not identify Claude account");
});

it("still rejects a malformed successful response", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("invalid-json", { status: 200 })));
  await expect(exchangeAccountOAuth("claude", "synthetic-code#state", "synthetic-verifier"))
    .rejects.toThrow("Invalid JSON response (200)");
});
