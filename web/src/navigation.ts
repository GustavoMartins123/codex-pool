import { type PassportPrincipal } from "./types";

export type View = "pulse" | "insights" | "mine" | "passes" | "console" | "accounts" | "models" | "setup";

export type InsightMode = "overview" | "capacity" | "flow" | "demand";

type HistoryMode = "push" | "replace";


const VIEWS: View[] = ["pulse", "insights", "mine", "passes", "console", "accounts", "models", "setup"];

export const INSIGHT_MODES: InsightMode[] = ["overview", "capacity", "flow", "demand"];


export function viewFromSearch(search: string): View {
  const candidate = new URLSearchParams(search).get("view") as View | null;
  return candidate && VIEWS.includes(candidate) ? candidate : "pulse";
}


export function queryValue(name: string) {
  return new URLSearchParams(window.location.search).get(name);
}


export function updateURL(changes: Record<string, string | null>, mode: HistoryMode = "replace") {
  const url = new URL(window.location.href);
  for (const [name, value] of Object.entries(changes)) {
    if (value) url.searchParams.set(name, value);
    else url.searchParams.delete(name);
  }
  window.history[`${mode}State`](null, "", `${url.pathname}${url.search}${url.hash}`);
}


export function allowedViews(principal: PassportPrincipal): View[] {
  if (principal.kind === "operator") return VIEWS;
  if (principal.kind === "guest") return ["mine", "setup"];
  return ["mine", "setup", "passes"];
}
