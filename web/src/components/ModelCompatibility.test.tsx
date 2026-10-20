// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { ModelCompatibility } from "./ModelCompatibility";
import { validCompatibility } from "../compatibility";
import type { ModelCompatibility as Verification } from "../types";

const evidence = (): Verification => ({ suite: "codex-pool/1", model: "claude-opus-5-5", provider: "claude", runtime: "a".repeat(64), codex_version: "codex-cli 0.159.1", started_at: new Date(Date.now() - 2000).toISOString(), finished_at: new Date(Date.now() - 1000).toISOString(), expired: false, checks: { text: { status: "passed", code: "exact_response" }, streaming: { status: "passed", code: "completed_stream" }, tools: { status: "failed", code: "unexpected_response" }, codex_edit: { status: "not_tested", code: "not_run" } } });
afterEach(cleanup);
it("does not infer verification from a model's declared capabilities", () => {
  render(<ModelCompatibility />);
  expect(screen.getAllByText("Not tested")).toHaveLength(4);
  expect(screen.queryByText("Passed")).toBeNull();
});
it("distinguishes passed, failed and untested checks and shows provenance", () => {
  render(<ModelCompatibility verification={evidence()} />);
  expect(screen.getAllByText("Passed")).toHaveLength(2);
  expect(screen.getByText("Failed")).toBeTruthy();
  expect(screen.getByText("Not tested")).toBeTruthy();
  expect(screen.getByText(/Operator-attested results/)).toBeTruthy();
});
it("removes passed badges when the runtime changes or evidence expires", () => {
  const value = evidence(); value.expired = true;
  const { rerender } = render(<ModelCompatibility verification={value} />);
  expect(screen.getAllByText("Expired")).toHaveLength(3);
  expect(screen.queryByText("Passed")).toBeNull();
  value.expired = false; value.finished_at = new Date(Date.now() - 8 * 86400_000).toISOString();
  rerender(<ModelCompatibility verification={value} />);
  expect(screen.queryByText("Passed")).toBeNull();
});
it("rejects evidence for another model and success without the required observation", () => {
  const value = evidence();
  expect(validCompatibility(value, value.model, value.provider)).toBe(true);
  expect(validCompatibility(value, "other-model", value.provider)).toBe(false);
  value.checks.tools = { status: "passed", code: "http_200" };
  expect(validCompatibility(value, value.model, value.provider)).toBe(false);
});
