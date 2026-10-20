import type { ModelCompatibility as Verification } from "../types";
import { compatibilityChecks } from "../compatibility";

export function ModelCompatibility({ verification }: { verification?: Verification }) {
  const expired = verification && (verification.expired || Date.now() - Date.parse(verification.finished_at) > 7 * 86400_000);
  return <section className="model-compatibility" aria-label="Verified compatibility">
    <strong>Verified compatibility</strong>
    <div className="compatibility-checks">{(Object.entries(compatibilityChecks) as [keyof typeof compatibilityChecks, string][]).map(([key, label]) => {
      const check = verification?.checks[key];
      const status = !check || check.status === "not_tested" ? "not_tested" : expired ? "expired" : check.status;
      const name = { passed: "Passed", failed: "Failed", not_tested: "Not tested", expired: "Expired" }[status];
      return <span key={key} className={`compatibility-check ${status}`}><span>{label}</span><b>{name}</b></span>;
    })}</div>
    {verification ? <small>Tested {new Date(verification.finished_at).toLocaleString()} · {verification.codex_version === "unavailable" ? "Codex client unavailable" : verification.codex_version}<br />{expired ? "Verification expired or the server changed. Run the checks again." : "Operator-attested results from synthetic checks in this pool. Individual accounts may differ."}</small>
      : <small>No test results registered for this model.</small>}
  </section>;
}
