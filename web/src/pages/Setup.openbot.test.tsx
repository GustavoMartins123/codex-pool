// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { SetupPage } from "./Setup";
import { loadMyClients, setupLinkMyClient } from "../api";
import { type ClientCredential } from "../types";

vi.mock("../api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api")>()),
  loadMyClients: vi.fn(),
  setupLinkMyClient: vi.fn(),
}));

const client: ClientCredential = { id: "c1", label: "Work laptop", status: "active", created_at: "2026-11-27T00:00:00Z" };
const base = window.location.origin;
const codexInstaller = `curl -sL "${base}/setup/codex/nonce-codex" | bash`;

beforeEach(() => {
  vi.mocked(loadMyClients).mockResolvedValue([client]);
  vi.mocked(setupLinkMyClient).mockResolvedValue({
    id: "c1",
    setup_urls: { codex: `${base}/config/codex/nonce-codex`, claude: `${base}/config/claude/nonce-claude` },
    nonce_expires_at: new Date(Date.now() + 300_000).toISOString(),
  });
});
afterEach(() => { cleanup(); vi.resetAllMocks(); });

function button(name: string) {
  return screen.getByRole("button", { name }) as HTMLButtonElement;
}

function codeBlock(text: string) {
  const matcher = (_: unknown, element: Element | null) => element?.tagName === "PRE" && element.textContent === text;
  return { find: () => screen.findByText(matcher), get: () => screen.getByText(matcher) };
}

const openBotEnvironment = `OPENAI_BASE_URL=${base}\nOPENAI_API_KEY=<access_token>\n# → OpenBot > Settings > Providers > ChatGPT > Environment`;

it("connects OpenBot with the Codex setup link", async () => {
  render(<SetupPage />);
  fireEvent.click(await screen.findByRole("button", { name: "OpenBot" }));
  expect(screen.getByText("Install OpenBot")).toBeTruthy();
  expect(screen.getByText(/OpenBot runs the Codex CLI of this computer/)).toBeTruthy();

  fireEvent.click(button("Generate link"));
  await waitFor(() => expect(setupLinkMyClient).toHaveBeenCalledWith("c1"));
  expect((await codeBlock(codexInstaller).find()).textContent).toBe(codexInstaller);
  expect(screen.getByText(/It is the same one-time link as in the Codex tab/)).toBeTruthy();
  expect(screen.getByText(/Settings > Providers and select Refresh/)).toBeTruthy();
});

it("offers provider environment variables for OpenBot when the Codex sign-in must stay", async () => {
  render(<SetupPage />);
  fireEvent.click(await screen.findByRole("button", { name: "OpenBot" }));
  expect(codeBlock(openBotEnvironment).get().textContent).toBe(openBotEnvironment);
  expect(screen.getByText(/route only OpenBot/)).toBeTruthy();
});

it("shares the generated Codex link between the Codex and OpenBot tabs", async () => {
  render(<SetupPage />);
  fireEvent.click(await screen.findByRole("button", { name: "OpenBot" }));
  fireEvent.click(button("Generate link"));
  await codeBlock(codexInstaller).find();
  fireEvent.click(button("Codex"));
  expect(codeBlock(codexInstaller).get().textContent).toBe(codexInstaller);
  expect(setupLinkMyClient).toHaveBeenCalledTimes(1);
});
