// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { authorizeQwenLogin, completeQwenLogin, startQwenLogin, type AccountContributionResult } from "../api";
import { QwenLogin } from "./QwenLogin";

vi.mock("../api", () => ({ startQwenLogin: vi.fn(), authorizeQwenLogin: vi.fn(), completeQwenLogin: vi.fn() }));
const callback = "http://127.0.0.1:12345/cli/callback?code=synthetic-code&state=synthetic-state";
beforeEach(() => {
  vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(startQwenLogin).mockResolvedValue({ session_id: "qwen-session", oauth_url: "https://signin.alibabacloud.com/oauth2/v1/auth?code_challenge=fixture" });
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.restoreAllMocks(); });

async function start(accountID?: string, onLinked = vi.fn().mockResolvedValue(undefined)) {
  const view = render(<QwenLogin accountID={accountID} onLinked={onLinked} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: accountID ? "Sign in to Alibaba Cloud" : "Sign in to Qwen" })); });
  fireEvent.change(screen.getByLabelText("Alibaba Cloud callback URL"), { target: { value: callback } });
  return { ...view, onLinked };
}

it("exchanges the pasted OAuth callback before requesting the existing Token Plan key", async () => {
  vi.mocked(authorizeQwenLogin).mockResolvedValue({ status: "awaiting_key" });
  vi.mocked(completeQwenLogin).mockResolvedValue({ status: "complete", account_id: "private-qwen" });
  const { onLinked } = await start();
  expect(screen.getByRole("link", { name: "Open Alibaba Cloud sign-in page" }).getAttribute("href")).toContain("signin.alibabacloud.com/oauth2/v1/auth");
  expect(screen.getByText(/local callback page may fail to load/)).toBeTruthy();
  expect(screen.queryByLabelText("Qwen Token Plan API key")).toBeNull();
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Confirm Alibaba Cloud authorization" })); });
  expect(authorizeQwenLogin).toHaveBeenCalledWith("qwen-session", callback);
  fireEvent.change(screen.getByLabelText("Qwen Token Plan API key"), { target: { value: " sk-sp-existing-key " } });
  await act(async () => { fireEvent.keyDown(screen.getByLabelText("Qwen Token Plan API key"), { key: "Enter" }); });
  expect(completeQwenLogin).toHaveBeenCalledWith("qwen-session", "sk-sp-existing-key");
  expect(onLinked).toHaveBeenCalledOnce();
});

it("links an existing account without requesting its API key again", async () => {
  vi.mocked(authorizeQwenLogin).mockResolvedValue({ status: "complete", account_id: "owned-qwen" });
  const { onLinked } = await start("owned-qwen");
  expect(startQwenLogin).toHaveBeenCalledWith("owned-qwen");
  await act(async () => { fireEvent.keyDown(screen.getByLabelText("Alibaba Cloud callback URL"), { key: "Enter" }); });
  expect(onLinked).toHaveBeenCalledOnce();
  expect(screen.queryByLabelText("Qwen Token Plan API key")).toBeNull();
  expect(completeQwenLogin).not.toHaveBeenCalled();
});

it("keeps an invalid callback editable and exchanges a corrected callback", async () => {
  vi.mocked(authorizeQwenLogin).mockRejectedValueOnce(new Error("Callback does not match this sign-in session")).mockResolvedValueOnce({ status: "awaiting_key" });
  await start();
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Confirm Alibaba Cloud authorization" })); });
  expect(screen.getByRole("alert").textContent).toContain("does not match");
  expect((screen.getByLabelText("Alibaba Cloud callback URL") as HTMLInputElement).value).toBe(callback);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Confirm Alibaba Cloud authorization" })); });
  expect(screen.getByLabelText("Qwen Token Plan API key")).toBeTruthy();
  expect(startQwenLogin).toHaveBeenCalledOnce();
});

it("shows expired authorization and permits fresh sign-in", async () => {
  vi.mocked(authorizeQwenLogin).mockResolvedValue({ status: "error", error: "Qwen login expired; sign in again" });
  const { onLinked } = await start();
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Confirm Alibaba Cloud authorization" })); });
  expect(screen.getByRole("alert").textContent).toContain("expired");
  expect(screen.getByRole("button", { name: "Sign in to Qwen" })).toBeTruthy();
  expect(onLinked).not.toHaveBeenCalled();
});

it("closes the reserved tab when sign-in cannot start", async () => {
  const popup = { opener: {}, close: vi.fn(), location: { replace: vi.fn() } };
  vi.mocked(window.open).mockReturnValue(popup as unknown as Window);
  vi.mocked(startQwenLogin).mockRejectedValue(new Error("Account contribution is unavailable"));
  render(<QwenLogin onLinked={vi.fn()} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Qwen" })); });
  expect(popup.close).toHaveBeenCalledOnce();
  expect(screen.getByRole("alert").textContent).toContain("contribution is unavailable");
});

it("does not report completion after the contribution dialog is unmounted", async () => {
  let resolve!: (result: AccountContributionResult) => void;
  vi.mocked(authorizeQwenLogin).mockReturnValue(new Promise(done => { resolve = done; }));
  const { unmount, onLinked } = await start("owned-qwen");
  fireEvent.click(screen.getByRole("button", { name: "Confirm Alibaba Cloud authorization" }));
  unmount();
  await act(async () => { resolve({ status: "complete", account_id: "owned-qwen" }); });
  expect(onLinked).not.toHaveBeenCalled();
});
