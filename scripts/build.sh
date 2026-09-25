#!/usr/bin/env bash
set -euo pipefail

arch="${1:-amd64}"
case "$arch" in
  amd64|arm64) ;;
  *) echo "Unsupported Linux architecture: $arch" >&2; exit 2 ;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="$root/dist/linux-$arch"
mkdir -p "$output"
rm -f "$output/codex-pool"

docker buildx build \
  --platform "linux/$arch" \
  --target binary \
  --output "type=local,dest=$output" \
  "$root"

test -x "$output/codex-pool"
printf 'Built %s\n' "$output/codex-pool"
