#!/usr/bin/env bash
set -euo pipefail
umask 077

fail() { printf '%s\n' "Setup failed: $*" >&2; exit 1; }
[[ $# -le 1 ]] || fail 'Usage: bash setup.sh [env-file]'
(( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4) )) || fail 'Bash 4.4 or newer is required.'
command -v openssl >/dev/null || fail 'OpenSSL is required.'
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_path=${1:-"$script_dir/.env"}
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
declare -A found=() positions=() values=()
lines=()
quoted_double='^"([^"]*)"[[:space:]]*(#.*)?$'
quoted_single="^'([^']*)'[[:space:]]*(#.*)?$"
while IFS= read -r line || [[ -n "$line" ]]; do
  line=${line%$'\r'}
  [[ ${#lines[@]} -ne 0 ]] || line=${line#$'\xef\xbb\xbf'}
  if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY)[[:space:]]*=(.*)$ ]]; then
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
  elif [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?(ADMIN_TOKEN|POOL_AUTH_ENCRYPTION_KEY|POOL_JWT_SECRET|POOL_CREDENTIAL_KEY)([[:space:]:]|$) ]]; then
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
