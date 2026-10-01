// @vitest-environment jsdom
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { DeferredChart, BarChart } from "./charts";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("mounts chart work only after intersection and disconnects its observer", () => {
  let intersect: IntersectionObserverCallback;
  const disconnect = vi.fn();
  vi.stubGlobal("IntersectionObserver", class {
    constructor(callback: IntersectionObserverCallback) { intersect = callback; }
    observe = vi.fn(); disconnect = disconnect;
  });
  const child = vi.fn(() => <p>Chart rendered</p>);
  const Child = child;
  const view = render(<DeferredChart><Child /></DeferredChart>);
  expect(child).not.toHaveBeenCalled();
  act(() => intersect([{ isIntersecting: false } as IntersectionObserverEntry], {} as IntersectionObserver));
  expect(child).not.toHaveBeenCalled();
  act(() => intersect([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver));
  expect(screen.getByText("Chart rendered")).toBeTruthy();
  expect(disconnect).toHaveBeenCalledOnce();
  view.unmount();
  expect(disconnect).toHaveBeenCalledTimes(2);
});

it("reports an unavailable visibility API explicitly", () => {
  vi.stubGlobal("IntersectionObserver", undefined);
  render(<DeferredChart><p>Chart rendered</p></DeferredChart>);
  expect(screen.getByRole("alert").textContent).toContain("unavailable");
  expect(screen.queryByText("Chart rendered")).toBeNull();
});

it("provides readable chart values without mounting the chart engine", () => {
  vi.stubGlobal("IntersectionObserver", class { observe() {} disconnect() {} });
  render(<BarChart data={[{ hour: "12:00", tokens: 123 }]} config={{ tokens: { label: "Tokens", color: "orange" } }}><span /></BarChart>);
  const summary = screen.getByText("View chart data");
  expect(summary.tagName).toBe("SUMMARY");
  summary.parentElement!.setAttribute("open", "");
  expect(screen.getByRole("table")).toBeTruthy();
  expect(screen.getByRole("columnheader", { name: "Tokens" })).toBeTruthy();
  expect(screen.getByRole("cell", { name: "123" })).toBeTruthy();
  expect(screen.getByRole("region", { name: "Chart values" }).tabIndex).toBe(0);
});
