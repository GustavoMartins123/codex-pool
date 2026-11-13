// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AccountContribution } from "./AccountContribution";
import { contributeAPIKey, exchangeAccountOAuth, exchangeClaudeRelogin, exchangeCodexRelogin, startAccountOAuth, startClaudeRelogin, startCodexRelogin } from "../api";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  startAccountOAuth: vi.fn(), exchangeAccountOAuth: vi.fn(),
  startClaudeRelogin: vi.fn(), exchangeClaudeRelogin: vi.fn(),
  startCodexRelogin: vi.fn(), exchangeCodexRelogin: vi.fn(),
  contributeAPIKey: vi.fn(),
}));

beforeEach(() => {
  vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(startAccountOAuth).mockResolvedValue({ oauth_url: "https://claude.com/authorize", verifier: "synthetic-verifier", state: "session-state" });
  vi.mocked(exchangeAccountOAuth).mockResolvedValue({ success: true, account_id: "claude-account" });
});

it("links a Qwen key with an explicitly configured Personal credit allowance", async () => {
  vi.mocked(contributeAPIKey).mockResolvedValue({ success: true, account_id: "qwen-account" });
  const onAdded = vi.fn().mockResolvedValue(undefined);
  render(<AccountContribution onClose={vi.fn()} onAdded={onAdded} />);
  fireEvent.click(screen.getByRole("button", { name: "Qwen" }));
  fireEvent.change(screen.getByLabelText("Qwen API key"), { target: { value: "sk-sp-synthetic-test-key" } });
  fireEvent.change(screen.getByLabelText("Credit source"), { target: { value: "personal_manual" } });
  fireEvent.change(screen.getByLabelText("Cycle credit allowance"), { target: { value: "25000" } });
  fireEvent.change(screen.getByLabelText("Credits remaining"), { target: { value: "18750" } });
  const now = Date.now();
  fireEvent.change(screen.getByLabelText("Subscription cycle start"), { target: { value: new Date(now - 86400000).toISOString().slice(0, 16) } });
  fireEvent.change(screen.getByLabelText("Subscription cycle end"), { target: { value: new Date(now + 29 * 86400000).toISOString().slice(0, 16) } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  await waitFor(() => expect(onAdded).toHaveBeenCalledOnce());
  expect(contributeAPIKey).toHaveBeenCalledWith("qwen", "sk-sp-synthetic-test-key", expect.objectContaining({ mode: "personal_manual", total_credits: 25000, remaining_credits: 18750 }));
});

it("keeps a rejected Qwen contribution open and shows the discovery error", async () => {
  vi.mocked(contributeAPIKey).mockRejectedValue(new Error("Qwen model discovery failed; the account was not added"));
  const onAdded = vi.fn();
  render(<AccountContribution onClose={vi.fn()} onAdded={onAdded} />);
  fireEvent.click(screen.getByRole("button", { name: "Qwen" }));
  fireEvent.change(screen.getByLabelText("Qwen API key"), { target: { value: "sk-sp-test" } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  expect((await screen.findByRole("alert")).textContent).toContain("discovery failed");
  expect(onAdded).not.toHaveBeenCalled();
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

async function start(provider: "Claude" | "Codex" = "Claude") {
  const onAdded = vi.fn().mockResolvedValue(undefined);
  render(<AccountContribution onClose={vi.fn()} onAdded={onAdded} />);
  fireEvent.click(screen.getByRole("button", { name: provider }));
  fireEvent.click(screen.getByRole("button", { name: `Sign in to ${provider}` }));
  const input = await screen.findByLabelText(provider === "Claude" ? "Claude authentication code" : "Authorization code or callback URL");
  return { input, onAdded };
}

it("explains and submits the full Claude authentication code", async () => {
  const { input, onAdded } = await start();
  expect(screen.getByText(/including the # and everything after it/)).toBeTruthy();
  fireEvent.change(input, { target: { value: "  synthetic-code#session-state  " } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  await waitFor(() => expect(onAdded).toHaveBeenCalledOnce());
  expect(exchangeAccountOAuth).toHaveBeenCalledWith("claude", "synthetic-code#session-state", "synthetic-verifier");
});

it.each(["synthetic-code#another-state", "synthetic-code#", "#session-state"])("rejects an invalid Claude code before sending it and allows correction: %s", async (code) => {
  const { input, onAdded } = await start();
  fireEvent.change(input, { target: { value: code } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  expect((await screen.findByRole("alert")).textContent).toContain("does not match this sign-in session");
  expect(exchangeAccountOAuth).not.toHaveBeenCalled();
  expect(onAdded).not.toHaveBeenCalled();
  fireEvent.change(input, { target: { value: "synthetic-code#session-state" } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  await waitFor(() => expect(onAdded).toHaveBeenCalledOnce());
  expect(startAccountOAuth).toHaveBeenCalledOnce();
});

it.each(["Claude", "Codex"] as const)("continues accepting matching callback URLs for %s", async (provider) => {
  const { input, onAdded } = await start(provider);
  fireEvent.change(input, { target: { value: "https://example.test/callback?code=synthetic-code&state=session-state" } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  await waitFor(() => expect(onAdded).toHaveBeenCalledOnce());
  expect(exchangeAccountOAuth).toHaveBeenCalledWith(provider.toLowerCase(), "synthetic-code", "synthetic-verifier");
});

it("rejects callback URLs from another session", async () => {
  const { input } = await start();
  fireEvent.change(input, { target: { value: "https://example.test/callback?code=synthetic-code&state=another-state" } });
  fireEvent.click(screen.getByRole("button", { name: "Add to pool" }));
  expect((await screen.findByRole("alert")).textContent).toContain("Callback does not match");
  expect(exchangeAccountOAuth).not.toHaveBeenCalled();
});

it.each(["claude", "codex"] as const)("renews the selected %s account through its relogin flow", async (provider) => {
  const startRelogin = provider === "claude" ? startClaudeRelogin : startCodexRelogin;
  const exchangeRelogin = provider === "claude" ? exchangeClaudeRelogin : exchangeCodexRelogin;
  vi.mocked(startRelogin).mockResolvedValue({ oauth_url: "https://example.test/authorize", verifier: "relogin-verifier", state: "relogin-state" });
  vi.mocked(exchangeRelogin).mockResolvedValue({ success: true, account_id: "existing-account" });
  const onAdded = vi.fn().mockResolvedValue(undefined);
  render(<AccountContribution onClose={vi.fn()} onAdded={onAdded} reloginAccountID="existing-account" reloginProvider={provider} />);
  expect(screen.getByRole("heading", { name: "Relogin account" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Codex" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Claude" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: `Sign in to ${provider === "claude" ? "Claude" : "Codex"}` }));
  const input = await screen.findByLabelText(provider === "claude" ? "Claude authentication code" : "Authorization code or callback URL");
  expect(startRelogin).toHaveBeenCalledWith("existing-account");
  fireEvent.change(input, { target: { value: provider === "claude" ? "new-code#relogin-state" : "new-code" } });
  fireEvent.click(screen.getByRole("button", { name: "Save credentials" }));
  await waitFor(() => expect(onAdded).toHaveBeenCalledOnce());
  expect(exchangeRelogin).toHaveBeenCalledWith(provider === "claude" ? "new-code#relogin-state" : "new-code", "relogin-verifier");
  expect(startAccountOAuth).not.toHaveBeenCalled();
  expect(exchangeAccountOAuth).not.toHaveBeenCalled();
});

it("keeps the Claude relogin dialog open when another account is rejected", async () => {
  vi.mocked(startClaudeRelogin).mockResolvedValue({ oauth_url: "https://claude.com/authorize", verifier: "relogin-verifier", state: "relogin-state" });
  vi.mocked(exchangeClaudeRelogin).mockRejectedValue(new Error("signed-in Claude account does not match the existing pool account"));
  const onAdded = vi.fn().mockResolvedValue(undefined);
  render(<AccountContribution onClose={vi.fn()} onAdded={onAdded} reloginAccountID="existing-account" reloginProvider="claude" />);
  fireEvent.click(screen.getByRole("button", { name: "Sign in to Claude" }));
  fireEvent.change(await screen.findByLabelText("Claude authentication code"), { target: { value: "new-code#relogin-state" } });
  fireEvent.click(screen.getByRole("button", { name: "Save credentials" }));
  expect((await screen.findByRole("alert")).textContent).toContain("does not match");
  expect(onAdded).not.toHaveBeenCalled();
  expect(screen.getByRole("dialog")).toBeTruthy();
});
