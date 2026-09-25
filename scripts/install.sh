#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
arch="$(uname -m)"
case "$arch" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "Unsupported Linux architecture: $arch" >&2; exit 2 ;;
esac

install_dir="${1:-$HOME/.local/bin}"
"$root/scripts/build.sh" "$arch"
mkdir -p "$install_dir"
temp="$(mktemp "$install_dir/.codex-pool.XXXXXX")"
trap 'rm -f "$temp"' EXIT
install -m 0755 "$root/dist/linux-$arch/codex-pool" "$temp"
mv -f "$temp" "$install_dir/codex-pool"
printf 'Installed %s\n' "$install_dir/codex-pool"
