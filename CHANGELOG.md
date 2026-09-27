# Changelog

Formato por fase do roadmap (`codex-pool-roadmap.md`, local). Datas em UTC.

## 2026-09-27 (revisão)

Correções sobre a entrega de CP-00/CP-01/CP-02/CP-03.

### CP-02 — Vault não era contornado em dois caminhos
- `replaceCodexAccountCredentials` (re-login do Codex) e `saveZAIOAuthAccount`
  (OAuth Z.ai) gravavam JSON **em texto claro** com `os.WriteFile`/`os.OpenFile`
  mesmo com `POOL_CREDENTIAL_KEY` configurado. Ambos agora passam por
  `writeAccountFile`.
- Novo helper `writeAccountFile`/`writeFileAtomic` em `credential_vault.go`;
  `atomicWriteJSON`, os writes admin de codex/grok/kimi e a migração/rollback
  do vault foram unificados nele. A migração passou a escrever de forma
  atômica (antes usava `os.WriteFile`, que pode truncar o arquivo de
  credencial num crash).
- Regressão: `credential_vault_coverage_test.go`
  (`TestInvariantNoCredentialFileBypassesTheVault`,
  `TestCredentialWritersGoThroughTheVault`) — falha se qualquer escritor
  voltar a gravar sem `Encode`.

### CP-01 —defaults e fail-closed
- `ip_privacy` passou a ser `*bool`: ausente no TOML ou no ambiente significa
  **ON**, e só `ip_privacy = false` / `PROXY_IP_PRIVACY=false` desliga. Antes o
  zero-value do bool deixava o controle OFF por padrão, contrariando o
  requisito de não persistir IP cru por padrão.
- `verifySensitiveFilePermissions` deixou de ser warn-only: arquivo de
  credencial group/world acessível agora **aborta o startup** com a lista dos
  caminhos e o comando de correção. O diretório do pool, que em bind mount
  host costuma chegar 0755, é corrigido para 0700 em vez de derrubar o
  processo. Criação de diretório de pool passou a 0700 nos três handlers.
- Cobertura POSIX: `TestVerifySensitiveFilePermissionsRemediatesDirectory`.

### CP-00 — baseline e gate de benchmark
- `TestBenchmarkRunnerAndBaselineComparison` era tautológico: salvava o report
  freshly em `t.TempDir()` como baseline e comparava o report com ele mesmo,
  então `HasRegression` nunca podia ser verdadeiro. Substituído por
  `TestCommittedBaselineIsLoadableAndComplete`,
  `TestCompareAgainstBaselineDetectsRegression` e
  `TestComparisonWithoutUsableBaselineIsNotReportedAsSuccess`.
- `MetricDelta.NoBaseline` marca métrica sem baseline utilizável. O baseline
  commitado foi gravado em modo mock (`total_duration_ms`/`ttft_ms`/
  `tokens_per_second` zerados) e, com o short-circuit em `base == 0`, ele
  reportava "OK" para qualquer latência. Agora a saída mostra `NO BASELINE` e
  um aviso explícito, em vez de sucesso silencioso.
- `internal/accountstate`: corrigida a indentação do comentário perdida e
  commitado o fix de paridade que estava pendente no working tree
  (`expired` é routável; `CredentialsExpired` fica abaixo de
  `HealthError`/`RateLimited` na precedência).

### CI
- `npm test` (vitest, 33 testes em 4 arquivos) nunca era executado; incluído.
- Step de bench renomeado e documentado: valida a integridade do harness e do
  baseline versionado, **não** é gate de performance enquanto o baseline não
  for re-gravado contra um alvo real.
- Verificado nesta revisão: `go vet`, `go test -race ./...` (44s) e
  `npm test` verdes em Linux `golang:1.26-bookworm` + gcc, e
  `go test -race ./...` verde no host Windows.

### Documentação
- `README.md` ganhou a seção "Security hardening" (atribuição de IP, política
  de acesso, privacidade, headers, redaction, permissões, vault e rotação).
- `config.toml.example` documenta `trusted_proxies`, `ip_access_allow`,
  `ip_access_deny`, `ip_privacy`, `origin_hash_window_hours`,
  `origin_retention_days` e o vault.

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
