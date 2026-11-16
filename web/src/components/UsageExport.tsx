import { useState } from "react";
import { downloadUsageExport } from "../api";

export function UsageExport({ scope, hours, principal }: { scope: "console" | "me"; hours: number; principal?: string }) {
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const run = async (format: "csv" | "json") => {
    setBusy(format);
    setError("");
    try {
      await downloadUsageExport(scope, { hours, format, principal });
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Export failed");
    } finally {
      setBusy("");
    }
  };

  return <div className="usage-export">
    <button type="button" className="quiet-button" disabled={Boolean(busy)} onClick={() => run("csv")}>{busy === "csv" ? "Exporting…" : "Export CSV"}</button>
    <button type="button" className="quiet-button" disabled={Boolean(busy)} onClick={() => run("json")}>{busy === "json" ? "Exporting…" : "Export JSON"}</button>
    {error && <small role="alert">{error}</small>}
  </div>;
}