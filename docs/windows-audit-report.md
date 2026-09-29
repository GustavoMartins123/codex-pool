# Auditoria adversarial de testes — Windows — codex-pool

Data: 2026-09-28. Escopo: quebrar o codex-pool sistematicamente no Windows e
transformar cada comportamento incorreto em teste reproduzível. **Nenhum bug
de produção foi corrigido nesta fase.**

## Ambiente Windows

| Componente  | Valor |
|-------------|-------|
| Windows     | Windows 11 x64 (PowerShell 5.1.26100.9444) |
| Go          | go1.26.1 windows/amd64 (GOROOT `C:\Program Files\Go`) |
| Node/npm    | v24.15.0 / 11.12.1 |
| Git         | 2.48.1.windows.1 |
| GOOS/GOARCH | windows / amd64 |
| CGO_ENABLED | 1 |
| CC          | `C:\msys64\ucrt64\bin\gcc.exe` |
| CXX         | `g++` |
| MSYS2/UCRT64| `C:\msys64\ucrt64\bin` (global; não há `.toolchains\msys64` local) |
| GCC         | 15.2.0 (Rev14, MSYS2) — exatamente a versão pinada pelo build.ps1 |

CI (`.github/workflows/ci.yml`) roda **apenas em ubuntu-latest**: nenhum job
Windows existe. Todos os achados abaixo são invisíveis para o CI atual.

## Baseline

| Comando | Resultado |
|---------|-----------|
| `go vet ./...` | OK |
| `go test -count=1 ./...` | OK (raiz 19.5s) |
| `go test -race -count=1 ./...` | OK |
| `go test -shuffle=on -count=1 ./...` | OK |
| `go test -shuffle=on -count=5 ./...` | OK |
| `go test -race -shuffle=on -count=3 ./...` | OK |
| `web: npm ci && npm test` | 44/44 em 5 arquivos |
| `web: npm run build` | OK |

Após a auditoria (com os novos testes): `go test ./...` e `-race` falham
**exatamente** nos 11 testes vermelhos intencionais listados abaixo;
`-race` não reporta nenhum `DATA RACE` novo; `go vet` OK.

## Build Windows

`.\scripts\windows\build.ps1`: OK — produz `dist\windows-amd64\codex-pool.exe`
(npm ci + vite build + go build com CGO/UCRT64). O mesmo script funciona de
outro CWD e em path com espaços (worktree `...\opencode\Codex Pool Audit`).

Observações de documentação (não são falhas de runtime):
- O README alega um shim `duckdb_windows_shim.go` "resolvendo emutls e
  std::call_once para GCC 15/16+". **Esse arquivo não existe no repositório.**
  O linkage DuckDB vem das libs estáticas pré-construídas do módulo
  `duckdb-go-bindings/lib/windows-amd64` + `-lstdc++ -lm --static` do UCRT64.
- O build.ps1 rejeita qualquer GCC ≠ 15.2.0, enquanto o README afirma
  suporte a GCC 16+. Contradição a esclarecer.

## Testes adicionados

Commits (todos na ordem abaixo):

| Commit | Conteúdo |
|--------|----------|
| `e3b6afc` | `windows_audit_test.go`, `windows_locked_file_test.go` — rename atômico vs arquivos abertos, watcher, sidecar de gap |
| `0c8f4a9` | `windows_audit_config_test.go` — watch de config (replace atômico, CRLF, case), restore rasgado |
| `90a8373` | `sse_fuzz_audit_test.go`, `duckdb_windows_audit_test.go` — fuzz do framer SSE, leak de LF na supressão, paths hostis |
| `e64ae77` | `startup_audit_test.go`, `scripts/windows/smoke_test.ps1` — startup em diretório limpo |
| `9624135` | `pool_concurrency_audit_test.go` — invariantes concorrentes de seleção/pin/replace |
| `d14e2af` | `provider_transition_audit_test.go` — caminhada randomizada com seed fixa (60 hops) |
| `bf8f9d0` | `persistence_audit_test.go` — stores corrompidos/truncados/read-only |
| `80f8816` | `stream_splice_audit_test.go` — anti-splice entre upstreams após primeiro byte |
| `b7e851b` | correção de lock no próprio teste do watcher |

Cobertura de fuzzing: o repositório tinha **zero** funções `Fuzz*`. Foram
adicionadas 3 (`sse_fuzz_audit_test.go`), executadas com `-fuzztime` 30s/20s
(sem crashes; corpus automaticamente enriquecido pela infra do Go).

## Bugs reais

### BUG-AUDIT-001
- Severidade: **alta** (Windows-only)
- Subsistema: `credential_vault.go` `writeFileAtomic` (todos os fluxos que persistem credenciais: token refresh via `atomicWriteJSON`, admin codex/grok/kimi/zai, migração do vault)
- Windows-only: sim (POSIX rename sobre fd aberto sempre funciona)
- Invariante: atualização atômica de credencial deve ser robusta a leitores concorrentes do destino
- Reprodução: `TestAuditWriteFileAtomicToleratesGoReaderHoldingDestination` (leitor Go comum: `os.Open` — o Go **não** abre com `FILE_SHARE_DELETE`), `TestAuditWriteFileAtomicReplacesFileHeldWithoutShareDelete` (handle sem compartilhamento = antivírus/indexador/editor), `TestAuditCredentialFilesAreNotObservablyTruncatedByWatcherReload` (refresh racedo com hot-reload)
- Esperado: replace conclui e o conteúdo é atualizado
- Atual: `os.Rename` falha com "Access is denied." e o refresh/token é perdido
- Hipótese de correção (fase futura): retry com backoff curto, `ReplaceFile`, ou abrir destino com `FILE_SHARE_DELETE` nos leitores internos
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-101
- Severidade: média (cross-platform)
- Subsistema: `watcher.go` `newPoolWatcher`
- Invariante: diretórios de provider criados após o startup devem ser assistidos
- Reprodução: `TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory`
- Esperado: credential nova em `pool/<provider>-criado-depois/` dispara hot-reload
- Atual: nenhum evento é entregue; a conta só aparece após `/admin/reload` ou restart
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-102
- Severidade: média (cross-platform)
- Subsistema: `analytics_reliability.go` `persistActiveAccountingGapSidecar`
- Invariante: writes concorrentes ao sidecar nunca produzem JSON corrompido
- Reprodução: `TestAuditAnalyticsGapSidecarConcurrentWritersNeverCorrupt` (corrompe já no round 1–2: 57 bytes de um writer + cauda de 8192 bytes do outro)
- Causa: nome `.tmp` fixo compartilhado, sem exclusão mútua entre `WriteFile`+`Rename`
- Impacto: gap contábil ativo silenciosamente descartado no restart
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-104
- Severidade: baixa (Windows-only)
- Subsistema: `watcher.go` `handleEvent` (`event.Name == pw.configPath`)
- Invariante: eventos de config devem casar com `CONFIG_PATH` de forma case-insensitive no NTFS
- Reprodução: `TestAuditWatcherMatchesConfigPathCaseInsensitively`
- Esperado: modify com casing diferente do path registrado recarrega config
- Atual: evento é classificado como pool e o hot-reload da config é perdido silenciosamente (no Linux o `Add` falha e ao menos loga)
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-105
- Severidade: média (cross-platform, amplificado no Windows pelo BUG-AUDIT-001)
- Subsistema: `passport_backup.go` `restorePairedBackup`
- Invariante: backup pareado (Bolt+DuckDB) restaura atomicamente ou não restaura nada
- Reprodução: `TestAuditRestorePairedBackupRollsBackWhenSecondRenameFails` — rename do Bolt succeeds, rename do DuckDB falha → Bolt restaurado ("before") e DuckDB intacto, sem rollback
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-106
- Severidade: média (cross-platform)
- Subsistema: `watcher.go` (watch de `config.toml` aponta para o arquivo, não para o diretório)
- Invariante: saves atômicos repetidos de config devem sempre disparar hot-reload
- Reprodução: `TestAuditWatcherSurvivesRepeatedAtomicConfigReplace` — o primeiro replace recarrega; o segundo é ignorado até restart
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-107
- Severidade: baixa (cross-platform)
- Subsistema: `sse.go` `sseFramer` (pseudo-evento de 1 byte para CRLF dividido entre reads)
- Invariante: suprimir um evento deve remover todos os seus bytes, inclusive o terminador, independentemente da segmentação de leitura
- Reprodução: `TestAuditSSESuppressionSplitCRLFDivergence` — saída `"data: keep\n\n"` vs `"\ndata: keep\n\n"` dependendo do ponto de corte
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-108
- Severidade: **alta** (bare exe; Windows é o caso canônico)
- Subsistema: `main.go`/`storage.go` `newUsageStore` (sem `MkdirAll` do diretório de `./data/proxy.db`)
- Invariante: primeira execução em diretório limpo deve funcionar (README: "Run the binary from a directory where you want pool/, data/...")
- Reprodução: `TestAuditFreshDirectoryStartupCreatesStorage` + `scripts/windows/smoke_test.ps1` (E2E: exe morre com `open ./data/proxy.db: The system cannot find the path specified.`)
- Mascaramento: o Dockerfile faz `mkdir -p /app/data`, por isso o Linux/Docker não vê o bug
- Extra: `main.go:661` hardcode `./data/analytics.db` (ignora env de path)
- Status: **NÃO CORRIGIDO**

### BUG-AUDIT-109
- Severidade: **alta** (cross-platform)
- Subsistema: `storage.go` `newUsageStore` → `bbolt.Open` (panic interno da bbolt em `invalid freelist page`)
- Invariante: store corrompido/truncado deve produzir erro tratável, não panic
- Reprodução: `TestAuditUsageStoreTruncatedBoltFailsGracefully` — panic escapa de `newUsageStore`; no startup (`main.go:499`) o processo morre com panic crú em todo restart até reparo manual
- Status: **NÃO CORRIGIDO**

## Races

- Nenhum `DATA RACE` novo com `-race -count=1 -shuffle=on` após os testes adicionados.
- Uma race detectada durante o desenvolvimento era do próprio teste de auditoria (leitura de `AccessToken` sem `a.mu`) — corrigida no teste (`b7e851b`); a produção travava corretamente.
- Race lógica (sem detector) documentada como BUG-AUDIT-102 (sidecar).

## Flakiness

- Nenhum flake observado em shuffle×5 / race×3 na baseline.
- Os testes do watcher dependem do debounce de 500ms; usam polling com timeout de 3–5s — estáveis nas execuções.
- `TestAuditAnalyticsGapSidecarConcurrentWritersNeverCorrupt` reproduz em ≤3 rounds consistentemente.

## Filesystem / paths

- Espaços + Unicode + acentos no diretório do pool/vault/DuckDB: OK (`TestAuditCredentialRoundTripInSpacedUnicodeDirectory`, `TestAuditDuckDBSpacedUnicodePathRoundTrip`).
- Path >260 chars (MAX_PATH): DuckDB/CGO abre normalmente (`TestAuditDuckDBLongPath`) — guard verde.
- Worktree em `C:\...\Temp\opencode\Codex Pool Audit`: `build.ps1` e `go test ./...` OK (apenas os vermelhos intencionais falharam) — sem bugs de quoting.
- CRLF: TOML de config recarrega corretamente com CRLF (`TestAuditWatcherReloadsCRLFConfigFile`). `.env` só existe para docker-compose (sem loader no Go).
- Case-insensitivity: BUG-AUDIT-104. Colisão `Main.json`/`main.json` é impossível no NTFS (mesmo arquivo) — sem problema adicional.

## Watcher

- Save atômico de credential dispara reload corretamente no Windows (`TestAuditWatcherHotReloadsAtomicCredentialSave`, verde).
- Novos subdiretórios de provider não são assistidos (BUG-AUDIT-101).
- Watch de config morre após o primeiro replace atômico (BUG-AUDIT-106) e é sensível a case (BUG-AUDIT-104).
- `.tmp`/dotfiles são filtrados corretamente pelo `handleEvent`.

## DuckDB / CGO / UCRT64

- Build CGO com GCC 15.2.0 UCRT64 OK; binário inicia e carrega DuckDB (parcialmente — ver BUG-AUDIT-108: morre antes por falta de `data/`; com `data/` criado manualmente o smoke prossegue até o Bolt).
- DuckDB corrompido e truncado: falha com erro tratável, sem crash CGO (`TestAuditDuckAnalyticsCorruptDatabaseFailsGracefully`, `TestAuditDuckAnalyticsTruncatedDatabaseFailsGracefully`, verdes).
- Spaces/Unicode/long path: OK.
- README vs realidade: shim `duckdb_windows_shim.go` inexistente; GCC 16 rejeitado pelo build apesar de anunciado.

## Passport

- Cobertura existente é forte (recovery concorrente single-use, rotação de chave atômica, revalidação de expiração em transação, join index com erro de backfill fatal).
- Restore de backup pareado é a lacuna encontrada (BUG-AUDIT-105).

## Pool / routing

- Invariantes sob concorrência passam com `-race`: pin estável sob 24 goroutines, `replace` concorrente sem deadlock/phantom accounts, conta em cooldown/exausta nunca selecionada, pin sobrevive a reload que mantém o ponteiro da conta (`pool_concurrency_audit_test.go`).
- Nota de falsa segurança (sem alterar o teste existente): `TestCandidateUsesPinUnlessExcluded` usa contas Codex sem `PlanType`; o caminho de pin requer plano pro — a asserção pode estar passando via seleção por score, não via pin.

## Streaming / WebSocket

- Anti-splice entre upstreams após primeiro byte: invariante mantida (`TestAuditStreamUpstreamDeathDoesNotSpliceFallbackContent`, verde).
- Cobertura WebSocket existente é ampla (pin, cyber swap, audit de turn race, rejeição, lifecycle).
- Fuzz do framer SSE: 3 alvos novos, chunk-independence + independência de terminador de linha (CR/LF/CRLF) + robustez — sem crashes em fuzzing curto.
- Divergência de supressão com CRLF dividido: BUG-AUDIT-107.

## Provider transitions

- Caminhada randomizada com seed fixa (60 hops entre Codex/Claude/Antigravity/Z.ai, streaming alternado): epoch nunca regride, sem IDs estranhos após switch, contexto preservado em switches, tool calls pareados, seeds de sessão Antigravity sempre frescas (`provider_transition_audit_test.go`, verde).
- Semântica confirmada: hop para o mesmo provider não é switch (continuação nativa via `previous_response_id`).

## Frontend

- `npm test` 44/44; `npm run build` OK.
- Já existe guard de stale-response (`App.race.test.tsx`) cobrindo "resposta antiga não sobrescreve nova" e revogação de pass.
- Lacuna: cobertura proporcionalmente pequena (5 arquivos) para o dashboard, mas sem falha Windows-específica encontrada.

## PowerShell scripts

- `build.ps1`/`install.ps1`/`dev_proxy.ps1`/`deploy.ps1`: usam `$PSScriptRoot` (sem dependência de CWD), checam `$LASTEXITCODE` após cada comando externo, restauram env do processo, usam `-LiteralPath` e temp com GUID no install. Empíricos: build OK da raiz, de outro CWD e em path com espaços.
- `install.ps1`: `Move-Item` sobre exe em execução falha (lock do Windows) — comportamento aceitável para instalação, mas sem mensagem amigável.
- Adicionado `scripts/windows/smoke_test.ps1` (infra de auditoria; hoje falha com BUG-AUDIT-108 — é o repro E2E).

## Fuzzing

- Executado: `FuzzAuditSSEFramerChunkIndependence` 30s (~688k execs), `FuzzAuditSSEDataExtractionLineEndingIndependence` 20s (~531k execs), `FuzzAuditParseSSEEventRobustness` seeds. Sem crashes/hangs; corpus cresceu automaticamente.

## Testes intencionalmente vermelhos (repros de bugs)

| Teste | Bug |
|-------|-----|
| `TestAuditWriteFileAtomicReplacesFileHeldWithoutShareDelete` | BUG-AUDIT-001 |
| `TestAuditWriteFileAtomicToleratesGoReaderHoldingDestination` | BUG-AUDIT-001 |
| `TestAuditCredentialFilesAreNotObservablyTruncatedByWatcherReload` | BUG-AUDIT-001 |
| `TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory` | BUG-AUDIT-101 |
| `TestAuditAnalyticsGapSidecarConcurrentWritersNeverCorrupt` | BUG-AUDIT-102 |
| `TestAuditWatcherMatchesConfigPathCaseInsensitively` | BUG-AUDIT-104 |
| `TestAuditRestorePairedBackupRollsBackWhenSecondRenameFails` | BUG-AUDIT-105 |
| `TestAuditWatcherSurvivesRepeatedAtomicConfigReplace` | BUG-AUDIT-106 |
| `TestAuditSSESuppressionSplitCRLFDivergence` | BUG-AUDIT-107 |
| `TestAuditFreshDirectoryStartupCreatesStorage` | BUG-AUDIT-108 |
| `TestAuditUsageStoreTruncatedBoltFailsGracefully` | BUG-AUDIT-109 |

Todos os demais testes de auditoria são guards verdes.

## Lacunas restantes

1. CI sem job Windows — os 9 bugs acima (incluindo 4 Windows-only) não são
   capturados pelo pipeline. Recomenda-se job `windows-latest` com Go+Node+
   MSYS2 UCRT64 (GCC 15.2.0) rodando vet/test e, num segundo momento, os
   tests vermelhos transformados em asserções pós-correção.
2. `go.mod` declara Go 1.25; ambiente local 1.26.1 (compatível). Não foi
   testada compilação com Go 1.25 exato (sem instalação adicional conforme
   regras de não-destrutividade).
3. Fuzzing limitado a SSE; candidatos futuros: traduções de formato
   (`format_translate_*`), parser de rate-limit headers, conversation IR.
4. Sem teste de processo-real de restart (kill -9 durante drain do outbox
   DuckDB); cobertura atual é reabertura pós-truncamento.
5. Auditoria de scripts PowerShell é empírica; não há harness automatizado
   (Pester não faz parte do projeto).
