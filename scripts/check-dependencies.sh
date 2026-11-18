#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
reports="${1:-tmp/supply-chain}"
mkdir -p "$reports"

status=0
npm --prefix web audit --json > "$reports/npm-audit.json" || true
node scripts/check-npm-audit.mjs "$reports/npm-audit.json" || status=1
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -format=json ./... > "$reports/govulncheck.json" || status=1
go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 mod -json -output "$reports/go-sbom.json" || status=1
(cd web && npm sbom --sbom-format=cyclonedx --package-lock-only) > "$reports/web-sbom.json" || status=1
exit "$status"
