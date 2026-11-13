// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { completeQwenLogin, qwenLoginStatus, startQwenLogin } from "../api";
import { QwenLogin } from "./QwenLogin";

vi.mock("../api", () => ({ startQwenLogin: vi.fn(), qwenLoginStatus: vi.fn(), completeQwenLogin: vi.fn() }));
beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(window, "open").mockReturnValue(null);
  vi.mocked(startQwenLogin).mockResolvedValue({ session_id: "qwen-session", oauth_url: "https://modelstudio.console.alibabacloud.com/console-login?notice=fixture" });
});
afterEach(() => { cleanup(); vi.useRealTimers(); vi.resetAllMocks(); vi.restoreAllMocks(); });

it("finishes browser authorization before requesting the existing Token Plan key", async () => {
  vi.mocked(qwenLoginStatus).mockResolvedValueOnce({ status: "pending" }).mockResolvedValueOnce({ status: "awaiting_key" });
  vi.mocked(completeQwenLogin).mockResolvedValue({ status: "complete", account_id: "private-qwen" });
  const linked = vi.fn().mockResolvedValue(undefined);
  render(<QwenLogin onLinked={linked} />);
  expect(screen.queryByRole("textbox")).toBeNull();
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Qwen" })); });
  expect(screen.getByRole("link", { name: "Open Alibaba Cloud sign-in page" }).getAttribute("href")).toContain("modelstudio.console.alibabacloud.com/console-login");
  await act(() => vi.advanceTimersByTimeAsync(2000));
  expect(screen.queryByLabelText("Qwen Token Plan API key")).toBeNull();
  await act(() => vi.advanceTimersByTimeAsync(2000));
  fireEvent.change(screen.getByLabelText("Qwen Token Plan API key"), { target: { value: " sk-sp-existing-key " } });
  await act(async () => { fireEvent.keyDown(screen.getByLabelText("Qwen Token Plan API key"), { key: "Enter" }); });
  expect(completeQwenLogin).toHaveBeenCalledWith("qwen-session", "sk-sp-existing-key");
  expect(linked).toHaveBeenCalledOnce();
  await act(() => vi.advanceTimersByTimeAsync(4000));
  expect(qwenLoginStatus).toHaveBeenCalledTimes(2);
});

it("links an existing account without requesting its API key again", async () => {
  vi.mocked(qwenLoginStatus).mockResolvedValue({ status: "complete", account_id: "owned-qwen" });
  const linked = vi.fn().mockResolvedValue(undefined);
  render(<QwenLogin accountID="owned-qwen" onLinked={linked} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Alibaba Cloud" })); });
  expect(startQwenLogin).toHaveBeenCalledWith("owned-qwen");
  await act(() => vi.advanceTimersByTimeAsync(2000));
  expect(linked).toHaveBeenCalledOnce();
  expect(screen.queryByLabelText("Qwen Token Plan API key")).toBeNull();
  expect(completeQwenLogin).not.toHaveBeenCalled();
});

it("shows an expired authorization and permits a fresh browser sign-in", async () => {
  vi.mocked(qwenLoginStatus).mockResolvedValue({ status: "error", error: "Qwen login expired; sign in again" });
  const linked = vi.fn();
  render(<QwenLogin onLinked={linked} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Qwen" })); });
  await act(() => vi.advanceTimersByTimeAsync(2000));
  expect(screen.getByRole("alert").textContent).toContain("expired");
  expect(screen.getByRole("button", { name: "Sign in to Qwen" })).toBeTruthy();
  expect(linked).not.toHaveBeenCalled();
});

it("closes the reserved tab and displays unsupported callback errors", async () => {
  const popup = { opener: {}, close: vi.fn(), location: { replace: vi.fn() } };
  vi.mocked(window.open).mockReturnValue(popup as unknown as Window);
  vi.mocked(startQwenLogin).mockRejectedValue(new Error("Alibaba Cloud only returns browser authorization to a local HTTP address"));
  render(<QwenLogin onLinked={vi.fn()} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Qwen" })); });
  expect(popup.close).toHaveBeenCalledOnce();
  expect(screen.getByRole("alert").textContent).toContain("local HTTP address");
  expect(screen.queryByRole("link")).toBeNull();
});

it("does not finish a rejected key or continue polling after unmount", async () => {
  vi.mocked(qwenLoginStatus).mockResolvedValue({ status: "awaiting_key" });
  vi.mocked(completeQwenLogin).mockResolvedValue({ status: "error", error: "Qwen key validation failed" });
  const linked = vi.fn();
  const view = render(<QwenLogin onLinked={linked} />);
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Sign in to Qwen" })); });
  await act(() => vi.advanceTimersByTimeAsync(2000));
  fireEvent.change(screen.getByLabelText("Qwen Token Plan API key"), { target: { value: "sk-sp-rejected" } });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Add to pool" })); });
  expect(screen.getByRole("alert").textContent).toContain("validation failed");
  expect(linked).not.toHaveBeenCalled();
  view.unmount();
  await act(() => vi.advanceTimersByTimeAsync(5000));
  expect(qwenLoginStatus).toHaveBeenCalledOnce();
});
