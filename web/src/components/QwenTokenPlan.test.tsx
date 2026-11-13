// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { loadQwenTokenPlan, saveQwenTokenPlan, refreshQwenTokenPlan } from "../api";
import { emptyQwenTokenPlan, QwenCreditBalance, QwenCreditManager, qwenTokenPlanConfig } from "./QwenTokenPlan";
import type { TokenPlanCredits } from "../types";

vi.mock("../api", () => ({ loadQwenTokenPlan: vi.fn(), saveQwenTokenPlan: vi.fn(), refreshQwenTokenPlan: vi.fn() }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

function credits(): TokenPlanCredits {
  return { edition: "personal", plan: "essential", total: 25000, used: 6250, remaining: 18750, cycle_start: new Date(Date.now() - 86400000).toISOString(), cycle_end: new Date(Date.now() + 29 * 86400000).toISOString(), updated_at: new Date().toISOString(), source: "manual" };
}

it("shows used and remaining credits as a percentage of the subscription allowance", () => {
  render(<QwenCreditBalance credits={credits()} />);
  expect(screen.getByText("25.0%")).toBeTruthy();
  expect(screen.getByText(/Manual balance/)).toBeTruthy();
  expect(screen.getByText("Credits used")).toBeTruthy();
  expect(screen.getByText("Credits remaining")).toBeTruthy();
  expect(screen.getByText("Cycle allowance")).toBeTruthy();
});

it("does not show stale or missing balances as zero usage", () => {
  const { rerender } = render(<QwenCreditBalance error="Cloud credit query failed" />);
  expect(screen.getByText("Cloud credit query failed")).toBeTruthy();
  expect(screen.queryByText(/0.0%/)).toBeNull();
  rerender(<QwenCreditBalance credits={{ ...credits(), cycle_end: new Date(Date.now() - 1).toISOString() }} />);
  expect(screen.getByText(/expired cycle/)).toBeTruthy();
  expect(screen.queryByText("25.0%")).toBeNull();
});

it("updates the saved Personal balance from the account owner controls", async () => {
  const balance = credits();
  const plan = { mode: "personal_manual" as const, total_credits: balance.total, remaining_credits: balance.remaining, cycle_start: balance.cycle_start, cycle_end: balance.cycle_end };
  vi.mocked(loadQwenTokenPlan).mockResolvedValue({ token_plan: plan, credits: balance, error: "" });
  vi.mocked(saveQwenTokenPlan).mockResolvedValue({ token_plan: { ...plan, remaining_credits: 12500 }, credits: { ...balance, used: 12500, remaining: 12500 }, error: "" });
  const changed = vi.fn().mockResolvedValue(undefined);
  render(<QwenCreditManager id="owned-qwen" onChanged={changed} />);
  fireEvent.click(screen.getByRole("button", { name: "Manage Token Plan credits" }));
  fireEvent.change(await screen.findByLabelText("Credits remaining"), { target: { value: "12500" } });
  fireEvent.click(screen.getByRole("button", { name: "Save credit tracking" }));
  await waitFor(() => expect(changed).toHaveBeenCalledOnce());
  expect(saveQwenTokenPlan).toHaveBeenCalledWith("owned-qwen", expect.objectContaining({ remaining_credits: 12500, total_credits: 25000 }));
  expect(screen.getByText("50.0%")).toBeTruthy();
});

it("clears the displayed Team balance if automatic refresh fails", async () => {
  vi.mocked(loadQwenTokenPlan).mockResolvedValue({ token_plan: { mode: "team_api", seat_id: "seat" }, credits: { ...credits(), edition: "team", source: "alibaba-api" }, error: "" });
  vi.mocked(refreshQwenTokenPlan).mockRejectedValue(new Error("Access denied"));
  render(<QwenCreditManager id="owned-qwen" />);
  fireEvent.click(screen.getByRole("button", { name: "Manage Token Plan credits" }));
  fireEvent.click(await screen.findByRole("button", { name: "Refresh Team balance" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Access denied");
  expect(screen.queryByText("25.0%")).toBeNull();
});

it("rejects a balance larger than its allowance", () => {
  expect(() => qwenTokenPlanConfig({ ...emptyQwenTokenPlan(), mode: "personal_manual", total: "100", remaining: "101" })).toThrow(/remaining balance/);
});

it("links the Personal Cloud account without manual credit values", async () => {
  vi.mocked(loadQwenTokenPlan).mockResolvedValue({ token_plan: null, credits: null, error: "Not configured" });
  const balance = { ...credits(), source: "alibaba-console" as const };
  vi.mocked(saveQwenTokenPlan).mockResolvedValue({ token_plan: { mode: "personal_cloud" }, credits: balance, error: "" });
  render(<QwenCreditManager id="cloud-qwen" />);
  fireEvent.click(screen.getByRole("button", { name: "Manage Token Plan credits" }));
  fireEvent.change(await screen.findByLabelText("Credit source"), { target: { value: "personal_cloud" } });
  fireEvent.change(screen.getByLabelText("Alibaba Cloud AccessKey ID"), { target: { value: "synthetic-key-id" } });
  fireEvent.change(screen.getByLabelText("Alibaba Cloud AccessKey secret"), { target: { value: "synthetic-secret" } });
  expect(screen.queryByLabelText("Cycle credit allowance")).toBeNull();
  expect(screen.queryByLabelText("Token Plan seat ID")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Save credit tracking" }));
  await waitFor(() => expect(saveQwenTokenPlan).toHaveBeenCalledWith("cloud-qwen", { mode: "personal_cloud", access_key_id: "synthetic-key-id", access_key_secret: "synthetic-secret", security_token: "" }));
  expect(await screen.findByText(/Automatic balance/)).toBeTruthy();
  expect((screen.getByLabelText("Alibaba Cloud AccessKey secret") as HTMLInputElement).value).toBe("");
  expect(screen.getByRole("button", { name: "Refresh Cloud balance" })).toBeTruthy();
});
