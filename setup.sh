#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() { printf '%s\n' "Setup failed: $*" >&2; exit 1; }
[[ $# -le 3 ]] || fail 'Usage: bash setup.sh [env-file] [--monitoring] [--deploy]'
monitoring=false
deploy=false
env_arg=''
for arg in "$@"; do
  case $arg in
    --monitoring) monitoring=true ;;
    --deploy) deploy=true ;;
    --*) fail "Unknown option: $arg" ;;
    *)
      [[ -z $env_arg ]] || fail 'Only one environment path is allowed.'
      env_arg=$arg ;;
  esac
done
(( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4) )) || fail 'Bash 4.4 or newer is required.'
command -v openssl >/dev/null || fail 'OpenSSL is required.'
if $deploy; then
  [[ -z $env_arg ]] || fail '--deploy requires the default .env; Docker Compose reads only the repo-root .env.'
  command -v docker >/dev/null || fail 'Docker is required for --deploy.'
fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_path=${env_arg:-"$script_dir/.env"}
[[ ! -L "$env_path" ]] || fail 'The environment file must not be a symlink.'
[[ ! -e "$env_path" || -f "$env_path" ]] || fail 'The environment path must be a regular file.'
[[ -d "$(dirname -- "$env_path")" ]] || fail 'The destination directory does not exist.'
lock="$env_path.setup-lock"
mkdir -m 700 -- "$lock" 2>/dev/null || fail 'Setup is already running, or its lock remains after an interruption.'
temp=''
cleanup() {
  [[ -z "$temp" ]] || rm -f -- "$temp"
  rmdir -- "$lock"
}
trap cleanup EXIT

source_path="$env_path"
[[ -f "$source_path" ]] || source_path="$script_dir/.env.example"
[[ -f "$source_path" ]] || fail 'The environment template is missing.'
nul_count=$(LC_ALL=C tr -cd '\000' < "$source_path" | wc -c)
(( nul_count == 0 )) || fail 'The environment file contains NUL bytes; use UTF-8 text.'
keys=(ADMIN_TOKEN POOL_AUTH_ENCRYPTION_KEY POOL_JWT_SECRET POOL_CREDENTIAL_KEY)
if $monitoring; then keys+=(MONITORING_METRICS_TOKEN GRAFANA_ADMIN_PASSWORD); fi
declare -A found=() positions=() values=()
lines=()
quoted_double='^"([^"]*)"[[:space:]]*(#.*)?$'
quoted_single="^'([^']*)'[[:space:]]*(#.*)?$"
while IFS= read -r line || [[ -n "$line" ]]; do
  line=${line%$'\r'}
  [[ ${#lines[@]} -ne 0 ]] || line=${line#$'\xef\xbb\xbf'}
  if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY|MONITORING_METRICS_TOKEN|GRAFANA_ADMIN_PASSWORD)[[:space:]]*=(.*)$ ]]; then
    key=${BASH_REMATCH[2]}
    [[ -z ${found[$key]:-} ]] || fail "Duplicate $key assignment."
    found[$key]=1
    positions[$key]=${#lines[@]}
    value=${BASH_REMATCH[3]}
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    if [[ "$value" =~ $quoted_double || "$value" =~ $quoted_single ]]; then
      value=${BASH_REMATCH[1]}
    else
      value=${value%%[[:space:]]#*}
      value="${value%"${value##*[![:space:]]}"}"
    fi
    if [[ -n "$value" ]]; then
      case ${value,,} in
        changeme|change-me|change_me|example|placeholder|replace-me|your-secret-here|your-key-here) fail "Invalid placeholder for $key." ;;
      esac
      minimum=32
      [[ "$key" != ADMIN_TOKEN ]] || minimum=16
      [[ ${#value} -ge $minimum && "$value" != *[\$\<\>\"\'\\]* && ! "$value" =~ [[:cntrl:]] ]] || fail "Invalid $key; edit it explicitly before rerunning setup."
    fi
    values[$key]="$value"
  elif [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY|MONITORING_METRICS_TOKEN|GRAFANA_ADMIN_PASSWORD)([[:space:]:]|$) ]]; then
    fail 'Malformed secret assignment.'
  fi
  lines+=("$line")
done < "$source_path"

generated=0
for key in "${keys[@]}"; do
  [[ -z ${values[$key]:-} ]] || continue
  value=$(openssl rand -hex 32) || fail 'Cryptographic secret generation failed.'
  [[ "$value" =~ ^[[:xdigit:]]{64}$ ]] || fail 'OpenSSL returned invalid key material.'
  if [[ -n ${found[$key]:-} ]]; then
    lines[${positions[$key]}]="$key=$value"
  else
    lines+=("$key=$value")
  fi
  generated=$((generated+1))
done

if [[ ! -f "$env_path" || $generated -gt 0 ]]; then
  temp=$(mktemp -- "$env_path.tmp.XXXXXX")
  printf '%s\n' "${lines[@]}" > "$temp"
  chmod 600 -- "$temp"
  [[ ! -L "$env_path" ]] || fail 'The environment file became a symlink.'
  mv -f -- "$temp" "$env_path"
  temp=''
fi
chmod 600 -- "$env_path"
printf 'Environment ready; generated %d secret(s).\n' "$generated"

if $deploy; then
  for name in pool data; do
    dir="$script_dir/$name"
    if [[ -e $dir ]]; then
      [[ -d $dir ]] || fail "$dir exists and is not a directory."
    else
      mkdir -m 700 -- "$dir" || fail "Could not create $dir."
    fi
  done
  deploy_args=()
  if $monitoring; then deploy_args+=(--monitoring); fi
  bash "$script_dir/scripts/deploy.sh" "${deploy_args[@]}" || fail 'Deploy failed; .env is ready. Fix Docker and rerun bash setup.sh --deploy. See: docker compose logs codex-pool'
  retries=${CODEX_POOL_HEALTH_RETRIES:-60}
  interval=${CODEX_POOL_HEALTH_INTERVAL:-2}
  healthy=false
  for ((attempt = 0; attempt < retries; attempt++)); do
    status=$(docker inspect -f '{{.State.Health.Status}}' codex-pool 2>/dev/null) || status=''
    if [[ $status == healthy ]]; then healthy=true; break; fi
    sleep "$interval"
  done
  $healthy || fail 'The pool did not become healthy in time. See: docker compose logs codex-pool'
  printf 'Pool is running. Next steps:\n'
  printf '  1. Open http://127.0.0.1:8989/ and complete "Set up this pool" to create the operator.\n'
  printf '     The form asks for the admin token; read it with: grep "^ADMIN_TOKEN=" .env\n'
  printf '  2. Add provider accounts with "Add pool account" in the dashboard.\n'
fi
