// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { AccessGate } from "./access";

afterEach(() => vi.unstubAllGlobals());

it("renders a username pattern the browser can compile with the unicodeSets flag", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ operator_exists: false }))));
  const { container } = render(<AccessGate onAccess={() => {}} />);
  await waitFor(() => expect(container.querySelector('input[autocomplete="username"]')).toBeTruthy());
  const input = container.querySelector('input[autocomplete="username"]') as HTMLInputElement;
  expect(input.pattern).toBeTruthy();
  // Browsers compile the pattern attribute with the v flag, where an unescaped
  // hyphen inside the character class is a syntax error.
  expect(() => new RegExp(input.pattern, "v")).not.toThrow();
  const anchored = new RegExp(`^(?:${input.pattern})$`, "v");
  expect(anchored.test("valid.user_name-1")).toBe(true);
  expect(anchored.test("bad space")).toBe(false);
});
