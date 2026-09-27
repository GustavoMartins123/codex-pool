# Changelog

Formato por fase do roadmap (`codex-pool-roadmap.md`, local). Datas em UTC.

## 2026-09-27

### Fase 0 (CP-00-real) — Portões verdes e decomposição
- Suite de testes verde no host Windows: auditorias de dados pulam quando o
  banco não existe (`antigravity_token_audit_test.go`), testes que executam
  scripts bash pulam no Windows, asserção de permissão POSIX-only.
- CI GitHub Actions (`.github/workflows/ci.yml`): build do frontend embutido,
  `go vet`, `go test -race ./...`, bench smoke. Nota: ainda não executado no
  GitHub (exige push); comandos equivalentes validados em container Linux
  `golang:1.26-bookworm`.
- Baseline de benchmark regenerada em modo mock (`bench/baselines/baseline.json`).
- Regra de decomposição: código de domínio novo em `internal/`.

### CP-01 — Security Hardening 2.0
- Headers de segurança baseline em todo response (`security_headers.go`):
  nosniff, Referrer-Policy, Permissions-Policy, CSP `frame-ancestors 'none'`,
  `Cache-Control: no-store` em superfícies autenticadas/credenciais.
- Política de acesso por IP/CIDR (`ip_access.go`): `PROXY_IP_ALLOW` /
  `PROXY_IP_DENY` (deny vence, loopback sempre permitido, testado contra
  spoof de X-Forwarded-For).
- Modo de privacidade de IP (`ip_privacy.go`, `PROXY_IP_PRIVACY` default on):
  sem persistência de raw IP (wipe no startup), traces com hash salted,
  `PROXY_ORIGIN_HASH_WINDOW_HOURS` (rotação por janela),
  `PROXY_ORIGIN_RETENTION_DAYS` (poda diária de metadados de origin).
- Redaction centralizada (`logredact.go`): `redactSecrets` aplicado via
  `safeText`, funil de logs/traces/erros (bearer/basic, sk-, JWT, pares
  key:value de segredo).
- Verificação de permissões de arquivos sensíveis no startup
  (`file_permissions.go`, POSIX).

### CP-02 — Credential Vault local
- `internal/credstore`: envelopes AES-256-GCM versionados por chave
  (`{"cpvault":1,"kv":N,...}`), nonce por registro, AAD de formato.
- Integração (`credential_vault.go`): leituras por `readAccountFile`,
  `atomicWriteJSON` e writes admin codificam antes de gravar; migração
  automática no startup; falha fechada sem chave; rollback via
  `-decrypt-credentials`; rotação com `POOL_CREDENTIAL_KEY_PREVIOUS`.
- Verificado por regressão que nenhuma superfície admin devolve tokens
  upstream ao browser (`admin_no_token_leak_test.go`).

### CP-03 — Modelo de estado unificado (v1, projeção)
- `internal/accountstate`: 12 estados, derivação por `Facts`, tabela de
  transições com ressurreição e re-enable, razões.
- Projeção (`account_state.go`): observação no poll de usage + seed inicial,
  histórico por conta (cap 20), `/admin/accounts` expõe
  `state`/`state_reason`/`state_routable`/`state_transitions`.
- Invariant de paridade estado↔gate de roteamento
  (`TestUnifiedStateMatchesRoutingAvailability`). Descoberta: credenciais
  expiradas seguem roteáveis (refresh on-demand).
- Débitos v2: DRAINING/MAINTENANCE autoritativos (depende do CP-04),
  persistência do histórico, ator administrativo nas transições.

### Fixes de operação (fora do roadmap)
- Hold de requisições sob exaustão de uso (`exhaustion_wait.go`,
  `PROXY_EXHAUSTION_WAIT_SECONDS` / `PROXY_EXHAUSTION_PREFER_WAIT`):
  espera pelo reset da janela 5h/semanal em vez de 503 imediato; fallback
  recusado não estranda a requisição; preferência por esperar o provider
  original quando o reset cabe no orçamento.
- `dev-start.ps1` corrigido (typo `POXY_DB_PATH`→`PROXY_DB_PATH`, caminho
  relativo, auto-build); build caches desrastreados do git.
