// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AccountContribution } from "./AccountContribution";
import { exchangeAccountOAuth, startAccountOAuth } from "../api";

vi.mock("../api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api")>(),
  startAccountOAuth: vi.fn(), exchangeAccountOAuth: vi.fn(),
}));

beforeEach(() => {
  vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(startAccountOAuth).mockResolvedValue({ oauth_url: "https://claude.com/authorize", verifier: "synthetic-verifier", state: "session-state" });
  vi.mocked(exchangeAccountOAuth).mockResolvedValue({ success: true, account_id: "claude-account" });
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
