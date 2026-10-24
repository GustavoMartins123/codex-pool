#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
output="$root/bin/runnerd"
if [[ $# -gt 1 ]]; then printf 'Usage: scripts/build-runner.sh [output-directory]\n' >&2; exit 1; fi
if [[ $# -eq 1 ]]; then output="$1"; fi
for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  directory="$output/$target_os-$target_arch"
  filename=runnerd
  if [[ "$target_os" == windows ]]; then filename=runnerd.exe; fi
  mkdir -p "$directory"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags='-s -w' -o "$directory/$filename" ./cmd/runnerd
  printf 'Built runnerd for %s\n' "$target"
done
