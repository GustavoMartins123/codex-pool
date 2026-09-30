import { useCallback, useEffect, useRef, useState } from "react";
import { loadAdminAccounts, loadModelCatalog, loadPoolStats, loadSignalAnalytics } from "./api";
import type { View } from "./navigation";
import { ResponseVersion } from "./response-version";
import type { AdminAccount, ModelDescriptor, PassportPrincipal, PoolStats, SignalAnalytics } from "./types";

type PageData =
  | { view: "pulse" | "insights"; stats: PoolStats; signal: SignalAnalytics }
  | { view: "accounts"; stats: PoolStats; accounts: AdminAccount[] }
  | { view: "models"; models: ModelDescriptor[] };

type Snapshot = { scope: string; data: PageData | null; loading: boolean; error: string };

export function usePageData(view: View, principal: PassportPrincipal | null, enabled: boolean) {
  const managed = enabled && principal?.kind === "operator" && principal.status === "active"
    && (view === "pulse" || view === "insights" || view === "accounts" || view === "models");
  const scope = managed ? JSON.stringify([principal.id, view]) : null;
  const activeScope = useRef<string | null>(null);
  const request = useRef<AbortController | null>(null);
  const [guard] = useState(() => new ResponseVersion());
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);

  const refresh = useCallback(async (): Promise<boolean> => {
    if (!scope || activeScope.current !== scope) return false;
    const version = guard.begin();
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setSnapshot(current => ({ scope, data: current?.scope === scope ? current.data : null, loading: true, error: "" }));
    try {
      let data: PageData;
      switch (view) {
        case "pulse":
        case "insights": {
          const [stats, signal] = await Promise.all([loadPoolStats(controller.signal), loadSignalAnalytics(controller.signal)]);
          data = { view, stats, signal };
          break;
        }
        case "accounts": {
          const [stats, accounts] = await Promise.all([loadPoolStats(controller.signal), loadAdminAccounts(controller.signal)]);
          data = { view, stats, accounts };
          break;
        }
        case "models": {
          const catalog = await loadModelCatalog(controller.signal);
          data = { view, models: catalog.models };
          break;
        }
        default:
          throw new Error("This page does not load dashboard data");
      }
      if (!guard.isCurrent(version) || activeScope.current !== scope) return false;
      setSnapshot({ scope, data, loading: false, error: "" });
      return true;
    } catch (cause) {
      if (!guard.isCurrent(version) || activeScope.current !== scope) return false;
      controller.abort();
      setSnapshot({ scope, data: null, loading: false, error: cause instanceof Error ? cause.message : "Unable to refresh page data" });
      return false;
    } finally {
      if (request.current === controller) request.current = null;
    }
  }, [guard, scope, view]);

  useEffect(() => {
    activeScope.current = scope;
    if (scope) {
      void refresh();
      const timer = window.setInterval(() => { void refresh(); }, 30_000);
      return () => {
        window.clearInterval(timer);
        activeScope.current = null;
        guard.invalidate();
        request.current?.abort();
        request.current = null;
      };
    }
    setSnapshot(null);
  }, [guard, refresh, scope]);

  const current = scope && snapshot?.scope === scope ? snapshot : null;
  return {
    managed,
    data: current?.data ?? null,
    loading: managed && (current === null || current.loading),
    error: current?.error ?? "",
    refresh,
  };
}
