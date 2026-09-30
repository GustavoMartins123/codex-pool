// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { createElement } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App } from "./App";

const imports = vi.hoisted(() => ({ mine: vi.fn(), setup: vi.fn(), passes: vi.fn(), analytics: vi.fn() }));

vi.mock("./api", async (importOriginal) => ({
  ...await importOriginal<typeof import("./api")>(),
  loadPassportMe: vi.fn(async () => ({ id: "member", kind: "member", status: "active" })),
}));
vi.mock("./pages/Mine", () => {
  imports.mine();
  return { PassportMine: () => createElement("p", null, "Profile loaded") };
});
vi.mock("./pages/Setup", () => {
  imports.setup();
  return { SetupPage: () => createElement("p", null, "Setup loaded") };
});
vi.mock("./pages/Analytics", () => {
  imports.analytics();
  return { Pulse: () => createElement("p", null, "Pulse loaded"), Insights: () => null };
});
vi.mock("./pages/Passes", () => {
  imports.passes();
  throw new Error("Page asset failed to load");
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("loads pages when selected and exposes a rejected page import", async () => {
  window.history.replaceState(null, "", "/");
  vi.spyOn(console, "error").mockImplementation(() => {});
  render(createElement(App));
  expect(await screen.findByText("Profile loaded")).toBeTruthy();
  expect(imports.mine).toHaveBeenCalledOnce();
  expect(imports.setup).not.toHaveBeenCalled();
  expect(imports.passes).not.toHaveBeenCalled();
  expect(imports.analytics).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: /setup/i }));
  expect(await screen.findByText("Setup loaded")).toBeTruthy();
  expect(imports.setup).toHaveBeenCalledOnce();
  expect(screen.queryByText("Profile loaded")).toBeNull();

  fireEvent.click(screen.getByRole("button", { name: /passes/i }));
  expect(await screen.findByRole("alert")).toHaveProperty("textContent", "Unable to load this page. Reload the page to try again.");
  expect(imports.passes).toHaveBeenCalledOnce();
  expect(screen.queryByText("Setup loaded")).toBeNull();
  expect(imports.analytics).not.toHaveBeenCalled();

  fireEvent.click(screen.getByRole("button", { name: /setup/i }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(await screen.findByText("Setup loaded")).toBeTruthy();
});
