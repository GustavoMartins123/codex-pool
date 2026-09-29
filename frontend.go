package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/cute_code_landing.html templates/og-image.png templates/og-image-transparent.webp templates/favicon.png
var friendContent embed.FS

//go:embed web/dist/index.html web/dist/assets/*
var signalRoomContent embed.FS

func (h *proxyHandler) serveFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := friendContent.ReadFile("templates/favicon.png")
	if err != nil {
		http.Error(w, "favicon not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

func (h *proxyHandler) servePassportSPA(w http.ResponseWriter, r *http.Request) {
	data, err := signalRoomContent.ReadFile("web/dist/index.html")
	if err != nil {
		http.Error(w, "internal error: signal room missing", http.StatusInternalServerError)
		return
	}
	noStore(w)
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (h *proxyHandler) serveSignalRoomAsset(w http.ResponseWriter, r *http.Request) {
	assetPath := strings.TrimPrefix(r.URL.Path, "/")
	data, err := signalRoomContent.ReadFile("web/dist/" + assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if contentType := mime.TypeByExtension(filepath.Ext(assetPath)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func (h *proxyHandler) serveOGImage(w http.ResponseWriter, r *http.Request) {
	data, err := friendContent.ReadFile("templates/og-image.png")
	if err != nil {
		http.Error(w, "image not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func (h *proxyHandler) serveHeroImage(w http.ResponseWriter, r *http.Request) {
	data, err := friendContent.ReadFile("templates/og-image-transparent.webp")
	if err != nil {
		http.Error(w, "image not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func (h *proxyHandler) getEffectivePublicURL(r *http.Request) string {
	if u := getPublicURL(); u != "" {
		return u
	}
	// Infer from request
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost:8989"
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func wantsPowerShell(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("shell"))) {
	case "powershell", "pwsh", "ps", "ps1":
		return true
	default:
		return false
	}
}

func (h *proxyHandler) serveCuteCodeLanding(w http.ResponseWriter, r *http.Request) {
	data, err := friendContent.ReadFile("templates/cute_code_landing.html")
	if err != nil {
		http.Error(w, "internal error: template missing", http.StatusInternalServerError)
		return
	}
	tmpl, err := template.New("cute-code").Parse(string(data))
	if err != nil {
		http.Error(w, "internal error: template parse failed", http.StatusInternalServerError)
		return
	}
	publicURL := h.getEffectivePublicURL(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_ = tmpl.Execute(w, map[string]string{"PublicURL": publicURL})
}

func (h *proxyHandler) generateCuteCodeSettingsForToken(token string, r *http.Request) ([]byte, error) {
	secret := getPoolJWTSecret()
	if secret == "" {
		return nil, fmt.Errorf("JWT secret not configured")
	}
	if h.passport == nil {
		return nil, fmt.Errorf("passport not configured")
	}
	var user *PoolUser
	if client := h.passport.redeemConfigDownloadNonce(token); client != nil {
		user = passportClientAsPoolUser(h, client)
	}
	if user == nil {
		return nil, fmt.Errorf("invalid token")
	}
	claudeAuth, err := generateClaudeAuth(secret, user)
	if err != nil {
		return nil, err
	}
	return generateCuteCodeSettingsJSON(h.getEffectivePublicURL(r), claudeAuth.AccessToken)
}

func (h *proxyHandler) serveCuteCodeSettingsConfig(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	token := strings.TrimPrefix(r.URL.Path, "/config/cute-code/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}
	settingsJSON, err := h.generateCuteCodeSettingsForToken(token, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(settingsJSON)
}

func (h *proxyHandler) serveCuteCodeSetupScript(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	token := strings.TrimPrefix(r.URL.Path, "/setup/cute-code/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}
	if h.passport == nil {
		http.Error(w, "passport not configured", http.StatusServiceUnavailable)
		return
	}
	// Peek only: the nonce stays valid for the config fetch inside the script.
	if h.passport.peekConfigDownloadNonce(token) == nil {
		http.Error(w, "invalid token", http.StatusNotFound)
		return
	}
	publicURL := h.getEffectivePublicURL(r)

	if wantsPowerShell(r) {
		script := fmt.Sprintf(`#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$ConfigUrl = '%s/config/cute-code/%s'

function Set-Utf8NoBom {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Value, $utf8)
}

Write-Host 'Installing cute-code...'
try {
  Invoke-Expression (Invoke-RestMethod 'https://git.irrigate.cc/pp/cute-code/raw/branch/main/install.ps1')
} catch {
  Write-Host "Warning: cute-code installer failed or was already installed: $_"
}

Write-Host 'Configuring cute-code for Codex Pool...'
$claudeDir = $env:CLAUDE_CONFIG_DIR
if ([string]::IsNullOrWhiteSpace($claudeDir)) { $claudeDir = Join-Path $HOME '.claude' }
New-Item -ItemType Directory -Force -Path $claudeDir | Out-Null
$settingsFile = Join-Path $claudeDir 'settings.json'

$incoming = Invoke-RestMethod -Uri $ConfigUrl
$settings = $null
try { $settings = Get-Content -Path $settingsFile -Raw | ConvertFrom-Json } catch {}
if ($null -eq $settings) { $settings = New-Object PSObject }

foreach ($prop in $incoming.PSObject.Properties) {
  $settings | Add-Member -MemberType NoteProperty -Name $prop.Name -Value $prop.Value -Force
}

Set-Utf8NoBom -Path $settingsFile -Value ($settings | ConvertTo-Json -Depth 20)
Write-Host "cute-code pool settings saved to $settingsFile"
Write-Host 'Run: cute-code --model gpt-6-astra'
`, publicURL, token)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
		return
	}

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
CONFIG_URL="%s/config/cute-code/%s"
CLAUDE_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
SETTINGS_FILE="$CLAUDE_DIR/settings.json"
TMP_FILE="$(mktemp)"

mkdir -p "$CLAUDE_DIR"

printf 'Installing cute-code...\n'
if command -v curl >/dev/null 2>&1; then
    curl -fsSL https://git.irrigate.cc/pp/cute-code/raw/branch/main/install.sh | bash || true
fi

curl -fsSL "$CONFIG_URL" -o "$TMP_FILE"

if command -v node >/dev/null 2>&1; then
    SETTINGS_FILE="$SETTINGS_FILE" TMP_FILE="$TMP_FILE" node << 'NODE_SCRIPT'
const fs = require('fs');
const settingsFile = process.env.SETTINGS_FILE;
const tmpFile = process.env.TMP_FILE;
let settings = {};
let incoming = {};
try { settings = JSON.parse(fs.readFileSync(settingsFile, 'utf8')); } catch {}
incoming = JSON.parse(fs.readFileSync(tmpFile, 'utf8'));
settings = { ...settings, ...incoming };
fs.writeFileSync(settingsFile, JSON.stringify(settings, null, 2) + '\n');
NODE_SCRIPT
elif command -v python3 >/dev/null 2>&1; then
    SETTINGS_FILE="$SETTINGS_FILE" TMP_FILE="$TMP_FILE" python3 << 'PYTHON_SCRIPT'
import json, os
settings_file = os.environ['SETTINGS_FILE']
tmp_file = os.environ['TMP_FILE']
try:
    with open(settings_file) as f: settings = json.load(f)
except Exception:
    settings = {}
with open(tmp_file) as f: incoming = json.load(f)
settings.update(incoming)
with open(settings_file, 'w') as f:
    json.dump(settings, f, indent=2)
    f.write('\n')
PYTHON_SCRIPT
else
    cp "$SETTINGS_FILE" "$SETTINGS_FILE.bak" 2>/dev/null || true
    cp "$TMP_FILE" "$SETTINGS_FILE"
fi

rm -f "$TMP_FILE"
printf 'cute-code pool settings saved to %%s\n' "$SETTINGS_FILE"
printf 'Run: cute-code --model gpt-6-astra\n'
`, publicURL, token)
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(script))
}

func (h *proxyHandler) serveCodexSetupScript(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	token := strings.TrimPrefix(r.URL.Path, "/setup/codex/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}
	publicURL := h.getEffectivePublicURL(r)

	if wantsPowerShell(r) {
		script := fmt.Sprintf(`#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Token = '%s'
$BaseUrl = '%s'

$authDir = Join-Path $HOME '.codex'
$configFile = Join-Path $authDir 'config.toml'
$authFile = Join-Path $authDir 'auth.json'
$modelCatalog = Join-Path $authDir 'model_catalog.json'
$mcpScript = Join-Path $authDir 'model_sync.ps1'
$restoreScript = Join-Path $authDir 'restore-config.ps1'
$Stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$nl = [Environment]::NewLine

# PS 5.1 compat wrapper: ConvertFrom-Json -Depth was added in PS 6
function ConvertFrom-JsonCompat {
  param([Parameter(ValueFromPipeline)]$InputObject)
  process {
    if ($PSVersionTable.PSVersion.Major -ge 6) {
      $InputObject | ConvertFrom-Json -Depth 20
    } else {
      $InputObject | ConvertFrom-Json
    }
  }
}

# PS 5.1 writes UTF-8 with BOM which breaks JSON/TOML parsers. Write without BOM.
function Set-Utf8NoBom {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Value, $utf8)
}

# Safe property check that works with Set-StrictMode -Version Latest
function Has-Property {
  param($Obj, [string]$Name)
  if ($null -eq $Obj) { return $false }
  if ($Obj -is [System.Collections.IDictionary]) { return $Obj.ContainsKey($Name) }
  return [bool]($Obj.PSObject.Properties.Name -contains $Name)
}

Write-Host 'Initializing Codex Pool setup...'
New-Item -ItemType Directory -Path $authDir -Force | Out-Null

Write-Host '1. Fetching credentials...'
$authUrl = "$BaseUrl/config/codex/$Token"
if (Test-Path $authFile) {
  Copy-Item -LiteralPath $authFile -Destination "$authFile.bak.$Stamp" -Force | Out-Null
}
if ($PSVersionTable.PSEdition -eq 'Desktop') {
  $authContent = (Invoke-WebRequest -UseBasicParsing -Uri $authUrl).Content
} else {
  $authContent = (Invoke-WebRequest -Uri $authUrl).Content
}
Set-Utf8NoBom -Path $authFile -Value $authContent

Write-Host '2. Fetching model catalog...'
try {
  $raw = Get-Content -Path $authFile -Raw
  $auth = $raw | ConvertFrom-JsonCompat
  $accessToken = $null
  if ($auth -and (Has-Property $auth 'tokens') -and (Has-Property $auth.tokens 'access_token')) {
    $accessToken = [string]$auth.tokens.access_token
  } elseif ($auth -and (Has-Property $auth 'access_token')) {
    $accessToken = [string]$auth.access_token
  }
  if (-not [string]::IsNullOrWhiteSpace($accessToken)) {
    $modelsUrl = $BaseUrl.TrimEnd('/') + '/backend-api/codex/models?client_version=0.159.0'
    $headers = @{ Authorization = "Bearer $accessToken" }
    $tmp = [System.IO.Path]::GetTempFileName()
    try {
      if ($PSVersionTable.PSEdition -eq 'Desktop') {
        Invoke-WebRequest -UseBasicParsing -Uri $modelsUrl -Headers $headers -OutFile $tmp -TimeoutSec 10 | Out-Null
      } else {
        Invoke-WebRequest -Uri $modelsUrl -Headers $headers -OutFile $tmp -TimeoutSec 10 | Out-Null
      }
      Move-Item -Force -Path $tmp -Destination $modelCatalog
      Write-Host "Model catalog saved to $modelCatalog"
    } catch {
      if (Test-Path $tmp) { Remove-Item -Force $tmp -ErrorAction SilentlyContinue }
      Write-Host "Warning: Could not fetch model catalog (non-fatal): $_"
    }
  }
} catch {
  Write-Host "Warning: Could not parse auth for model catalog fetch (non-fatal): $_"
}

Write-Host '3. Installing model sync MCP sidecar...'
$mcpContent = @'
param(
  [string]$BaseUrl = ""
)

$ErrorActionPreference = 'Stop'

$authDir = Join-Path $HOME '.codex'
$authFile = Join-Path $authDir 'auth.json'
$modelCatalog = Join-Path $authDir 'model_catalog.json'

# PS 5.1 compat wrapper: ConvertFrom-Json -Depth was added in PS 6
function ConvertFrom-JsonCompat {
  param([Parameter(ValueFromPipeline)]$InputObject)
  process {
    if ($PSVersionTable.PSVersion.Major -ge 6) {
      $InputObject | ConvertFrom-Json -Depth 20
    } else {
      $InputObject | ConvertFrom-Json
    }
  }
}

function Refresh-ModelCatalog {
  param([string]$Url)
  if ([string]::IsNullOrWhiteSpace($Url)) { return }
  if (-not (Test-Path $authFile)) { return }

  try {
    $raw = Get-Content -Path $authFile -Raw
    $auth = $raw | ConvertFrom-JsonCompat
  } catch {
    return
  }

  $token = $null
  if ($auth -and $auth.tokens -and $auth.tokens.access_token) {
    $token = [string]$auth.tokens.access_token
  } elseif ($auth -and $auth.access_token) {
    $token = [string]$auth.access_token
  }
  if ([string]::IsNullOrWhiteSpace($token)) { return }

  $modelsUrl = $Url.TrimEnd('/') + '/backend-api/codex/models?client_version=0.159.0'
  $headers = @{ Authorization = "Bearer $token" }

  try {
    $tmp = [System.IO.Path]::GetTempFileName()
    if ($PSVersionTable.PSEdition -eq 'Desktop') {
      Invoke-WebRequest -UseBasicParsing -Uri $modelsUrl -Headers $headers -OutFile $tmp -TimeoutSec 5 | Out-Null
    } else {
      Invoke-WebRequest -Uri $modelsUrl -Headers $headers -OutFile $tmp -TimeoutSec 5 | Out-Null
    }
    Move-Item -Force -Path $tmp -Destination $modelCatalog
  } catch {
    if ($tmp -and (Test-Path $tmp)) { Remove-Item -Force -Path $tmp -ErrorAction SilentlyContinue }
  }
}

function Write-McpResponse {
  param(
    [string]$Payload,
    [string]$Transport = 'framed'
  )

  if ($Transport -eq 'jsonl') {
    [Console]::Out.WriteLine($Payload)
    [Console]::Out.Flush()
    return
  }

  $bytes = [System.Text.Encoding]::UTF8.GetBytes($Payload)
  $nl = [Environment]::NewLine
  [Console]::Out.Write("Content-Length: " + $bytes.Length + $nl + $nl + $Payload)
  [Console]::Out.Flush()
}

Refresh-ModelCatalog -Url $BaseUrl

# Keep pulling the live catalog every 15 minutes while Codex is running, so
# newly released upstream models show up in /model without a restart.
Start-Job -Name 'codex-pool-model-sync' -ArgumentList @($BaseUrl, ${function:Refresh-ModelCatalog}) -ScriptBlock {
  param($Url, $Fn)
  Set-Item -Path 'function:Refresh-ModelCatalog' -Value $Fn
  while ($true) {
    Start-Sleep -Seconds 900
    Refresh-ModelCatalog -Url $Url
  }
} | Out-Null

while ($true) {
  $transport = 'framed'
  $contentLength = 0
  $body = ''

  $firstLine = [Console]::In.ReadLine()
  if ($null -eq $firstLine) { exit 0 }

  if ($firstLine -match '^[ \t]*\{') {
    $transport = 'jsonl'
    $body = $firstLine
  } else {
    $line = $firstLine
    while ($true) {
      if ($line -eq '') { break }
      if ($line -match '^(?i)content-length:') {
        $lengthText = ($line -split ':', 2)[1].Trim()
        [int]::TryParse($lengthText, [ref]$contentLength) | Out-Null
      }
      $line = [Console]::In.ReadLine()
      if ($null -eq $line) { exit 0 }
    }
  }

  if ($transport -eq 'framed' -and $contentLength -le 0) { continue }
  if ($transport -eq 'framed') {
    $buffer = New-Object char[] $contentLength
    $readTotal = 0
    while ($readTotal -lt $contentLength) {
      $readNow = [Console]::In.Read($buffer, $readTotal, $contentLength - $readTotal)
      if ($readNow -le 0) { exit 0 }
      $readTotal += $readNow
    }
    $body = -join $buffer
  }

  try {
    $request = $body | ConvertFrom-JsonCompat
  } catch {
    continue
  }

  if (-not $request.PSObject.Properties.Name.Contains('id')) {
    continue
  }

  $method = ''
  if ($request.PSObject.Properties.Name.Contains('method')) {
    $method = [string]$request.method
  }

  $result = $null
  switch ($method) {
    'initialize' {
      $result = @{
        protocolVersion = '2024-11-05'
        capabilities = @{
          tools = @{ listChanged = $false }
          resources = @{ listChanged = $false }
          prompts = @{ listChanged = $false }
        }
        serverInfo = @{
          name = 'model_sync'
          version = '1.0.0'
        }
      }
    }
    'tools/list' { $result = @{ tools = @() } }
    'resources/list' { $result = @{ resources = @() } }
    'prompts/list' { $result = @{ prompts = @() } }
    'ping' { $result = @{} }
    default {
      $errorResponse = @{
        jsonrpc = '2.0'
        id = $request.id
        error = @{
          code = -32601
          message = 'Method not found'
        }
      } | ConvertTo-Json -Compress -Depth 20
      Write-McpResponse -Payload $errorResponse -Transport $transport
      continue
    }
  }

  $response = @{
    jsonrpc = '2.0'
    id = $request.id
    result = $result
  } | ConvertTo-Json -Compress -Depth 20
  Write-McpResponse -Payload $response -Transport $transport
}
'@
Set-Content -Path $mcpScript -Value $mcpContent -Encoding UTF8

$restoreContent = @'
param([string]$BackupId = '')
$authDir = Join-Path $HOME '.codex'
$configFile = Join-Path $authDir 'config.toml'
$authFile = Join-Path $authDir 'auth.json'
if (-not $BackupId) {
  Write-Host 'Available backups:'
  Get-ChildItem -Path $authDir -Filter 'config.toml.bak.*' -ErrorAction SilentlyContinue |
    ForEach-Object { $_.Name -replace '^config\.toml\.bak\.', '' }
  Write-Host 'Usage: restore-config.ps1 -BackupId <backup-id>'
  exit 0
}
$configBackup = Join-Path $authDir "config.toml.bak.$BackupId"
if (-not (Test-Path $configBackup)) {
  Write-Error "No backup found for id: $BackupId"
  exit 1
}
Copy-Item -LiteralPath $configBackup -Destination $configFile -Force
$authBackup = Join-Path $authDir "auth.json.bak.$BackupId"
if (Test-Path $authBackup) {
  Copy-Item -LiteralPath $authBackup -Destination $authFile -Force
}
Write-Host "Restored backup $BackupId"
'@
Set-Utf8NoBom -Path $restoreScript -Value $restoreContent

# Find PowerShell executable path robustly
$mcpCommand = $null
try { $mcpCommand = (Get-Process -Id $PID).Path } catch {}
if ([string]::IsNullOrWhiteSpace($mcpCommand)) {
  try { $mcpCommand = (Get-Command pwsh -ErrorAction SilentlyContinue).Source } catch {}
}
if ([string]::IsNullOrWhiteSpace($mcpCommand)) {
  try { $mcpCommand = (Get-Command powershell -ErrorAction SilentlyContinue).Source } catch {}
}
if ([string]::IsNullOrWhiteSpace($mcpCommand)) {
  $mcpCommand = 'powershell'
}

$modelCatalogToml = $modelCatalog -replace '\\', '\\\\'
$mcpScriptToml = $mcpScript -replace '\\', '\\\\'
$mcpCommandToml = $mcpCommand -replace '\\', '\\\\'

Write-Host '4. Updating configuration...'
if (Test-Path $configFile) {
  Copy-Item -LiteralPath $configFile -Destination "$configFile.bak.$Stamp" -Force | Out-Null
}
if (-not (Test-Path $configFile)) {
  New-Item -ItemType File -Path $configFile -Force | Out-Null
}

$existing = ''
try { $existing = Get-Content -Path $configFile -Raw } catch {}
if ($null -eq $existing) { $existing = '' }

# Only a root model selects the default; profile models remain independent.
$rootConfig = ($existing -split '(?m)^[ \t]*\[', 2)[0]
if ($rootConfig -notmatch '(?m)^[ \t]*model[ \t]*=') {
  $existing = 'model = "gpt-6-astra"' + $nl + $existing
  Set-Utf8NoBom -Path $configFile -Value $existing
}

if ($existing -notmatch 'codex-pool') {
  $new = @"
# Codex Pool Proxy Config
model_provider = "codex-pool"
chatgpt_base_url = "$BaseUrl/backend-api"
experimental_realtime_webrtc_call_base_url = "$BaseUrl/v1"
experimental_realtime_ws_base_url = "$BaseUrl/v1"
realtime.version = "v3"
realtime.transport = "webrtc"
model_catalog_json = "$modelCatalogToml"

$existing

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "$BaseUrl"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true

[model_providers.codex-pool.features]
responses_websockets_v2 = true

[mcp_servers.model_sync]
command = "$mcpCommandToml"
args = ["-NoLogo", "-NoProfile", "-File", "$mcpScriptToml", "$BaseUrl"]
"@

  Set-Utf8NoBom -Path $configFile -Value $new
  Write-Host "Configuration updated in $configFile"
} else {
  # Existing codex-pool setup: refresh the pool-managed entries in place so
  # they point at the current base URL. Unrelated user configuration is
  # preserved; the prior file was backed up as config.toml.bak.$Stamp.
  $existing = $existing -replace '(?ms)^[ \t]*\[model_providers\.codex-pool(\.features)?\].*?(?=^[ \t]*\[|\z)', ''
  $existing = $existing -replace '(?ms)^[ \t]*\[mcp_servers\.(model_sync|codex_pool_model_sync)\].*?(?=^[ \t]*\[|\z)', ''
  $existing = $existing -replace '(?m)^[ \t]*chatgpt_base_url[ \t]*=.*', ('chatgpt_base_url = "' + $BaseUrl + '/backend-api"')
  $existing = $existing -replace '(?m)^[ \t]*experimental_realtime_webrtc_call_base_url[ \t]*=.*', ('experimental_realtime_webrtc_call_base_url = "' + $BaseUrl + '/v1"')
  $existing = $existing -replace '(?m)^[ \t]*experimental_realtime_ws_base_url[ \t]*=.*', ('experimental_realtime_ws_base_url = "' + $BaseUrl + '/v1"')

  if ($existing -notmatch '(?m)^[ \t]*model_catalog_json[ \t]*=') {
    $existing = 'model_catalog_json = "' + $modelCatalogToml + '"' + $nl + $existing
  }

  if ($existing -notmatch '(?m)^[ \t]*experimental_realtime_webrtc_call_base_url[ \t]*=') {
    $existing = 'experimental_realtime_webrtc_call_base_url = "' + $BaseUrl + '/v1"' + $nl + $existing
  }

  if ($existing -notmatch '(?m)^[ \t]*experimental_realtime_ws_base_url[ \t]*=') {
    $existing = 'experimental_realtime_ws_base_url = "' + $BaseUrl + '/v1"' + $nl + $existing
  }

  if ($existing -notmatch '(?m)^[ \t]*realtime\.version[ \t]*=' -and $existing -notmatch '(?m)^\[realtime\]') {
    $existing = 'realtime.version = "v3"' + $nl + 'realtime.transport = "webrtc"' + $nl + $existing
  }

  $existing = $existing.TrimEnd() +
    $nl + $nl +
    '[model_providers.codex-pool]' + $nl +
    'name = "OpenAI via codex-pool proxy"' + $nl +
    'base_url = "' + $BaseUrl + '"' + $nl +
    'wire_api = "responses"' + $nl +
    'requires_openai_auth = true' + $nl +
    'supports_websockets = true' + $nl +
    $nl +
    '[model_providers.codex-pool.features]' + $nl +
    'responses_websockets_v2 = true' + $nl +
    $nl +
    '[mcp_servers.model_sync]' + $nl +
    'command = "' + $mcpCommandToml + '"' + $nl +
    'args = ["-NoLogo", "-NoProfile", "-File", "' + $mcpScriptToml + '", "' + $BaseUrl + '"]' + $nl

  Set-Utf8NoBom -Path $configFile -Value $existing
  Write-Host "Configuration updated in $configFile"
}

if (Test-Path ("$configFile.bak.$Stamp")) {
  Write-Host "Previous configuration backed up as config.toml.bak.$Stamp (restore with: restore-config.ps1 -BackupId $Stamp)"
}

try {
  if (Get-Command codex -ErrorAction SilentlyContinue) {
    & codex features enable realtime_conversation | Out-Null
  }
} catch {}

Write-Host 'Setup complete! You are ready to use the pool.'
`, token, publicURL)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
		return
	}

	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
TOKEN="%s"
BASE_URL="%s"
AUTH_DIR="$HOME/.codex"
CONFIG_FILE="$AUTH_DIR/config.toml"
AUTH_FILE="$AUTH_DIR/auth.json"
MODEL_CATALOG="$AUTH_DIR/model_catalog.json"
MCP_SCRIPT="$AUTH_DIR/model_sync.sh"

echo "Initializing Codex Pool setup..."
mkdir -p "$AUTH_DIR"
STAMP=$(date +%%Y%%m%%d-%%H%%M%%S)

echo "1. Fetching credentials..."
if [ -f "$AUTH_FILE" ]; then
    cp "$AUTH_FILE" "$AUTH_FILE.bak.$STAMP"
    chmod 600 "$AUTH_FILE.bak.$STAMP" 2>/dev/null || true
fi
curl -sL "$BASE_URL/config/codex/$TOKEN" -o "$AUTH_FILE"
chmod 600 "$AUTH_FILE"

echo "2. Fetching model catalog..."
ACCESS_TOKEN=$(sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$AUTH_FILE" | head -n 1)
if [ -n "${ACCESS_TOKEN:-}" ]; then
    curl --connect-timeout 5 --max-time 10 -fsSL \
        -H "Authorization: Bearer $ACCESS_TOKEN" \
        "${BASE_URL%%/}/backend-api/codex/models?client_version=0.159.0" \
        -o "$MODEL_CATALOG" 2>/dev/null && chmod 600 "$MODEL_CATALOG" 2>/dev/null || true
fi

echo "3. Installing model sync MCP sidecar..."
cat <<'EOF' > "$MCP_SCRIPT"
#!/bin/bash
set -euo pipefail

BASE_URL="${1:-}"
AUTH_DIR="${HOME}/.codex"
AUTH_FILE="$AUTH_DIR/auth.json"
MODEL_CATALOG="$AUTH_DIR/model_catalog.json"
CLIENT_VERSION="0.159.0"

refresh_model_catalog() {
    if [ -z "$BASE_URL" ] || [ ! -f "$AUTH_FILE" ]; then
        return 0
    fi

    local token
    token=$(sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$AUTH_FILE" | head -n 1)
    if [ -z "${token:-}" ]; then
        return 0
    fi

    local tmp_file
    tmp_file=$(mktemp "${MODEL_CATALOG}.tmp.XXXXXX")
    if curl --connect-timeout 2 --max-time 5 -fsSL -H "Authorization: Bearer $token" \
        "${BASE_URL%%/}/backend-api/codex/models?client_version=${CLIENT_VERSION}" \
        -o "$tmp_file"; then
        mv "$tmp_file" "$MODEL_CATALOG"
        chmod 600 "$MODEL_CATALOG" 2>/dev/null || true
    else
        rm -f "$tmp_file"
    fi
}

read_request() {
    local line content_length
    content_length=0

    if ! IFS= read -r line; then
        return 1
    fi
    line="${line%%$'\r'}"

    if [[ "$line" == \{* ]]; then
        MCP_TRANSPORT_MODE="jsonl"
        REQUEST_BODY="$line"
        return 0
    fi

    while true; do
        if [ -z "$line" ]; then
            break
        fi
        case "$line" in
            [Cc]ontent-[Ll]ength:*|[Cc]ONTENT-[Ll]ENGTH:*|CONTENT-LENGTH:*|content-length:*)
                content_length=$(printf '%%s' "${line#*:}" | tr -d '[:space:]')
                ;;
        esac
        if ! IFS= read -r line; then
            return 1
        fi
        line="${line%%$'\r'}"
    done

    if [ -z "$content_length" ] || ! [[ "$content_length" =~ ^[0-9]+$ ]] || [ "$content_length" -le 0 ]; then
        return 1
    fi

    MCP_TRANSPORT_MODE="framed"
    REQUEST_BODY=$(dd bs=1 count="$content_length" 2>/dev/null)
    return 0
}

write_response() {
    local payload="$1"
    if [ "${MCP_TRANSPORT_MODE:-framed}" = "jsonl" ]; then
        printf '%%s\n' "$payload"
        return
    fi

    local length
    length=$(printf '%%s' "$payload" | LC_ALL=C wc -c | tr -d '[:space:]')
    printf 'Content-Length: %%s\r\n\r\n%%s' "$length" "$payload"
}

handle_request() {
    local request="$1"
    local method id payload

    method=$(printf '%%s' "$request" | sed -n 's/.*"method"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
    id=$(printf '%%s' "$request" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*\([^,}]*\).*/\1/p' | head -n 1)
    if [ -z "${id:-}" ]; then
        return 0
    fi

    case "$method" in
        initialize)
            payload='{"jsonrpc":"2.0","id":'"$id"',"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{"listChanged":false},"resources":{"listChanged":false},"prompts":{"listChanged":false}},"serverInfo":{"name":"model_sync","version":"1.0.0"}}}'
            ;;
        tools/list)
            payload='{"jsonrpc":"2.0","id":'"$id"',"result":{"tools":[]}}'
            ;;
        resources/list)
            payload='{"jsonrpc":"2.0","id":'"$id"',"result":{"resources":[]}}'
            ;;
        prompts/list)
            payload='{"jsonrpc":"2.0","id":'"$id"',"result":{"prompts":[]}}'
            ;;
        ping)
            payload='{"jsonrpc":"2.0","id":'"$id"',"result":{}}'
            ;;
        *)
            payload='{"jsonrpc":"2.0","id":'"$id"',"error":{"code":-32601,"message":"Method not found"}}'
            ;;
    esac

    write_response "$payload"
}

# Refresh now and keep pulling the live catalog every 15 minutes while
# Codex is running, so newly released upstream models show up in /model.
(
    while true; do
        refresh_model_catalog >/dev/null 2>&1 || true
        sleep 900
    done
) >/dev/null 2>&1 &

while true; do
    REQUEST_BODY=""
    if ! read_request; then
        exit 0
    fi
    handle_request "$REQUEST_BODY"
done
EOF
chmod 700 "$MCP_SCRIPT"

cat <<'EOF' > "$AUTH_DIR/restore-config.sh"
#!/bin/bash
# Restore a codex-pool configuration backup created by the setup script.
# Usage: restore-config.sh [backup-id]    (run without arguments to list backups)
set -euo pipefail
AUTH_DIR="${HOME}/.codex"
CONFIG_FILE="$AUTH_DIR/config.toml"
AUTH_FILE="$AUTH_DIR/auth.json"

if [ $# -eq 0 ]; then
    echo "Available backups:"
    ls -1 "$AUTH_DIR"/config.toml.bak.* 2>/dev/null | sed 's/.*\.bak\.//' || true
    echo "Usage: $0 <backup-id>"
    exit 0
fi

STAMP="$1"
CONFIG_BACKUP="$AUTH_DIR/config.toml.bak.$STAMP"
AUTH_BACKUP="$AUTH_DIR/auth.json.bak.$STAMP"
if [ ! -f "$CONFIG_BACKUP" ]; then
    echo "No backup found for id: $STAMP" >&2
    exit 1
fi
cp "$CONFIG_BACKUP" "$CONFIG_FILE"
if [ -f "$AUTH_BACKUP" ]; then
    cp "$AUTH_BACKUP" "$AUTH_FILE"
fi
chmod 600 "$CONFIG_FILE" "$AUTH_FILE" 2>/dev/null || true
echo "Restored backup $STAMP"
EOF
chmod 700 "$AUTH_DIR/restore-config.sh"

echo "4. Updating configuration..."
AUTH_DIR="${AUTH_DIR:-$HOME/.codex}"
AUTH_FILE="${AUTH_FILE:-$AUTH_DIR/auth.json}"
STAMP="${STAMP:-$(date +%%Y%%m%%d-%%H%%M%%S)}"
if [ -f "$CONFIG_FILE" ]; then
    cp "$CONFIG_FILE" "$CONFIG_FILE.bak.$STAMP"
    chmod 600 "$CONFIG_FILE.bak.$STAMP" 2>/dev/null || true
fi
if [ ! -f "$CONFIG_FILE" ]; then
    touch "$CONFIG_FILE"
fi

# Only a root model selects the default; profile models remain independent.
if ! awk '/^[[:space:]]*\[/ { exit } /^[[:space:]]*model[[:space:]]*=/ { found=1 } END { exit !found }' "$CONFIG_FILE"; then
    TEMP_FILE=$(mktemp)
    printf 'model = "gpt-6-astra"\n' > "$TEMP_FILE"
    cat "$CONFIG_FILE" >> "$TEMP_FILE"
    mv "$TEMP_FILE" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
fi

# Check if config already exists to avoid duplication
if ! grep -q "codex-pool" "$CONFIG_FILE"; then
    # Create temp file with pool config at TOP, then append existing config
    TEMP_FILE=$(mktemp)
    cat <<EOF > "$TEMP_FILE"
# Codex Pool Proxy Config
model_provider = "codex-pool"
chatgpt_base_url = "$BASE_URL/backend-api"
experimental_realtime_webrtc_call_base_url = "$BASE_URL/v1"
experimental_realtime_ws_base_url = "$BASE_URL/v1"
realtime.version = "v3"
realtime.transport = "webrtc"
model_catalog_json = "$MODEL_CATALOG"

EOF
    # Append existing config
    cat "$CONFIG_FILE" >> "$TEMP_FILE"

    # Add model_providers section at the end (sections go after top-level keys)
    cat <<EOF >> "$TEMP_FILE"

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "$BASE_URL"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true

[model_providers.codex-pool.features]
responses_websockets_v2 = true

[mcp_servers.model_sync]
command = "bash"
args = ["$MCP_SCRIPT", "$BASE_URL"]
EOF

    mv "$TEMP_FILE" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    echo "Configuration updated in $CONFIG_FILE"
else
    # Existing codex-pool setup: refresh the pool-managed entries in place so
    # they point at the current base URL. Unrelated user configuration is
    # preserved; the prior file was backed up as config.toml.bak.$STAMP.
    TEMP_FILE=$(mktemp)
    awk '
        /^[[:space:]]*\[model_providers\.codex-pool\.features\]/ { skip = 1; next }
        /^[[:space:]]*\[model_providers\.codex-pool\]/ { skip = 1; next }
        /^[[:space:]]*\[mcp_servers\.model_sync\]/ { skip = 1; next }
        /^[[:space:]]*\[mcp_servers\.codex_pool_model_sync\]/ { skip = 1; next }
        /^[[:space:]]*\[/ { skip = 0 }
        !skip { print }
    ' "$CONFIG_FILE" | sed \
        -e "s|^[[:space:]]*chatgpt_base_url[[:space:]]*=.*|chatgpt_base_url = \"$BASE_URL/backend-api\"|" \
        -e "s|^[[:space:]]*experimental_realtime_webrtc_call_base_url[[:space:]]*=.*|experimental_realtime_webrtc_call_base_url = \"$BASE_URL/v1\"|" \
        -e "s|^[[:space:]]*experimental_realtime_ws_base_url[[:space:]]*=.*|experimental_realtime_ws_base_url = \"$BASE_URL/v1\"|" \
        > "$TEMP_FILE"

    if ! grep -Eq '^[[:space:]]*model_catalog_json[[:space:]]*=' "$TEMP_FILE"; then
        TEMP_KEY=$(mktemp)
        echo "model_catalog_json = \"$MODEL_CATALOG\"" > "$TEMP_KEY"
        cat "$TEMP_FILE" >> "$TEMP_KEY"
        mv "$TEMP_KEY" "$TEMP_FILE"
    fi

    if ! grep -Eq '^[[:space:]]*experimental_realtime_webrtc_call_base_url[[:space:]]*=' "$TEMP_FILE"; then
        TEMP_KEY=$(mktemp)
        echo "experimental_realtime_webrtc_call_base_url = \"$BASE_URL/v1\"" > "$TEMP_KEY"
        cat "$TEMP_FILE" >> "$TEMP_KEY"
        mv "$TEMP_KEY" "$TEMP_FILE"
    fi

    if ! grep -Eq '^[[:space:]]*experimental_realtime_ws_base_url[[:space:]]*=' "$TEMP_FILE"; then
        TEMP_KEY=$(mktemp)
        echo "experimental_realtime_ws_base_url = \"$BASE_URL/v1\"" > "$TEMP_KEY"
        cat "$TEMP_FILE" >> "$TEMP_KEY"
        mv "$TEMP_KEY" "$TEMP_FILE"
    fi

    if ! grep -Eq '^[[:space:]]*realtime\.version[[:space:]]*=' "$TEMP_FILE" && ! grep -q '^\[realtime\]' "$TEMP_FILE"; then
        TEMP_KEY=$(mktemp)
        echo 'realtime.version = "v3"' > "$TEMP_KEY"
        echo 'realtime.transport = "webrtc"' >> "$TEMP_KEY"
        cat "$TEMP_FILE" >> "$TEMP_KEY"
        mv "$TEMP_KEY" "$TEMP_FILE"
    fi

    cat <<EOF >> "$TEMP_FILE"

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "$BASE_URL"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true

[model_providers.codex-pool.features]
responses_websockets_v2 = true

[mcp_servers.model_sync]
command = "bash"
args = ["$MCP_SCRIPT", "$BASE_URL"]
EOF

    mv "$TEMP_FILE" "$CONFIG_FILE"
    chmod 600 "$CONFIG_FILE"
    echo "Configuration updated in $CONFIG_FILE"
fi

if [ -f "$CONFIG_FILE.bak.$STAMP" ]; then
    echo "Previous configuration backed up as config.toml.bak.$STAMP (restore with: bash \"$AUTH_DIR/restore-config.sh\" $STAMP)"
fi

if command -v codex >/dev/null 2>&1; then
    codex features enable realtime_conversation >/dev/null 2>&1 || true
fi

echo "Setup complete! You are ready to use the pool."
`, token, publicURL)

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(script))
}

func (h *proxyHandler) serveGeminiSetupScript(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	token := strings.TrimPrefix(r.URL.Path, "/setup/gemini/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	if h.passport == nil {
		http.Error(w, "passport not configured", http.StatusServiceUnavailable)
		return
	}
	// The Gemini script embeds the credentials itself, so this is the single
	// redemption of the nonce.
	user := passportClientAsPoolUser(h, h.passport.redeemConfigDownloadNonce(token))
	if user == nil {
		http.Error(w, "invalid token", http.StatusNotFound)
		return
	}

	// Generate pool OAuth credentials for this user
	secret := getPoolJWTSecret()
	if secret == "" {
		http.Error(w, "JWT secret not configured", http.StatusServiceUnavailable)
		return
	}
	geminiAuth, err := generateGeminiAuth(secret, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	publicURL := h.getEffectivePublicURL(r)

	// Script sets env vars to bypass Google OAuth validation and route through proxy
	// Uses GOOGLE_GENAI_USE_GCA + GOOGLE_CLOUD_ACCESS_TOKEN to skip getTokenInfo() check
	if wantsPowerShell(r) {
		script := fmt.Sprintf(`#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$BaseUrl = '%s'
$PoolToken = '%s'

# PS 5.1 writes UTF-8 with BOM which breaks parsers. Write without BOM.
function Set-Utf8NoBom {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Value, $utf8)
}

Write-Host 'Configuring Gemini CLI for pool access...'
Write-Host ''

# Set env vars for the current session
$env:CODE_ASSIST_ENDPOINT = $BaseUrl
$env:GOOGLE_GENAI_USE_GCA = '1'
$env:GOOGLE_CLOUD_ACCESS_TOKEN = $PoolToken

# Persist env vars for future PowerShell sessions
$profilePath = $PROFILE.CurrentUserAllHosts
New-Item -ItemType Directory -Force -Path (Split-Path $profilePath) | Out-Null
if (-not (Test-Path $profilePath)) { New-Item -ItemType File -Force -Path $profilePath | Out-Null }

$start = '# >>> Gemini Pool Configuration >>>'
$end = '# <<< Gemini Pool Configuration <<<'
$nl = [Environment]::NewLine
$blockLines = @(
  $start,
  ('$env:CODE_ASSIST_ENDPOINT = "' + $BaseUrl + '"'),
  ('$env:GOOGLE_GENAI_USE_GCA = "1"'),
  ('$env:GOOGLE_CLOUD_ACCESS_TOKEN = "' + $PoolToken + '"'),
  $end
)
$block = $blockLines -join $nl

$existing = ''
try { $existing = Get-Content -Path $profilePath -Raw } catch {}
if ($null -eq $existing) { $existing = '' }

$pattern = [regex]::Escape($start) + '.*?' + [regex]::Escape($end)
if ([regex]::IsMatch($existing, $pattern, [Text.RegularExpressions.RegexOptions]::Singleline)) {
  $updated = [regex]::Replace($existing, $pattern, $block, [Text.RegularExpressions.RegexOptions]::Singleline)
} else {
  $sep = ''; if ($existing -and -not ($existing.EndsWith($nl))) { $sep = $nl }
  $updated = $existing + $sep + $nl + $block + $nl
}

Set-Utf8NoBom -Path $profilePath -Value $updated
Write-Host ("Added Gemini pool config to " + $profilePath)

Write-Host ''
Write-Host 'Setup complete!'
Write-Host ''
Write-Host ("Gemini CLI will use the pool proxy at: " + $BaseUrl)
Write-Host 'No Google login required - validation is bypassed.'
Write-Host ''
Write-Host 'Start a new terminal, or run: . $PROFILE'
`, publicURL, geminiAuth.AccessToken)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
		return
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e
BASE_URL="%s"
POOL_TOKEN="%s"

echo "Configuring Gemini CLI for pool access..."
echo ""

# Add env vars to shell profile
add_to_profile() {
    for profile in "$HOME/.zshrc" "$HOME/.bashrc"; do
        if [ -f "$profile" ]; then
            # Remove old Gemini-related env vars
            grep -v "GEMINI_API_KEY=" "$profile" 2>/dev/null | \
            grep -v "GOOGLE_GEMINI_BASE_URL=" 2>/dev/null | \
            grep -v "CODE_ASSIST_ENDPOINT=" 2>/dev/null | \
            grep -v "GOOGLE_GENAI_USE_GCA=" 2>/dev/null | \
            grep -v "GOOGLE_CLOUD_ACCESS_TOKEN=" 2>/dev/null > "$profile.tmp" || true
            mv "$profile.tmp" "$profile"

            # Add pool configuration
            cat >> "$profile" << 'ENVEOF'

# Gemini Pool Configuration
export CODE_ASSIST_ENDPOINT="%s"
export GOOGLE_GENAI_USE_GCA=1
export GOOGLE_CLOUD_ACCESS_TOKEN="%s"
ENVEOF
            echo "✓ Added Gemini pool config to $(basename $profile)"
            return
        fi
    done

    # Fallback: create .zshrc
    cat >> "$HOME/.zshrc" << 'ENVEOF'

# Gemini Pool Configuration
export CODE_ASSIST_ENDPOINT="%s"
export GOOGLE_GENAI_USE_GCA=1
export GOOGLE_CLOUD_ACCESS_TOKEN="%s"
ENVEOF
    echo "✓ Created ~/.zshrc with Gemini pool config"
}

add_to_profile

echo ""
echo "Setup complete!"
echo ""
echo "Gemini CLI will use the pool proxy at: $BASE_URL"
echo "No Google login required - validation is bypassed."
echo ""
echo "Run 'source ~/.zshrc' or start a new terminal, then run 'gemini'."
`, publicURL, geminiAuth.AccessToken,
		publicURL, geminiAuth.AccessToken,
		publicURL, geminiAuth.AccessToken)

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(script))
}

// serveAntigravitySetupScript configures the Antigravity CLI (agy) to use the
// pool. The CLI has a Gemini API key mode selected by modelProvider=gemini in
// ~/.gemini/antigravity-cli/settings.json; requests then go to
// GOOGLE_GEMINI_BASE_URL/v1beta/... with the GEMINI_API_KEY, which the pool
// authenticates through its gemini_api_key credential parsing.
func (h *proxyHandler) serveAntigravitySetupScript(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/setup/antigravity/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	if h.passport == nil {
		http.Error(w, "passport not configured", http.StatusServiceUnavailable)
		return
	}
	// The script embeds the pool API key, so this is the single redemption of
	// the nonce.
	user := passportClientAsPoolUser(h, h.passport.redeemConfigDownloadNonce(token))
	if user == nil {
		http.Error(w, "invalid token", http.StatusNotFound)
		return
	}

	secret := getPoolJWTSecret()
	if secret == "" {
		http.Error(w, "JWT secret not configured", http.StatusServiceUnavailable)
		return
	}
	apiKey := generateGeminiAPIKey(secret, user)

	publicURL := h.getEffectivePublicURL(r)

	noStore(w)

	if wantsPowerShell(r) {
		script := fmt.Sprintf(`#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$BaseUrl = '%s'
$ApiKey = '%s'

# PS 5.1 writes UTF-8 with BOM which breaks parsers. Write without BOM.
function Set-Utf8NoBom {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Value, $utf8)
}

# Atomic variant for existing files: full write + flush to a temp sibling,
# then swap into place so a mid-write failure cannot truncate the original.
function Set-Utf8NoBomAtomic {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  $tmp = Join-Path (Split-Path -Parent $Path) ('.' + (Split-Path -Leaf $Path) + '.tmp')
  $stream = New-Object System.IO.FileStream($tmp, [System.IO.FileMode]::Create, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
  try {
    $bytes = $utf8.GetBytes($Value)
    $stream.Write($bytes, 0, $bytes.Length)
    $stream.Flush($true)
  } finally {
    $stream.Close()
  }
  try { (Get-Acl -LiteralPath $Path) | Set-Acl -LiteralPath $tmp } catch {}
  try {
    [System.IO.File]::Replace($tmp, $Path, $null)
  } catch {
    Copy-Item -LiteralPath $tmp -Destination $Path -Force
    Remove-Item -LiteralPath $tmp -Force
  }
}

Write-Host 'Configuring Antigravity CLI for pool access...'
Write-Host ''

# Select the Gemini API key provider in ~/.gemini/antigravity-cli/settings.json
$settingsDir = Join-Path $HOME '.gemini\antigravity-cli'
New-Item -ItemType Directory -Force -Path $settingsDir | Out-Null
$settingsPath = Join-Path $settingsDir 'settings.json'
if (Test-Path $settingsPath) {
  $backup = $settingsPath + '.bak.' + (Get-Date -Format 'yyyyMMddHHmmss')
  Copy-Item $settingsPath $backup
  $settings = $null
  try {
    $settings = Get-Content $settingsPath -Raw | ConvertFrom-Json
  } catch {
    Write-Host ($settingsPath + ' is not valid JSON.')
    Write-Host ('A copy was saved as ' + $backup)
    Write-Host 'Fix or remove the file, then re-run this setup.'
    exit 1
  }
  if ($null -eq $settings) {
    Write-Host ($settingsPath + ' is empty or unreadable.')
    Write-Host ('A copy was saved as ' + $backup)
    Write-Host 'Fix or remove the file, then re-run this setup.'
    exit 1
  }
  if ($null -ne $settings.PSObject.Properties['modelProvider']) {
    $settings.modelProvider = 'gemini'
  } else {
    $settings | Add-Member -NotePropertyName modelProvider -NotePropertyValue 'gemini'
  }
  $json = $settings | ConvertTo-Json -Depth 10
  Set-Utf8NoBomAtomic -Path $settingsPath -Value $json
} else {
  $json = '{"modelProvider": "gemini"}'
  Set-Utf8NoBom -Path $settingsPath -Value $json
}
Write-Host ('Set modelProvider to gemini in ' + $settingsPath)

# Point the CLI at the pool using Gemini API key mode
$env:GEMINI_API_KEY = $ApiKey
$env:GOOGLE_GEMINI_BASE_URL = $BaseUrl

# Persist for future PowerShell, CMD, Windows Terminal, and IDE processes
[Environment]::SetEnvironmentVariable('GEMINI_API_KEY', $ApiKey, 'User')
[Environment]::SetEnvironmentVariable('GOOGLE_GEMINI_BASE_URL', $BaseUrl, 'User')
Write-Host 'Saved GEMINI_API_KEY and GOOGLE_GEMINI_BASE_URL to the user environment.'

# Remove pool blocks written by earlier setup versions from the PowerShell profile
$profilePath = $PROFILE.CurrentUserAllHosts
if (Test-Path $profilePath) {
  $start = '# >>> Antigravity Pool Configuration >>>'
  $end = '# <<< Antigravity Pool Configuration <<<'
  $existing = ''
  try { $existing = Get-Content -Path $profilePath -Raw } catch {}
  if ($null -ne $existing -and $existing) {
    $pattern = [regex]::Escape($start) + '.*?' + [regex]::Escape($end)
    if ([regex]::IsMatch($existing, $pattern, [Text.RegularExpressions.RegexOptions]::Singleline)) {
      $updated = [regex]::Replace($existing, $pattern, '', [Text.RegularExpressions.RegexOptions]::Singleline)
      Set-Utf8NoBom -Path $profilePath -Value $updated
      Write-Host ('Removed stale pool block from ' + $profilePath)
    }
  }
}

Write-Host ''
Write-Host 'Setup complete!'
Write-Host ''
Write-Host ('Antigravity CLI will use the pool proxy at: ' + $BaseUrl)
Write-Host 'No Google login required - the pool key authenticates requests.'
Write-Host ''
if (-not (Get-Command agy -ErrorAction SilentlyContinue)) {
  Write-Host 'Antigravity CLI is not installed.'
  Write-Host 'Install it with:'
  Write-Host '  irm https://antigravity.google/cli/install.ps1 | iex'
} else {
  Write-Host 'Start a new terminal, then run agy.'
}
`, publicURL, apiKey)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
		return
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e
BASE_URL="%s"
API_KEY="%s"

echo "Configuring Antigravity CLI for pool access..."
echo ""

# Point the CLI at the pool using Gemini API key mode
export GEMINI_API_KEY="$API_KEY"
export GOOGLE_GEMINI_BASE_URL="$BASE_URL"

# Select modelProvider=gemini in ~/.gemini/antigravity-cli/settings.json.
# The edit always goes through a real JSON parser so values like
# modelProvider=null or keys nested in other objects cannot fool a text match.
SETTINGS_DIR="$HOME/.gemini/antigravity-cli"
SETTINGS_FILE="$SETTINGS_DIR/settings.json"
mkdir -p "$SETTINGS_DIR"

set_provider() {
    if command -v python3 >/dev/null 2>&1; then
        python3 - "$SETTINGS_FILE" <<'PY'
import json
import os
import sys
import tempfile

path = sys.argv[1]
with open(path, encoding="utf-8") as handle:
    data = json.load(handle)
if not isinstance(data, dict):
    raise SystemExit("settings.json root must be an object")
data["modelProvider"] = "gemini"
directory = os.path.dirname(path) or "."
tmp_fd, tmp = tempfile.mkstemp(prefix=".settings.", suffix=".tmp", dir=directory)
try:
    with os.fdopen(tmp_fd, "w", encoding="utf-8") as handle:
        json.dump(data, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(tmp, os.stat(path).st_mode & 0o777)
    os.replace(tmp, path)
except BaseException:
    try:
        os.unlink(tmp)
    except OSError:
        pass
    raise
PY
    elif command -v node >/dev/null 2>&1; then
        node -e 'const fs=require("fs"),path=require("path");const p=process.argv[1];const data=JSON.parse(fs.readFileSync(p,"utf8"));if(data===null||typeof data!=="object"||Array.isArray(data)){console.error("settings.json root must be an object");process.exit(1);}data.modelProvider="gemini";const tmp=path.join(path.dirname(p),".settings."+process.pid+".tmp");try{const fd=fs.openSync(tmp,"w",0o600);try{fs.writeFileSync(fd,JSON.stringify(data,null,2)+"\n","utf8");fs.fsyncSync(fd);}finally{fs.closeSync(fd);}try{fs.chmodSync(tmp,fs.statSync(p).mode&0o777);}catch(e){}fs.renameSync(tmp,p);}catch(err){try{fs.unlinkSync(tmp);}catch(e){}throw err;}' "$SETTINGS_FILE"
    else
        echo "✗ python3 or node is required to edit $SETTINGS_FILE safely."
        return 2
    fi
}

if [ ! -f "$SETTINGS_FILE" ]; then
    printf '{\n  "modelProvider": "gemini"\n}\n' > "$SETTINGS_FILE"
else
    STAMP="$(date +%%Y%%m%%d%%H%%M%%S)"
    rm -f "$SETTINGS_FILE".bak.* "$SETTINGS_DIR"/.settings.*.tmp 2>/dev/null || true
    cp "$SETTINGS_FILE" "$SETTINGS_FILE.bak.$STAMP"
    if ! set_provider; then
        echo "  A copy was saved as $SETTINGS_FILE.bak.$STAMP."
        echo '  Merge {"modelProvider": "gemini"} into the file manually, then re-run this setup.'
        exit 1
    fi
fi
echo "✓ Set modelProvider to gemini in $SETTINGS_FILE"

# Persist env vars in every existing shell profile between pool markers
add_to_profile() {
    updated=0
    for profile in "$HOME/.zshrc" "$HOME/.bashrc"; do
        if [ -f "$profile" ]; then
            # Remove a previous pool block so re-runs stay idempotent
            sed -i.bak '/# >>> Antigravity Pool Configuration >>>/,/# <<< Antigravity Pool Configuration <<</d' "$profile"
            rm -f "$profile.bak"
            cat >> "$profile" << 'ENVEOF'

# >>> Antigravity Pool Configuration >>>
export GEMINI_API_KEY="%s"
export GOOGLE_GEMINI_BASE_URL="%s"
# <<< Antigravity Pool Configuration <<<
ENVEOF
            echo "✓ Added Antigravity pool config to $(basename "$profile")"
            updated=1
        fi
    done

    if [ "$updated" -eq 0 ]; then
        case "${SHELL:-}" in
            *bash*) profile="$HOME/.bashrc" ;;
            *) profile="$HOME/.zshrc" ;;
        esac
        cat >> "$profile" << 'ENVEOF'

# >>> Antigravity Pool Configuration >>>
export GEMINI_API_KEY="%s"
export GOOGLE_GEMINI_BASE_URL="%s"
# <<< Antigravity Pool Configuration <<<
ENVEOF
        echo "✓ Created $(basename "$profile") with Antigravity pool config"
    fi
}

add_to_profile

echo ""
echo "Setup complete!"
echo ""
echo "Antigravity CLI will use the pool proxy at: $BASE_URL"
echo "No Google login required - the pool key authenticates requests."
echo ""
if ! command -v agy >/dev/null 2>&1; then
    echo "Antigravity CLI is not installed."
    echo "Install it with:"
    echo "  curl -fsSL https://antigravity.google/cli/install.sh | bash"
else
    echo "Start a new terminal (or 'source ~/.zshrc' / 'source ~/.bashrc'), then run 'agy'."
fi
`, publicURL, apiKey,
		apiKey, publicURL,
		apiKey, publicURL)

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(script))
}

func (h *proxyHandler) serveClaudeSetupScript(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	token := strings.TrimPrefix(r.URL.Path, "/setup/claude/")
	if token == "" || strings.Contains(token, "/") {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	// Validate token and get user
	if h.passport == nil {
		http.Error(w, "passport not configured", http.StatusServiceUnavailable)
		return
	}
	// The Claude script embeds the credentials itself, so this is the single
	// redemption of the nonce.
	user := passportClientAsPoolUser(h, h.passport.redeemConfigDownloadNonce(token))
	if user == nil {
		http.Error(w, "invalid token", http.StatusNotFound)
		return
	}

	// Generate Claude API key (JWT)
	secret := getPoolJWTSecret()
	if secret == "" {
		http.Error(w, "JWT secret not configured", http.StatusServiceUnavailable)
		return
	}
	claudeAuth, err := generateClaudeAuth(secret, user)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	publicURL := h.getEffectivePublicURL(r)

	if wantsPowerShell(r) {
		script := fmt.Sprintf(`#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$BaseUrl = '%s'
$OAuthToken = '%s'

# PS 5.1 writes UTF-8 with BOM which breaks JSON parsers. Write without BOM.
function Set-Utf8NoBom {
  param([string]$Path, [string]$Value)
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Value, $utf8)
}

function Read-JsonObject {
  param([string]$Path)
  if (-not (Test-Path $Path)) { return (New-Object PSObject) }
  try {
    $raw = Get-Content -Path $Path -Raw
    if ([string]::IsNullOrWhiteSpace($raw)) { return (New-Object PSObject) }
    $obj = $raw | ConvertFrom-Json
    if ($null -eq $obj) { return (New-Object PSObject) }
    return $obj
  } catch {
    $backup = $Path + '.bak'
    Copy-Item -Path $Path -Destination $backup -Force -ErrorAction SilentlyContinue
    Write-Host ("Warning: could not parse " + $Path + "; backed it up to " + $backup + " and recreated it.")
    return (New-Object PSObject)
  }
}

function Has-Property {
  param($Obj, [string]$Name)
  if ($null -eq $Obj) { return $false }
  if ($Obj -is [System.Collections.IDictionary]) { return $Obj.Contains($Name) }
  $prop = $null
  try { $prop = $Obj.PSObject.Properties[$Name] } catch { return $false }
  return ($null -ne $prop)
}

function Get-Property {
  param($Obj, [string]$Name)
  if (-not (Has-Property $Obj $Name)) { return $null }
  if ($Obj -is [System.Collections.IDictionary]) { return $Obj[$Name] }
  return $Obj.PSObject.Properties[$Name].Value
}

function Remove-ObjectProperty {
  param([object]$Object, [string]$Name)
  if ($null -eq $Object) { return }
  if ($Object -is [System.Collections.IDictionary]) {
    if ($Object.Contains($Name)) { $Object.Remove($Name) | Out-Null }
    return
  }
  try {
    if ($Object.PSObject.Properties[$Name]) {
      $Object.PSObject.Properties.Remove($Name)
    }
  } catch {}
}

Write-Host 'Configuring Claude Code for pool access...'
Write-Host ''

$conflictingEnvVars = @(
  'ANTHROPIC_AUTH_TOKEN',
  'ANTHROPIC_API_KEY',
  'CLAUDE_CODE_USE_BEDROCK',
  'CLAUDE_CODE_USE_VERTEX',
  'CLAUDE_CODE_USE_FOUNDRY',
  'CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST',
  'ANTHROPIC_UNIX_SOCKET',
  'CLAUDE_CODE_SIMPLE'
)

# Claude Code disables Claude.ai OAuth mode when these auth/provider vars are present.
foreach ($name in $conflictingEnvVars) {
  Remove-Item -Path ("Env:" + $name) -ErrorAction SilentlyContinue
  [Environment]::SetEnvironmentVariable($name, $null, 'User')
}

# Set env vars for this process and the user's default Windows environment.
$env:ANTHROPIC_BASE_URL = $BaseUrl
$env:CLAUDE_CODE_OAUTH_TOKEN = $OAuthToken
[Environment]::SetEnvironmentVariable('ANTHROPIC_BASE_URL', $BaseUrl, 'User')
[Environment]::SetEnvironmentVariable('CLAUDE_CODE_OAUTH_TOKEN', $OAuthToken, 'User')

# Persist env vars for future PowerShell sessions.
$profilePath = $PROFILE.CurrentUserAllHosts
New-Item -ItemType Directory -Force -Path (Split-Path $profilePath) | Out-Null
if (-not (Test-Path $profilePath)) { New-Item -ItemType File -Force -Path $profilePath | Out-Null }

$start = '# >>> Claude Code Pool Configuration >>>'
$end = '# <<< Claude Code Pool Configuration <<<'
$nl = [Environment]::NewLine
$blockLines = @(
  $start,
  'Remove-Item Env:\ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue',
  'Remove-Item Env:\ANTHROPIC_API_KEY -ErrorAction SilentlyContinue',
  'Remove-Item Env:\CLAUDE_CODE_USE_BEDROCK -ErrorAction SilentlyContinue',
  'Remove-Item Env:\CLAUDE_CODE_USE_VERTEX -ErrorAction SilentlyContinue',
  'Remove-Item Env:\CLAUDE_CODE_USE_FOUNDRY -ErrorAction SilentlyContinue',
  'Remove-Item Env:\CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST -ErrorAction SilentlyContinue',
  'Remove-Item Env:\ANTHROPIC_UNIX_SOCKET -ErrorAction SilentlyContinue',
  'Remove-Item Env:\CLAUDE_CODE_SIMPLE -ErrorAction SilentlyContinue',
  ('$env:ANTHROPIC_BASE_URL = "' + $BaseUrl + '"'),
  ('$env:CLAUDE_CODE_OAUTH_TOKEN = "' + $OAuthToken + '"'),
  $end
)
$block = $blockLines -join $nl

$existing = ''
try { $existing = Get-Content -Path $profilePath -Raw } catch {}
if ($null -eq $existing) { $existing = '' }

$pattern = [regex]::Escape($start) + '.*?' + [regex]::Escape($end)
if ([regex]::IsMatch($existing, $pattern, [Text.RegularExpressions.RegexOptions]::Singleline)) {
  $updated = [regex]::Replace($existing, $pattern, $block, [Text.RegularExpressions.RegexOptions]::Singleline)
} else {
  $sep = ''; if ($existing -and -not ($existing.EndsWith($nl))) { $sep = $nl }
  $updated = $existing + $sep + $nl + $block + $nl
}

Set-Utf8NoBom -Path $profilePath -Value $updated
Write-Host ("Added Claude Code pool config to " + $profilePath)

# Claude Code reads user settings from CLAUDE_CONFIG_DIR when set, otherwise ~/.claude.
$claudeDir = $env:CLAUDE_CONFIG_DIR
if ([string]::IsNullOrWhiteSpace($claudeDir)) { $claudeDir = Join-Path $HOME '.claude' }
New-Item -ItemType Directory -Force -Path $claudeDir | Out-Null

# Update settings.json with pool env and remove auth/provider settings that override OAuth.
$settingsFile = Join-Path $claudeDir 'settings.json'
$settings = Read-JsonObject -Path $settingsFile
Remove-ObjectProperty -Object $settings -Name 'apiKeyHelper'
$envObj = Get-Property $settings 'env'
if ($null -eq $envObj) { $envObj = New-Object PSObject }
foreach ($name in $conflictingEnvVars) { Remove-ObjectProperty -Object $envObj -Name $name }
$envObj | Add-Member -MemberType NoteProperty -Name ANTHROPIC_BASE_URL -Value $BaseUrl -Force
$envObj | Add-Member -MemberType NoteProperty -Name CLAUDE_CODE_OAUTH_TOKEN -Value $OAuthToken -Force
$settings | Add-Member -MemberType NoteProperty -Name env -Value $envObj -Force
Set-Utf8NoBom -Path $settingsFile -Value ($settings | ConvertTo-Json -Depth 10)
Write-Host ("Updated " + $settingsFile)

# Update ~/.claude.json (skip onboarding)
$claudeJsonFile = Join-Path $HOME '.claude.json'
$claudeJson = Read-JsonObject -Path $claudeJsonFile
$claudeJson | Add-Member -MemberType NoteProperty -Name hasCompletedOnboarding -Value $true -Force
Set-Utf8NoBom -Path $claudeJsonFile -Value ($claudeJson | ConvertTo-Json -Depth 10)
Write-Host ("Updated " + $claudeJsonFile)

Write-Host ''
Write-Host 'Setup complete!'
Write-Host ''
Write-Host ("Claude Code will now use the pool proxy at: " + $BaseUrl)
Write-Host ''
Write-Host 'Start a new terminal, or run: . $PROFILE'
`, publicURL, claudeAuth.AccessToken)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(script))
		return
	}

	script := fmt.Sprintf(`#!/bin/bash
IS_SOURCED=0
if [ -n "$ZSH_VERSION" ]; then
    case $ZSH_EVAL_CONTEXT in *:file) IS_SOURCED=1 ;; esac
elif [ -n "$BASH_VERSION" ]; then
    if [ "${BASH_SOURCE[0]}" != "$0" ]; then IS_SOURCED=1; fi
fi
ERREXIT_WAS_SET=0
case $- in *e*) ERREXIT_WAS_SET=1 ;; esac
set -e
BASE_URL="%s"
OAUTH_TOKEN="%s"

echo "Configuring Claude Code for pool access..."
echo ""

CONFLICTING_ENV_VARS=(
    ANTHROPIC_AUTH_TOKEN
    ANTHROPIC_API_KEY
    CLAUDE_CODE_USE_BEDROCK
    CLAUDE_CODE_USE_VERTEX
    CLAUDE_CODE_USE_FOUNDRY
    CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST
    ANTHROPIC_UNIX_SOCKET
    CLAUDE_CODE_SIMPLE
)

# Claude Code disables Claude.ai OAuth mode when these auth/provider vars are present.
for name in "${CONFLICTING_ENV_VARS[@]}"; do
    unset "$name"
done

# Set env vars in the current shell if this script is sourced
export ANTHROPIC_BASE_URL="$BASE_URL"
export CLAUDE_CODE_OAUTH_TOKEN="$OAUTH_TOKEN"

# Add env vars to shell profile (Claude Code reads tokens from process.env)
add_to_profile() {
    for profile in "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile"; do
        if [ -f "$profile" ]; then
            tmp="$profile.tmp"
            cp "$profile" "$tmp"
            for name in ANTHROPIC_BASE_URL CLAUDE_CODE_OAUTH_TOKEN "${CONFLICTING_ENV_VARS[@]}"; do
                grep -v -E "^[[:space:]]*(export[[:space:]]+)?${name}=" "$tmp" > "$tmp.next" || true
                mv "$tmp.next" "$tmp"
            done
            mv "$tmp" "$profile"

            # Add pool configuration
            cat >> "$profile" << 'ENVEOF'

# Claude Code Pool Configuration
unset ANTHROPIC_AUTH_TOKEN
unset ANTHROPIC_API_KEY
unset CLAUDE_CODE_USE_BEDROCK
unset CLAUDE_CODE_USE_VERTEX
unset CLAUDE_CODE_USE_FOUNDRY
unset CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST
unset ANTHROPIC_UNIX_SOCKET
unset CLAUDE_CODE_SIMPLE
export ANTHROPIC_BASE_URL="%s"
export CLAUDE_CODE_OAUTH_TOKEN="%s"
ENVEOF
            echo "✓ Added Claude Code pool config to $(basename $profile)"
            return
        fi
    done

    # Fallback: create .zshrc
    cat >> "$HOME/.zshrc" << 'ENVEOF'

# Claude Code Pool Configuration
unset ANTHROPIC_AUTH_TOKEN
unset ANTHROPIC_API_KEY
unset CLAUDE_CODE_USE_BEDROCK
unset CLAUDE_CODE_USE_VERTEX
unset CLAUDE_CODE_USE_FOUNDRY
unset CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST
unset ANTHROPIC_UNIX_SOCKET
unset CLAUDE_CODE_SIMPLE
export ANTHROPIC_BASE_URL="%s"
export CLAUDE_CODE_OAUTH_TOKEN="%s"
ENVEOF
    echo "✓ Created ~/.zshrc with Claude Code pool config"
}

CLAUDE_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
mkdir -p "$CLAUDE_DIR"
SETTINGS_FILE="$CLAUDE_DIR/settings.json"
CLAUDE_JSON="$HOME/.claude.json"

# Update settings.json with env vars
update_settings() {
    if command -v node &> /dev/null; then
        node << 'NODE_SCRIPT'
const fs = require('fs');
const path = require('path');
const dir = process.env.CLAUDE_CONFIG_DIR || path.join(process.env.HOME, '.claude');
const file = path.join(dir, 'settings.json');
const conflictingEnvVars = [
  'ANTHROPIC_AUTH_TOKEN',
  'ANTHROPIC_API_KEY',
  'CLAUDE_CODE_USE_BEDROCK',
  'CLAUDE_CODE_USE_VERTEX',
  'CLAUDE_CODE_USE_FOUNDRY',
  'CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST',
  'ANTHROPIC_UNIX_SOCKET',
  'CLAUDE_CODE_SIMPLE',
];
let settings = {};
try { settings = JSON.parse(fs.readFileSync(file, 'utf8')); } catch {}
delete settings.apiKeyHelper;
settings.env = settings.env || {};
for (const name of conflictingEnvVars) delete settings.env[name];
settings.env.ANTHROPIC_BASE_URL = '%s';
settings.env.CLAUDE_CODE_OAUTH_TOKEN = '%s';
fs.writeFileSync(file, JSON.stringify(settings, null, 2) + '\n');
console.log('✓ Updated settings.json (node)');
NODE_SCRIPT
    elif command -v python3 &> /dev/null; then
        python3 << 'PYTHON_SCRIPT'
import json, os
dir = os.environ.get('CLAUDE_CONFIG_DIR') or os.path.expanduser('~/.claude')
file = os.path.join(dir, 'settings.json')
conflicting_env_vars = [
    'ANTHROPIC_AUTH_TOKEN',
    'ANTHROPIC_API_KEY',
    'CLAUDE_CODE_USE_BEDROCK',
    'CLAUDE_CODE_USE_VERTEX',
    'CLAUDE_CODE_USE_FOUNDRY',
    'CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST',
    'ANTHROPIC_UNIX_SOCKET',
    'CLAUDE_CODE_SIMPLE',
]
try:
    with open(file) as f: settings = json.load(f)
except: settings = {}
settings.pop('apiKeyHelper', None)
settings.setdefault('env', {})
for name in conflicting_env_vars:
    settings['env'].pop(name, None)
settings['env']['ANTHROPIC_BASE_URL'] = '%s'
settings['env']['CLAUDE_CODE_OAUTH_TOKEN'] = '%s'
with open(file, 'w') as f: json.dump(settings, f, indent=2); f.write('\n')
print("✓ Updated settings.json (python)")
PYTHON_SCRIPT
    else
        [ -f "$SETTINGS_FILE" ] && cp "$SETTINGS_FILE" "$SETTINGS_FILE.bak"
        cat > "$SETTINGS_FILE" << 'EOF'
{
  "env": {
    "ANTHROPIC_BASE_URL": "%s",
    "CLAUDE_CODE_OAUTH_TOKEN": "%s"
  }
}
EOF
        echo "✓ Created settings.json (bash fallback)"
    fi
}

# Update ~/.claude.json with hasCompletedOnboarding
update_claude_json() {
    if command -v node &> /dev/null; then
        node << 'NODE_SCRIPT'
const fs = require('fs');
const path = require('path');
const file = path.join(process.env.HOME, '.claude.json');
let config = {};
try { config = JSON.parse(fs.readFileSync(file, 'utf8')); } catch {}
config.hasCompletedOnboarding = true;
fs.writeFileSync(file, JSON.stringify(config, null, 2) + '\n');
console.log('✓ Updated .claude.json (node)');
NODE_SCRIPT
    elif command -v python3 &> /dev/null; then
        python3 << 'PYTHON_SCRIPT'
import json, os
file = os.path.expanduser("~/.claude.json")
try:
    with open(file) as f: config = json.load(f)
except: config = {}
config['hasCompletedOnboarding'] = True
with open(file, 'w') as f: json.dump(config, f, indent=2); f.write('\n')
print("✓ Updated .claude.json (python)")
PYTHON_SCRIPT
    else
        [ -f "$CLAUDE_JSON" ] && cp "$CLAUDE_JSON" "$CLAUDE_JSON.bak"
        if [ -f "$CLAUDE_JSON" ]; then
            # Try to preserve existing content (basic append)
            tmp=$(mktemp)
            cat "$CLAUDE_JSON" | sed 's/}$/,"hasCompletedOnboarding":true}/' > "$tmp"
            mv "$tmp" "$CLAUDE_JSON"
        else
            echo '{"hasCompletedOnboarding":true}' > "$CLAUDE_JSON"
        fi
        echo "✓ Updated .claude.json (bash fallback)"
    fi
}

update_settings
update_claude_json
add_to_profile

echo ""
echo "Setup complete!"
echo ""
echo "Claude Code will now use the pool proxy at: $BASE_URL"
echo ""
echo "Run 'source ~/.zshrc' (or ~/.bashrc) or start a new terminal, then run 'claude'."
if [ "$IS_SOURCED" -eq 1 ] && [ "$ERREXIT_WAS_SET" -eq 0 ]; then
    set +e
fi
`, publicURL, claudeAuth.AccessToken,
		publicURL, claudeAuth.AccessToken,
		publicURL, claudeAuth.AccessToken,
		publicURL, claudeAuth.AccessToken, // node
		publicURL, claudeAuth.AccessToken, // python
		publicURL, claudeAuth.AccessToken) // bash fallback

	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Write([]byte(script))
}

// hashAccountID creates a short anonymized hash of an account identifier
func hashAccountID(id string) string {
	h := sha256.Sum256([]byte(id + "pool-salt-2024"))
	return hex.EncodeToString(h[:])[:12]
}

// formatPlanWithTier appends the rate limit tier suffix (e.g. "max 20x") if available.
func formatPlanWithTier(planType, tier string) string {
	switch tier {
	case "default_claude_max_20x":
		return planType + " 20x"
	case "default_claude_max_5x":
		return planType + " 5x"
	default:
		return planType
	}
}

// PoolStats represents anonymized pool statistics
type PoolStats struct {
	TotalAccounts    int               `json:"total_accounts"`
	ActiveAccounts   int               `json:"active_accounts"`
	Accounts         []AccountStats    `json:"accounts"`
	AggregateUsage   AggregateStats    `json:"aggregate"`
	CapacityAnalysis *CapacityAnalysis `json:"capacity_analysis,omitempty"`
	Last24hTokens    int64             `json:"last_24h_tokens"`
	DailyCosts       []DailyCostEntry  `json:"daily_costs,omitempty"`
	CyberPolicy      CyberPolicyStats  `json:"cyber_policy"`
	GeneratedAt      time.Time         `json:"generated_at"`
}

// CyberPolicyStats summarizes how often the cyber_policy safety net
// has fired since process start. Operators read this to alert when
// suppressions happen without successful swaps (i.e. the cyber pool
// is depleted) or when synthetic refusals are being emitted.
type CyberPolicyStats struct {
	// Healthy is true when there's been at least one cyber_policy
	// detection AND every detection was paired with either a
	// successful swap or a buffered/4xx retry. False when suppressions
	// fell back to synthetic refusal (no cyber candidate available).
	Healthy bool `json:"healthy"`
	// CyberCandidatesAvailable is the count of live, non-disabled
	// cyber_access accounts in the pool right now. Zero means the
	// next cyber_policy hit will fall back to a synthetic refusal.
	CyberCandidatesAvailable int `json:"cyber_candidates_available"`
	// Counters by action: "suppressed_ws", "suppressed_sse",
	// "suppressed_buffered", "swap_succeeded", "swap_no_candidate",
	// "retry_buffered", "retry_4xx".
	Counters map[string]int64 `json:"counters"`
	// PerAccount["shiv_1"]["suppressed_ws"] = 5 — useful when you
	// want to see which non-cyber account is hitting the policy
	// classifier most often.
	PerAccount map[string]map[string]int64 `json:"per_account,omitempty"`
}

type AccountStats struct {
	ID                        string            `json:"id"` // hashed
	Type                      string            `json:"type"`
	PlanType                  string            `json:"plan_type"`
	ResetWindows              ResetWindowPolicy `json:"reset_windows"`
	Status                    string            `json:"status"` // healthy, degraded, cooldown, verification_required, dead
	NeedsVerification         bool              `json:"needs_verification,omitempty"`
	HealthBlocked             bool              `json:"health_blocked,omitempty"`
	Penalty                   float64           `json:"penalty"`
	PrimaryWindowUsed         float64           `json:"primary_window_used_pct"`
	SecondaryWindowUsed       float64           `json:"secondary_window_used_pct"`
	PrimaryWindowAvailable    bool              `json:"primary_window_available"`
	SecondaryWindowAvailable  bool              `json:"secondary_window_available"`
	PrimaryUsageReported      bool              `json:"primary_usage_reported"`
	SecondaryUsageReported    bool              `json:"secondary_usage_reported"`
	PrimaryResetMinutes       int               `json:"primary_reset_minutes"`
	SecondaryResetMinutes     int               `json:"secondary_reset_minutes"`
	PrimaryWindowMinutes      int               `json:"primary_window_minutes"`
	SecondaryWindowMinutes    int               `json:"secondary_window_minutes"`
	PrimaryPaceRatio          float64           `json:"primary_pace_ratio"`
	SecondaryPaceRatio        float64           `json:"secondary_pace_ratio"`
	AccountAddedAt            string            `json:"account_added_at,omitempty"`
	TotalInputTokens          int64             `json:"total_input_tokens"`
	TotalCachedTokens         int64             `json:"total_cached_tokens"`
	TotalOutputTokens         int64             `json:"total_output_tokens"`
	TotalReasoningTokens      int64             `json:"total_reasoning_tokens"`
	TotalBillableTokens       int64             `json:"total_billable_tokens"`
	Last24hTokens             int64             `json:"last_24h_tokens"`
	CacheHitRate              float64           `json:"cache_hit_rate_pct"`
	CreditsBalance            float64           `json:"credits_balance,omitempty"`
	HasCredits                bool              `json:"has_credits"`
	Score                     float64           `json:"score"`
	ScoreTooltip              string            `json:"score_tooltip,omitempty"`
	IsPrimary                 bool              `json:"is_primary"` // highest score for this provider type
	SubscriptionCostMonthly   float64           `json:"subscription_cost_monthly"`
	SubscriptionSpend         float64           `json:"subscription_spend"`
	SubscriptionBillingCycles int               `json:"subscription_billing_cycles"`
	CostTrackingStartedAt     string            `json:"cost_tracking_started_at,omitempty"`
	SubscriptionLabel         string            `json:"subscription_label"`
	APICostEstimate           float64           `json:"api_cost_estimate"` // all-time
	APICostLast30d            float64           `json:"api_cost_last_30d"` // last 30 days
	ROI                       float64           `json:"roi"`               // all-time API value / subscription spend
	ResetCreditsAvailable     int               `json:"reset_credits_available"`
	ResetCreditExpirations    []string          `json:"reset_credit_expirations,omitempty"`
	ResetCreditsKnown         bool              `json:"reset_credits_known"`
}

type AggregateStats struct {
	TotalInputTokens         int64                          `json:"total_input_tokens"`
	TotalCachedTokens        int64                          `json:"total_cached_tokens"`
	TotalOutputTokens        int64                          `json:"total_output_tokens"`
	TotalReasoningTokens     int64                          `json:"total_reasoning_tokens"`
	TotalBillableTokens      int64                          `json:"total_billable_tokens"`
	AvgPrimaryUsed           float64                        `json:"avg_primary_window_used_pct"`
	AvgSecondaryUsed         float64                        `json:"avg_secondary_window_used_pct"`
	OverallCacheHitRate      float64                        `json:"overall_cache_hit_rate_pct"`
	TotalAPICost             float64                        `json:"total_api_cost"`
	TotalSubscriptionCost    float64                        `json:"total_subscription_cost"`
	TotalSubscriptionMonthly float64                        `json:"total_subscription_monthly"`
	OverallROI               float64                        `json:"overall_roi"`
	CostByProvider           map[string]ProviderCostSummary `json:"cost_by_provider,omitempty"`
}

// CapacityAnalysis contains token capacity estimation data for the stats API.
type CapacityAnalysis struct {
	TotalSamples int64                       `json:"total_samples"`
	Plans        map[string]PlanCapacityInfo `json:"plans"`
	ModelFormula string                      `json:"model_formula"`
}

type PlanCapacityInfo struct {
	SampleCount                int64   `json:"sample_count"`
	Confidence                 string  `json:"confidence"`
	TotalInputTokens           int64   `json:"total_input_tokens"`
	TotalOutputTokens          int64   `json:"total_output_tokens"`
	TotalCachedTokens          int64   `json:"total_cached_tokens"`
	TotalReasoningTokens       int64   `json:"total_reasoning_tokens"`
	OutputMultiplier           float64 `json:"output_multiplier"`
	EstimatedPrimaryCapacity   int64   `json:"estimated_5h_capacity"`
	EstimatedSecondaryCapacity int64   `json:"estimated_7d_capacity"`
}

// quotaPaceRatio compares current quota consumption with an even burn across
// the elapsed portion of a reset window. Values over one will exhaust the
// window early if the current pace holds.
func quotaPaceRatio(usedPercent float64, resetMinutes, windowMinutes int) float64 {
	if usedPercent <= 0 || resetMinutes <= 0 || windowMinutes <= 0 || resetMinutes >= windowMinutes {
		return 0
	}
	elapsed := windowMinutes - resetMinutes
	// Usage is reported in percentage-point increments. Before one point of
	// even-burn budget has elapsed, extrapolating the first non-zero reading is
	// dominated by measurement resolution (for example, 1% in 23 minutes of a
	// seven-day window appears to be 62.6%/day).
	if float64(elapsed) < float64(windowMinutes)/100 {
		return 0
	}
	return usedPercent / (100 * float64(elapsed) / float64(windowMinutes))
}

func (h *proxyHandler) handlePoolStats(w http.ResponseWriter, r *http.Request) {
	accounts := h.pool.allAccounts()

	stats := PoolStats{
		TotalAccounts: len(accounts),
		Accounts:      []AccountStats{},
		GeneratedAt:   time.Now(),
	}

	var totalInput, totalCached, totalOutput, totalReasoning, totalBillable int64
	var primarySum, secondarySum float64
	var primaryCount, secondaryCount int
	activeCount := 0
	last24hByAccount := make(map[string]int64)
	if h.store != nil {
		if recent, err := h.store.getRecentRequestUsage(1); err == nil {
			for _, usage := range recent {
				throughput := usage.InputTokens + usage.OutputTokens
				if usage.AccountType == AccountTypeClaude {
					throughput += usage.CachedInputTokens
				}
				last24hByAccount[usage.AccountID] += throughput
			}
		}
	}

	for _, acc := range accounts {
		acc.mu.Lock()

		status := "healthy"
		healthBlocked := accountHealthBlockedLocked(acc)
		needsVerification := acc.NeedsVerification || strings.TrimSpace(acc.VerificationURL) != ""
		if acc.Dead || acc.Disabled {
			status = "dead"
		} else if needsVerification {
			status = "verification_required"
		} else if accountCoolingDownLocked(acc, stats.GeneratedAt) || accountUsageExhaustedLocked(acc) {
			status = "cooldown"
		} else if healthBlocked || acc.Penalty > 2.0 {
			status = "degraded"
		}
		if !healthBlocked && (status == "healthy" || status == "degraded") {
			activeCount++
		}

		accType := string(acc.Type)

		cacheHitRate := float64(0)
		if acc.Totals.TotalInputTokens > 0 {
			cacheHitRate = float64(acc.Totals.TotalCachedTokens) / float64(acc.Totals.TotalInputTokens) * 100
		}

		primaryReset := 0
		secondaryReset := 0
		if !acc.Usage.PrimaryResetAt.IsZero() {
			primaryReset = int(time.Until(acc.Usage.PrimaryResetAt).Minutes())
			if primaryReset < 0 {
				primaryReset = 0
			}
		}
		if !acc.Usage.SecondaryResetAt.IsZero() {
			secondaryReset = int(time.Until(acc.Usage.SecondaryResetAt).Minutes())
			if secondaryReset < 0 {
				secondaryReset = 0
			}
		}

		primaryUsed := accountPrimaryUsageLocked(acc) * 100
		secondaryUsed := accountSecondaryUsageLocked(acc) * 100
		primaryAvailable := usagePrimaryWindowAvailable(acc.Usage)
		secondaryAvailable := usageSecondaryWindowAvailable(acc.Usage)
		primaryReported := primaryAvailable
		secondaryReported := secondaryAvailable
		if acc.Type == AccountTypeAntigravity || acc.Type == AccountTypeZAI {
			primaryReported = acc.Usage.PrimaryUsageReported
			secondaryReported = acc.Usage.SecondaryUsageReported
		}

		breakdown := scoreBreakdown{}
		score := float64(0)
		if !acc.Dead && !acc.Disabled && !healthBlocked {
			breakdown = scoreAccountBreakdownLocked(acc, stats.GeneratedAt)
			score = breakdown.Score
		}
		scoreTooltip := scoreTooltipFromBreakdownLocked(acc, stats.GeneratedAt, breakdown)

		as := AccountStats{
			ID:                       hashAccountID(acc.ID),
			Type:                     accType,
			PlanType:                 formatPlanWithTier(acc.PlanType, acc.RateLimitTier),
			ResetWindows:             resetWindowPolicy(acc.Type, acc.PlanType),
			Status:                   status,
			NeedsVerification:        needsVerification,
			HealthBlocked:            healthBlocked,
			Penalty:                  acc.Penalty,
			PrimaryWindowUsed:        primaryUsed,
			SecondaryWindowUsed:      secondaryUsed,
			PrimaryWindowAvailable:   primaryAvailable,
			SecondaryWindowAvailable: secondaryAvailable,
			PrimaryUsageReported:     primaryReported,
			SecondaryUsageReported:   secondaryReported,
			PrimaryResetMinutes:      primaryReset,
			SecondaryResetMinutes:    secondaryReset,
			PrimaryWindowMinutes:     acc.Usage.PrimaryWindowMinutes,
			SecondaryWindowMinutes:   acc.Usage.SecondaryWindowMinutes,
			PrimaryPaceRatio:         quotaPaceRatio(primaryUsed, primaryReset, acc.Usage.PrimaryWindowMinutes),
			SecondaryPaceRatio:       quotaPaceRatio(secondaryUsed, secondaryReset, acc.Usage.SecondaryWindowMinutes),
			AccountAddedAt:           acc.AddedAt.UTC().Format(time.RFC3339),
			TotalInputTokens:         acc.Totals.TotalInputTokens,
			TotalCachedTokens:        acc.Totals.TotalCachedTokens,
			TotalOutputTokens:        acc.Totals.TotalOutputTokens,
			TotalReasoningTokens:     acc.Totals.TotalReasoningTokens,
			TotalBillableTokens:      acc.Totals.TotalBillableTokens,
			Last24hTokens:            last24hByAccount[acc.ID],
			CacheHitRate:             cacheHitRate,
			HasCredits:               acc.Usage.HasCredits,
			CreditsBalance:           acc.Usage.CreditsBalance,
			Score:                    score,
			ScoreTooltip:             scoreTooltip,
			ResetCreditsAvailable:    acc.ResetCreditsAvailable,
			ResetCreditsKnown:        !acc.ResetCreditsRetrievedAt.IsZero(),
		}
		for _, credit := range acc.RateLimitResetCredits {
			as.ResetCreditExpirations = append(as.ResetCreditExpirations, credit.ExpiresAt.UTC().Format(time.RFC3339Nano))
		}

		totalInput += acc.Totals.TotalInputTokens
		totalCached += acc.Totals.TotalCachedTokens
		totalOutput += acc.Totals.TotalOutputTokens
		totalReasoning += acc.Totals.TotalReasoningTokens
		totalBillable += acc.Totals.TotalBillableTokens
		if primaryReported {
			primarySum += accountPrimaryUsageLocked(acc)
			primaryCount++
		}
		if secondaryReported {
			secondarySum += accountSecondaryUsageLocked(acc)
			secondaryCount++
		}

		acc.mu.Unlock()
		stats.Accounts = append(stats.Accounts, as)
	}

	// Mark the highest-scoring account per provider type as primary
	highestScore := make(map[string]float64)
	highestIdx := make(map[string]int)
	for i, as := range stats.Accounts {
		if !as.HealthBlocked && (as.Status == "healthy" || as.Status == "degraded") && as.Score > highestScore[as.Type] {
			highestScore[as.Type] = as.Score
			highestIdx[as.Type] = i
		}
	}
	for _, idx := range highestIdx {
		stats.Accounts[idx].IsPrimary = true
	}

	stats.ActiveAccounts = activeCount

	overallCacheRate := float64(0)
	if totalInput > 0 {
		overallCacheRate = float64(totalCached) / float64(totalInput) * 100
	}

	avgPrimary := float64(0)
	avgSecondary := float64(0)
	if primaryCount > 0 {
		avgPrimary = (primarySum / float64(primaryCount)) * 100
	}
	if secondaryCount > 0 {
		avgSecondary = (secondarySum / float64(secondaryCount)) * 100
	}

	stats.AggregateUsage = AggregateStats{
		TotalInputTokens:     totalInput,
		TotalCachedTokens:    totalCached,
		TotalOutputTokens:    totalOutput,
		TotalReasoningTokens: totalReasoning,
		TotalBillableTokens:  totalBillable,
		AvgPrimaryUsed:       avgPrimary,
		AvgSecondaryUsed:     avgSecondary,
		OverallCacheHitRate:  overallCacheRate,
	}

	// Load capacity analysis from store
	if h.store != nil {
		caps, err := h.store.loadAllPlanCapacity()
		if err == nil && len(caps) > 0 {
			analysis := &CapacityAnalysis{
				Plans:        make(map[string]PlanCapacityInfo),
				ModelFormula: "effective = input + (cached × 0.1) + (output × mult) + (reasoning × mult)",
			}
			for planType, cap := range caps {
				analysis.TotalSamples += cap.SampleCount
				confidence := "low"
				if cap.SampleCount >= 20 {
					confidence = "high"
				} else if cap.SampleCount >= 5 {
					confidence = "medium"
				}
				mult := cap.OutputMultiplier
				if mult == 0 {
					mult = 4.0
				}
				var estPrimary, estSecondary int64
				if cap.EffectivePerPrimaryPct > 0 {
					estPrimary = int64(cap.EffectivePerPrimaryPct)
				}
				if cap.EffectivePerSecondaryPct > 0 {
					estSecondary = int64(cap.EffectivePerSecondaryPct)
				}
				analysis.Plans[planType] = PlanCapacityInfo{
					SampleCount:                cap.SampleCount,
					Confidence:                 confidence,
					TotalInputTokens:           cap.TotalInputTokens,
					TotalOutputTokens:          cap.TotalOutputTokens,
					TotalCachedTokens:          cap.TotalCachedTokens,
					TotalReasoningTokens:       cap.TotalReasoningTokens,
					OutputMultiplier:           mult,
					EstimatedPrimaryCapacity:   estPrimary,
					EstimatedSecondaryCapacity: estSecondary,
				}
			}
			stats.CapacityAnalysis = analysis
		}
	}

	// Include last 24h tokens aggregate from hourly buckets
	if h.store != nil {
		if hourly, err := h.store.getGlobalHourlyUsage(24); err == nil {
			for _, hu := range hourly {
				throughput := hu.InputTokens + hu.OutputTokens
				if hu.AccountType == string(AccountTypeClaude) {
					throughput += hu.CachedTokens
				}
				stats.Last24hTokens += throughput
			}
		}
	}

	// Populate cost data from analytics store
	if h.analyticsStore != nil {
		// Per-account costs and their measurement periods.
		allTimeCostStats, _ := h.analyticsStore.getAllTimeAccountCostStats()
		last30dCosts, _ := h.analyticsStore.getCostByAccount(30)

		// Build account ID -> real ID mapping for cost lookup
		// (stats use hashed IDs, costs use real IDs)
		accountIDMap := make(map[string]string) // hashed -> real
		accountAddedAt := make(map[string]time.Time)
		for _, acc := range accounts {
			acc.mu.Lock()
			accountIDMap[hashAccountID(acc.ID)] = acc.ID
			accountAddedAt[acc.ID] = acc.AddedAt
			acc.mu.Unlock()
		}

		// Update per-account cost fields
		for i := range stats.Accounts {
			as := &stats.Accounts[i]
			realID := accountIDMap[as.ID]

			subCost, subLabel := getSubscriptionCost(AccountType(as.Type), accountPlanForSubscription(as.PlanType))
			if acc := h.pool.getLocked(realID); acc != nil {
				acc.mu.Lock()
				if acc.MonthlyCost > 0 {
					subCost = acc.MonthlyCost
				}
				acc.mu.Unlock()
			}
			costStats := allTimeCostStats[realID]
			as.SubscriptionCostMonthly = subCost
			as.SubscriptionLabel = subLabel
			as.APICostEstimate = costStats.CostUSD
			as.APICostLast30d = last30dCosts[realID]
			spendStart := accountAddedAt[realID]
			if spendStart.IsZero() {
				// Legacy in-memory fixtures can lack an admission timestamp. Keep
				// historical cost data as their compatibility fallback only.
				spendStart = costStats.FirstSeen
			}
			if spendStart.IsZero() {
				// A paid account with no recorded API use still consumed its current
				// subscription billing cycle.
				spendStart = stats.GeneratedAt
			}
			as.CostTrackingStartedAt = spendStart.UTC().Format(time.RFC3339)
			as.SubscriptionSpend, as.SubscriptionBillingCycles = estimateSubscriptionSpend(subCost, spendStart, stats.GeneratedAt)
			if as.SubscriptionSpend > 0 {
				as.ROI = as.APICostEstimate / as.SubscriptionSpend
			}
		}

		// Provider-level aggregation
		providerCosts := make(map[string]ProviderCostSummary)
		for i := range stats.Accounts {
			as := &stats.Accounts[i]
			pcs := providerCosts[as.Type]
			pcs.APICost += as.APICostEstimate
			pcs.SubscriptionCost += as.SubscriptionSpend
			pcs.MonthlySubscriptionCost += as.SubscriptionCostMonthly
			pcs.AccountCount++
			providerCosts[as.Type] = pcs
		}
		for k, pcs := range providerCosts {
			if pcs.SubscriptionCost > 0 {
				pcs.ROI = pcs.APICost / pcs.SubscriptionCost
			}
			providerCosts[k] = pcs
		}
		stats.AggregateUsage.CostByProvider = providerCosts

		// Pool-level ROI covers the same current-account set on both sides.
		// Historical API value from removed accounts cannot be compared with the
		// subscription spend of only the accounts still in the pool.
		var totalAPICost, totalSubCost, totalSubMonthly float64
		for _, pcs := range providerCosts {
			totalAPICost += pcs.APICost
			totalSubCost += pcs.SubscriptionCost
			totalSubMonthly += pcs.MonthlySubscriptionCost
		}
		stats.AggregateUsage.TotalAPICost = totalAPICost
		stats.AggregateUsage.TotalSubscriptionCost = totalSubCost
		stats.AggregateUsage.TotalSubscriptionMonthly = totalSubMonthly
		if totalSubCost > 0 {
			stats.AggregateUsage.OverallROI = totalAPICost / totalSubCost
		}

		// Daily cost trend for chart
		if dailyCosts, err := h.analyticsStore.getDailyCosts(30); err == nil {
			stats.DailyCosts = dailyCosts
		}
	}

	stats.CyberPolicy = h.computeCyberPolicyStats(accounts)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// computeCyberPolicyStats summarises the metrics counters for the
// pool-stats UI/API. Healthy means: either no suppressions fired yet,
// or every suppression event was paired with a successful swap or a
// buffered/4xx retry — i.e. no synthetic-refusal fallbacks AND there's
// still a cyber candidate available for the next hit.
func (h *proxyHandler) computeCyberPolicyStats(accounts []*Account) CyberPolicyStats {
	out := CyberPolicyStats{
		Counters:   map[string]int64{},
		PerAccount: map[string]map[string]int64{},
	}

	now := time.Now()
	for _, a := range accounts {
		a.mu.Lock()
		if a.CyberAccess && !a.Dead && !a.Disabled && !accountHealthBlockedLocked(a) &&
			(a.ExpiresAt.IsZero() || a.ExpiresAt.After(now) || h.cfg.disableRefresh) {
			out.CyberCandidatesAvailable++
		}
		a.mu.Unlock()
	}

	if h.metrics == nil {
		out.Healthy = out.CyberCandidatesAvailable > 0
		return out
	}

	snap := h.metrics.cyberPolicySnapshot()
	for k, v := range snap {
		out.Counters[k.action] += v
		if k.account != "" {
			perAcc, ok := out.PerAccount[k.account]
			if !ok {
				perAcc = map[string]int64{}
				out.PerAccount[k.account] = perAcc
			}
			perAcc[k.action] += v
		}
	}

	suppressions := out.Counters["suppressed_ws"] + out.Counters["suppressed_sse"] + out.Counters["suppressed_buffered"]
	resolutions := out.Counters["swap_succeeded"] + out.Counters["retry_buffered"] + out.Counters["retry_4xx"]
	noCandidate := out.Counters["swap_no_candidate"]

	switch {
	case suppressions == 0:
		out.Healthy = out.CyberCandidatesAvailable > 0
	case noCandidate > 0 || resolutions < suppressions:
		out.Healthy = false
	default:
		out.Healthy = out.CyberCandidatesAvailable > 0
	}
	return out
}

// accountPlanForSubscription normalizes plan type strings for subscription cost lookup.
func accountPlanForSubscription(planType string) string {
	// The plan type from the pool may include tier info like "pro 5x", "pro 20x"
	// Normalize for subscription lookup
	pt := strings.ToLower(strings.TrimSpace(planType))
	switch {
	case strings.Contains(pt, "20x"):
		return "max_20x"
	case strings.Contains(pt, "5x"):
		return "max_5x"
	case strings.Contains(pt, "team"):
		return "team"
	case strings.Contains(pt, "prolite"):
		return "prolite"
	case strings.Contains(pt, "pro"):
		return "pro"
	case strings.Contains(pt, "plus"):
		return "plus"
	case strings.Contains(pt, "coding"):
		return "coding_plan"
	case strings.Contains(pt, "zai"):
		return "coding_plan"
	default:
		return pt
	}
}

// handleWhoami reports only live Passport client credentials as pool users.
func (h *proxyHandler) handleWhoami(w http.ResponseWriter, r *http.Request) {
	var userID string
	var userType string
	originID := hashRequestOrigin(r, h.originHashSalt())
	if h.passport != nil {
		identity, issuedAt, _, parsed := parsePoolCredentialRequest(r, getPoolJWTSecret())
		if parsed {
			if _, _, allowed := h.passport.authorizeIssuedCredential(identity, issuedAt); allowed {
				userID = identity
				userType = "pool_user"
			}
		}
	}

	if userID == "" {
		userID = strings.TrimPrefix(originID, "ip_")
		userType = "anonymous"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"user_id":   userID,
		"origin_id": originID,
		"type":      userType,
	})
}

// PoolUserStats represents a user's usage for the leaderboard.
type PoolUserStats struct {
	UserID              string    `json:"user_id"`
	TotalBillableTokens int64     `json:"total_billable_tokens"`
	TotalInputTokens    int64     `json:"total_input_tokens"`
	TotalOutputTokens   int64     `json:"total_output_tokens"`
	RequestCount        int64     `json:"request_count"`
	FirstSeen           time.Time `json:"first_seen"`
	LastSeen            time.Time `json:"last_seen"`
}

type PoolOriginStats struct {
	OriginID            string    `json:"origin_id"`
	TotalBillableTokens int64     `json:"total_billable_tokens"`
	TotalInputTokens    int64     `json:"total_input_tokens"`
	TotalOutputTokens   int64     `json:"total_output_tokens"`
	RequestCount        int64     `json:"request_count"`
	FirstSeen           time.Time `json:"first_seen"`
	LastSeen            time.Time `json:"last_seen"`
}

type AdminOriginStats struct {
	OriginID            string    `json:"origin_id"`
	RawIP               string    `json:"raw_ip,omitempty"`
	LastUserID          string    `json:"last_user_id,omitempty"`
	LastUserAgent       string    `json:"last_user_agent,omitempty"`
	LastPath            string    `json:"last_path,omitempty"`
	TotalBillableTokens int64     `json:"total_billable_tokens"`
	TotalInputTokens    int64     `json:"total_input_tokens"`
	TotalOutputTokens   int64     `json:"total_output_tokens"`
	RequestCount        int64     `json:"request_count"`
	FirstSeen           time.Time `json:"first_seen"`
	LastSeen            time.Time `json:"last_seen"`
}

// handlePoolUsers returns the public leaderboard of all users' usage.
func (h *proxyHandler) handlePoolUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.getAllUserUsage()
	if err != nil {
		http.Error(w, "failed to fetch user usage", http.StatusInternalServerError)
		return
	}

	// Convert to API format
	stats := make([]PoolUserStats, len(users))
	for i, u := range users {
		stats[i] = PoolUserStats{
			UserID:              u.UserID,
			TotalBillableTokens: u.TotalBillableTokens,
			TotalInputTokens:    u.TotalInputTokens,
			TotalOutputTokens:   u.TotalOutputTokens,
			RequestCount:        u.RequestCount,
			FirstSeen:           u.FirstSeen,
			LastSeen:            u.LastSeen,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"users":       stats,
		"total_users": len(stats),
	})
}

// handlePoolOrigins returns the public leaderboard of hashed incoming origin usage.
func (h *proxyHandler) handlePoolOrigins(w http.ResponseWriter, r *http.Request) {
	origins, err := h.store.getAllOriginUsage()
	if err != nil {
		http.Error(w, "failed to fetch origin usage", http.StatusInternalServerError)
		return
	}

	stats := make([]PoolOriginStats, len(origins))
	for i, origin := range origins {
		stats[i] = PoolOriginStats{
			OriginID:            origin.OriginID,
			TotalBillableTokens: origin.TotalBillableTokens,
			TotalInputTokens:    origin.TotalInputTokens,
			TotalOutputTokens:   origin.TotalOutputTokens,
			RequestCount:        origin.RequestCount,
			FirstSeen:           origin.FirstSeen,
			LastSeen:            origin.LastSeen,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"origins":       stats,
		"total_origins": len(stats),
	})
}

// handleAdminOrigins returns hashed origin usage joined with admin-only raw attribution metadata.
func (h *proxyHandler) handleAdminOrigins(w http.ResponseWriter, r *http.Request) {
	originFilter := strings.TrimSpace(r.URL.Query().Get("origin_id"))

	origins, err := h.store.getAllOriginUsage()
	if err != nil {
		http.Error(w, "failed to fetch origin usage", http.StatusInternalServerError)
		return
	}
	metas, err := h.store.getAllOriginMetadata()
	if err != nil {
		http.Error(w, "failed to fetch origin metadata", http.StatusInternalServerError)
		return
	}

	metaByID := make(map[string]OriginMetadata, len(metas))
	for _, meta := range metas {
		metaByID[meta.OriginID] = meta
	}

	stats := make([]AdminOriginStats, 0, len(origins))
	for _, origin := range origins {
		if originFilter != "" && origin.OriginID != originFilter {
			continue
		}
		meta := metaByID[origin.OriginID]
		firstSeen := origin.FirstSeen
		if firstSeen.IsZero() {
			firstSeen = meta.FirstSeen
		}
		lastSeen := origin.LastSeen
		if lastSeen.IsZero() {
			lastSeen = meta.LastSeen
		}
		stats = append(stats, AdminOriginStats{
			OriginID:            origin.OriginID,
			RawIP:               h.rawIPForAdmin(meta.RawIP),
			LastUserID:          meta.LastUserID,
			LastUserAgent:       meta.LastUserAgent,
			LastPath:            meta.LastPath,
			TotalBillableTokens: origin.TotalBillableTokens,
			TotalInputTokens:    origin.TotalInputTokens,
			TotalOutputTokens:   origin.TotalOutputTokens,
			RequestCount:        origin.RequestCount,
			FirstSeen:           firstSeen,
			LastSeen:            lastSeen,
		})
	}

	if originFilter != "" && len(stats) == 0 {
		if meta, ok := metaByID[originFilter]; ok {
			stats = append(stats, AdminOriginStats{
				OriginID:      meta.OriginID,
				RawIP:         meta.RawIP,
				LastUserID:    meta.LastUserID,
				LastUserAgent: meta.LastUserAgent,
				LastPath:      meta.LastPath,
				FirstSeen:     meta.FirstSeen,
				LastSeen:      meta.LastSeen,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"origins":       stats,
		"total_origins": len(stats),
	})
}

// handleDailyBreakdown returns combined daily token usage from all accounts.
func (h *proxyHandler) handleDailyBreakdown(w http.ResponseWriter, r *http.Request) {
	type DayUsage struct {
		Date     string             `json:"date"`
		Surfaces map[string]float64 `json:"surfaces"`
		Total    float64            `json:"total"`
	}

	// Aggregate daily data from all accounts
	combined := make(map[string]*DayUsage) // date -> usage

	accounts := h.pool.allAccounts()
	for _, acc := range accounts {
		if acc.Type != AccountTypeCodex || acc.Dead {
			continue
		}

		data, err := h.fetchDailyBreakdownData(acc)
		if err != nil {
			continue
		}

		for _, day := range data {
			if combined[day.Date] == nil {
				combined[day.Date] = &DayUsage{
					Date:     day.Date,
					Surfaces: make(map[string]float64),
				}
			}
			for surface, val := range day.Surfaces {
				combined[day.Date].Surfaces[surface] += val
				combined[day.Date].Total += val
			}
		}
	}

	// Convert to sorted slice
	var result []DayUsage
	for _, v := range combined {
		result = append(result, *v)
	}
	// Sort by date
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].Date > result[j].Date {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"days":          result,
		"account_count": len(accounts),
	})
}

// handleUserDaily returns a user's daily usage over the last N days.
func (h *proxyHandler) handleUserDaily(w http.ResponseWriter, r *http.Request) {
	// Extract user ID from path: /api/pool/users/:id/daily
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/api/pool/users/")
	path = strings.TrimSuffix(path, "/daily")
	userID := path

	if userID == "" {
		http.Error(w, "user ID required", http.StatusBadRequest)
		return
	}

	// Get days parameter (default 30)
	days := 30
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}

	daily, err := h.store.getUserDailyUsage(userID, days)
	if err != nil {
		http.Error(w, "failed to fetch daily usage", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"user_id": userID,
		"days":    days,
		"daily":   daily,
	})
}

// handleUserHourly returns a user's hourly usage over the last N hours.
func (h *proxyHandler) handleUserHourly(w http.ResponseWriter, r *http.Request) {
	// Extract user ID from path: /api/pool/users/:id/hourly
	path := r.URL.Path
	path = strings.TrimPrefix(path, "/api/pool/users/")
	path = strings.TrimSuffix(path, "/hourly")
	userID := path

	if userID == "" {
		http.Error(w, "user ID required", http.StatusBadRequest)
		return
	}

	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	hourly, err := h.store.getUserHourlyUsage(userID, hours)
	if err != nil {
		http.Error(w, "failed to fetch hourly usage", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"user_id": userID,
		"hours":   hours,
		"hourly":  hourly,
	})
}

// handleGlobalHourly returns global hourly usage (all users combined) over the last N hours.
func (h *proxyHandler) handleGlobalHourly(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if hParam := r.URL.Query().Get("hours"); hParam != "" {
		if n, err := strconv.Atoi(hParam); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	hourly, err := h.store.getGlobalHourlyUsage(hours)
	if err != nil {
		http.Error(w, "failed to fetch hourly usage", http.StatusInternalServerError)
		return
	}

	// Calculate aggregate totals for the period
	var totalBillable, totalInput, totalOutput, totalCached, totalReasoning, totalRequests int64
	for _, h := range hourly {
		totalBillable += h.BillableTokens
		totalInput += h.InputTokens
		totalOutput += h.OutputTokens
		totalCached += h.CachedTokens
		totalReasoning += h.ReasoningTokens
		totalRequests += h.RequestCount
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"hours":  hours,
		"hourly": hourly,
		"totals": map[string]int64{
			"billable_tokens":  totalBillable,
			"input_tokens":     totalInput,
			"output_tokens":    totalOutput,
			"cached_tokens":    totalCached,
			"reasoning_tokens": totalReasoning,
			"request_count":    totalRequests,
		},
	})
}
