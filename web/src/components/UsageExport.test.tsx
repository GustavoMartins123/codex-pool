// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { UsageExport } from "./UsageExport";
import { downloadUsageExport } from "../api";

vi.mock("../api", () => ({ downloadUsageExport: vi.fn() }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

it("exports the selected principal from the console in both formats", async () => {
  vi.mocked(downloadUsageExport).mockResolvedValue(undefined);
  render(<UsageExport scope="console" hours={720} principal="member-1" />);
  fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
  await waitFor(() => expect(downloadUsageExport).toHaveBeenCalledWith("console", { hours: 720, format: "csv", principal: "member-1" }));
  fireEvent.click(screen.getByRole("button", { name: "Export JSON" }));
  await waitFor(() => expect(downloadUsageExport).toHaveBeenCalledWith("console", { hours: 720, format: "json", principal: "member-1" }));
});

it("exports only the member's own usage from Mine", async () => {
  vi.mocked(downloadUsageExport).mockResolvedValue(undefined);
  render(<UsageExport scope="me" hours={168} />);
  fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
  await waitFor(() => expect(downloadUsageExport).toHaveBeenCalledWith("me", { hours: 168, format: "csv", principal: undefined }));
});

it("disables both buttons while a download runs and surfaces backend errors", async () => {
  let fail: (error: Error) => void = () => {};
  vi.mocked(downloadUsageExport).mockImplementation(() => new Promise<void>((_, reject) => { fail = reject; }));
  render(<UsageExport scope="console" hours={24} />);
  fireEvent.click(screen.getByRole("button", { name: "Export CSV" }));
  expect((screen.getByRole("button", { name: "Exporting…" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Export JSON" }) as HTMLButtonElement).disabled).toBe(true);
  fail(new Error("analytics query failed"));
  expect((await screen.findByRole("alert")).textContent).toBe("analytics query failed");
  await waitFor(() => expect((screen.getByRole("button", { name: "Export CSV" }) as HTMLButtonElement).disabled).toBe(false));
});