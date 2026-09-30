// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { createElement } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App } from "./App";
import { loadAdminAccounts, loadModelCatalog, loadPoolStats, loadSignalAnalytics } from "./api";

vi.mock("./api", async (importOriginal) => ({
  ...await importOriginal<typeof import("./api")>(),
  loadPassportMe: vi.fn(async () => ({ id: "operator", kind: "operator", status: "active" })),
  loadPoolStats: vi.fn(async () => ({ generated_at: "2026-09-29T00:00:00Z", active_accounts: 1, total_accounts: 1, last_24h_tokens: 0 })),
  loadSignalAnalytics: vi.fn(),
  loadAdminAccounts: vi.fn(async () => []),
  loadModelCatalog: vi.fn(async () => ({ models: [] })),
}));
vi.mock("./pages/Models", () => ({ Models: () => createElement("p", null, "Model page") }));
vi.mock("./pages/Setup", () => ({ SetupPage: () => createElement("p", null, "Setup page") }));
vi.mock("./pages/Accounts", () => ({
  Accounts: ({ onAccountsChanged }: { onAccountsChanged: () => Promise<void> }) => createElement("button", {
    onClick: () => { void onAccountsChanged(); },
  }, "Reload account data"),
}));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

it("loads the selected page once at boot and refreshes only its resources", async () => {
  window.history.replaceState(null, "", "/?view=models");
  render(createElement(App));
  expect(await screen.findByText("Model page")).toBeTruthy();
  expect(loadModelCatalog).toHaveBeenCalledOnce();
  expect(loadPoolStats).not.toHaveBeenCalled();
  expect(loadSignalAnalytics).not.toHaveBeenCalled();
  expect(loadAdminAccounts).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(loadModelCatalog).toHaveBeenCalledTimes(2));
  fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
  expect(await screen.findByRole("button", { name: "Reload account data" })).toBeTruthy();
  expect(loadPoolStats).toHaveBeenCalledOnce();
  expect(loadAdminAccounts).toHaveBeenCalledOnce();

  fireEvent.click(screen.getByRole("button", { name: "Reload account data" }));
  await waitFor(() => expect(loadAdminAccounts).toHaveBeenCalledTimes(2));
  expect(loadPoolStats).toHaveBeenCalledTimes(2);
  expect(loadModelCatalog).toHaveBeenCalledTimes(2);
  expect(loadSignalAnalytics).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: "Setup" }));
  expect(await screen.findByText("Setup page")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Refresh" })).toBeNull();
  expect(screen.queryByText("1 of 1 accounts live")).toBeNull();
  expect(loadPoolStats).toHaveBeenCalledTimes(2);
  expect(loadAdminAccounts).toHaveBeenCalledTimes(2);
  expect(loadModelCatalog).toHaveBeenCalledTimes(2);
});
