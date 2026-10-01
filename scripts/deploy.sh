#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

compose_args=()
services=(codex-pool)
case ${1:-} in
  '') [[ $# -eq 0 ]] || { printf 'Unexpected empty argument.\n' >&2; exit 1; } ;;
  --monitoring)
    [[ $# -eq 1 ]] || { printf 'Usage: scripts/deploy.sh [--monitoring]\n' >&2; exit 1; }
    compose_args=(-f docker-compose.yml -f compose.monitoring.yml)
    services+=(prometheus grafana)
    ;;
  *) printf 'Usage: scripts/deploy.sh [--monitoring]\n' >&2; exit 1 ;;
esac

docker compose "${compose_args[@]}" build codex-pool
docker compose "${compose_args[@]}" up -d "${services[@]}"
