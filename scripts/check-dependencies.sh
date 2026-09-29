#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
reports="${1:-tmp/supply-chain}"
mkdir -p "$reports"

npm --prefix web audit --audit-level=low --json > "$reports/npm-audit.json"
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -format=json ./... > "$reports/govulncheck.json"
go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 mod -json -output "$reports/go-sbom.json"
(cd web && npm sbom --sbom-format=cyclonedx --package-lock-only) > "$reports/web-sbom.json"
