// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ProviderAccess } from "./ProviderAccess";
import type { Policy } from "../governance-api";

afterEach(cleanup);
it("represents none, selected providers, and explicit allow all without an empty permissive selection", () => {
  const change = vi.fn();
  function Fixture() {
    const [value, setValue] = useState<Policy["providers"]>({ deny: ["*"] });
    return <ProviderAccess value={value} onChange={next => { change(next); setValue(next); }} />;
  }
  render(<Fixture />);
  expect(screen.getByText(/No providers allowed/)).toBeTruthy();
  fireEvent.click(screen.getByRole("checkbox", { name: "Claude" }));
  expect(change).toHaveBeenLastCalledWith({ allow: ["claude"], deny: [] });
  fireEvent.click(screen.getByRole("checkbox", { name: "Claude" }));
  expect(change).toHaveBeenLastCalledWith({ allow: [], deny: ["*"] });
  fireEvent.click(screen.getByRole("button", { name: "Allow all providers" }));
  expect(change).toHaveBeenLastCalledWith({ allow: ["*"], deny: [] });
  expect(screen.getAllByRole("checkbox").every(item => (item as HTMLInputElement).checked)).toBe(true);
  fireEvent.click(screen.getByRole("button", { name: "Block all providers" }));
  expect(change).toHaveBeenLastCalledWith({ allow: [], deny: ["*"] });
});
