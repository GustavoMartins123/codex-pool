<p align="center">
  <img src="logo.png" alt="codex-pool" width="400">
</p>

<h1 align="center">codex-pool</h1>

<p align="center">
  <strong>Pool your accounts. Share with friends. Never swap credentials again.</strong>
</p>

---

> [!NOTE]
> **Fork Notice & Attribution**:
> This repository is a **fork** maintained by [Gustavo Martins](https://github.com/GustavoMartins123) with custom modifications and enhancements based on the original project created by **[Darvell](https://github.com/darvell)** ([darvell/codex-pool](https://github.com/darvell/codex-pool)).
>
> **Key additions and modifications implemented in this fork:**
> - **Native Windows / MSYS2 UCRT64 Support**: Automated build and install scripts (`scripts/windows/build.ps1`, `install.ps1`, `dev_proxy.ps1`, `deploy.ps1`) with automatic local toolchain detection and a complete C runtime shim (`duckdb_windows_shim.go`) resolving emutls and `std::call_once` linkage for DuckDB on modern GCC (GCC 15 and GCC 16+).
> - **Complete Z.ai Account Integration**: Support for `glm-5.3`, `glm-5.3-flash`, and `glm-5.3-flashx`, browser-based OAuth authentication with automated session polling (`/api/pool/accounts/zai/login/init`), 1-click credential import from ZCode Desktop (`~/.zcode/cli/config.json`), and secure multi-attribute account persistence.
> - **Advanced Google Antigravity Management**: Dedicated OAuth login, token re-validation and relogin workflows, quota pacing and reset window tracking, and automatic context compaction.
> - **Expanded Provider Ecosystem**: Integration and quota monitoring for OpenCode Go (`opencode_go`), Grok, Xiaomi (MiMo), Kimi, MiniMax, and Adversarial (CyberKimi).
> - **Heterogeneous Context Handoff & Transitions**: Portable conversations using canonical intermediate representation (IR) across disparate providers (Codex, Claude, Antigravity, Z.ai).

---

A reverse proxy that distributes coding-agent sessions across pooled provider accounts. Got three Codex accounts? Five Claude logins? The proxy spreads your usage across all of them automatically - no manual switching, no juggling auth files. Google subscription accounts use the Antigravity sign-in flow; Gemini remains the API-key provider.

The setup dashboard configures **Codex CLI**, **Claude Code**, **Gemini CLI**, **Grok Build**, **Pi**, and **Cute Code**. Grok Build runs through the proxy without its own login and can select the other pool models; Pi merges pool providers into its existing `models.json`.

For browser, mobile, or CLI speech-to-speech agents, see [Realtime voice agents through codex-pool](docs/realtime-voice-agent.md). It uses a pooled ephemeral secret followed by a direct WebRTC session.

<p align="center">
  <img src="screenshots/analytics-dashboard.png" alt="Pool Analytics" width="700">
</p>

---

## Why

You hit rate limits. You have multiple accounts. Swapping credentials is annoying.

Or maybe you want to pool accounts with friends - everyone throws their accounts into the pot, everyone benefits from the combined capacity.

**codex-pool** handles it:
- Distributes sessions across all your accounts for each service
- Routes to whichever account has capacity
- Pins conversations to the same account (ensures standard cached token performance)
- Auto-refreshes tokens before they expire
- Proxies WebSocket upgrades (including Codex Responses WS and realtime `/ws` flows)
- Tracks usage so you can see who's burning through quota

---

## Screenshots

### Setup Dashboard

<p align="center">
  <img src="screenshots/local-mode.png" alt="Local Mode" width="700">
</p>

### Friends Mode
Share your pool with others using a friend code.

<p align="center">
  <img src="screenshots/friends-mode-login.png" alt="Friends Mode" width="500">
</p>

---

## Quick Start

### 1. Add your accounts

```bash
mkdir -p pool/codex pool/claude pool/gemini pool/antigravity pool/zai pool/opencode_go pool/grok pool/xiaomi pool/kimi pool/minimax

# Codex accounts
cp ~/.codex/auth.json pool/codex/work.json
cp ~/backup/.codex/auth.json pool/codex/personal.json

# Claude accounts
cp ~/.claude/credentials.json pool/claude/main.json

# Gemini accounts
cp ~/.gemini/oauth_creds.json pool/gemini/main.json

# Z.ai accounts (or use Dashboard / ZCode Desktop import)
cat > pool/zai/main.json <<'EOF'
{"api_key": "your-zcode-api-key"}
EOF

# OpenCode Go subscription (API key from https://opencode.ai/auth)
cat > pool/opencode_go/main.json <<'EOF'
{"api_key": "sk-..."}
EOF

# Xiaomi MiMo accounts
cat > pool/xiaomi/main.json <<'EOF'
{"api_key": "tp-..."}
EOF

# Kimi accounts
cat > pool/kimi/main.json <<'EOF'
{"api_key": "sk-..."}
EOF

# MiniMax accounts
cat > pool/minimax/main.json <<'EOF'
{"api_key": "..."}
EOF

chmod 600 pool/*/*.json
```

Structure:
```
pool/
├── codex/
│   ├── work.json
│   └── personal.json
├── claude/
│   └── main.json
├── gemini/
│   └── main.json
├── antigravity/
│   └── main.json
├── zai/
│   └── main.json
├── opencode_go/
│   └── main.json
├── grok/
│   └── main.json
├── xiaomi/
│   └── main.json
├── kimi/
│   └── main.json
└── minimax/
    └── main.json
```

### 2. Run it

Builds include the dashboard. Run the binary from a directory where you want
`pool/`, `data/`, and optional `config.toml` to live.

**Linux (Docker with Buildx):**

```bash
./scripts/build.sh amd64
./dist/linux-amd64/codex-pool
```

Use `arm64` for a Linux ARM64 host. To build and install for the current Linux
architecture, run `./scripts/install.sh` (default: `~/.local/bin`); pass a
directory to change the install location.

**Windows (PowerShell, Go 1.25+, Node.js 24+, and MSYS2 UCRT64 GCC):**

```powershell
.\scripts\windows\build.ps1
.\dist\windows-amd64\codex-pool.exe
```

Run `.\scripts\windows\install.ps1` to build and install into
`$env:LOCALAPPDATA\Programs\codex-pool`, or pass `-InstallDir` to choose a
directory. The build scripts automatically detect MSYS2 UCRT64 from workspace-local
toolchains (`.toolchains\msys64\ucrt64\bin`) or standard installation (`C:\msys64\ucrt64\bin`),
or you can pass a custom path with `-CompilerBin`.

Both GCC 15 and modern GCC 16+ are fully supported out-of-the-box thanks to our
built-in DuckDB C++ runtime shim (`duckdb_windows_shim.go`) which bridges emulated
TLS and `std::call_once` linkage.
Windows builds target `amd64`; DuckDB's pinned bindings do not
include a Windows ARM64 target. Installation copies only the executable and
does not change `PATH` or create a service.

For development, use `scripts/dev_proxy.sh` on Linux or
`scripts/windows/dev_proxy.ps1` on Windows after building the dashboard. For
Docker deployment, use `scripts/deploy.sh` or
`scripts/windows/deploy.ps1`, respectively.

### 3. Point your CLI

**Codex** - `~/.codex/config.toml`:
```toml
model_provider = "codex-pool"
chatgpt_base_url = "http://127.0.0.1:8989/backend-api"

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "http://127.0.0.1:8989/v1"
wire_api = "responses"
requires_openai_auth = true
```

**Claude Code**:
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8989"
export ANTHROPIC_API_KEY="pool"
```

**Gemini CLI**:
```bash
export CODE_ASSIST_ENDPOINT="http://127.0.0.1:8989"
```

**Google Antigravity account**: open the dashboard, choose "Contribute an account", then press "Google Antigravity". The popup completes the callback automatically. Pasting the callback URL remains available when popups are blocked.

The sign-in flow uses Antigravity's shipped Google OAuth client and its fixed `http://localhost:51121/oauth-callback` redirect, matching CLIProxyAPI and VibeProxy. When the pool runs on the same machine as the browser, the popup completes on its own. For a remote pool, paste the failed localhost callback URL into the contribution dialog; the state and PKCE verifier are still checked before exchange.

`ANTIGRAVITY_OAUTH_CLIENT_ID`, `ANTIGRAVITY_OAUTH_CLIENT_SECRET`, and `ANTIGRAVITY_OAUTH_REDIRECT_URI` remain available for tests or a separately registered Google OAuth client. `ANTIGRAVITY_CLIENT_VERSION` overrides the Antigravity client version used in upstream requests. `UPSTREAM_ANTIGRAVITY_BASE`, `UPSTREAM_ANTIGRAVITY_DAILY_BASE`, and `UPSTREAM_ANTIGRAVITY_ONBOARD_BASE` override the production, generation, and onboarding Cloud Code Assist hosts.

**Z.ai Individual Coding Plan account**: choose "Z.ai" in "Contribute an account" to sign in via the browser ZCode OAuth flow (with automated session polling), import your existing configuration with a single click from ZCode Desktop (`~/.zcode/cli/config.json`), or enter an API key manually. The pool checks the active Individual Coding Plan and a model request before saving the linked account with full identity metadata in `pool/zai/`. On Linux, files are created with mode `0600`.

---

## Pool Passport

Members sign in with a username or email and may add a passkey. Members and operators can create revocable guest passes whose magic links open the pool directly. Each principal can keep separately labelled client credentials and inspect token usage over time; operators can manage principals, provider accounts, passes, audit events, and analytics health from the Signal Room.

Existing pool-user IDs and credentials migrate into guest principals. During the migration window, the former `friend_code` lets an existing holder choose a username and password; when the browser still has its old setup token, Passport claims the same principal ID and preserves its history. The code never authorizes ordinary API or provider requests. Clear it after migration to disable further account claims while the independently persisted analytics salt keeps historical origin hashes stable.

---

## Configuration

```toml
listen_addr = "127.0.0.1:8989"
pool_dir = "pool"
db_path = "./data/proxy.db"
public_url = "https://pool.example.com"

# Migration-only salt seed. Remove only after Passport has persisted analytics_salt.
friend_code = "former-secret"

[pool_users]
jwt_secret = "32-char-secret-for-existing-tokens"
storage_path = "./data/pool_users.json"
```

Set `POOL_AUTH_ENCRYPTION_KEY` to a stable 32-byte secret (hex or base64) before starting Passport. `ADMIN_TOKEN` remains the break-glass operator credential.

Environment variable `PROXY_MAX_INMEM_BODY_BYTES` controls how large a request body can be before the proxy streams it directly (no retries). Default is 16777216 (16 MiB).

### Smart routing

The default `balanced` policy scores quota headroom, reset timing, health,
TTFT, throughput, conversation affinity, inflight load, and recent failures.
Select another built-in profile per request with
`X-Pool-Routing: fast` or use a model alias such as
`pool/throughput/gpt-5.6`. Available profiles are `balanced`, `fast`,
`throughput`, `quota-saver`, `drain`, `sticky`, and `legacy`.

Profiles and their weights are configurable under `[routing]` and
`[routing.profiles.<name>]`; see `config.toml.example`. Route traces expose
the selected profile, normalized signals, weights, alternatives, and score.

When a conversation changes provider, the pool keeps provider-local IDs
separate, removes incompatible response/session/cache identifiers, rebuilds
portable message and tool history, and compacts normalized context above
100,000 estimated tokens. The response headers `X-Pool-Context-Handoff`,
`X-Pool-Context-Compacted`, and `X-Pool-Context-Warning` describe those
actions when applicable.

### Model capability discovery

Authenticated clients can query `GET /api/pool/models` for the pool's model catalog, current account availability, and provider capabilities. The response includes a `schema_version`; clients should ignore fields they do not understand and treat an unknown schema version as unsupported.

Models with provider-hosted web search advertise both `capabilities.web_search` and a declarative `native_tools.web_search` route:

```json
{
  "id": "grok-4.5",
  "provider": "grok",
  "capabilities": { "web_search": true },
  "native_tools": {
    "web_search": {
      "protocol": "openai-responses",
      "endpoint": "/v1/responses",
      "tool_type": "web_search"
    }
  },
  "available_now": true
}
```

Native-tool endpoints are same-origin relative paths. The `native_tools` map key is also the wire tool name for protocols that require one; `tool_type` is the provider-specific type value. Capability means the model and protocol support the tool, while `available_now` separately reports whether an account can currently be routed. The catalog never includes account credentials.

---

## Credential Formats

**Codex** - `pool/codex/*.json`
```json
{"tokens": {"access_token": "...", "refresh_token": "...", "account_id": "acct_..."}}
```

**Claude** - `pool/claude/*.json`
```json
{"claudeAiOauth": {"accessToken": "...", "refreshToken": "...", "expiresAt": 1234567890000}}
```

**Gemini** - `pool/gemini/*.json`
```json
{"access_token": "ya29...", "refresh_token": "1//...", "expiry_date": 1234567890000}
```

**Antigravity** - `pool/antigravity/*.json`
```json
{"type":"antigravity","access_token":"ya29...","refresh_token":"1//...","email":"person@example.com","project_id":"project-id","expiry_date":1234567890000}
```

**Z.ai** - `pool/zai/*.json`
```json
{"api_key": "...", "auth_type": "oauth", "user_id": "...", "business_token": "...", "zcode_jwt": "...", "plan_type": "coding_plan", "label": "my-account"}
```
*(Or simple `{"api_key": "..."}` for manual keys)*

**OpenCode Go** - `pool/opencode_go/*.json`
```json
{"api_key": "sk-..."}
```

**Grok** - `pool/grok/*.json`
```json
{"access_token": "...", "refresh_token": "...", "plan_type": "grok"}
```

**Xiaomi (MiMo)** - `pool/xiaomi/*.json`
```json
{"api_key": "tp-..."}
```

**Kimi** - `pool/kimi/*.json`
```json
{"api_key": "sk-..."}
```

**MiniMax** - `pool/minimax/*.json`
```json
{"api_key": "..."}
```

OpenCode Go models are namespaced as `opencode-go/<model-id>` (e.g. `opencode-go/longcat-2.0`), matching OpenCode's own config convention. Bare IDs also route to Go unless another provider already claims them (`kimi-k3` is Go-only; bare `mimo-v2.5-pro` stays on Xiaomi, bare `grok-4.6` stays on Grok). Go quota (rolling/weekly/monthly from `GET /zen/go/v1/usage`) is polled every 15 minutes; the weekly window drives routing score. Configure a different endpoint with `UPSTREAM_OPENCODE_GO_BASE`.

Antigravity model names come from Google's live `fetchAvailableModels` response. Use `antigravity/<model-id>` to force this provider. `/api/pool/models`, `/v1/models`, `/v1beta/models`, Pi, Cute Code, and the Codex catalog consume the same registry. Temporary quota exhaustion changes `available_now` without removing a supported model from the catalog.

---

## Credits & Upstream Reference

This project is a fork of the original [codex-pool](https://github.com/darvell/codex-pool) created by **[Darvell](https://github.com/darvell)**. Special thanks and full credit to Darvell for the foundational architecture, reverse proxy design, session multiplexing, and dashboard implementation.

---

## Disclaimer

This pools credentials you own. Using multiple accounts or sharing access may violate terms of service. If something goes sideways, that's on you.

---

## License

MIT
