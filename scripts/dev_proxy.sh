#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

export PROXY_DEBUG="${PROXY_DEBUG:-1}"
export POOL_DIR="${POOL_DIR:-./pool}"
export PROXY_LISTEN_ADDR="${PROXY_LISTEN_ADDR:-127.0.0.1:8989}"

exec go run .

