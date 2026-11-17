#!/usr/bin/env bash
# Local mock stack for manual validation: a fake OpenAI-compatible upstream, a
# TLS bridge in front of the container pool, and live fake-agent runners.
#
#   bash scripts/mock-stack.sh [up]      bring everything up (idempotent)
#   bash scripts/mock-stack.sh down      stop only the host processes we started
#   bash scripts/mock-stack.sh status    container, ports and process liveness
#   bash scripts/mock-stack.sh pair      (re)pair the operator runner
#   bash scripts/mock-stack.sh turn ...  forward to scripts/mock/turn.mjs turn
#   bash scripts/mock-stack.sh smoke [items|conversations|runners|approve]
#
# Flags: --reset (wipe ./data, ./pool and the state dir; mock instance only),
#        --no-runner, --member-runner, --auto-approve, --smoke,
#        --pair-timeout SECONDS (default 300), --help.
#
# Requires .env from scripts/setup.sh (ADMIN_TOKEN). Pairing is approved by a
# human at /?view=runners; the pair link carries the command key and is never
# printed. Runtime state, secrets, certs and logs live in tmp/mock-stack/
# (0600, gitignored). Container deploys go through scripts/deploy.sh only, and
# `down` leaves the container running.
set -euo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
state="${MOCK_STACK_DIR:-$root/tmp/mock-stack}"
origin="${MOCK_STACK_ORIGIN:-http://127.0.0.1:8989}"
export MOCK_STACK_DIR="$state" MOCK_STACK_ORIGIN="$origin"
pids="$state/pids"
logs="$state/logs"

command=up
reset=false
with_runner=true
member_runner=false
auto_approve=false
run_smoke=false
pair_timeout=300
forward=()

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

while [[ $# -gt 0 ]]; do
  case $1 in
    up|down|status|smoke|turn|pair) command=$1; shift ;;
    --reset) reset=true; shift ;;
    --no-runner) with_runner=false; shift ;;
    --member-runner) member_runner=true; shift ;;
    --auto-approve) auto_approve=true; shift ;;
    --smoke) run_smoke=true; shift ;;
    --pair-timeout) [[ $# -ge 2 ]] || die "--pair-timeout needs a value"; pair_timeout=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *)
      if [[ $command == turn || $command == smoke ]]; then forward+=("$1"); shift
      else printf 'Unknown argument: %s\n' "$1" >&2; usage >&2; exit 1; fi ;;
  esac
done
[[ $pair_timeout =~ ^[0-9]+$ && $pair_timeout -gt 0 ]] || die "--pair-timeout needs a positive number of seconds"
trap 'printf "Failed at line %s; logs in %s\n" "$LINENO" "$logs" >&2' ERR

json_field() { # file key
  node -e '
    const fs = require("fs");
    try {
      const value = JSON.parse(fs.readFileSync(process.argv[1], "utf8"))[process.argv[2]];
      if (value !== undefined && value !== null) console.log(value);
    } catch {}
  ' "$1" "$2"
}

pid_of() { [[ -f "$pids/$1.pid" ]] && cat "$pids/$1.pid" || true; }
alive() { local pid; pid="$(pid_of "$1")"; [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; }

wait_until() { # label timeout_seconds command...
  local label=$1 timeout=$2
  shift 2
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  printf 'Timed out after %ss waiting for %s.\n' "$timeout" "$label" >&2
  return 1
}

start_bg() { # name command...
  local name=$1
  shift
  mkdir -p "$pids" "$logs"
  if command -v setsid >/dev/null 2>&1; then
    setsid "$@" >"$logs/$name.log" 2>&1 &
  else
    "$@" >"$logs/$name.log" 2>&1 &
  fi
  printf '%s\n' "$!" >"$pids/$name.pid"
  wait_until "$name to stay up" 5 alive "$name" || die "$name exited immediately; see $logs/$name.log"
}

stop_bg() { # name
  local pid
  pid="$(pid_of "$1")"
  if [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    local deadline=$((SECONDS + 8))
    while kill -0 "$pid" 2>/dev/null && (( SECONDS < deadline )); do sleep 0.2; done
    if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid" 2>/dev/null || true; fi
    log "stopped $1 (pid $pid)"
  fi
  rm -f "$pids/$1.pid"
}

listens() { # host port
  node -e '
    const net = require("net");
    const socket = net.connect({ host: process.argv[1], port: Number(process.argv[2]) });
    socket.setTimeout(1500);
    socket.on("connect", () => { socket.destroy(); process.exit(0); });
    socket.on("error", () => process.exit(1));
    socket.on("timeout", () => { socket.destroy(); process.exit(1); });
  ' "$1" "$2"
}

free_port() {
  node -e '
    const net = require("net");
    const server = net.createServer();
    server.once("error", () => process.exit(1));
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      server.close(() => console.log(port));
    });
  '
}

http_ok() { node -e 'fetch(process.argv[1]).then((r) => process.exit(r.ok ? 0 : 1), () => process.exit(1));' "$1"; }
tls_ok() { node -e '
  const https = require("https");
  const fs = require("fs");
  const req = https.get(process.argv[1], { ca: fs.readFileSync(process.argv[2]) }, (res) => {
    res.resume();
    process.exit(res.statusCode === 200 ? 0 : 1);
  });
  req.on("error", () => process.exit(1));
' "$1" "$2"; }

container_exists() { docker inspect codex-pool >/dev/null 2>&1; }
container_health() { [[ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' codex-pool 2>/dev/null)" == healthy ]]; }

# A stopped container has no endpoint gateway and docker renders that as the
# literal "invalid IP"; the network's IPAM gateway is stable in every state.
gateway_ip() {
  local network
  network="$(docker inspect -f '{{range $name, $net := .NetworkSettings.Networks}}{{$name}}{{"\n"}}{{end}}' codex-pool 2>/dev/null | awk 'NF { print; exit }')"
  [[ -n $network ]] || return 0
  docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{"\n"}}{{end}}' "$network" 2>/dev/null \
    | awk '/^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }'
}

pool_target() { node -e 'const u = new URL(process.argv[1]); console.log(`${u.hostname}:${u.port || (u.protocol === "https:" ? 443 : 80)}`);' "$origin"; }

# The pair log holds a QR code and a codexpool:// link embedding the command
# key; only these status lines are ever shown.
pair_excerpt() {
  sed -e 's#codexpool://[^[:space:]]*#[pair link redacted]#g' "$1" \
    | grep -aE '^(Machine code:|Expires:|Paired runner |Pool |Pairing attempt ended:|Finishing the pairing)' || true
}

write_launch() { # file runner_state name workspaces
  node -e '
    const fs = require("fs");
    const [file, runnerState, poolURL, name, workspaces, codex, allowAuto] = process.argv.slice(1);
    const launch = {
      state: runnerState, pool_url: poolURL, name, workspaces, codex,
      allow_paths: [], labels: ["mock", "live"], allow_auto: allowAuto === "true",
    };
    fs.writeFileSync(file, `${JSON.stringify(launch, null, 2)}\n`, { mode: 0o600 });
  ' "$1" "$2" "$3" "$4" "$5" "$state/bin/fake-agent" "$auto_approve"
}

runner_online() { # who timeout_seconds
  node --input-type=module -e '
    import fs from "node:fs";
    import { stackContext, appToken, api, readSecret, readJSON, waitFor, writeSecret } from "./scripts/mock/lib.mjs";
    const [who, timeout] = process.argv.slice(1);
    const ctx = stackContext();
    const runner = readJSON(ctx.file(who === "member" ? "runner-member-state.json" : "runner-state.json"), null);
    if (!runner?.runner_id) throw new Error(`no paired runner state for ${who}`);
    const tokenFile = ctx.file(`${who}-app-token.txt`);
    const password = readSecret(ctx.file(`${who}-password.txt`));
    let token = fs.existsSync(tokenFile) ? readSecret(tokenFile) : null;
    const list = async () => {
      if (!token) {
        token = await appToken(ctx.origin, ctx.credentials[who].email, password, { ca: ctx.ca });
        writeSecret(tokenFile, token);
      }
      try {
        return await api(ctx.origin, "GET", "/api/me/runners", { bearer: token, ca: ctx.ca, expect: [200] });
      } catch {
        token = null;
        return null;
      }
    };
    await waitFor(async () => {
      const res = await list();
      return (res?.json?.runners ?? []).some((r) => r.id === runner.runner_id && r.online);
    }, { label: `${who} runner online`, timeout: Number(timeout) * 1000, interval: 1000 });
    console.log(`${who} runner ${runner.runner_id} is online`);
  ' "$1" "$2"
}

preflight() {
  need docker; need go; need node; need openssl
  docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
  [[ -f "$root/.env" ]] || die ".env is missing; run scripts/setup.sh first"
  node -e '
    const fs = require("fs");
    const env = fs.readFileSync(".env", "utf8");
    if (!/^ADMIN_TOKEN=.+$/m.test(env)) { console.error("ADMIN_TOKEN is not set in .env"); process.exit(1); }
  ' || die ".env has no ADMIN_TOKEN; run scripts/setup.sh first"
  mkdir -p "$state/bin" "$pids" "$logs" "$state/tls" "$state/workspaces"
}

build_binaries() {
  log "building runnerd and the fake agent (host build, host processes only)"
  go build -o "$state/bin/runnerd" ./cmd/runnerd
  go test -c -o "$state/bin/fake-agent" ./internal/runner
  chmod 0700 "$state/bin/runnerd" "$state/bin/fake-agent"
}

resolve_gateway() {
  gateway="$(gateway_ip || true)"
  if [[ -z $gateway ]]; then
    log "no pool container yet; deploying once so compose creates its network"
    bash "$root/scripts/deploy.sh"
    gateway="$(gateway_ip || true)"
    # That first boot creates ./data, which would make setup.mjs keep an empty
    # roster instead of seeding the fake accounts.
    if [[ $had_data == false ]]; then
      docker compose stop codex-pool >/dev/null
      rm -rf "$root/data"
      mkdir -p "$root/data"
    fi
  fi
  [[ -n $gateway ]] || die "could not discover the docker gateway IP for codex-pool"
}

resolve_ports() {
  local saved_up saved_bridge
  saved_up="$(json_field "$state/ports.json" upstream)"
  saved_bridge="$(json_field "$state/ports.json" bridge)"
  upstream_port=$saved_up
  bridge_port=$saved_bridge
  if [[ -z $upstream_port ]] || { ! alive upstream && listens "$gateway" "$upstream_port"; }; then
    upstream_port="$(free_port)"
  fi
  local pool_host pool_port
  pool_host="$(pool_target)"; pool_port="${pool_host##*:}"; pool_host="${pool_host%:*}"
  if [[ -z $bridge_port ]] || { ! alive bridge && listens "$pool_host" "$bridge_port"; }; then
    bridge_port="$(free_port)"
  fi
  node -e '
    const fs = require("fs");
    const [file, upstream, bridge] = process.argv.slice(1);
    fs.writeFileSync(file, `${JSON.stringify({ upstream: Number(upstream), bridge: Number(bridge) }, null, 2)}\n`, { mode: 0o600 });
  ' "$state/ports.json" "$upstream_port" "$bridge_port"
  bridge_url="https://127.0.0.1:$bridge_port"
  upstream_url="http://$gateway:$upstream_port/v1"
}

configure_and_deploy() {
  log "configuring the mock provider (upstream $upstream_url)"
  node scripts/mock/setup.mjs --configure --upstream-url "$upstream_url"
  local before after
  before="$(cat "$state/config.sha" 2>/dev/null || true)"
  after="$(sha256sum "$root/pool/config.toml" "$root/.env" | sha256sum | cut -d' ' -f1)"
  log "deploying the container through scripts/deploy.sh"
  bash "$root/scripts/deploy.sh"
  if [[ $before != "$after" ]] && container_exists; then
    docker compose restart codex-pool >/dev/null
  fi
  printf '%s\n' "$after" >"$state/config.sha"
  node -e '
    const fs = require("fs");
    const [file, origin, gateway, upstream, bridge] = process.argv.slice(1);
    fs.writeFileSync(file, `${JSON.stringify({ origin, gateway, upstream_port: Number(upstream), bridge_port: Number(bridge) }, null, 2)}\n`, { mode: 0o600 });
  ' "$state/stack.json" "$origin" "$gateway" "$upstream_port" "$bridge_port"
}

start_upstream() {
  if alive upstream; then
    log "upstream already running (pid $(pid_of upstream))"
  else
    log "starting mock upstream on $gateway:$upstream_port"
    start_bg upstream node scripts/mock/upstream.mjs --port "$upstream_port" --bind "$gateway" --model mock-model
  fi
  wait_until "the mock upstream to answer" 30 http_ok "http://$gateway:$upstream_port/v1/models" \
    || die "upstream did not come up; see $logs/upstream.log"
}

start_bridge() {
  local cert="$state/tls/cert.pem" key="$state/tls/key.pem"
  if [[ ! -f $cert || ! -f $key ]]; then
    log "generating a self-signed certificate for the TLS bridge"
    openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
      -keyout "$key" -out "$cert" -subj "/CN=localhost" \
      -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
    chmod 0600 "$key" "$cert"
  fi
  if alive bridge; then
    log "TLS bridge already running (pid $(pid_of bridge))"
  else
    log "starting TLS bridge $bridge_url -> $origin"
    start_bg bridge node scripts/mock/bridge.mjs --cert "$cert" --key "$key" \
      --listen "127.0.0.1:$bridge_port" --target "$(pool_target)"
  fi
  wait_until "the TLS bridge to proxy /healthz" 30 tls_ok "$bridge_url/healthz" "$cert" \
    || die "bridge did not come up; see $logs/bridge.log"
}

wait_pool() {
  wait_until "the pool container to report healthy" 180 container_health \
    || die "container never became healthy; check: docker compose logs codex-pool"
  wait_until "$origin/healthz" 30 http_ok "$origin/healthz" || die "pool /healthz is not answering"
  log "pool is healthy at $origin"
}

pair_runner() { # who
  local who=$1 name launch runner_state workspaces
  if [[ $who == member ]]; then
    name=mock-member-runner
    launch="$state/launch-member.json"
    runner_state="$state/runner-member-state.json"
    workspaces="$state/member-runner/workspaces"
  else
    name=mock-runner
    launch="$state/launch.json"
    runner_state="$state/runner-state.json"
    workspaces="$state/workspaces"
  fi
  mkdir -p "$workspaces"
  write_launch "$launch" "$runner_state" "$bridge_url" "$name" "$workspaces"
  if alive "$who-runner"; then
    log "$who runner already running (pid $(pid_of "$who-runner"))"
    return 0
  fi
  if [[ ! -f $runner_state ]]; then
    log "pairing $name"
    start_bg "$who-pair" env -u OPENAI_API_KEY -u CLAUDE_CODE_OAUTH_TOKEN SSL_CERT_FILE="$state/tls/cert.pem" \
      "$state/bin/runnerd" pair --config "$launch"
    wait_until "the pairing code" 30 grep -aq '^Machine code: ' "$logs/$who-pair.log" \
      || { pair_excerpt "$logs/$who-pair.log"; die "runnerd did not print a pairing code"; }
    local code
    code="$(sed -n 's/^Machine code: //p' "$logs/$who-pair.log" | head -1)"
    printf '\n'
    log "approve the pairing in the web UI as $who"
    printf '    open %s/?view=runners\n' "$origin"
    printf '    pending card %s -> Approve, machine code %s\n' "$name" "$code"
    printf '    waiting up to %ss (the pair link stays in %s, never printed)\n\n' "$pair_timeout" "$logs/$who-pair.log"
    local deadline=$((SECONDS + pair_timeout))
    while [[ ! -f $runner_state ]]; do
      if ! alive "$who-pair"; then
        pair_excerpt "$logs/$who-pair.log"
        die "pairing ended before $who approved it; re-run: bash scripts/mock-stack.sh pair"
      fi
      (( SECONDS < deadline )) || die "pairing timed out after ${pair_timeout}s; re-run: bash scripts/mock-stack.sh pair"
      sleep 1
    done
    pair_excerpt "$logs/$who-pair.log"
    stop_bg "$who-pair"
  fi
  log "starting $name"
  start_bg "$who-runner" env -u OPENAI_API_KEY -u CLAUDE_CODE_OAUTH_TOKEN SSL_CERT_FILE="$state/tls/cert.pem" \
    "$state/bin/runnerd" run --config "$launch"
  runner_online "$who" 60 || die "$who runner never came online; see $logs/$who-runner.log"
}

run_smoke_suites() {
  [[ -f "$root/web/mock-stack.browser.mjs" ]] || die "web/mock-stack.browser.mjs is missing"
  [[ -n ${CHROME_PATH:-} ]] || die "smoke needs CHROME_PATH pointing at a Chrome/Chromium binary"
  local suites=("${forward[@]}")
  if [[ ${#suites[@]} -eq 0 ]]; then suites=(items conversations runners approve); fi
  node web/mock-stack.browser.mjs "${suites[@]}"
}

summary() {
  printf '\n'
  log "mock stack ready"
  printf '    pool          %s (container codex-pool)\n' "$origin"
  printf '    TLS bridge    %s -> %s\n' "$bridge_url" "$origin"
  printf '    upstream      %s\n' "$upstream_url"
  printf '    state dir     %s (credentials.json holds the logins)\n' "$state"
  printf '    logs          %s\n' "$logs"
  printf '    stop hosts    bash scripts/mock-stack.sh down (container stays up)\n'
  printf '    drive a turn  bash scripts/mock-stack.sh turn --who operator --action hold --input "Run the deploy script."\n'
  printf '    browser smoke CHROME_PATH=... bash scripts/mock-stack.sh smoke\n'
}

reset_stack() {
  log "--reset: wiping the mock instance (./data, ./pool, $state)"
  local name
  for name in operator-runner member-runner operator-pair member-pair bridge upstream; do stop_bg "$name"; done
  if container_exists; then docker compose stop codex-pool >/dev/null; fi
  rm -rf "$root/data" "$root/pool" "$state" \
    || die "could not wipe ./data, ./pool or $state; remove them and re-run"
  # Docker creates a missing bind source as root, and the pool runs as an
  # unprivileged user that could not write into it.
  mkdir -p "$root/data"
  mkdir -p "$state/bin" "$pids" "$logs" "$state/tls" "$state/workspaces"
}

case $command in
  down)
    mkdir -p "$pids"
    for name in operator-runner member-runner operator-pair member-pair bridge upstream; do stop_bg "$name"; done
    log "host processes stopped; the codex-pool container is still up"
    ;;
  status)
    printf 'pool container: %s\n' "$(container_exists && docker inspect -f '{{.State.Status}} ({{if .State.Health}}{{.State.Health.Status}}{{else}}no healthcheck{{end}})' codex-pool || echo absent)"
    printf 'origin:         %s\n' "$origin"
    if [[ -f $state/ports.json ]]; then
      printf 'upstream port:  %s\n' "$(json_field "$state/ports.json" upstream)"
      printf 'bridge port:    %s\n' "$(json_field "$state/ports.json" bridge)"
    fi
    for name in upstream bridge operator-runner member-runner; do
      if alive "$name"; then printf '%-15s running (pid %s)\n' "$name" "$(pid_of "$name")"
      else printf '%-15s stopped\n' "$name"; fi
    done
    ;;
  smoke)
    run_smoke_suites
    ;;
  turn)
    node scripts/mock/turn.mjs turn "${forward[@]}"
    ;;
  pair)
    preflight
    [[ -f $state/ports.json ]] || die "no stack yet; run: bash scripts/mock-stack.sh up"
    bridge_port="$(json_field "$state/ports.json" bridge)"
    bridge_url="https://127.0.0.1:$bridge_port"
    pair_runner operator
    if [[ $member_runner == true ]]; then pair_runner member; fi
    ;;
  up)
    preflight
    if [[ $reset == true ]]; then reset_stack; fi
    if command -v flock >/dev/null 2>&1; then
      exec 9>"$state/lock"
      flock -n 9 || die "another mock-stack.sh run holds $state/lock"
    fi
    had_data=false
    if [[ -d $root/data && -n "$(ls -A "$root/data" 2>/dev/null)" ]]; then had_data=true; fi
    build_binaries
    resolve_gateway
    resolve_ports
    configure_and_deploy
    start_upstream
    start_bridge
    wait_pool
    log "provisioning principals, policies, grants and traffic"
    node scripts/mock/setup.mjs
    if [[ $with_runner == true ]]; then
      pair_runner operator
      if [[ $member_runner == true ]]; then pair_runner member; fi
      log "seeding conversations"
      node scripts/mock/turn.mjs seed
    else
      log "--no-runner: skipping pairing and conversation seeding"
    fi
    node scripts/mock/setup.mjs --check
    if [[ $run_smoke == true ]]; then run_smoke_suites; fi
    summary
    ;;
esac
