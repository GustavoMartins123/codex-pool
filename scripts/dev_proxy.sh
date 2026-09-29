#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

export PROXY_DEBUG="${PROXY_DEBUG:-1}"
export POOL_DIR="${POOL_DIR:-./pool}"
export PROXY_LISTEN_ADDR="${PROXY_LISTEN_ADDR:-127.0.0.1:8989}"

# The vault is mandatory: default to a stable per-workspace dev key so local
# pool files stay decryptable across restarts. Delete .dev-vault-key to
# rotate (existing files then need POOL_CREDENTIAL_KEY_PREVIOUS).
if [ -z "${POOL_CREDENTIAL_KEY:-}" ]; then
    key_file="$root/.dev-vault-key"
    if [ ! -s "$key_file" ]; then
        umask 077
        openssl rand -hex 32 > "$key_file"
        echo "generated dev vault key: $key_file"
    fi
    export POOL_CREDENTIAL_KEY="$(cat "$key_file")"
fi

exec go run .

