# Codex Pool — Auditoria do roadmap e plano de evolução

**Data de referência:** 28 de setembro de 2026  
**Repositório:** `GustavoMartins123/codex-pool`  
**Commit auditado:** `8fb3416c0256da48c519525cf929d93077a65ab9` (`8fb3416`)  
**Documento de entrada:** `codex-pool-roadmap(1).md`, com 61 fases, de CP-00 a CP-60.  
**Entrega:** diagnóstico fundamentado, reconciliação de todas as fases, prioridades de segurança, desenho de contas de membros e novo plano F1–F22.  
**Estado deste documento:** relatório de investigação e planejamento de implementação; não é uma declaração de que as correções propostas já foram aplicadas.

> **Decisão principal:** corrigir o isolamento de conversas e os comportamentos fail-open de configuração antes de habilitar contribuição/compartilhamento de contas por membros. O projeto já possui várias das fundações do roadmap. O trabalho agora é fechar lacunas, unificar invariantes e tornar essas capacidades operáveis — não reescrevê-las.

## Sumário

1. [Conclusão executiva](#1-conclusão-executiva)
2. [Método, evidências e resultados de validação](#2-método-evidências-e-resultados-de-validação)
3. [Registro de segurança: P0 e P1](#3-registro-de-segurança-p0-e-p1)
4. [Crítica técnica e de produto](#4-crítica-técnica-e-de-produto)
5. [Reconciliação das 61 fases originais](#5-reconciliação-das-61-fases-originais)
6. [Comparação com produtos semelhantes](#6-comparação-com-produtos-semelhantes)
7. [Contas adicionadas por membros e compartilhadas pelo operador](#7-contas-adicionadas-por-membros-e-compartilhadas-pelo-operador)
8. [Novo roadmap F1–F22, por blocos](#8-novo-roadmap-f1f22-por-blocos)
9. [Dependências e ordem de execução](#9-dependências-e-ordem-de-execução)
10. [Critérios transversais de conclusão](#10-critérios-transversais-de-conclusão)
11. [Novas ideias e decisões de escopo](#11-novas-ideias-e-decisões-de-escopo)
12. [Referências e rastreabilidade](#12-referências-e-rastreabilidade)

---

## 1. Conclusão executiva

### 1.1 O que o projeto já é

O código auditado não é apenas um balanceador de arquivos de autenticação. Ele reúne Passport com principals e credenciais de cliente, sessões e passkeys, autorização administrativa, políticas por credencial, controle de quota, múltiplos protocolos, IR para handoff, estados nativos por provider, dry-run de transição, orquestração `pool/auto`, circuit breakers em vários níveis, analytics com mecanismos de recuperação e uma interface operacional própria. Isso está materialmente além de várias descrições de “a implementar” do documento original. [E04] [E10] [E12] [E17] [E18] [E20] [E21] [E23] [E25] [E27]

A direção de produto mais defensável continua sendo **controle de capacidade e acesso a contas/providers para clientes de programação e agentes**. A diferenciação proposta é: isolamento real entre pessoas, compartilhamento explícito de capacidade, troca de provider com perdas conhecidas, e diagnóstico que explique decisões sem expor segredos. Essa é uma recomendação de produto derivada da arquitetura observada, não uma afirmação de liderança de mercado.

### 1.2 O que muda em relação ao roadmap antigo

| Decisão | Justificativa |
|---|---|
| Preservar as fundações existentes | Passport, vault, IR, circuit breaker e frontend não precisam de substitutos paralelos. |
| Separar segurança de sequência de features | P0/P1 expressam urgência/risco; F1–F22 expressam pacotes de implementação. |
| Antecipar isolamento e autorização por recurso | Contas privadas não são seguras apenas porque o menu é restrito por role. |
| Trazer testes de contrato, browser e falhas para o início | A suite existente é valiosa, mas não comprova todos os fluxos reais. |
| Separar simulação de tráfego sombra | O shadow existente envia requisições reais; CP-47 descrevia uma decisão sem envio. |
| Trocar percentuais subjetivos por evidências | “70% pronto” não identifica qual invariável falta, qual teste passou ou qual caminho não foi inspecionado. |
| Desmembrar a ideia de contas de membros | Ownership, cadastro, grants, contabilização e revogação precisam de contratos próprios. |
| Adiar complexidade sem dados | HA, roteamento semântico e escalonamento automático só entram após requisitos e medições que os justifiquem. |

### 1.3 Ordem de decisão recomendada

**Primeiro:** fechar P0-01, P1-01 e P1-02. Em paralelo, triar dependências e colocar os contratos de browser/segurança na CI.

**Depois:** construir autorização por conta e contribuição privada, sem ampliar o papel `member` para `operator`. Só então liberar grants para uso por terceiros e refletir o acesso efetivo em inferência, catálogo, métricas e UI.

**Na sequência:** consolidar decisões de routing, budgets agregados, handoff/IR e observabilidade. A melhoria de UI deve acompanhar esses contratos, não aguardar o fim de todo o backend.

**Por último:** aumentar escala organizacional, HA e inteligência adaptativa. Não é necessário implementar 61 features em linha reta para obter um produto confiável.

---

## 2. Método, evidências e resultados de validação

### 2.1 Escopo efetivamente coberto

O roadmap anexado foi confrontado com o commit fixado, sua árvore de arquivos, trechos de código e testes relacionados às superfícies críticas, o workflow e os logs da CI. Foram examinados caminhos de autenticação, contribuição OAuth, rede/configuração, seleção de modelos, handoff, vault, accounting, frontend e seus contratos de API. A comparação externa utiliza documentação dos próprios produtos.

A profundidade não é idêntica em todas as 61 fases. O documento distingue:

- **C — confirmado no código:** comportamento ou estrutura visível nos trechos inspecionados.
- **CI — execução confirmada:** comando e resultado observados na CI do SHA fixado.
- **L — reprodução local:** teste executado nesta investigação em fonte isolada, com origem conferida.
- **P — parcial:** base existente, mas objetivo integral ainda não demonstrado.
- **NE — não evidenciado:** a investigação não encontrou comprovação suficiente; não significa prova de inexistência em todo o repositório.
- **D — decisão proposta:** desenho ou requisito futuro, ainda não implementado.

**“Parcial” e “NE” não autorizam criar código duplicado.** A primeira tarefa da fase correspondente é confirmar o ponto de integração e seus testes. Arquivo presente, enum declarado, comentário “seguro” e build verde não substituem uma prova de comportamento.

### 2.2 CI consultada no commit exato

| Item | Resultado observado |
|---|---|
| Run | `36471149835`, job `109093427357`, resultado `success`. |
| Ambiente da execução | Linux; Go 1.26.8 e Node 24.21.0 nos logs consultados. |
| Web | Build concluído; 44 testes Vitest em 5 arquivos passaram. |
| Go | `go vet ./...` e `go test -race -count=1 ./...` concluídos com sucesso. |
| Benchmark | Testes do harness executados. O workflow distingue baseline mock de gate real de performance. |
| Dependências npm | Log de instalação informa 4 alertas: 3 moderados e 1 alto. Não foi obtido o diagnóstico completo de `npm audit` para atribuir pacote/CVE/explorabilidade. |
| Bundle | JS principal de 534,36 kB, 163,62 kB gzip; aviso acima de 500 kB. CSS de 106,83 kB, 19,62 kB gzip. |
| Limite da evidência | CI verde não comprova benchmark com upstream real, visual QA, fluxo browser atualizado ou ausência de falhas de autorização entre objetos. |

Fontes: workflow e execução [E02] [E03]. O `go.mod` declara Go 1.25.0; isso é distinto da versão de Go usada na CI. A diferença deve ser explícita na matriz de ambientes, não corrigida por suposição. [E32]

### 2.3 Teste de segurança realmente executado

Foi reconstruída uma cópia de `ip_access.go` obtida do GitHub. Seu Git blob hash foi conferido:

```text
7f3da694014701fbe9d57f2051185a0f05e7b011
```

Esse hash coincide com o blob do commit auditado. O módulo isolado usa somente a biblioteca padrão e foi executado em Go 1.23.2/Linux. **Não foi um build local do repositório inteiro.**

Comando executado:

```bash
GOTOOLCHAIN=local GO111MODULE=off go test -race -v \
  -run '^TestAudit' ip_access.go ip_access_audit_test.go
```

Resultados:

```text
PASS TestAuditObserveInvalidAllowDisablesRestriction
PASS TestAuditControlValidAllowRestrictsOutsider
PASS TestAuditObserveLoopbackOverridesDeny
```

Um segundo teste expressa a propriedade desejada, não o comportamento atual:

```bash
GOTOOLCHAIN=local GO111MODULE=off go test -v \
  -run '^TestDesired' ip_access.go ip_access_audit_test.go
```

Resultado esperado e observado nesta investigação:

```text
FAIL TestDesiredInvalidAllowMustNotOpenAccess
FAIL-OPEN: invalid explicit allowlist opens access; reject startup config instead
```

O caso usa os endereços de documentação `192.0.2.0/33` e `198.51.100.20`; não testa uma infraestrutura de terceiros. Os fontes e os logs estão no pacote de evidências. [E07]

### 2.4 O que não foi executado

Não houve login em contas reais de providers, envio de prompts reais, teste de invasão de um deploy, medição de latência contra upstreams, reprodução autenticada ponta a ponta do P0, renderização visual do dashboard nem novo build completo local do repositório. A evidência de build completo vem da CI consultada.

O repositório não foi alterado, e não houve commit, push, alteração de contas ou deploy. Os novos arquivos desta entrega são o relatório, o plano derivado e a reprodução local isolada. Isso preserva a distinção entre **investigar/validar**, **especificar a correção** e **aplicar a correção em produção**.

---

## 3. Registro de segurança: P0 e P1

### 3.1 Escala e triagem

**P0:** bloqueia a expansão multiusuário ou uma publicação que mantenha uma violação grave de isolamento.  
**P1:** deve ser resolvido no próximo ciclo de segurança; pode depender de uma configuração ou capacidade habilitada.  
**P2:** confiabilidade, operação, integridade, desempenho ou UX sem gravidade P0/P1 demonstrada.

As classificações são prioridades desta auditoria, não pontuações CVSS calculadas. Um **alerta de dependência não é automaticamente uma vulnerabilidade explorável**, e uma **proteção futura necessária não deve ser apresentada como um incidente já ocorrido**.

| ID | Natureza | Prioridade | Evidência | Destino |
|---|---|---:|---|---|
| P0-01 | Chave global de handoff sem principal permite colisão de histórico | P0 | Fluxo estático confirmado; sem exploração ao vivo | F1 |
| P1-01 | Allowlist IP inválida vira acesso irrestrito | P1 | Código e reprodução local em fonte idêntica | F2.A |
| P1-02 | Falha de TOML degrada para defaults; caminho inicial diverge de CONFIG_PATH do watcher | P1, quando configurações carregam controles de segurança | Fluxo estático confirmado | F2.A e F7 |
| P1-03 | Traffic shadow exige limites e política explícita de envio de dados | P1 condicional | Implementação envia tráfego real e remove cancelamento do contexto | F2.C e F11 |
| P1-04 | Alertas npm precisam de triagem e gate de supply chain | P1 de triagem | 1 alto + 3 moderados nos logs; detalhes não obtidos | F2.D |
| P1-05 | Vault upstream opcional é inadequado como default de nova instalação compartilhada | P1 de hardening para o novo produto | PlainStore sem chave confirmado; não é bypass da cifra configurada | F2.B e F5 |

### P0-01 — Isolamento de histórico no handoff

**Constatação.** Em `proxyRequest`, a identidade é autenticada, mas o identificador de conversa continua vindo do corpo ou dos headers do cliente. O caminho inspecionado usa esse valor diretamente ao consultar o estado de `pool/auto` e ao chamar `prepareProviderContextHandoff`. O wrapper não recebe principal e chama `store.Prepare` com o mesmo ID. O store é único por `proxyHandler`, e seu mapa é indexado apenas por `conversationID`. [E04] [E05] [E06]

**Fluxo relevante:**

```text
Principal A autenticado -> conversation_id = "mesmo-id" -> store.records["mesmo-id"]
Principal B autenticado -> conversation_id = "mesmo-id" -> mesmo registro

Troca de provider:
histórico do registro + mensagem atual -> payload reescrito para o upstream
```

`Prepare` faz merge do histórico retido com a entrada atual e o renderiza quando há transição. Portanto, o ponto de confiança é incorreto: um ID escolhido pelo cliente está sendo tratado como identidade global de um objeto de conversa. A autorização do principal não se propaga para a chave desse objeto. [E05]

**Pré-condições e impacto.** São necessários dois principals autenticados no mesmo processo, colisão ou conhecimento do ID e contexto retido; a transição precisa percorrer o caminho de handoff aplicável. O efeito possível é mistura/injeção de contexto de usuários diferentes e envio de conteúdo de um deles em uma requisição do outro. Não há evidência nesta auditoria de que alguém explorou isso em produção. A estrutura corresponde ao tipo de falha que a autorização por objeto deve impedir. [W06]

**Correção exigida.** Introduzir uma chave interna canônica com identidade autenticada, por exemplo a tupla tipada `(principal_id, conversation_id)`. Não usar concatenação ambígua com separadores que também sejam aceitos nos campos. O identificador externo/nativo permanece separado. A chave precisa alcançar lookup, merge, gravação de resposta, pins, seeds nativos, recuperação, websocket, payloads grandes, diagnósticos e qualquer execução shadow.

A escolha entre compartilhar a conversa entre credenciais do mesmo principal ou isolá-la também por `client_id` é uma política explícita. O mínimo obrigatório é não compartilhar entre principals distintos. Credenciais diferentes com fronteiras de acesso incompatíveis não podem aproveitar estado anterior para acessar uma conta revogada.

**Migração e contenção.** Registros antigos em memória não contêm prova de ownership. Invalidá-los é mais seguro que atribuí-los ao próximo usuário que apresentar o ID. Não basta “limpar o mapa uma vez”: a chave e todos os produtores/consumidores precisam mudar. Até a correção, não ampliar o acesso multiusuário ao handoff; separar processos de fronteiras de confiança distintas é uma contenção, não a solução final.

**Prova de fechamento.** Criar dois principals, usar o mesmo ID externo, inserir sentinelas diferentes e capturar o corpo de upstream fake em trocas de provider. Nenhuma sentinela pode atravessar a fronteira. Repetir para HTTP, WS, retorno ao provider original, gravação de assistant text e recuperação. O teste precisa falhar no commit anterior e passar no corrigido. A suite `-race` é adicional; ela não substitui a asserção de isolamento.

### P1-01 — Allowlist malformada falha aberta

**Constatação.** `parseIPNetList` descarta entradas inválidas silenciosamente. Uma configuração explicitamente restritiva composta apenas de entradas inválidas vira uma lista vazia. `restricted()` então retorna falso, e `permitted()` considera vazio como permitir todos. O teste local confirmou esse comportamento. [E07]

**Exemplo controlado:** `192.0.2.0/33` não é uma rede IPv4 válida. O sistema não deve interpretar esse erro como intenção de remover a restrição.

**Correção.** O parser deve devolver erro com o campo e a entrada inválida. Diferenciar configuração ausente, lista explicitamente vazia e lista inválida. Startup com política inválida deve abortar; hot reload inválido deve manter a última configuração válida sem publicar parcialmente o novo estado.

**Loopback.** A exceção que permite loopback antes da denylist também foi confirmada. É um comportamento documentado, não uma descoberta de bypass remoto universal. Ainda assim, não deve isentar indiscriminadamente inferência e administração: atrás de um proxy local, o IP efetivo precisa ser resolvido corretamente. Manter healthchecks locais em uma superfície explícita, em vez de liberar todas as rotas para qualquer tráfego que pareça loopback.

**Validação.** IPv4/IPv6, entrada parcialmente inválida, allow+deny, loopback por rota, proxy confiável/não confiável e XFF forjado. O teste de propriedade desejada incluído nesta entrega permanece vermelho até a correção.

### P1-02 — Configuração de segurança não pode cair silenciosamente para defaults

`buildConfig` registra warning quando `loadConfigFile("config.toml")` falha e continua com `fileCfg` vazio. Separadamente, o watcher usa `CONFIG_PATH` quando essa variável está presente. Assim, o arquivo observado para mudanças pode não ser o mesmo carregado inicialmente. [E08] [E09] [E24]

O default de escuta no trecho examinado é loopback: não se deve transformar esse achado numa alegação de “abertura automática à internet”. O risco é um deploy que configura escuta/segredos por ambiente, mas depende do TOML para políticas, provider restrictions ou trusted proxies, subir sem a intenção de controle esperada.

**Correção.** Resolver o caminho de configuração uma vez; carregar, validar e publicar um snapshot imutável versionado. Configuração explicitamente selecionada e ilegível/inválida deve impedir startup. Ausência permitida deve ser uma decisão documentada, distinta de erro de parse. Acrescentar `config validate`, diagnóstico de precedência env/TOML e diff do hot reload.

**Gate.** Testes com TOML truncado, arquivo sem permissão, `CONFIG_PATH` alternativo, env inválido, campo desconhecido de segurança e reload concorrente. Não descarregar a configuração vigente antes de validar a nova.

### P1-03 — Tráfego sombra é envio real de dados, não uma simulação

`maybeStartShadow` reescreve o modelo e cria outra execução de `proxyRequest`. O caminho exclui certos pedidos com tools e usa um limite de corpo, mas continua enviando conteúdo real. Além disso, utiliza `context.WithoutCancel`; o helper não cria seu próprio deadline. [E22]

Não foi demonstrado um bypass das políticas: o caminho normal reautentica e há checagem de modelo antes do shadow. O problema é operacional e de governança: um operador pode interpretar “shadow” como não envio, enquanto há outro consumidor de quota e outro destino potencial de dados.

**Correção.** Dois produtos separados:
- `decision-shadow`: avalia uma cópia de fatos e não faz chamada ao upstream nem reserva recursos.
- `traffic-shadow`: feature explicitamente habilitada, com destinos autorizados, consentimento/política, orçamento próprio, limite de concorrência, deadline e accounting separado.

Requisições sombra não podem herdar o mesmo estado de conversa de produção nem prolongar indefinidamente consumo após cancelamento. Caso a comparação continue após o cliente sair, isso deve ser uma política explícita e finita, não consequência incidental de `WithoutCancel`.

**Gate.** Provar zero chamadas no decision-shadow; teto de custo/concorrência no traffic-shadow; cancelamento/deadline; isolamento de estado; negação por grant e política; ausência de tools não significa ausência de conteúdo sensível.

### P1-04 — Dependências: triar antes de declarar vulnerabilidade corrigida

O log de `npm ci` registra quatro alertas. Isso é evidência suficiente para abrir triagem, mas insuficiente para nomear o pacote, afirmar exposição no bundle ou prescrever um upgrade específico. [E03]

**Execução da fase:** produzir `npm audit --json`, separar produção/dev/build, rastrear o caminho da dependência, avaliar alcance e aplicar correção mínima compatível. Complementar com análise Go (`govulncheck` ou mecanismo equivalente), SBOM, integridade de releases e atualização/pinning de Actions. Não usar `npm audit fix --force` indiscriminadamente.

**Gate.** A exceção precisa ter motivo, responsável, prazo e alcance. Um alerta alto não pode desaparecer apenas porque a job ignora o exit code. Tampouco se deve bloquear todo trabalho por uma dependência de desenvolvimento sem avaliar o risco concreto.

### P1-05 — Default de credenciais para instalações compartilhadas

`buildCredentialStore` retorna `PlainStore` quando não existe `POOL_CREDENTIAL_KEY`. Quando configurada, a cifra tem integração centralizada de escrita/migração; portanto, o achado não é “o vault não funciona”. É a incompatibilidade entre **compatibilidade legacy opcional** e a promessa de armazenar contas pessoais de terceiros com proteção por padrão. [E14]

**Correção.** Introduzir um perfil de instalação compartilhada que exija vault ativo antes de cadastrar contas. Migração de instalação antiga deve ser assistida, com verificação, rollback controlado e aviso inequívoco. Separar as chaves: Passport, vault upstream e recuperação de backups têm finalidades diferentes. A rotação de Passport já existe e deve ser reaproveitada. [E13]

**Modelo de ameaça.** Criptografia em repouso protege cópias do diretório sem a chave. Não impede que o administrador do host ou um processo comprometido com a chave leia o segredo em uso. A UI não pode prometer que o operador do servidor é tecnicamente incapaz de acessar material que esse servidor precisa descriptografar.

**Gate.** Nova contribuição bloqueada em modo compartilhado sem cifra; nenhum token upstream retornado em GET/listagem/inspector; rotações, reinício, restore e falhas de escrita testados; debug com secrets explicitamente incompatível com esse perfil.

### P1-06 — Transporte WebSocket roteia por path e repassa modelos de outros providers ao upstream Codex *(adicionado em 30/09/2026, pós-auditoria)*

O proxy trata upgrade WebSocket antes do model-routing HTTP (`applyStreamedModelRoute`): em `proxyRequestWebSocket` o upstream é escolhido só pelo path, e `inspectClient` (cyber_swap_ws.go) rejeita no máximo os modelos listados em `modelRequiresHTTPProviderRoute` (grok/kimi/minimax/zai/xiaomi/adversarial). Modelos de providers ausentes dessa lista — antigravity incluído — têm o `response.create` **repassado integralmente ao upstream ChatGPT**, que recusa com 400 "not supported when using Codex with a ChatGPT account". O conteúdo do turno trafega para o provedor errado antes da recusa. Reproduzido em 30/09/2026 com codex CLI 0.159.0: essa versão passou a usar de fato o transporte Responses-over-WebSocket (`supports_websockets` era inerte antes); erro in-band `{"type":"error","status":400}` não dispara o fallback para HTTP do CLI (só `426` no handshake ou falha de transporte — verificado em codex-rs/codex-api `responses_websocket.rs` e core `client.rs`).

**Correção.** Em camadas: (a) imediata no cliente — `supports_websockets = false` no provider do pool força HTTP POST, que já roteia por modelo; (b) no pool — incluir todo provider com catálogo próprio em `modelRequiresHTTPProviderRoute` e recusar o turno WS com mensagem acionável ("use HTTP POST /responses"), sem repassar nada ao ChatGPT; parar de escrever `supports_websockets = true` nas configs geradas (frontend.go); (c) definitiva — rotear turnos WS por modelo executando o turno via HTTP no provider correto e traduzindo SSE de volta para frames WS, quando houver demanda real por WS.

**Modelo de ameaça.** Não é só disponibilidade: é fluxo de dados. O prompt de um turno solicitado para modelo não-OpenAI chega inteiro ao upstream Codex antes de qualquer rejeição, com autenticação de conta ChatGPT válida.

**Gate.** Teste com fake upstream provando que nenhum frame de modelo não-OpenAI chega ao upstream Codex; rejeição in-band com mensagem clara quando não houver rota; catálogo de modelos não pode depender de edição manual da lista de rejeição (derivado do registry); catálogo via WS validado contra o catálogo HTTP.

### 3.2 Correções recentes que não devem ser reabertas como trabalho novo

O código lido preserva CSRF e vínculo do ator na contribuição, aposenta o código de amigo como autoridade, inicializa Passport de forma obrigatória e propaga erros nos backfills de índices de join/recovery. Há ainda rotação da chave Passport e nonces de download. Esses itens devem permanecer como regressões obrigatórias, não reaparecer como features inexistentes. [E10] [E11] [E12] [E13] [E24]

No frontend, o recovery consulta o backend antes de mostrar o formulário, normaliza campos de resposta e possui testes para contratos e respostas concorrentes na CI. Um novo desenho não deve restaurar os antigos fluxos `/reveal` ou reutilizar setup tokens long-lived só por conveniência do wizard. [E28] [E29] [E03]

---

## 4. Crítica técnica e de produto

### 4.1 Arquitetura: consolidar contratos, não fazer uma reescrita geral

A concentração de decisões em `main.go` e de telas/estado em `web/src/App.tsx` torna difícil provar que todos os caminhos aplicam o mesmo controle. Isso aparece concretamente no handoff sem principal e na diferença entre o fluxo convencional e os retornos antecipados de catálogo, contexto nativo, passthrough, WebSocket e corpos grandes. Não significa que todos esses caminhos estejam vulneráveis; significa que a arquitetura precisa de uma matriz de invariantes por entrada. [E04] [E24] [E27]

A extração recomendada é incremental: autorização de recursos, contexto, decisão de rota, admissão e eventos tornam-se módulos com entradas/saídas tipadas. O `package main` permanece como composição. Não mover milhares de linhas sem teste de paridade, nem transformar cada função em um microserviço.

O `ProviderRegistry` já existe. Melhorar o contrato de provider é mais útil do que introduzir outro SDK interno concorrente. Providers configuráveis e federação também já entram no startup; isso não comprova coordenação HA ou isolamento entre organizações. [E24] [E26]

### 4.2 Autorização deve filtrar candidatos, não apenas bloquear o escolhido

Em `pool/auto`, a seleção observada não recebe o universo permitido por principal. O caminho principal verifica modelo/provider depois de a orquestração escolher. Isso evita despachar o vencedor proibido, mas pode resultar em 403 mesmo quando existe um candidato autorizado que atenderia ao pedido. É uma deficiência de disponibilidade e explicabilidade; **não foi demonstrada execução de um modelo proibido nesse caminho**. [E04] [E21]

A solução é resolver primeiro o conjunto autorizado de contas/modelos, depois aplicar capacidade e saúde, e por fim pontuar. Cada retry e fallback revalida a autorização vigente. `pool/auto`, aliases, pins e hints não podem ampliar o conjunto autorizado.

A listagem de modelos também retorna antes da admissão no trecho inspecionado. Para contas privadas, o catálogo e os números de capacidade deverão ser uma projeção por principal. Não basta negar a geração e continuar revelando quais contas pessoais, planos e quotas existem. [E04]

### 4.3 Políticas por credencial não equivalem a limites por pessoa

`policyInflight`, `policyReserved` e as chaves de usage são indexados por `clientID`. A política vem da credencial, da configuração por ID/label ou do default. Isso implementa um controle útil por cliente, mas não o orçamento agregado de principal anunciado no CP-06. [E17]

Não denominar automaticamente isso “bypass”: o contrato atual é por credencial. O problema aparece quando a UI ou a documentação promete “limite deste membro” e esse membro possui várias credenciais com contadores independentes.

O novo modelo deve aplicar, simultaneamente, orçamento global/org quando existir, principal, credencial, grant e conta. Labels não devem ser autoridade durável: podem mudar ou colidir. Herdar políticas significa calcular a interseção de permissões e a composição de limites, não pegar o primeiro objeto não vazio.

### 4.4 Contas: estados projetados não são comandos operacionais

O lifecycle está ligado aos fatos existentes e ao sweep de observação. `Account` ainda carrega flags e não contém ownership/grants ou controles gerais de draining, pools lógicos e concorrência por conta no trecho lido. Exibir `DRAINING` em um enum não garante o comportamento de preservar conversas existentes e recusar novas admissões. [E15] [E16] [E24] [E34]

Recomendação: separar **estado administrativo desejado** de **estado operacional observado**. Uma conta pode estar administrativamente em draining e operacionalmente rate-limited. Tentar colocar toda combinação num único enum tende a explodir a máquina de estados ou esconder fatos importantes.

Conservar o comportamento de refresh on-demand: expiração de access token não deve, por si só, ser tratada como morte definitiva da conta. Um contrato novo precisa demonstrar paridade com os predicados atuais antes de alterá-los.

### 4.5 Handoff: validade sintática não é preservação semântica

A implementação já normaliza tool pairs antes e depois da compactação. Portanto, não é correto repetir que “não existe validação pós-corte”. O gap é mais amplo: preservar significado, ordem, proveniência e capacidades entre protocols/providers. [E05] [E18]

`summaryHandoffMessages` concatena texto visível em uma mensagem de sistema e descarta partes não textuais nesse caminho. Isso não é uma sumarização semântica garantida, e transportar conteúdo anterior como instrução de sistema merece revisão de fronteiras de confiança. O relatório não afirma que um prompt específico consegue explorar esse comportamento; recomenda preservar a classificação do conteúdo e explicitar perdas. [E19]

O produto deve reportar: quantidade de pares de tools preservados, anexos removidos, redução de contexto, ausência de estado nativo, incompatibilidade de capacidade e necessidade de nova sessão. Um fallback que altera essas condições não deve ser chamado de transparente.

Os modos mecânicos `native`, `full-history`, `safe-history` e `summary` já existem. As permissões `never`, `stateless-only`, `before-first-token` e `translate-context` são outra dimensão: **política de quando a troca pode ocorrer**, não novo nome para o mesmo transformador. [E19]

### 4.6 Simulação deve ser realmente livre de efeitos

O dry-run de transição já clona os registros e usa um store temporário. É uma base correta a reaproveitar. O simulador de seleção de conta ainda precisa de um contrato de avaliação sobre snapshot imutável. [E20]

Não é suficiente chamar o seletor real com um booleano `dryRun` se ele atualiza métricas, consume half-open probes ou modifica pins. No breaker observado, `CanAllow` pode expirar leases antigos; no manager, a obtenção de uma entrada pode criá-la. O comentário “non-mutating” não é prova de pureza. [E23]

Separar `Evaluate(snapshot, request) -> decision` de `Admit(decision, currentRevision) -> lease`. A paridade deve ser provada em snapshots iguais; uma simulação não precisa prever exatamente o estado que existirá milissegundos depois sob concorrência.

### 4.7 Métricas: a semântica importa tanto quanto a persistência

Em experimentos, `firstWrite` pode ser preenchido por `WriteHeader` ou `Flush`; isso não prova que o primeiro token semântico chegou. `ResponseBytes/4` mede uma aproximação por bytes da resposta, incluindo possível envelope de protocolo, e não o contador real de tokens. O erro de `db.Update` em `Record` é ignorado. [E22]

Recomendação: distinguir TTFB, primeiro evento, TTFT semântico, duração de geração e duração total; marcar tokens como `reported`, `estimated` ou `unknown`; contabilizar falha de persistência. Não alimentar automaticamente um router “inteligente” com uma métrica rotulada incorretamente.

Já existem reserva de disco, outbox, estado de analytics e accounting gaps. O Incident Center deve consumir esses sinais, não inventar outra fonte de verdade. O startup ainda abre SQLite legado além de Bolt e DuckDB: sua retirada precisa de migração e reconciliação, não de remoção direta. [E24] [E25] [E29]

**Economia de produto:** valor API-equivalente é um contrafactual de preço, não dinheiro efetivamente economizado nem receita. O cálculo de diferença entre API-equivalente e assinatura existe na UI; mostrar essas grandezas separadas evita conclusões comerciais exageradas. [E27] [E28]

### 4.8 Desempenho e limites: medir antes de otimizar a arquitetura errada

Há proteção de corpo, caminho de spool e um gate de grandes replay bodies; não é correto reportar “todas as requisições ficam sem limite em RAM”. Ainda assim, histórico de conversas, cópias de IR, duplicação de blobs, loops de escolha e budgets sob locks precisam de cenários de carga. [E04] [E24]

O store de handoff limita a quantidade de registros, não um orçamento total explícito de bytes. O limite de entradas de circuit breaker aciona tentativa de eviction; o trecho examinado ainda cria entrada depois disso, portanto o número 8192 não deve ser descrito como teto rígido garantido. [E05] [E23]

Benchmarks devem comparar base/candidato no mesmo hardware e separar custo do proxy da latência do provider. O benchmark mock continua útil para testar o harness, mas não valida redução de TTFT nem capacidade de produção. [E02] [E37]

### 4.9 UI/UX: preservar o que funciona e testar tarefas reais

A UI já tem navegação por URL, separação de views por role, estados de quota não reportada e confirmação de ação associada à conta e à operação. São capacidades reais, não itens a implementar do zero. [E27] [E28]

Os principais gaps de produto são: distinção entre conta upstream e credencial de cliente; política efetiva explicável; acesso por conta; diagnóstico de routing acessível; execução de ações longas com estado durável; e uma rotina de browser reproduzível.

O harness `account-flows.browser.mjs` usa dependência externa de Puppeteer, default de Chrome em caminho macOS e mocks antigos: `/reveal` com `setup_token`, em contraste com `/setup-link` e `setup_urls` do cliente atual. Também não oferece o status de recovery esperado pelo fluxo atual. Isso demonstra desalinhamento de contratos do teste; a execução real do harness não foi realizada nesta auditoria. [E29] [E30]

O build reporta um chunk principal grande, mas não se mediu lentidão em aparelho real. Ação válida: lazy loading por workspace, separar código de charts, revisar carga de fontes e estabelecer budgets. Ação inválida: declarar “o dashboard é lento” apenas pelo tamanho do bundle. [E03] [E27]

### 4.10 API: unificar fronteiras sem quebrar clientes existentes

O frontend chama famílias distintas como `/admin`, `/api/pool`, `/api/console`, `/api/me` e `/api/principals`. `decode<T>` aceita JSON nulo em sucesso e confia em cast estático; no erro, assume uma forma simples. [E29]

A API versionada deve criar um contrato administrativo estável com DTOs, paginação, erros tipados, idempotência de mutações e controle de concorrência. Manter adaptadores temporários nas rotas atuais; medir uso antes de removê-las. Não obrigar Codex/Claude/Gemini a adotar um protocolo proprietário para usar recursos básicos.

Novos campos de controle do pool devem ser consumidos pelo gateway e retirados antes do upstream. Auth scopes, identidade do ator e IDs de recurso não podem ser aceitos do body como verdade quando já existe identidade autenticada.

### 4.11 Operação: atomicidade, durabilidade e restauração são coisas diferentes

`writeFileAtomic` centraliza encode, arquivo temporário, permissão 0600 e rename. Não há `Sync` de arquivo/diretório no helper lido; portanto, sua atomicidade de substituição não comprova persistência após queda de energia. O ajuste é de durabilidade com semântica de plataforma, sem afirmar que toda escrita atual corrompe dados. [E14]

A sequência de shutdown já inicia draining de WebSockets e chama shutdown do servidor. O próximo trabalho é validar readiness, requests HTTP em andamento, pollers, flush e fechamento ordenado das stores, não cadastrar endpoints duplicados. [E24]

Um backup operacional precisa da configuração, metadados de ownership/grants, estado de autorização, credenciais cifradas e instruções de recuperação de chaves. Backup restaurável não pode ressuscitar grants ou credenciais revogadas por acidente. A chave de backup não deve simplesmente ser a mesma chave crua de vault, como sugeria a anotação antiga de CP-48.

---

## 5. Reconciliação das 61 fases originais

Esta seção conserva os IDs CP e o agrupamento do documento fornecido, mas substitui o estado por uma conclusão auditável. A coluna “Destino” aponta para o novo roadmap F. **Nenhuma linha significa homologação integral em produção.** As capacidades marcadas como não comprovadas devem passar por inventário de implementação/testes antes de se abrir trabalho novo.

A pré-fase `CP-00-real`, acrescentada como anotação no documento original, foi absorvida pelo gate transversal de F3: CI, evidência de benchmark e decomposição incremental. Não é uma 62ª feature independente.

### Bloco original A — Fundação e hardening

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-00 — Baseline, benchmarks e invariantes** | **Parcial · CI.** Build, vet, race e harness passaram na CI. Não há baseline de latência real validada nesta investigação; mock não é gate de performance. Acrescentar microbenchmarks, cenários determinísticos e carga controlada antes de otimizar. [E02] [E03] [E37] | F3 |
| **CP-01 — Security Hardening 2.0** | **Base existente + falhas.** CSRF, autorização, headers e isolamento administrativo têm implementação. Fechar P0/P1 do contexto e da configuração; não repetir hardening já entregue nem tratar redaction por padrão como garantia de remoção de todo segredo. [E07] [E08] [E10] [E11] | F1–F2 |
| **CP-02 — Credential Vault local** | **v1 existente.** Vault upstream e caminho de escrita centralizado existem. Exigir cifra no perfil compartilhado, revisar durabilidade e recuperação. Diferenciar rotação da chave de contas da CLI já existente para Passport. [E13] [E14] | F2.B; F19 |

### Bloco original B — Account control plane

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-03 — Modelo de estado unificado das contas** | **Projeção existente.** Lifecycle é derivado dos fatos e observado no startup. Separar intenção administrativa de saúde observada; histórico durável e ator explícito são evolução, não prova de que hoje haja draining autoritativo. [E15] [E16] [E24] [E34] | F8 |
| **CP-04 — Controle avançado por conta** | **Parcial.** Disabled, origem permitida e atributos de plano/saúde existem. Ownership/grants, max concurrency geral por conta, reservas e draining não estão no Account inspecionado. Criar controles com semântica de roteamento e persistência. [E15] [E16] | F4; F8 |
| **CP-05 — Pools lógicos de capacidade** | **Não comprovado.** Diretórios por provider e federação não equivalem a pools lógicos. Modelar agrupamento como seleção de recursos, nunca como concessão implícita de acesso. Confirmar usos existentes antes de adicionar nova entidade. [E15] [E24] | F8 |

### Bloco original C — Políticas por principal

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-06 — Policy Engine v1** | **Parcial · por cliente.** Admission, allow/deny e reserva de tokens existem. Contadores são por clientID, não orçamento agregado de principal. Acrescentar composição, proveniência e modo de enforcement explícitos. [E17] | F9 |
| **CP-07 — Fair Scheduling** | **Não comprovado.** A existência de prioridade ou limite de concorrência não demonstra weighted fair queue. Construir fila limitada e cancelável apenas após medir saturação; preservar hot path sem espera quando há capacidade. [E17] | F10 |
| **CP-08 — Reserved Capacity** | **Não comprovado.** Reserva in-flight de tokens de policy não é reserva de capacidade de conta/workload. Unidades, confiança da quota e regras de empréstimo precisam ser definidas antes de aplicar percentuais. [E15] [E17] | F8; F10 |

### Bloco original D — Routing

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-09 — Routing configurável** | **Base existente.** Routing configurável já é ligado no startup e possui schema próprio. Reusar perfis/pesos referidos no roadmap; fechar elegibilidade, testes golden e nomenclatura de métricas, sem segundo router. [E09] [E24] [E35] | F11 |
| **CP-10 — Routing Explain & Simulator** | **Parcial · escopos distintos.** Existe dry-run de transição com clone de estado. Isso não substitui simulador completo de seleção de conta/policy. Extrair avaliação pura e reaproveitar os diagnósticos existentes. [E20] [E21] [E23] | F11 |
| **CP-11 — Conversation Pinning 2.0** | **Parcial + bloqueio P0.** Há pinning e estado de conversa, mas o handoff HTTP não está isolado por principal. Não considerar o objetivo de namespace concluído só porque um mapa de pins recebe userID. Persistência/TTL precisam de contrato unificado. [E04] [E05] [E06] | F1; F12 |
| **CP-12 — Routing por capability** | **Parcial.** Extração de capabilities já participa de pool/auto e transições. Unificar matching por modelo/conta, com origem/confiança dos dados. Separar capacidade do modelo de capabilities do transporte ou de hosted tools/MCP. [E04] [E19] [E21] | F11; F13 |

### Bloco original E — Provider switch e IR

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-13 — Pool Conversation IR** | **Mais que embrião.** ConversationIR já modela roles, conteúdos e metadados e é usado no handoff. A conversão ainda passa por Message legado; unificação integral dos adapters não foi comprovada. Evoluir por contrato de paridade. [E18] [E05] | F13 |
| **CP-14 — Safe Context Compaction** | **Parcial · validação já existe.** Sanitização de tool pairs roda antes e depois da compactação. O gap é orçamento por modelo, preservação semântica, multimodal e testes longos; não recriar o validator básico como se fosse ausente. [E05] [E19] | F13 |
| **CP-15 — Provider Handoff Universal** | **Implementação relevante.** Há modos mecânicos de transição, estado nativo por provider, recovery e dry-run. Faltam comprovação matricial e política de quando perder contexto/trocar destino. Isolamento vem antes de ampliação de providers. [E05] [E06] [E19] [E20] | F1; F13 |

### Bloco original F — Confiabilidade

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-16 — Circuit Breaker hierárquico** | **Mais avançado que a anotação.** Manager possui provider, account, account+model e account+capability, além de HALF_OPEN. Não recriar escopo model. Auditar paridade em cada entrada, pureza do simulador e limite efetivo de cardinalidade. [E23] | F14; F11 |
| **CP-17 — Health Canaries** | **Parcial.** Pollers de uso, modelos e saúde já são iniciados. Uma matriz empírica de stream/tools/imagem por conta não está demonstrada integralmente. Unificar esta fase com CP-52 e limitar quota/custo dos probes. [E24] | F14 |
| **CP-18 — Refresh Coordination** | **Revalidar o gap, não duplicar.** O roadmap já descreve deduplicação de refresh e o código possui transporte/pollers próprios. Não há prova local nova de todos os requisitos de jitter/deadline/cancelamento. Formalizar o coordenador existente com testes concorrentes. [E24] | F14 |
| **CP-19 — Idempotency e Retry Safety** | **Parcial.** RequestUsage já separa IDs e número de tentativa. Isso não comprova idempotência upstream nem segurança após primeira saída visível. Definir commit point, retry-safe/unsafe e contabilização por tentativa. [E15] [E04] | F13; F15 |
| **CP-20 — Graceful Shutdown & Readiness** | **Parcial · fluxo existente.** Shutdown inicia drain de WS e shutdown do servidor. Validar readiness, rejeição de novas admissões e fechamento de stores/pollers de forma conjunta; não criar endpoints de saúde duplicados. [E24] | F19 |

### Bloco original G — Observabilidade

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-21 — Latency & Reliability Analytics** | **Base substancial.** Há stores, accounting gaps e estado de saúde de analytics. Não certificar percentis/EWMA completos sem verificar instrumentação. Corrigir a semântica das métricas de experimentos e reconciliar fontes. [E22] [E24] [E25] [E29] | F15 |
| **CP-22 — OpenTelemetry** | **Não comprovado.** O go.mod lido não declara SDK OpenTelemetry; isso não basta para negar qualquer integração indireta. Exigir trace ponta a ponta demonstrável e exportação segura, a partir dos pontos de instrumentação existentes. [E32] [E04] | F15 |
| **CP-23 — Structured Logging** | **Parcial / consolidar.** Há muitos log.Printf e eventos operacionais no código examinado. Centralizar schema, request/attempt IDs, níveis e redaction; não confundir logs estruturados de segurança com captura de prompts. [E04] [E24] | F15 |

### Bloco original H — UI/UX

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-24 — Information Architecture da administração** | **Parcial · IA já existente.** Oito views, permissões por role e navegação via URL já estão presentes. Reorganizar por tarefas e profundidade, não adicionar todas as novas seções na barra principal de uma vez. [E27] | F16 |
| **CP-25 — Accounts UX 2.0** | **Parcial.** Lista e ações de conta existem, com confirmação associada a conta+ação. Inspector, permissões, grupos, bulk actions e novos controles devem refletir backend real, preservando essas proteções. [E27] [E28] | F8; F16 |
| **CP-26 — Visualização de quota e forecast** | **Parcial · forecast já existe.** WeeklyPace e insights já apresentam estimativas e estados sem dados. Evoluir intervalo de confiança, fontes e backtesting; não criar um segundo forecast e nem chamar estimativa de garantia. [E27] | F15; F16 |
| **CP-27 — Routing Lab UX** | **Parcial / nova UX.** O backend de dry-run de transição fornece uma fundação. Routing Lab deve distinguir simulação de conta, diagnóstico de transição e override real; este último exige escopo, expiração e auditoria. [E20] | F11; F16 |
| **CP-28 — Policies UX** | **Contrato ainda incompleto.** A política efetiva por herança não pode ser só uma tela enquanto enforcement é por clientID. Construir composição no backend e retornar sua explicação para um editor validado. [E17] [E29] | F9; F16 |
| **CP-29 — Incident Center** | **Parcial em sinais, não centro.** AccountingGap e health de analytics já fornecem eventos acionáveis. Não há comprovação de um ciclo completo de incidentes agrupados, acknowledge e resolução. Implementá-lo sobre eventos existentes. [E25] [E29] | F17 |
| **CP-30 — UX de onboarding e setup** | **Parcial · setup existente.** Fluxos de clientes, nonces e geração de configuração já existem. Adicionar teste de conectividade graduado e diagnósticos; preservar o contrato setup-link atual, sem reintroduzir reveal legado. [E12] [E29] [E30] | F16 |
| **CP-31 — Command Palette** | **Não comprovado.** Command palette completa não foi demonstrada. É um acelerador de UX posterior à navegação estável; deve chamar as mesmas ações autorizadas, sem caminho privilegiado próprio. [E27] | F16.D |
| **CP-32 — Activity Timeline** | **Parcial em fontes.** Há audit API e transições, mas isso não prova timeline única com filtros e correlação. Unificar IDs de evento, ator e recursos, preservando privacidade e retenção. [E29] [E05] | F17 |
| **CP-33 — Mobile UX operacional** | **Validação real pendente.** O objetivo mobile é válido; não foi realizado browser/visual QA nesta investigação. Criar journeys de operação, estados offline/stale e testes de viewport, em vez de afirmar responsividade apenas por CSS. [E27] [E30] | F3; F16 |
| **CP-34 — Accessibility & Keyboard First** | **Parcial / medir.** Já há textos e aria labels em componentes inspecionados. Contraste, foco, leitores de tela e teclado precisam de testes. Adotar WCAG 2.2 AA como meta, sem declarar conformidade não medida. [E27] [W07] | F3; F16 |

### Bloco original I — Virtual models e experiência

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-35 — Virtual Models / Aliases** | **Parcial · pool/auto existe.** Aliases simples e modelos pool/auto já são uma abstração de intenção. Criar aliases com candidatos/constraints de forma declarativa e autorizada; não tratar o produto como se só roteasse modelo literal. [E09] [E21] | F18 |
| **CP-36 — Fallback Chains** | **Base existente.** O grafo é ligado a providers configuráveis no startup e já aparece no roadmap. Fechar políticas por principal/modelo, budgets de transição e semântica de perdas; não substituir por outro loop de retry. [E24] [E36] | F13; F18 |
| **CP-37 — User Routing Hints** | **Parcial.** Perfil de routing já circula por header e pode ser imposto pela política. Hints novos precisam de validação e clipping pelo servidor; não são permissão de escolher conta privada. [E04] [E17] | F18 |

### Bloco original J — Alertas e automação

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-38 — Anomaly Detection v1** | **Parcial em sinais.** Gaps de accounting e falhas já são detectáveis. Alertas de anomalia com baseline, histerese e deduplicação não foram comprovados de ponta a ponta. Construir regras explicáveis antes de ML. [E25] [E22] | F17 |
| **CP-39 — Notifications** | **Não comprovado.** Notificação externa com fila durável, retries, assinatura e proteção de destino não foi demonstrada. Implementar sobre eventos, tratando webhook como saída de dados e superfície de SSRF. [W08] | F17 |

### Bloco original K — API e ferramentas

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-40 — Admin API v1** | **API existente, contrato v1 pendente.** Há várias famílias de endpoints administrativos e de usuário. Não foi demonstrado OpenAPI versionado completo. Estabilizar DTOs/erros/paginação e adaptação das rotas antigas. [E10] [E29] | F7 |
| **CP-41 — `poolctl`** | **Não comprovado.** CLI administrativa poolctl completa não foi demonstrada. Construí-la sobre a API estável, sem acesso direto a Bolt/arquivos nem segredo administrativo hardcoded. [E13] [E29] | F19 |
| **CP-42 — Config versioning** | **Gap concreto.** ConfigFile não tem versionamento explícito, e startup/watcher podem usar caminhos diferentes. Validação e publicação atômica são pré-requisitos da evolução de schema. [E08] [E09] [E24] | F2.A; F7 |

### Bloco original L — Testing Lab

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-43 — Compatibility Matrix** | **Parcial · não homologada.** CI unitária/race não equivale a matriz de clientes/providers reais. Versionar fixtures, versões de cliente e cenários; distinguir fake determinístico de smoke com provider real. [E02] [E03] [E30] | F3; F13 |
| **CP-44 — Adapter Contract Tests** | **Parcial.** IR e translators carregam semântica específica já existente. Transformar regressões em contratos/goldens de request e stream; incluir perda de metadados e traduções sem suporte. [E18] [E19] [E05] | F3; F13 |
| **CP-45 — Fuzzing** | **Não certificado.** Fuzzing contínuo com corpus, budgets e regressões não foi comprovado. Antes de criar targets, inventariar Fuzz* existentes; cobrir parsers expostos e invariantes de memória/tempo. [E02] | F3 |
| **CP-46 — Chaos Testing** | **Parcial em mecanismos.** Há recuperação de accounting, circuitos e shutdown, mas não uma certificação de chaos matrix completa. Testar faults determinísticos com fake upstream/store antes de injeção em ambiente vivo. [E23] [E24] [E25] | F3; F14; F19 |
| **CP-47 — Shadow Routing** | **Divergência de semântica.** O shadow implementado chama proxyRequest e envia prompt. O original exigia não enviar. Manter dois modos explicitamente distintos; decision-shadow puro e traffic-shadow controlado. [E22] | F2.C; F11 |

### Bloco original M — Backup e operação

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-48 — Backup Automation** | **Manual segundo o roadmap; ampliar.** Preservar o backup/restore já descrito e verificar seu contrato antes de automatizar. Novo backup deve incluir ownership/grants e recuperação do vault; não reutilizar a mesma chave crua para tudo. [E14] [E24] | F19 |
| **CP-49 — Automatic Restore Verification** | **Homologação pendente.** Validação de manifest não comprova restauração operacional. Exigir restore isolado com autenticação, permissões, dados de usage, versões de chave e credenciais revogadas continuando revogadas. [E12] [E14] [E24] | F19 |

### Bloco original N — Escala e organizações

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-50 — Provider Plugin SDK** | **Interface já existe.** Provider e ProviderRegistry já são contratos concretos. Modularizar adapters e SDK interno com capability contracts; plugin executável carregado em runtime não é requisito atual. [E26] | F20 |
| **CP-51 — Model Lifecycle & Provider Change History** | **Parcial.** Discovery e pollers já existem no startup. Falta demonstrar lifecycle/histórico durável de alterações e notificações por consumidor afetado. Tratar regressão de capacidade como mudança versionada. [E24] | F14 |
| **CP-52 — Capability Probes** | **Fundir com CP-17.** Probes empíricos fazem parte da mesma capacidade de canaries. Compartilhar scheduler, budgets, autenticação e matriz de resultados em vez de duas implementações. [E24] | F14 |
| **CP-53 — Multi-node / HA** | **Futuro condicionado.** Federação não implica estado de controle distribuído. HA exige coordenação de grants, quotas, pins, refresh e jobs; implementar apenas após requisito de disponibilidade medido. [E24] [E17] | F21 |
| **CP-54 — SSO / OIDC** | **Futuro condicionado.** Passport e passkeys existem; OIDC não foi demonstrado. Fazer vinculação de identidade sem conceder role por email/domínio não verificado e preservar break-glass seguro. [E12] [E29] | F20 |
| **CP-55 — Organizations & Teams** | **Futuro condicionado.** Principals existem, mas organização/time não são o modelo atual lido. Introduzir escopo acima deles só com isolamento já resolvido e budgets/agregação definidos. [E12] [E17] | F20 |

### Bloco original O — Inteligência futura

| Fase original | Estado e conclusão | Próxima ação / destino |
|---|---|---|
| **CP-56 — Adaptive Routing** | **Há heurística, não aprendizado provado.** pool/auto já combina sinais, inclusive priors fixos por modelo. Rotular isso como aprendizado adaptativo seria incorreto. Medir qualidade e confiança antes de autoajustar pesos. [E21] | F18; F22 |
| **CP-57 — Semantic Router** | **Futuro condicionado.** Classificação semântica do workload não foi comprovada. Primeiro permitir hints explícitos e aliases; classificador deve ter custo, privacidade, fallback e qualidade mensurados. [E21] | F22 |
| **CP-58 — Automatic Model Escalation** | **Futuro condicionado.** O gateway não conhece por si só se os testes do repositório do usuário passaram. Escalação precisa de feedback autenticado do cliente/agente, budget e prevenção de loops. [E04] | F22 |
| **CP-59 — Policy Engine v2 com CEL** | **Futuro condicionado.** CEL não deve anteceder o modelo simples de política e grants. Se adotado, exigir ambiente restrito, limites de avaliação, versionamento e explicação; não substituir validações invariantes por scripts. [E17] | F22 |
| **CP-60 — Universal LLM Protocol** | **Reformular como contrato interno primeiro.** IR interno é necessário; protocolo público proprietário não é consequência obrigatória. Estabilizar adapters e tipos antes de prometer um Universal LLM Protocol externo. [E18] [E26] | F13; F22 |

### Síntese da reconciliação

O erro do roadmap antigo não é ter ambição. É apresentar, lado a lado, fundações já implementadas, gaps de integração, ideias de UX e capacidades de longo prazo como se todas fossem tarefas equivalentes e sequenciais. A nova execução deve fechar **invariantes verticais**: autorização real + API + UI + testes + migração, em vez de marcar um arquivo isolado como “fase concluída”.

---

## 6. Comparação com produtos semelhantes

**Base:** documentação oficial consultada durante a investigação. Esta comparação é funcional e arquitetural, não benchmark, auditoria de segurança desses produtos ou levantamento completo de preços/licenças. Uma feature documentada não prova que esteja disponível em toda edição comercial.

| Produto | O que a documentação confirma | Aprendizado válido para o Codex Pool |
|---|---|---|
| **CLIProxyAPI** | Protocolos compatíveis, OAuth e múltiplas contas com round-robin, tools/multimodal, SDK Go. O README atual informa que as estatísticas integradas foram retiradas a partir de v6.10.0 e indica soluções externas. [W01] | É o comparador mais próximo para conectividade de clientes e contas. O Pool deve competir pela operação e governança confiáveis, não apenas pela quantidade de providers. A integração de accounting própria pode ser útil, desde que consistente. |
| **LiteLLM** | Chaves virtuais com budgets/model access, gerenciamento de usuários/equipes, rotação e restrições sobre quem pode gerar chaves. [W02] | Separar credencial do consumidor de credencial upstream e oferecer políticas efetivas claras. Não copiar a interface sem copiar a semântica de limites e lifecycle. |
| **Bifrost** | Chaves virtuais podem ser limitadas a credenciais específicas do provider; budgets são verificados com os de team/customer. A documentação distingue isso de rate limits, que são de virtual key, não de team/customer. [W03] | A restrição por credencial upstream é o padrão mais diretamente aproveitável para contas privadas de membros. Não pressupor que “tem teams” já implique fairness ou RPM agregado entre todas as chaves. |
| **Portkey** | Virtual Keys foram migradas para **Model Catalog**, com credenciais organizacionais, compartilhamento entre workspaces e controles de modelos/budgets/rate limits. A página atual já usa a identificação PRISMA AIRS AI Gateway. [W04] | Catálogo deve ser uma visão governada, não só uma lista global. Evitar desenhar o novo Pool a partir de documentação antiga de Virtual Keys como se fosse o produto atual. |
| **OpenRouter** | Routing oferece ordenação/restrição de providers, controle de fallback e `require_parameters` para exigir suporte aos parâmetros. [W05] | O usuário precisa saber o que foi permitido, o que pode mudar no fallback e por que uma rota foi rejeitada. Hints devem restringir ou priorizar dentro da policy, nunca ampliar acesso. |

### 6.1 Onde investir

**Posicionamento recomendado:** um control plane self-hosted para capacidade de contas, clientes de programação e agentes, com isolamento de contexto, contribuição privada de membros e diagnóstico de protocolo. Não há nesta pesquisa prova de que essa combinação seja exclusiva; ela é uma direção coerente com a base existente.

**Evitar a competição errada:** copiar playgrounds genéricos, editores de prompts ou dezenas de gráficos não resolve o problema atual de saber quem usa qual conta, por qual permissão, com qual perda de contexto e qual consumo.

**Aprender sem importar complexidade:** limites/grants e catálogo governado entram cedo. Marketplace, billing multilateral, arbitragem de assinaturas e HA distribuído não entram por imitação de concorrente.

### 6.2 Matriz de valor proposta

| Decisão de produto | Valor esperado | Evidência necessária antes de ampliar |
|---|---|---|
| Contas privadas com grants explícitos | Compartilhar capacidade sem compartilhar o segredo ou tornar tudo global | Testes de autorização por objeto e revogação em todos os caminhos |
| Diagnóstico unificado de route/handoff | Reduzir tempo gasto investigando 429, troca de provider e incompatibilidade | Trace de pedido real correlacionado com decisão e erro |
| Quota com confiança e dados frescos | Evitar decisões baseadas em “0%” desatualizado ou estimado | Backtesting e origem da observação |
| Controle de consumo por principal | Evitar multiplicar limites ao criar novas credenciais | Teste com várias chaves da mesma pessoa |
| Compatibilidade publicada por versão | Tornar upgrades previsíveis para os clientes realmente usados | Fixtures e smokes reproduzíveis; resultados não apenas declarados |

---

## 7. Contas adicionadas por membros e compartilhadas pelo operador

### 7.1 Veredito

**A ideia é válida e merece implementação prioritária após o isolamento e o hardening.** Ela resolve um problema real de produto: permitir colaboração sem obrigar o operador a cadastrar manualmente toda conta e sem colocar toda credencial nova à disposição do grupo inteiro.

Hoje, a contribuição é restrita ao operador e o `Account` lido não contém ownership/grants. O recurso não deve ser implementado trocando `operator` por `member` num `if`. É um novo modelo de autorização sobre recursos, usando o Passport existente. [E10] [E11] [E15]

### 7.2 Cinco conceitos que precisam ser diferentes

| Conceito | Significado | Não confundir com |
|---|---|---|
| **Principal** | Pessoa ou identidade de serviço autenticada no Pool | Conta do provider |
| **Conta upstream** | Recurso que fornece capacidade no provider | API key do cliente do Pool |
| **Secret material** | OAuth token, refresh token, API key ou outro segredo armazenado no vault | Metadata visível da conta |
| **Credencial de cliente** | Chave que um cliente usa para consumir o Pool | Autorização para exportar a credencial upstream |
| **Grant** | Concessão de uso de determinada conta, a um sujeito, sob condições | Ownership ou participação num pool lógico |

`owner_principal_id`, `added_by_principal_id` e `upstream_account_id` também são campos diferentes. Quem realizou um cadastro não é necessariamente o dono; uma conta organizacional pode ter sido cadastrada por um operador.

### 7.3 Regra padrão proposta

```text
Membro A cadastra conta A1
    dono: Membro A
    administrada pela instalação
    uso padrão: Membro A + operador autorizado
    uso por Membro B / guest / serviço: NEGADO sem grant
    exportação do segredo upstream: não oferecida pela API/UI
```

O operador pode compartilhar o **uso** de A1 com outro principal por uma API administrativa, sem entregar o token upstream. O dono recebe uma visão de quem pode consumir a conta, com motivo e expiração da concessão.

Há uma decisão de produto que deve ser explícita na entrada:
- `owner_operator_only`: conta não delegável a terceiros;
- `operator_may_delegate`: dono aceita delegação pelo operador, com auditoria e visibilidade.

Para implementar exatamente a ideia apresentada, o segundo modo pode ser escolhido no cadastro, claramente explicado. O modo não pode mudar silenciosamente após o membro entregar a conta. A autorização técnica para usar uma conta também não substitui a necessidade de respeitar as condições do provider/plano aplicável; não há uma conclusão contratual uniforme nesta auditoria.

**Limite de confiança:** o operador da aplicação pode ter uso administrativo sem uma ação de revelar segredo. O administrador do host, contudo, controla o processo e as chaves. “Não devolvemos o segredo na UI” é uma garantia implementável; “o dono do servidor nunca consegue acessar o segredo” não é uma garantia desse desenho.

### 7.4 Permissões e papéis

Permissões de domínio propostas, e não rotas já existentes:

```text
accounts:contribute
accounts:read_own
accounts:use_own
accounts:rotate_own_secret
accounts:withdraw_own
accounts:manage
accounts:grant_use
accounts:revoke_grant
accounts:audit
```

Permitir `accounts:contribute` a membros selecionados; não promovê-los a operadores. Grants não podem conceder `accounts:manage` ou `accounts:grant_use` como efeito de conceder inferência.

| Ação | Dono membro | Outro membro sem grant | Outro membro com grant API | Operador autorizado |
|---|---:|---:|---:|---:|
| Cadastrar conta própria | Com capability | Com capability para as próprias | Idem | Sim |
| Usar A1 | Sim, sujeito a limites | Não | Dentro do grant | Sim, sujeito a regras administrativas |
| Ver segredo upstream por GET | Não | Não | Não | Não pela API normal |
| Ver detalhes administrativos de A1 | Dados próprios permitidos | Não | Somente projeção necessária | Sim |
| Renovar a credencial de A1 | Com reautenticação e prova de vínculo | Não | Não | Fluxo administrativo autorizado |
| Conceder uso de A1 a terceiro | Não no modelo inicial | Não | Não | Se a conta permitir delegação |
| Retirar a própria contribuição | Sim | Não | Não | Pode suspender/administrar |
| Ler prompts de outros usuários de A1 | Não por ser dono da conta | Não | Não | Não por mero direito de usar A1 |

Ownership de capacidade não dá direito de ler conversas de consumidores. Estatísticas por consumidor devem expor somente o necessário para operação e prestação de contas.

### 7.5 Modelo de dados proposto

**AccountMetadata:** `id`, `provider_id`, `upstream_account_fingerprint`, `owner_principal_id`, `added_by_principal_id`, `ownership_mode`, `delegation_policy`, `secret_ref`, `desired_state`, `revision`, `created_at`, `updated_at`.

**AccountGrant:** `id`, `account_id`, `subject_type`, `subject_id`, `audience`, `allowed_models`, `allowed_capabilities`, `budget_policy_id`, `expires_at`, `created_by`, `reason`, `revoked_at`, `revision`.

**Uso observado:** `request_id`, `attempt_id`, `consumer_principal_id`, `client_credential_id`, `resource_account_id`, `account_owner_principal_id`, `grant_id`, `purpose`, `usage_quality`.

Não colocar o token upstream dentro do objeto de grant. Não usar email como chave de ownership. Um fingerprint para deduplicação não deve vazar o identificador externo em endpoints de usuários sem acesso.

### 7.6 Autorização efetiva

```text
autenticação válida
AND principal ativo
AND credencial do cliente ativa
AND conta não retirada/suspensa
AND (dono OU direito operacional do operador OU grant válido)
AND audiência e capabilities permitidas
AND modelo/provider permitido pela policy efetiva
AND limites de principal/credencial/grant/conta
AND elegibilidade operacional da conta
```

Participar de um pool lógico não concede acesso. O pool organiza o conjunto já autorizado.

Essa mesma decisão precisa existir antes de scoring, pin reuse, fallback, troca de provider, resposta de catálogo, diagnóstico e execução de probes em nome do usuário. Um grant revogado não pode sobreviver no cache de permissões porque uma conversa ficou pinned.

### 7.7 API proposta

As rotas abaixo são **contratos sugeridos**, não endpoints existentes no commit auditado:

| Método e rota | Função | Proteção |
|---|---|---|
| `POST /api/v1/account-contributions` | Inicia contribuição OAuth/API key para provider aprovado | Capability, rate limit, limite de payload e vínculo ao ator |
| `GET /api/v1/me/accounts` | Lista contas próprias e projeções de contas concedidas | Filtragem pelo principal; sem segredo |
| `GET /api/v1/me/accounts/{id}` | Inspector permitido | Autorização por objeto |
| `POST /api/v1/me/accounts/{id}/withdraw` | Retira contribuição própria | Reautenticação conforme risco, idempotência |
| `POST /api/v1/admin/accounts/{id}/grants` | Concede uso a sujeito definido | Permissão administrativa, consentimento de delegação e CSRF se sessão |
| `DELETE /api/v1/admin/accounts/{id}/grants/{grantId}` | Revoga uso | Idempotência e atualização de revision |
| `GET /api/v1/admin/accounts/{id}/access` | Explica acesso efetivo | Metadados seguros, ator auditado |
| `POST /api/v1/routing/simulate` | Explica a rota no conjunto autorizado | Snapshot puro; sem prompt para upstream |

Exemplo de payload de concessão:

```json
{
  "subject_type": "principal",
  "subject_id": "principal-b",
  "audience": ["inference_api"],
  "allowed_models": ["alias-code-approved"],
  "budget_policy_id": "budget-collaboration",
  "expires_at": "2026-10-15T00:00:00Z",
  "reason": "Colaboracao autorizada pelo operador"
}
```

O grant é vinculado ao sujeito autenticado. “Compartilhar pela API” não significa uma URL anônima que qualquer pessoa possa usar, nem permitir que o cliente envie `owner_principal_id` para adquirir ownership.

### 7.8 Cadastro seguro e consistência entre arquivo e banco

Reutilizar OAuth state, CSRF e vínculo de ator já existentes. O fluxo deve carregar principal, provider, intenção e expiração; o callback não pode trocar o proprietário a partir de um campo livre do cliente. Manter o state de uso único e PKCE onde o provider suportar/exigir.

Membro não pode cadastrar livremente base URLs, redirects, arquivos arbitrários ou comandos de provider. Novos destinos precisam ser aprovados pelo operador; isso evita transformar a contribuição em superfície de SSRF/egress livre. Trata-se de prevenção para a feature nova, não de afirmação de uma SSRF já explorada. [W08]

Metadados em Bolt e segredo em arquivo cifrado não participam da mesma transação. Usar um fluxo idempotente: metadata `pending`, escrita do segredo, ativação com referência validada. Após crash, recuperar ou limpar registros pendentes/orfandades com segurança. Nunca marcar a conta roteável antes de persistir a autorização e o segredo com sucesso.

Conta duplicada deve ser detectada pelo identificador estável do provider/tenant quando disponível. Não deixar uma nova contribuição “roubar” silenciosamente ownership de uma conta já cadastrada.

### 7.9 Revogação, suspensão e saída do membro

Revogar grant impede **novas admissões** imediatamente após o commit da revisão. A política para streams já admitidos precisa ser explícita: concluir dentro de um limite ou interromper por uma ação emergencial. Não prometer revogação instantânea em um upstream que já recebeu o prompt.

Retirada do dono remove a conta das novas escolhas, invalida grants aplicáveis e impede fallback/pins de reutilizá-la. Transferência de ownership é uma operação distinta, auditada, não consequência automática de suspender o membro.

Se uma identidade OIDC for removida ou um backup for restaurado, revogações precisam continuar válidas conforme o modelo de recovery. Nenhuma rotina de refresh deve ressuscitar uma conta retirada pelo dono.

### 7.10 Quotas, custo e incentivo

Controlar duas dimensões: **quem consome** e **qual recurso foi consumido**. Usage da conta serve ao dono/operador; budget do consumidor serve à política de acesso. Não debitar toda utilização ao dono como se ele tivesse originado todos os pedidos.

Uma reserva de capacidade para o dono é uma boa extensão, mas deve usar unidades confiáveis. Em providers com quota opaca, mostrar reserva estimada ou limite de concorrência em vez de fingir conhecer tokens restantes.

Não implementar agora “créditos negociáveis” ou pagamento automático entre membros. Primeiro é necessário saber medir consumo, tratar erros, manter reconciliação e definir regras de custo real versus API-equivalente.

### 7.11 UI mínima correta para a capacidade completa

**Membro:** “Minhas contas” para capacidade upstream; “Meus clientes” para chaves do Pool. Cada conta mostra saúde, quota/frescor, quem pode usar, delegação habilitada, último uso agregado e botão de retirar contribuição.

**Operador:** inspector de conta com proprietário, permissões efetivas, grants, histórico, capacidade e ações. Compartilhar exige revisão do destinatário, modelos, validade e impacto. Mostrar “Privada: dono + operador” como estado inicial.

**Consumidor com grant:** vê a capacidade autorizada sem precisar conhecer email/token do dono. Uma negativa deve explicar a ação possível sem revelar a existência de outras contas privadas.

### 7.12 Testes de aceite específicos

O conjunto obrigatório inclui: duas pessoas com mesmo conversation ID; listagem/GET sem grant; uso por alias/pool-auto/fallback; conta pinned após revogação; grant expirando durante fila; callbacks OAuth trocados entre atores; upload duplicado; retirada do dono; restauração; várias credenciais do mesmo consumidor; e uso de account ID arbitrário no body.

**Critério de segurança:** um consumidor não autorizado provoca **zero tentativas upstream** com a conta privada. A ausência de um botão no frontend não conta como teste.

---

## 8. Novo roadmap F1–F22, por blocos

**Convenção:** `F4.B`, por exemplo, é um bloco implementável dentro da fase F4. P0/P1 são os achados de segurança da seção 3, não novos nomes de fases.

Cada bloco deve produzir uma mudança pequena e testável. O commit segue o padrão já usado no repositório, como `fix(auth): ...`, `feat(accounts): ...` ou `test(routing): ...`, com escopo ajustado ao trabalho real. Nenhuma fase é concluída apenas por ter criado tipos ou tela.

A referência de código é o commit auditado. Ao iniciar uma implementação, comparar o HEAD atual com esse SHA e atualizar o delta: mudanças posteriores podem já ter fechado um bloco.


### BLOCO A — Segurança e evidência

#### F1 — Isolamento de conversas e fechamento do P0

**Objetivo:** Impedir compartilhamento de histórico entre principals sem alterar a continuidade legítima de uma conversa.

**Reaproveitar:** `proxyRequest`, `conversationHandoffStore`, native session state e testes de transição. Não reescrever os adapters nesta correção.

**Dependências e gate:** Nenhuma feature nova. Esta fase bloqueia a expansão multiusuário do handoff.

**F1.A — Reprodução vertical.** Adicionar teste autenticado com dois principals, mesmo ID externo e fake upstream que capture o payload. Travar a regressão antes do patch.

**F1.B — Chave canônica.** Criar tipo interno para escopo de conversa; aplicar a lookup, gravação, recovery, pins, HTTP/WS/streamed e Antigravity. Identidade vem da autenticação, nunca do body.

**F1.C — Snapshots e migração.** Separar identificadores nativos e escopo interno; invalidar estado antigo sem dono; devolver snapshots sem aliases mutáveis para quem lê fora do lock.

**F1.D — Fechamento.** Testar volta ao provider original, assistant recording, revogação e diagnóstico. Fazer commit de segurança pequeno, sem features de UI não relacionadas.

**Aceite:** O teste de vazamento falha no commit base e passa no candidato; nenhum sentinel de A chega ao upstream de B; continuidade do mesmo principal preservada; `go test -race` verde.

**Rollout/rollback:** Canário com identidades sintéticas; eliminar estado em memória não atribuível. Rollback não pode reabrir compartilhamento multiusuário vulnerável.

**Rastreabilidade:** P0-01; CP-11, CP-13, CP-15.

---
#### F2 — Configuração fail-closed, vault e superfície de segurança

**Objetivo:** Fazer o deploy corresponder à política pretendida e estabelecer um perfil seguro para instalações compartilhadas.

**Reaproveitar:** Parsers/config atual, middleware IP, vault centralizado, Passport e rotação existentes.

**Dependências e gate:** F1 para o gate de publicação multiusuário; blocos de configuração podem ser desenvolvidos em paralelo.

**F2.A — Rede e configuração.** Parser de CIDR com erro; caminho único para startup/watcher; rejeitar TOML/configuração explicitamente inválida; reload all-or-nothing; remover exceção loopback indiscriminada das rotas de dados/admin.

**F2.B — Segredos e diagnóstico.** Perfil compartilhado exige vault; documentar chaves distintas; proteger debug/trace com secrets; incluir estado de proteção sem expor valores; verificar falhas e migrações de escrita.

**F2.C — Shadow seguro.** Separar os nomes e flags de decision-shadow/traffic-shadow; limitar deadline, concorrência e destinos do tráfego duplicado; não habilitar shadow real por uma opção ambígua de simulação.

**F2.D — Supply chain.** Obter audit JSON, triar os quatro alertas observados, corrigir dependências alcançáveis, executar análise Go e gerar SBOM; adotar exceções revisáveis, sem fix --force automático.

**Aceite:** Reprodução IP incluída passa a exigir rejeição de configuração; startup e watcher usam o mesmo arquivo; reload inválido não remove restrições; nova contribuição compartilhada recusa vault ausente; alertas têm resolução ou justificativa rastreável.

**Rollout/rollback:** Mudanças de defaults com migration notes e comando de diagnóstico. Não quebrar instalações locais silenciosamente; o operador deve escolher explicitamente o perfil de compatibilidade.

**Rastreabilidade:** P1-01 a P1-05; CP-01, CP-02, CP-42, CP-47.

---
#### F3 — Base de testes, contratos, browser e medições reproduzíveis

**Objetivo:** Transformar a evidência de CI em gates que cubram as invariantes do produto, não apenas compilação e mocks felizes.

**Reaproveitar:** Workflow atual, Vitest, testes Go, bench runner e fixtures existentes. Primeiro inventariar targets Fuzz* e testes de falha já presentes.

**Dependências e gate:** Gate inicial independente; regressões de F1/F2 entram imediatamente e todos os outros blocos dependem deste padrão.

**F3.A — Matriz de entradas.** Catalogar HTTP, WS, streamed/oversized, native context, passthrough e admin; para cada um registrar autenticação, policy, cancelamento, contabilização e tratamento de erro.

**F3.B — Contratos e browser.** Atualizar mocks de recovery/status e setup-link; tornar Chromium/Puppeteer ou alternativa uma dependência reproduzível de CI; testar login, setup, conta, pass e recuperação em desktop/mobile.

**F3.C — Performance.** Medir baseline local determinística com fake upstream temporizado; separar overhead do proxy e upstream; guardar hardware, versões, cenário, distribuição e hash. Benchmark real opcional em ambiente controlado.

**F3.D — Fuzz e faults.** Expandir corpus de SSE/JSON/tool-pairs/gzip/zstd/OAuth; injetar cancelamentos, 401/429/5xx, truncamento, erro de Bolt e indisponibilidade DuckDB; limitar tempo e memória.

**Aceite:** Comandos reproduzíveis documentados; browser executado no CI; fixtures representam a API atual; performance mock nunca rotulada performance real; falha de segurança e de contrato bloqueia merge.

**Rollout/rollback:** Adicionar gates por classe para não tornar toda PR dependente de contas pagas. Smokes reais separados, com quotas e segredos de teste.

**Rastreabilidade:** CP-00-real, CP-00, CP-33, CP-34, CP-43, CP-44, CP-45, CP-46.

---

### BLOCO B — Capacidade compartilhada e governança

#### F4 — Modelo de ownership e autorização por conta

**Objetivo:** Estabelecer a fronteira de acesso de cada conta antes de permitir contribuição por membros.

**Reaproveitar:** Principal/ClientCredential do Passport e Account existente. Criar módulo de autorização de recursos, não outro sistema de login.

**Dependências e gate:** F1 e contrato de F3; perfil seguro de F2 para publicação.

**F4.A — Dados e migração.** Adicionar metadata versionada, owner, added_by e secret_ref; distinguir contas operator-managed legadas de novas contribuições. Produzir preview de migração e preservar acesso existente só por regra explícita.

**F4.B — Authorizer.** Implementar decisão tipada de read/use/manage/grant/withdraw com deny-by-default e reason codes. Membership de pool lógico não concede permissão.

**F4.C — Cobertura vertical.** Plugar autorização em seleção, pins, auto, fallback, catálogo e diagnósticos; evitar leaks de email, ID externo e quota de contas não autorizadas.

**F4.D — Revisões e cache.** Versionar metadata/permissões; invalidar caches por revision; testar TOCTOU entre avaliação, admissão e revogação.

**Aceite:** Sem autorização, zero upstream attempts e nenhuma metadata privada; testes por papel e recurso; migração não torna contas de membros globais; owned account não dá acesso a prompts de outros consumidores.

**Rollout/rollback:** Criar metadata e executar avaliação em modo comparativo sem abrir acesso novo; ativar enforcement antes da primeira contribuição.

**Rastreabilidade:** CP-04, CP-05, CP-06; nova ideia de contas de membros.

---
#### F5 — Contribuição privada de contas por membros

**Objetivo:** Permitir cadastro autônomo com uso restrito ao dono e operador, sem ampliar privilégios administrativos.

**Reaproveitar:** Fluxos de contribuição/OAuth existentes com CSRF, state e vínculo ao ator; vault e endpoints de setup atuais.

**Dependências e gate:** F2, F3 e F4. A liberação para múltiplos membros exige também o limite agregado mínimo de F9.B.

**F5.A — Capability e elegibilidade.** accounts:contribute concedida explicitamente; providers aprovados; limite de contas e tentativas por principal; não aceitar base URL arbitrária ou campos de ownership do cliente.

**F5.B — Onboarding.** Fluxos separados para OAuth e API key; ownership vinculado ao ator autenticado; state de uso único; confirmação de delegação e uso pelo operador antes da ativação.

**F5.C — Persistência resiliente.** Estado pending até metadata e secret_ref estarem válidos; escrita cifrada idempotente; recuperação de crash e deduplicação por identidade estável do provider.

**F5.D — Experiência do membro.** Minhas contas separada de Meus clientes; estados pendente/verificando/ativa/precisa de login; retiro de contribuição e rotação própria com proteção adequada.

**Aceite:** Membro A cadastra A1 e B não a lista nem usa; callbacks trocados falham; segredo não aparece na resposta de consulta; duplicatas não mudam ownership; falha de gravação não deixa conta roteável sem autorização.

**Rollout/rollback:** Feature flag, grupo controlado de contribuidores e somente contas privadas. Não abrir compartilhamento com terceiros nesta fase.

**Rastreabilidade:** CP-02, CP-04, CP-25, CP-30; nova contribuição privada.

---
#### F6 — Grants de uso e compartilhamento administrativo pela API

**Objetivo:** Permitir ao operador conceder uso de uma conta a terceiros sob escopo explícito e revogável.

**Reaproveitar:** Authorizer de F4, contribuição de F5 e audit do Passport; grants não carregam secrets.

**Dependências e gate:** F4; F5 para compartilhar contribuições de membros; F9.B antes de liberação ampla.

**F6.A — Contrato de grant.** Sujeito, account, audiência, modelos/capabilities, validade, budget, autor, motivo e revision. Validar operator_may_delegate antes de conceder.

**F6.B — Mutações seguras.** POST/DELETE idempotentes, CSRF para sessão, autorização por objeto e controle de versão; restringir service credentials administrativas ao escopo necessário.

**F6.C — Revogação.** Bloquear novas admissões após commit; invalidar pins/caches; política de streams já iniciados explícita; retirada do dono não pode ser revertida por refresh.

**F6.D — Transparência.** Inspector de permissões efetivas para dono/operador; trilha de auditoria e notificação de alteração; consumo atribuído a consumidor e recurso separadamente.

**Aceite:** Matriz A/B/operador/guest/serviço cobre GET, inferência, alias, fallback, WS e expiração em fila; grant revogado causa zero novas tentativas; segredo upstream nunca é compartilhado.

**Rollout/rollback:** Compartilhamento pontual por destinatário, com expiração. Grants globais e grupos dinâmicos só depois de terem semântica e testes próprios.

**Rastreabilidade:** CP-04, CP-06, CP-28, CP-40; compartilhamento pedido pelo usuário.

---
#### F7 — API administrativa estável e evolução de configuração

**Objetivo:** Tornar UI, CLI e automação clientes dos mesmos contratos, com migração compatível.

**Reaproveitar:** Rotas e handlers existentes; DTOs de Passport/Accounts; validação fail-closed de F2. As rotas novas de F5/F6 já devem nascer com schema; esta fase consolida toda a superfície.

**Dependências e gate:** F2, F3 e modelo de F4.

**F7.A — Inventário e OpenAPI.** Descrever superfícies atuais, auth/CSRF e modelos de erro; criar /api/v1 para administração e recursos de usuário; gerar tipos/testes de contrato sem reimplementar o domínio.

**F7.B — Semântica HTTP.** Paginação por cursor, limites de listagem, códigos de erro estáveis, request_id, status assíncrono para ações longas, idempotência e revisão/If-Match nas mutações.

**F7.C — Compatibilidade.** Adaptar rotas antigas aos novos serviços; registrar uso para depreciação; tratar 204/JSON inválido e erros estruturados no frontend sem casts silenciosos.

**F7.D — Config migrations.** config_version, validate/migrate/diff e snapshot efetivo redigido; manter uma única resolução de caminho e precedência; não expor valores de secrets.

**Aceite:** Schemas exercitados contra respostas reais dos handlers; testes de clientes atuais verdes; erro previsível em config inválida; requests repetidos não duplicam conta/grant/ação.

**Rollout/rollback:** Versão nova convivendo com adaptadores antigos. Remoção só após política documentada de compatibilidade, nunca como efeito colateral de reorganização de arquivos.

**Rastreabilidade:** CP-40, CP-42 e fundação de CP-41.

---
#### F8 — Lifecycle autoritativo, controles por conta e pools lógicos

**Objetivo:** Dar controle real de operação e capacidade sem misturar saúde observada com intenção administrativa.

**Reaproveitar:** accountstate, account availability, Account e scoring existente; authorizer de F4.

**Dependências e gate:** F4, F7 e gates de F3; F6 para contas concedidas.

**F8.A — Estado desejado/observado.** Persistir enabled, draining, maintenance e withdrawn separadamente da saúde; transições com ator, razão e revision; regras para refresh não desfazer suspensão.

**F8.B — Controles.** Max concurrency por conta com aquisição atômica, priority/weight, tags/notes, model restrictions e limites de fallback. Notas privadas têm projeção própria.

**F8.C — Pools.** Conta em múltiplos pools; default por policy; fallback entre pools só por regra explícita e após autorização; migração de diretórios sem mudar provider identity.

**F8.D — Reservas.** Reservas rígidas/elásticas em unidades conhecidas; quotas opacas marcadas como estimadas; preservação de capacidade do dono e de workloads foreground.

**Aceite:** Draining recusa conversas novas e mantém somente pins válidos e autorizados; max concurrency não excede o teto sob race; pool membership não abre acesso; estados sobrevivem a restart.

**Rollout/rollback:** Ativar controles um a um, com métricas de rejeição e simulação. Valores novos têm defaults compatíveis e não reduzem capacidade sem preview.

**Rastreabilidade:** CP-03, CP-04, CP-05, CP-08, CP-25.

---
#### F9 — Política efetiva e budgets agregados

**Objetivo:** Controlar a pessoa e o recurso, não apenas cada chave de cliente de forma isolada.

**Reaproveitar:** ClientPolicy, policyAdmission, token reservations, Release e contadores duráveis atuais.

**Dependências e gate:** F3, F4 e contratos de F7; integra grants de F6 quando habilitados.

**F9.A — Composição.** Definir interseção global/role/principal/credential/grant e defaults; negar explicitamente sem herança por coincidência de label; fornecer origem de cada permissão/limite.

**F9.B — Orçamento agregado mínimo.** Contadores de principal compartilhados por todas as credenciais e de recurso/grant; admissão e reserva atômicas; revogação e reinício não multiplicam orçamento.

**F9.C — Unidades e modos.** Requests/tokens/janelas e limites USD reais ou API-equivalentes explicitamente separados; observe/soft/hard; política de uso desconhecido sem assumir custo zero.

**F9.D — Editor efetivo.** Preview backend com fontes e restrições, erro antes de salvar e diff auditável; alertas graduados com deduplicação.

**Aceite:** Duas ou mais chaves do mesmo principal não excedem o agregado; concorrência não ultrapassa reserva; retry não duplica request lógico; restore/restart mantêm os limites aplicáveis; UI e enforcement retornam a mesma decisão.

**Rollout/rollback:** Modo observe para limites novos não relacionados a autorização; permissões privadas sempre hard-enforced. F9.B é gate de liberação ampla de contas de membros.

**Rastreabilidade:** CP-06, CP-28 e parte de CP-08.

---
#### F10 — Fila justa, backpressure e admissão sob saturação

**Objetivo:** Evitar que um consumidor monopolize capacidade sem criar filas ilimitadas ou latência artificial fora de saturação.

**Reaproveitar:** Admissão/pacing atuais, budgets de F9 e limites por conta de F8.

**Dependências e gate:** F3, F8 e F9.

**F10.A — Scheduler.** Fila por principal/workload com algoritmo explícito e peso limitado; escolher WFQ/DRR pela carga medida, não por nome de feature.

**F10.B — Limites de espera.** Cap global e por sujeito, tempo máximo, cancelamento, rejeição com Retry-After quando adequado e nenhum waiter órfão.

**F10.C — Integração.** Reservar orçamento no momento correto, revalidar grant/estado ao sair da fila e liberar exatamente uma vez; foreground não gera starvation permanente de background.

**F10.D — Medição.** Cenários de usuário agressivo + vários leves; workloads de durações distintas; métricas de espera e distribuição de serviço.

**Aceite:** Memória e waiters limitados; revogação durante espera respeitada; fair share medido em carga homogênea e heterogênea; sem regressão material no caminho sem saturação.

**Rollout/rollback:** Desabilitado por padrão até medição; ativação por pool/workload e retorno ao admission fail-fast sem perda de contadores.

**Rastreabilidade:** CP-07, CP-08.

---

### BLOCO C — Routing, contexto e confiabilidade

#### F11 — Decision core puro, explain e Routing Lab

**Objetivo:** Compartilhar uma única lógica explicável entre inferência, simulação e comparação de estratégias.

**Reaproveitar:** Pesos/perfis, traces, pool/auto, dry-run de transição e breaker existentes.

**Dependências e gate:** F3, F4, F7 e filtro efetivo de F9.

**F11.A — Evaluate/Admit.** Avaliar snapshot imutável de contas autorizadas e capabilities; separar pontuação de mutações/probe leases; revalidar revision na admissão.

**F11.B — Razões estáveis.** Reason codes para policy, grant, state, quota, capability e circuit; trace sem segredo/prompt; usuário só vê dados do seu universo autorizado.

**F11.C — Simulação.** Endpoint account-route e integração do dry-run de transição já existente; zero upstream calls e zero alteração de pins/contadores; comparação de estratégias no mesmo snapshot.

**F11.D — Shadow e overrides.** Decision-shadow sem prompt; traffic-shadow permanece mecanismo separado de F2.C. Overrides reais têm TTL, autor e escopo, sem bypass de autorização.

**Aceite:** Mesmos fatos e seed geram decisão reproduzível; simulator e evaluator real têm paridade; simular não muda circuit entries/inflight; pool/auto seleciona alternativa autorizada em vez de apenas escolher proibida e falhar.

**Rollout/rollback:** Shadow de decisão para observar divergências antes de substituir o caminho de escolha; contrato legado mantido até paridade.

**Rastreabilidade:** CP-09, CP-10, CP-12, CP-27, CP-47.

---
#### F12 — Pins e estado de conversa escopados, limitados e recuperáveis

**Objetivo:** Controlar stickiness e contexto sem leaks, crescimento de memória ou ressuscitação de acesso revogado.

**Reaproveitar:** Chave canônica de F1, conv pins, handoff store e contexto nativo; não unir mapas com semânticas distintas sem contrato.

**Dependências e gate:** F1, F3 e F4.

**F12.A — Registro.** Principal, conversation ID externo, provider/account, creation/last_seen, expiry, reason e revisions; distinguir pin de estado de protocolo.

**F12.B — Retenção.** TTL/LRU e teto de bytes, não só de quantidade; quotas por principal; tratar blobs multimodais por referência controlada e expiração.

**F12.C — Persistência opcional.** Recovery com revalidação de grant, estado e credencial; proteção de conteúdo em repouso; não restaurar IDs nativos inválidos automaticamente.

**F12.D — Operação.** Release/manual migration auditáveis; painel de pins com escopo; cancelamento/retirada do dono invalida acesso conforme contrato.

**Aceite:** IDs iguais entre principals nunca colidem; limites efetivos de registros e bytes; restore não reabre grants revogados; release seguro sob requests concorrentes; snapshots não vazam maps/slices mutáveis.

**Rollout/rollback:** Primeiro isolamento e limites em memória; persistência é opt-in com migration e política de retenção explícita.

**Rastreabilidade:** CP-11 e retenção de CP-13/CP-15.

---
#### F13 — IR, compactação, handoff e retry com contratos semânticos

**Objetivo:** Trocar provider e recuperar falhas sem cortar pares, inventar resultados ou esconder perda de capacidade/contexto.

**Reaproveitar:** ConversationIR, Message bridges, translators, sanitizeConversationToolPairs, transition modes e fallback graph.

**Dependências e gate:** F1, F3, F11 e F12.

**F13.A — IR por contrato.** Inventariar diferenças dos adapters; migrar um par por vez; preservar roles/proveniência, tools e multimodal; manter opacidade de assinaturas/IDs nativos.

**F13.B — Compactação.** Budget por modelo/capability com estimativa qualificada; pares indivisíveis; preservar ordem; não promover texto não confiável a instrução de sistema; emitir relatório de perda.

**F13.C — Política de transição.** never/stateless-only/before-first-token/translate-context por policy; tolerância a perdas explícita; nenhuma troca de destino fora dos grants e regras de dados.

**F13.D — Retry seguro.** Definir ponto de saída visível, request lógico versus attempts, idempotency keys quando suportadas e rollback de leases; não repetir tools nem geração já comprometida silenciosamente.

**Aceite:** Matriz texto/tools/paralelas/imagem/contexto longo/cancelamento/429/retorno ao provider; validator antes/depois; testes longos com fixtures; zero resultado de tool fabricado; cada perda aparece em diagnóstico estruturado.

**Rollout/rollback:** Feature flags por adapter e modo; comparações golden; fallback antigo não pode ser usado como bypass de política quando o novo caminho rejeita.

**Rastreabilidade:** CP-12, CP-13, CP-14, CP-15, CP-19, CP-36, CP-43, CP-44, CP-60 interno.

---
#### F14 — Saúde, refresh, circuitos e lifecycle de modelos

**Objetivo:** Descobrir falhas e mudanças de capacidade sem consumir quota descontroladamente ou derrubar recursos saudáveis.

**Reaproveitar:** CircuitBreakerManager de quatro níveis, refresh/pollers, discovery e estados observados existentes.

**Dependências e gate:** F3 e controles de F8; scopes de F4 para probes e projeções.

**F14.A — Circuitos.** Validar composição provider/account/model/capability em todas as entradas; limites efetivos de cardinalidade e probe lease; endpoint scope apenas se houver falha concreta que o exija.

**F14.B — Refresh.** Formalizar deduplicação existente; waiters canceláveis, timeout, backoff+jitter e resultados de erro tipados; não ressuscitar withdrawn/disabled administrativamente.

**F14.C — Canaries unificados.** Fundir CP-17/52; auth/model/quota baratos e probes pagos explicitamente classificados; orçamento global/por conta, jitter e opt-in de testes de imagem/contexto.

**F14.D — Lifecycle.** Catalog changes versionadas, last_verified, confidence, deprecation/removal e consumidores afetados; não trocar capabilities desconhecidas por false/true arbitrários.

**Aceite:** Um refresh por conta sob concorrência; probe vazado expira; falha de um modelo não derruba conta inteira; alteração externa reproduzida por fixture; canary respeita budgets e ownership.

**Rollout/rollback:** Health passivo primeiro; testes pagos só por autorização e budget; detecção não desabilita modelos globais automaticamente por um único resultado frágil.

**Rastreabilidade:** CP-16, CP-17, CP-18, CP-51, CP-52.

---

### BLOCO D — Observabilidade e experiência de produto

#### F15 — Accounting confiável, métricas corretas e rastreamento

**Objetivo:** Dar ao router e ao operador dados com significado, origem e qualidade conhecidos.

**Reaproveitar:** Bolt/outbox/DuckDB, SQLite legado, accounting gaps, pricing, RequestUsage e métricas existentes.

**Dependências e gate:** F3 e identidade/recurso de F4.

**F15.A — Ledger de uso.** Chave de idempotência de accounting por evento/attempt; separar request lógico, retry e shadow; consumer versus owner; erro de write deve aparecer, não sumir em Record.

**F15.B — Semântica.** TTFB/primeiro evento/TTFT semântico; tokens reported/estimated/unknown; preços e timestamps de origem; não rotular bytes/4 como throughput real.

**F15.C — Agregações e tracing.** Percentis e SLOs com cardinalidade limitada; OpenTelemetry/structured logs por etapa, sem prompt/secrets por padrão; correlação request/attempt/incident.

**F15.D — Consolidação e previsões.** Reconciliar stores e preparar aposentadoria do SQLite legado; forecast com confiança e backtesting; distinguir uso, quota e valor API-equivalente.

**Aceite:** Fault de persistência visível; nenhuma duplicação por retry/replay; métricas de stream comparadas a fixture com primeiros eventos distintos; cada dashboard informa freshness/coverage; nenhum conteúdo sensível nas exportações padrão.

**Rollout/rollback:** Instrumentação em paralelo ao dado legado, com reconciliação; remover uma fonte apenas depois de demonstrar paridade de períodos e dimensões.

**Rastreabilidade:** CP-21, CP-22, CP-23, CP-26.

---
#### F16 — UI/UX operacional, setup, mobile e acessibilidade

**Objetivo:** Organizar operações reais por papel sem transformar o dashboard numa coleção de controles sem efeito.

**Reaproveitar:** Views/URLs, insights, recovery guard, confirmação de ações, Api client e identidade visual existentes.

**Dependências e gate:** F3 e F4 para estrutura; cada subfluxo depende do backend correspondente de F5–F15.

**F16.A — Arquitetura de informação.** Workspace de operador por operação; membro distingue contas e clientes; aprofundamento por inspector/deep link; filtros e seleção preservados na URL.

**F16.B — Operação de contas/políticas.** Owner, grants, quota/freshness, health, estado desejado/observado e ações; preview de impacto; bulk actions com resultado por item e confirmação proporcional ao risco.

**F16.C — Setup e feedback.** Teste conexão por etapas: auth/catalog primeiro, geração/tools somente opt-in com custo informado; estados pending/success/error/cancel; erro com request_id e ação segura.

**F16.D — Entrega de UI.** Lazy chunks por workspace/charts, orçamento de bundle medido; keyboard/focus/live regions/table fallback; mobile com jornadas prioritárias; palette só reutiliza comandos autorizados.

**Aceite:** Browser CI cobre jornadas reais, inclusive respostas atrasadas e clipboard failure; nenhum label de sucesso sem backend confirmar; viewport móvel e teclado utilizáveis; critérios WCAG 2.2 AA medidos, sem promessa de conformidade não avaliada.

**Rollout/rollback:** Entregas verticais por tela, sem redesenhar toda a identidade de uma vez. Acessibilidade e mobile fazem parte de cada tela, não de uma fase corretiva no fim.

**Rastreabilidade:** CP-24, CP-25, CP-26, CP-27, CP-28, CP-30, CP-31, CP-33, CP-34.

---
#### F17 — Incidentes, timeline, anomalias e notificações

**Objetivo:** Transformar sinais existentes em ações operacionais sem alertas duplicados, vazamentos ou tempestades.

**Reaproveitar:** Audit do Passport, transições, accounting gaps e métricas corrigidas de F15.

**Dependências e gate:** F7, F15 e actions/backend de F8/F14; UI de F16.

**F17.A — Evento comum.** Schema com actor/resource/time/request/revision/severity; retenção e projeção por principal; tratar logs de diagnóstico como fonte distinta de audit imutável.

**F17.B — Incidentes.** Agrupar sintomas por fingerprint; open/acknowledged/resolved; histerese e período silencioso; impacto e ação sugerida sem executar mitigação destrutiva automaticamente.

**F17.C — Anomalias.** Burn/rate de erro/latência e modelo removido com baseline mínimo; mostrar confiança; suppressão durante manutenção e tratamento de dados ausentes.

**F17.D — Notificações.** Outbox durável, retries limitados, assinatura e redaction; allowlist de destinos/controle SSRF; conectores específicos só depois do webhook genérico seguro.

**Aceite:** Repetição do mesmo erro atualiza um incidente em vez de criar centenas; webhook indisponível não bloqueia inferência; credenciais/prompts não saem; destinatário removido não continua recebendo dados.

**Rollout/rollback:** Notificações em modo teste para operador e eventos de maior valor; mitigação automática, se futura, exige autorização e rollback próprios.

**Rastreabilidade:** CP-29, CP-32, CP-38, CP-39.

---
#### F18 — Aliases declarativos e pool/auto fundamentado em dados

**Objetivo:** Expor intenções estáveis ao cliente sem heurísticas opacas ou seleção de recursos proibidos.

**Reaproveitar:** ModelAliases, pool/auto, perfis, catalog, routing hints e fallback graph atuais.

**Dependências e gate:** F9, F11, F13 e F15.

**F18.A — Aliases compostos.** Candidatos, constraints, fallback e strategy versionados; alias é resolvido dentro do conjunto permitido; não cadastrar segunda máquina de seleção.

**F18.B — Candidatos dinâmicos.** Usar catálogo disponível/autorizado e discovery; evitar depender exclusivamente da lista hardcoded de modelos.

**F18.C — Sinais honestos.** Substituir priors fixos gradualmente por medições com amostra/TTL; separar performance estimada, adequação de capabilities e qualidade de resposta não medida.

**F18.D — Hints e explicação.** Hints aceitos/ignorados com motivo; identidade do modelo/provider realmente usado na resposta; decisão e perda de contexto visíveis ao operador e ao consumidor permitido.

**Aceite:** Alias não amplia acesso; modelo descontinuado sai com diagnóstico; cold start explícito; benchmark/golden mostra decisão estável; qualidade não é inferida só de nome/preço do modelo.

**Rollout/rollback:** Configuração opt-in por alias/perfil e decision-shadow comparativo. Não renomear modelos existentes de forma incompatível.

**Rastreabilidade:** CP-35, CP-36, CP-37 e preparação de CP-56.

---

### BLOCO E — Operação e recuperação

#### F19 — Shutdown, backup verificável, durabilidade e poolctl

**Objetivo:** Atualizar e recuperar a instalação sem perder integridade de permissões, segredos ou usage.

**Reaproveitar:** beginDrain/shutdown existentes, stores, flags de backup/restore descritas no roadmap e rotação Passport.

**Dependências e gate:** F2, F3, F4 e API estável de F7; metadados de F6 incluídos quando presentes.

**F19.A — Desligamento.** Readiness acompanha drain; bloquear novas admissões, aguardar requests/WS sob deadline, parar pollers/shadow, flush de filas e fechamento ordenado das stores.

**F19.B — Escrita e backup.** Durabilidade de temp/rename com Sync adequado à plataforma; inventário completo de arquivos/config/metadata/keys; backup cifrado com chave de recuperação separada e retenção.

**F19.C — Restore drill.** Restaurar em diretório/processo isolado; validar schema, grants, nonces, sessões/revogações, accounting e disponibilidade de segredos; gerar PASS/FAIL com checksum e manifesto.

**F19.D — CLI.** poolctl sobre API: status, contas, grants, policy effective, explain, incidentes e backup verify; tokens curtos/escopados e saída JSON; não abrir Bolt diretamente.

**Aceite:** Shutdown não deixa novos requests admitidos após drain; restore funcional sem ressuscitar acesso revogado; falhas intermediárias detectadas; CLI e UI usam mesmas permissões; RPO/RTO medidos em drill, não inventados.

**Rollout/rollback:** Primeiro restore manual documentado, depois agendamento/off-host. Não anunciar backup saudável apenas por existir um arquivo.

**Rastreabilidade:** CP-20, CP-41, CP-48, CP-49; durabilidade de CP-02.

---

### BLOCO F — Expansão condicionada

#### F20 — Providers modulares, organizações, times e SSO

**Objetivo:** Expandir integração e organização sem enfraquecer o modelo de identidade/conta já consolidado.

**Reaproveitar:** ProviderRegistry e Passport existentes; não introduzir uma implementação paralela de autenticação ou plugin runtime arbitrário.

**Dependências e gate:** F4, F7 e F9; contribuição/grants estáveis antes de adicionar escopos organizacionais.

**F20.A — Adapters internos.** Formalizar contratos opcionais de discovery/health/capabilities e extrair providers por paridade; exemplos de provider configurável, sem carregar código remoto.

**F20.B — Organização/time.** Escopo explícito acima de principal e recursos; migração para organização default; policies e budgets agregados; separar time de propriedade de conta pessoal.

**F20.C — OIDC.** Issuer/audience/subject estáveis, linking controlado, sem promoção por email isolado; suspensão sincronizada e logout/revogação; manter break-glass testado.

**F20.D — Delegação organizacional.** Admin de time não vira administrador global; grants e auditoria limitados por escopo; avaliar SCIM apenas com demanda real.

**Aceite:** Duas organizações não compartilham catálogo/contexto/contas sem regra; SSO não toma conta preexistente por coincidência de email; remoção do IdP bloqueia acesso conforme política; adapters passam contratos.

**Rollout/rollback:** Implantação single-organization compatível primeiro. Edição organizacional só quando houver usuários reais que precisem dessa hierarquia.

**Rastreabilidade:** CP-50, CP-54, CP-55.

---
#### F21 — HA e coordenação entre instâncias

**Objetivo:** Atender um requisito demonstrado de disponibilidade/capacidade, não apenas colocar dois processos atrás de um balanceador.

**Reaproveitar:** Contratos de estado, grants, usage, refresh e recovery já estabilizados; federação como integração distinta.

**Dependências e gate:** F9, F12, F19 e escopos de F20. Requisito de disponibilidade e carga precisa estar medido.

**F21.A — Modelo de consistência.** Decidir quais operações exigem linearização: revogação, budgets, refresh, nonces e ownership; escolher store/coordenação por requisitos, não por lista de tecnologias.

**F21.B — Leases e líderes.** Fencing para refresh/jobs, consumo único de nonce, reserva distribuída de quota e invalidação de grants; evitar execução duplicada após partição.

**F21.C — Routing state.** Pins, sessões e histórico com scope; policy revision entre nós; downgrade seguro quando o control store está indisponível.

**F21.D — Operação.** Deploy rolling, partição de rede, perda do líder, restore e observabilidade por nó; documentar o que falha fechado e o que pode operar degradado.

**Aceite:** Revogação é respeitada entre nós; duas instâncias não excedem orçamento por corrida; ausência de split-brain de refresh/nonce; chaos test comprova comportamento sob partição.

**Rollout/rollback:** Não compartilhar o mesmo arquivo Bolt entre processos. Piloto com controle externo e rollback explícito para single-node.

**Rastreabilidade:** CP-53.

---
#### F22 — Routing adaptativo, semântica, escalonamento e linguagem de políticas

**Objetivo:** Adicionar inteligência somente onde dados e feedback provem ganho acima do custo e da complexidade.

**Reaproveitar:** Evaluator puro, aliases, IR e telemetria de F11/F13/F15/F18.

**Dependências e gate:** F13, F15 e F18; metas de qualidade/latência/custo definidas por workload.

**F22.A — Adaptive routing.** Avaliação offline/decision-shadow com dados confiáveis; baseline heurística preservada; limites de exploração e de custo; sem autoaprendizado a partir de métrica incorreta.

**F22.B — Semantic routing.** Opt-in para análise de conteúdo; privacidade, custo e fallback de classificador; comparar com hints explícitos mais baratos antes de torná-lo padrão.

**F22.C — Escalonamento.** Receber resultado autenticado do cliente/agente, como teste passou/falhou; budget e máximo de transições; não usar autoconfiança textual do modelo como prova de sucesso.

**F22.D — CEL e protocolo.** Adotar CEL só se políticas tipadas se tornarem insuficientes, com limites e versionamento. Protocolo público próprio somente se surgir consumidor concreto que precise dele; IR interno permanece prioridade.

**Aceite:** Ganho demonstrado por workload e não só na média; nenhum aumento de acesso/egress por inteligência; custo adicional contabilizado; rollback para heurística; feedback malicioso não captura o router.

**Rollout/rollback:** Experimentos opt-in, pequenos e mensurados. Não é gate para o produto inicial de capacidade compartilhada confiável.

**Rastreabilidade:** CP-56, CP-57, CP-58, CP-59 e CP-60 externo.

---
#### F23 — Roteamento por modelo no transporte WebSocket *(adicionado em 30/09/2026, pós-auditoria)*

**Objetivo:** Fechar o P1-06 — nenhum turno solicitado para modelo de provider não-OpenAI pode trafegar pelo upstream Codex, em nenhum transporte.

**Reaproveitar:** `inspectClient`/`codexRelayState` (cyber_swap_ws.go), `modelRequiresHTTPProviderRoute`, `applyStreamedModelRoute`, pipeline HTTP de tradução por provider e testes de `proxy_websocket_cyber_swap_test.go`. Não criar lista manual de modelos: derivar do registry/provider.

**Dependências e gate:** Independente das demais fases; (b) e (c) abaixo podem ser feitos em commits separados.

**F23.A — Contenção (curto prazo).** Incluir antigravity (e qualquer provider com catálogo próprio) em `modelRequiresHTTPProviderRoute`; recusa in-band com mensagem "use HTTP POST /responses"; atualizar configs geradas em frontend.go para não anunciar `supports_websockets` enquanto o WS não roteia por modelo. Aceite do cliente enquanto isso: `supports_websockets = false` no TOML.

**F23.B — Contrato de rejeição.** Definir frame de erro estável para "modelo sem rota WS" e teste provando que zero frames chegam ao upstream Codex com fake upstream; cobrir também prewarm (`generate=false`) e turnos com alias/effort cap aplicados em `inspectClient`.

**F23.C — Roteamento efetivo (se houver demanda).** Executar o turno WS via pipeline HTTP do provider correto e traduzir a resposta de volta para frames WS (SSE→WS), com contabilização, circuit breaker e limites idênticos ao caminho HTTP. Só entrar com medição de uso real de WS; HTTP POST cobre o caso hoje.

**Aceite:** Nenhum byte de prompt de modelo não-OpenAI no upstream Codex; recusa é acionável para o cliente; comportamento idêntico em HTTP e WS para catálogo e erros; `go test -race` verde.

**Rollout/rollback:** F23.A/B são contenção sem mudança de contrato de sucesso; rollback volta a rejeição antiga apenas se outro cliente depender do repasse (não esperado). F23.C é opt-in por provider.

**Rastreabilidade:** P1-06.

---

## 9. Dependências e ordem de execução

### 9.1 A numeração não obriga uma fila cega

As fases agrupam entregas; os blocos podem avançar em paralelo quando suas dependências estão satisfeitas. **Não esperar F19 para testar a primeira recuperação de credenciais** e não esperar F16 para tornar o cadastro utilizável. Os blocos mínimos de operação/UI acompanham a feature que estão liberando.

| Marco de publicação | Blocos mínimos | Critério de passagem |
|---|---|---|
| **M0 — Base multiusuário segura** | F1, F2.A–D e regressões essenciais de F3 | P0 fechado, configurações inválidas rejeitadas, alertas de dependência triados, shadow real controlado |
| **M1 — Contribuição privada** | F4, F5, contrato de F7.A, F9.B, UI de F16.A/C, recuperação mínima de F19.B/C | Conta nova privada, vault ativo, limite agregado, retirada testada e recuperação demonstrada |
| **M2 — Compartilhamento por grants** | F6, composição de F9, revogação/controles aplicáveis de F8, inspector de F16.B | Terceiro só usa com grant; revogação vale em alias/fallback/pin/WS; dono conhece delegação |
| **M3 — Diagnóstico confiável** | F11–F15 e telas correspondentes | Decisão explicável, métricas semanticamente corretas, perdas de handoff visíveis, simulação sem envio |
| **M4 — Operação sustentável** | F10 quando necessário, F17–F19 | Saturação previsível, alertas úteis, aliases estáveis, shutdown/backup/restore automatizáveis |
| **M5 — Expansão** | F20–F22 conforme demanda | Requisito real de organização/HA/inteligência e evidência de benefício |

**Regra para bloqueios:** implementação de UI pode avançar contra contrato fake versionado, mas publicação de um botão que concede acesso real exige backend e regressões reais. Uma tela demonstrável não fecha uma fase de autorização.

### 9.2 Dependências críticas

```text
F1 (isolamento) ───────────────> F12 (pins/contexto)
        │                              │
F2 + F3 ──> F4 (autorização) ──> F5 / F6 / F7 / F9
                │                         │
                └──> F8 ──> F10           └──> F11 (decisão)
                                                  │
                                     F12 + F11 ──> F13 (handoff)
F3 + F4 ──> F15 (dados) ──> F17 (incidentes)
F11 + F13 + F15 ──> F18 (aliases/auto) ──> F22 (inteligência)

F19.B/C (recuperação mínima) acompanha M1; não fica adiado até o fim.
F20 / F21 só entram com escopos e consistência já definidos.
```

### 9.3 Próxima sequência concreta de commits

A ordem recomendada para começar é: teste de isolamento de handoff; correção da chave interna e suas integrações; parser de rede com erro; resolução única de configuração; atualização do harness browser; resultado de triagem de dependências; tipo/authorizer de conta; migração explícita de metadata; cadastro privado; limite agregado e provas de recuperação; grants e revogação.

Cada item deve seguir o formato de commit do repositório e conter apenas a mudança correspondente. Não juntar segurança de contexto, redesign visual e refatoração de todos os providers num único commit.

---

## 10. Critérios transversais de conclusão

### 10.1 Evidência mínima por bloco

Um bloco só pode ser marcado como validado quando houver implementação, teste que prova o requisito, comando/ambiente/resultado registrado, efeitos de migração documentados e rollback avaliado. Para segurança, deve haver teste negativo e pelo menos um controle positivo, evitando “correções” que simplesmente bloqueiam todo uso.

O changelog do bloco deve registrar o commit anterior, o commit da mudança, o comportamento preservado, o comportamento alterado e os limites de validação. Percentuais sem esse material devem ser removidos.

### 10.2 Gates por superfície

| Superfície alterada | Provas obrigatórias |
|---|---|
| Autenticação/ownership/grants | Matriz de atores, acesso cruzado, revogação, CSRF quando aplicável, restart e erro de storage |
| Roteamento | Candidatos autorizados antes do score, revalidação no dispatch, paridade de snapshot, fairness quando há fila |
| Handoff/IR | Roles/proveniência, tools, conteúdo multimodal, ID nativo, perdas, colisão entre usuários, ida e volta entre providers |
| SSE/WS/retry | Cancelamento, primeira saída visível, truncamento, lease liberada, usage sem duplicação lógica |
| Vault/config | Chave errada/ausente, ciphertext inválido, permissão, escrita interrompida, rotação, migração e restore |
| UI/UX | Browser real, resposta tardia, estado vazio/erro/stale, teclado/foco, viewport móvel, ações destrutivas |
| Analytics | Idempotência, erro de persistência visível, qualidade do dado, reconciliação e retenção |
| Backup/HA | Restore operacional, revogação preservada, consistência de grants/quotas e comportamento sob falhas |

### 10.3 Comandos-base para as próximas implementações

Os comandos abaixo são um **roteiro de execução futura**, não resultados novos desta auditoria:

```bash
# Repositório: usar a versão Go suportada pelo projeto/CI.
go test ./...
go test -race -count=1 ./...
go vet ./...

# Frontend.
npm --prefix web ci
npm --prefix web test
npm --prefix web run build

# Supply chain: registrar JSON e triar, não aplicar force sem análise.
npm --prefix web audit --json

# Harness: serve para testar o próprio benchmark.
go run ./bench/runner -mock
```

Targets de fuzz, browser e benchmarks específicos devem ser adicionados à documentação/CI com os nomes reais implementados. Não fingir que um script de browser existe antes de criá-lo. Para Windows, separar invariantes portáveis de verificações POSIX, sem pular silenciosamente o requisito de segurança correspondente.

### 10.4 Invariantes de produto

**Autorização:** negar antes de enviar. Nenhum alias, fallback, pin, probe, provider switch ou requisição grande amplia o conjunto autorizado. Catálogo, métricas e diagnóstico usam a mesma fronteira.

**Contexto:** identidade de conversa sempre escopada. O dono da conta de capacidade não ganha propriedade sobre os prompts de seus consumidores. Estado nativo e conteúdo semântico não são intercambiáveis.

**Consumo:** separar request lógico, tentativa e trabalho sombra. Quando o upstream não oferece idempotência, o produto não promete exactly-once. Retry antes do primeiro token reduz certos riscos, mas não prova que o upstream não tenha processado a tentativa anterior.

**Dados:** unknown não é zero; stale não é atual; estimativa não é medição. API-equivalente não é gasto real. A UI deve mostrar essas diferenças.

**Operação:** falha de configuração explícita não desabilita controles. Um backup só é promovido a verificado após restore. Um estado administrativo retirado não é reativado por um refresh de credencial bem-sucedido.

### 10.5 Modelo de registro de conclusão

```text
Bloco: F6.C
Estado: planejado | implementado | validado | bloqueado
Base SHA:
Commit da implementação:
Requisito demonstrado:
Teste negativo (base):
Teste positivo (candidato):
Comandos e ambiente:
Migração / rollback:
Compatibilidade preservada:
Limitações ainda conhecidas:
Evidências:
```

Esse registro substitui “fase 100% concluída” sem referência. Validado em CI com fake upstream e validado com provider real são estados diferentes e devem continuar distinguíveis.

---

## 11. Novas ideias e decisões de escopo

### 11.1 Ideias que valem entrar após os bloqueadores

**Perfil de privacidade e destino por workload.** Um alias pode exigir conjunto aprovado de providers, região quando verificável, proibir traffic-shadow e limitar perdas de handoff. A política informa o motivo de não fazer fallback. Isso é uma extensão de grants e capability matching, não inspeção compulsória de todo prompt.

**Relatório de prontidão da instalação.** Um comando/painel informa: vault ativo, configuração validada, último restore, gaps de accounting, CI/release em uso, limitação de origem e qualidade dos dados. Não apresentar “sistema seguro” como um selo absoluto; mostrar verificações específicas com evidências e data.

**Credenciais efêmeras e just-in-time para automação.** Service accounts recebem grants com escopo/validade curtos, sem usar o token global de administração em cada integração. Operações sensíveis podem exigir step-up/reautenticação, reutilizando Passport/passkeys.

**Cápsula de diagnóstico exportável.** Exportar versão, reason codes, IDs correlacionáveis e configuração redigida. Nada de copiar o prompt, token OAuth ou headers arbitrários para um “copiar diagnóstico”. É uma função de suporte de alto valor.

**Explicação de perda de contexto.** Ao mudar provider, exibir “histórico completo”, “anexos não transportados” ou “nova sessão nativa”, em formato consumível pela API e UI. Dar ao usuário a opção de proibir a degradação em vez de esconder a transição.

**Reserva para o dono da contribuição.** Uma conta cedida pode ter reserva de concorrência/capacidade para seu dono. Só usar percentuais quando a unidade e a medição forem confiáveis; em quotas opacas, fornecer controles conservadores e claramente estimados.

**Configuração como revisão.** O operador compara alterações de policy/routing, simula impacto, aplica uma versão e consegue reverter. O histórico não guarda secrets, apenas referências. Isso reduz erros operacionais mais diretamente que acrescentar estratégias “inteligentes”.

### 11.2 Ideias a adiar ou rejeitar no estado atual

| Ideia | Decisão e razão |
|---|---|
| Refazer todo o backend em microserviços | Rejeitar agora; aumenta coordenação sem resolver os contratos que já faltam. |
| Redis/Postgres/NATS simultaneamente “para escalar” | Adiar; escolher pelo problema de consistência/HA demonstrado. |
| Expor token upstream para facilitar integração | Rejeitar no produto normal; integração usa credencial do Pool com grant. |
| Fallback sempre automático para qualquer provider disponível | Rejeitar; capacidade técnica não implica permissão de destino nem preservação de contexto. |
| Classificar toda conversa com outro LLM por padrão | Adiar; custo, latência e exposição de conteúdo precisam de benefício medido e opt-in. |
| Autoescalonar porque o modelo declarou baixa confiança | Não usar como critério suficiente; precisa de feedback/avaliação externa e budget. |
| Marketplace de contas/contribuições remuneradas | Fora do escopo atual; medição, governança e condições de uso ainda precisam estar definidas. |
| Universal LLM Protocol público como pré-requisito | Rejeitar como pré-requisito; IR/adapters internos bastam para entregar valor inicialmente. |
| Novo gráfico para cada métrica | Rejeitar como estratégia de UX; priorizar tarefas e decisões acionáveis. |
| Remover mecanismos existentes antes de provar paridade | Rejeitar; migração de auth, stores e protocolos exige reversibilidade e testes. |

### 11.3 Critério para uma sugestão continuar no roadmap

Uma sugestão deve ter um problema observável, um usuário beneficiado, uma integração identificada, um custo/risco conhecido e um teste de aceite. “Outros produtos possuem” é contexto, não justificativa suficiente.

A contribuição privada de membros satisfaz esse critério quando vem acompanhada de isolamento, grants e retirada de acesso. Um botão liberado a membros sem esses contratos não satisfaz.

---

## 12. Referências e rastreabilidade

O documento original fornecido pelo usuário foi a base de organização CP-00…CP-60, princípios e critérios de conclusão. As afirmações sobre o estado novo usam fontes do SHA fixado. Referências marcadas como ponto de extensão do roadmap, como account state e smart router, não significam que todos os seus testes foram reexecutados nesta investigação.

As URLs de código são fixadas no commit auditado. A documentação dos produtos externos foi consultada durante a investigação e pode evoluir. No pacote de evidências, o documento original e o manifesto permitem reproduzir a comparação de versões.

| Ref. | Fonte | Escopo de uso |
|---|---|---|
| [E01] | Snapshot do repositório | Commit auditado; não equivale a todos os deploys do operador. |
| [E02] | CI configurada | Build web, Vitest, vet, race e harness de benchmark. |
| [E03] | Execução de CI consultada | Job 109093427357; resultados e avisos conferidos nos logs. |
| [E04] | Entrada HTTP e roteamento | proxyRequest; autenticação; extração de conversation_id; admission; shadow; pool/auto; handoff. |
| [E05] | Handoff: chave, merge e persistência em memória | conversationHandoffStore.Prepare; lookup por conversationID; merge e render. |
| [E06] | Handoff: singleton e wrapper | getContextHandoff e prepareProviderContextHandoff. |
| [E07] | Controle de rede | parseIPNetList, configure, restricted e permitted. Cópia local com hash Git conferido. |
| [E08] | Configuração e defaults | buildConfig continua após falha de leitura do TOML; defaults e variáveis de ambiente. |
| [E09] | Schema de configuração | ConfigFile, loadConfigFile, getters, hot reload; não há config_version nesse schema. |
| [E10] | Autorização e rotas | checkAdminAuth, checkProviderContributionAuth, CSRF, headers e middleware IP. |
| [E11] | Regressões de contribuição e legacy | Código de amigo aposentado; autenticação e vínculo OAuth ao ator. |
| [E12] | Passport e migrações | Principals, ClientCredentials, nonces, backfill de índices, retirement do legado. |
| [E13] | Rotação da chave Passport | Rotação transacional da cifra de download tokens; diferente da chave das contas upstream. |
| [E14] | Vault upstream | PlainStore sem chave; encode centralizado; escrita temp/rename; migração e rollback. |
| [E15] | Modelo Account | Account, UsageSnapshot, RequestUsage; ausência de ownership/grants no modelo lido. |
| [E16] | Elegibilidade de contas | Predicados de disponibilidade; limites de quota; saúde/cooldown. |
| [E17] | Policy admission | ClientPolicy, contadores e reservas por clientID; CheckModel/CheckProvider. |
| [E18] | Representação intermediária | IR existente e conversão por Message legado. |
| [E19] | Compatibilidade de transições | Modos native/full-history/safe-history/summary; summary textual. |
| [E20] | Dry-run de transição | Clona o estado antes de chamar Prepare; endpoint administrativo. |
| [E21] | Orquestração pool/auto | Candidatos e priors fixos; perfis; pontuação; ausência de filtro por principal na assinatura examinada. |
| [E22] | Experimentos e shadow | maybeStartShadow envia tráfego real; context.WithoutCancel; Record e métricas estimadas. |
| [E23] | Circuit breakers | Provider, account, account+model e account+capability; half-open; threshold de limpeza. |
| [E24] | Startup, stores e shutdown | Passport obrigatório, retirement, Bolt/DuckDB/SQLite, pollers, watcher, HTTP server, beginDrain. |
| [E25] | Confiabilidade de accounting | AccountingGap, reserva de disco, recordReliably e sidecar. |
| [E26] | Contrato Provider | ProviderRegistry e interface existente; providers configuráveis. |
| [E27] | Aplicação web | Navegação por URL/role; quota/forecast; tela central e estado. |
| [E28] | Testes web de contratos e render | Confirmações por conta/ação, recovery gating, CSRF, modelos desconhecidos. |
| [E29] | Cliente da API web | setup-link, recuperação, CSRF, normalização e decode genérico. |
| [E30] | Harness browser | Mocks legacy/reveal incompatíveis com contratos atuais; dependência externa de browser. |
| [E31] | Dependências/scripts web | Scripts dev/build/test; Vitest; dependências fixadas. |
| [E32] | Dependências Go | Go declarado 1.25.0; dependências das três stores; não declara SDK OTel. |
| [E33] | Diretrizes de contribuição | Mudanças atômicas, padrões do repositório e validação antes de commits. |
| [E34] | Estado projetado | Referência do roadmap e integração observada no startup; detalhes não reexecutados isoladamente. |
| [E35] | Routing existente | Referência já citada no roadmap; estender pesos/perfis existentes, não criar seletor paralelo. |
| [E36] | Grafo de fallback | Referência do roadmap; uso do grafo confirmado no wiring de providers. |
| [E37] | Benchmark versionado | Baseline e runner referidos no roadmap; CI trata mock como teste do harness. |
| [W01] | CLIProxyAPI — repositório oficial | OAuth multiaccount, protocolos, SDK e status da telemetria integrada. |
| [W02] | LiteLLM — Virtual Keys | Governança de chaves, budgets, rotação e restrições de criação. |
| [W03] | Bifrost — Virtual Keys | Restrição por provider key, budgets e escopos de rate limits. |
| [W04] | Portkey — migração para Model Catalog | Virtual Keys deprecated; modelo atual de catálogo e governança. |
| [W05] | OpenRouter — Provider Routing | order, only/ignore, fallback e require_parameters. |
| [W06] | OWASP — Object Level Authorization | Referência conceitual para autorização por objeto, não prova do achado. |
| [W07] | W3C — WCAG 2.2 | Referência de acessibilidade para os novos gates. |
| [W08] | OWASP — SSRF Prevention | Referência preventiva para URLs, providers e webhooks; não achado confirmado. |

### Arquivos da entrega

- `codex-pool-auditoria-roadmap-2026-09-28.md`: relatório completo e reconciliação das 61 fases.
- `codex-pool-plano-execucao-2026-09-28.md`: recorte operacional com segurança, F1–F22, dependências e gates.
- `evidencias/repro_ip/`: fonte idêntica, testes e logs da reprodução.
- `evidencias/manifesto-auditoria.json`: snapshot, hash do original e limites da validação.
- `original/codex-pool-roadmap-original.md`: cópia sem alterações do documento recebido.

**Conclusão:** o projeto deve evoluir por fechamento de fronteiras de confiança e contratos, não por acumulação de features. A sequência proposta preserva o que já foi construído, trata primeiro o risco de mistura de contexto, torna contas de membros um recurso realmente governado e deixa inteligência/escala dependentes de evidência.


[E01]: https://github.com/GustavoMartins123/codex-pool/commit/8fb3416c0256da48c519525cf929d93077a65ab9 "Snapshot do repositório"
[E02]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/.github/workflows/ci.yml "CI configurada"
[E03]: https://github.com/GustavoMartins123/codex-pool/actions/runs/36471149835 "Execução de CI consultada"
[E04]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/main.go#L2110-L2745 "Entrada HTTP e roteamento"
[E05]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/context_handoff.go#L1260-L1455 "Handoff: chave, merge e persistência em memória"
[E06]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/context_handoff.go#L1700-L1890 "Handoff: singleton e wrapper"
[E07]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/ip_access.go "Controle de rede"
[E08]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/main.go#L138-L235 "Configuração e defaults"
[E09]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/config.go "Schema de configuração"
[E10]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/router.go "Autorização e rotas"
[E11]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/friend_account_routes_test.go "Regressões de contribuição e legacy"
[E12]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/passport.go "Passport e migrações"
[E13]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/passport_key_rotation.go "Rotação da chave Passport"
[E14]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/credential_vault.go "Vault upstream"
[E15]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/pool.go#L1-L225 "Modelo Account"
[E16]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/account_availability.go "Elegibilidade de contas"
[E17]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/passport_policy.go "Policy admission"
[E18]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/conversation_ir.go "Representação intermediária"
[E19]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/provider_transition.go "Compatibilidade de transições"
[E20]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/provider_transition_debug.go "Dry-run de transição"
[E21]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/pool_auto.go "Orquestração pool/auto"
[E22]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/experiments.go "Experimentos e shadow"
[E23]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/circuit_breaker.go "Circuit breakers"
[E24]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/main.go#L440-L850 "Startup, stores e shutdown"
[E25]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/analytics_reliability.go "Confiabilidade de accounting"
[E26]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/provider.go "Contrato Provider"
[E27]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/web/src/App.tsx "Aplicação web"
[E28]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/web/src/App.test.ts "Testes web de contratos e render"
[E29]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/web/src/api.ts "Cliente da API web"
[E30]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/web/account-flows.browser.mjs "Harness browser"
[E31]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/web/package.json "Dependências/scripts web"
[E32]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/go.mod "Dependências Go"
[E33]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/AGENTS.md "Diretrizes de contribuição"
[E34]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/account_state.go "Estado projetado"
[E35]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/smart_router.go "Routing existente"
[E36]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/fallback_graph.go "Grafo de fallback"
[E37]: https://github.com/GustavoMartins123/codex-pool/blob/8fb3416c0256da48c519525cf929d93077a65ab9/bench/baselines/baseline.json "Benchmark versionado"
[W01]: https://github.com/router-for-me/CLIProxyAPI "CLIProxyAPI — repositório oficial"
[W02]: https://docs.litellm.ai/docs/proxy/virtual_keys "LiteLLM — Virtual Keys"
[W03]: https://docs.getbifrost.ai/features/governance/virtual-keys "Bifrost — Virtual Keys"
[W04]: https://portkey.ai/docs/product/ai-gateway/virtual-keys "Portkey — migração para Model Catalog"
[W05]: https://openrouter.ai/docs/guides/routing/provider-selection "OpenRouter — Provider Routing"
[W06]: https://api-security.owasp.org/editions/2023/en/0xa1-broken-object-level-authorization/ "OWASP — Object Level Authorization"
[W07]: https://www.w3.org/TR/WCAG22/ "W3C — WCAG 2.2"
[W08]: https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html "OWASP — SSRF Prevention"
