import type { ModelCompatibility } from "./types";

export const compatibilityChecks = { text: "Text", streaming: "Streaming", tools: "Tools", codex_edit: "Codex editing" } as const;
const passedCodes = { text: "exact_response", streaming: "completed_stream", tools: "tool_round_trip", codex_edit: "file_edit_verified" } as const;

export function validCompatibility(value: unknown, model: string, provider: string): value is ModelCompatibility {
  if (!value || typeof value !== "object") return false;
  const v = value as ModelCompatibility;
  return v.suite === "codex-pool/1" && v.model === model && v.provider === provider && typeof v.runtime === "string" && /^[a-f0-9]{64}$/.test(v.runtime) &&
    typeof v.codex_version === "string" && /^[a-zA-Z0-9 ._+()/-]{1,80}$/.test(v.codex_version) && typeof v.expired === "boolean" &&
    Number.isFinite(Date.parse(v.started_at)) && Number.isFinite(Date.parse(v.finished_at)) && Date.parse(v.finished_at) >= Date.parse(v.started_at) &&
    !!v.checks && Object.keys(v.checks).length === 4 && (Object.keys(compatibilityChecks) as (keyof typeof compatibilityChecks)[]).every(name => {
      const check = v.checks[name];
      return check && (check.status === "passed" && check.code === passedCodes[name] || check.status === "not_tested" && check.code === "not_run" ||
        check.status === "failed" && ["request_failed", "unexpected_response", "client_failed", "timeout"].includes(check.code));
    }) && !(v.checks.codex_edit.status === "passed" && v.codex_version === "unavailable");
}
