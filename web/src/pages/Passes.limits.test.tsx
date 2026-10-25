// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Passes } from "./Passes";
import { loadPasses } from "../api";

vi.mock("../api", () => ({ loadPasses: vi.fn() }));
beforeEach(() => vi.mocked(loadPasses).mockResolvedValue([]));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

function expiryInput() {
  return screen.getByLabelText(/Expiry/) as HTMLInputElement;
}

it("requires a bounded expiry for member passes and prefills one", async () => {
  render(<Passes />);
  await screen.findByRole("button", { name: "Create pass" });
  const input = expiryInput();
  expect(input.required).toBe(true);
  expect(input.value).not.toBe("");
  const days = (new Date(input.max).getTime() - Date.now()) / 86_400_000;
  expect(days).toBeGreaterThan(89);
  expect(days).toBeLessThanOrEqual(90);
  expect(new Date(input.value).getTime()).toBeLessThanOrEqual(new Date(input.max).getTime());
});

it("keeps the expiry optional for operators", async () => {
  render(<Passes operator />);
  await screen.findByRole("button", { name: "Create pass" });
  const input = expiryInput();
  expect(input.required).toBe(false);
  expect(input.value).toBe("");
  expect(input.max).toBe("");
});
