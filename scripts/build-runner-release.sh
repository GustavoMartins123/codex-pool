#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${1:-$root/dist/runner-release}"
rm -rf "$output"
mkdir -p "$output/unpacked"
"$root/scripts/build-runner.sh" "$output/unpacked"

for target in windows-amd64 windows-arm64 linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
  dir="$output/unpacked/$target"
  if [[ "$target" == windows-* ]]; then
    (cd "$dir" && zip -q "$output/runnerd-$target.zip" runnerd.exe)
  else
    tar -C "$dir" -czf "$output/runnerd-$target.tar.gz" runnerd
  fi
done
(cd "$output" && sha256sum runnerd-*.zip runnerd-*.tar.gz > SHA256SUMS)
rm -rf "$output/unpacked"
printf 'Runner release artifacts written to %s\n' "$output"
