# Retenção e arquivamento de conversas

Pesquisa e benchmarks locais de 24/10/2026. Escopo: estudar a persistência de
histórico introduzida em `5e6f4c4` e comparar protótipos isolados de armazenamento.
O estudo não habilita arquivamento automático nem altera conversas existentes.

## Recomendação e invariantes

Preservar o transcript completo até uma exclusão explícita do titular. Depois de
168 horas sem interação, mover a conversa elegível para **Arquivadas** e comprimir
os eventos sem perdas. Um resumo gerado por modelo pode servir ao contexto do
agente, mas nunca substitui o histórico original. A idade da conversa não autoriza
expirar o conteúdo nem sua única versão íntegra. Backups podem ter uma política
operacional de rotação, desde que o acervo ainda retido continue integralmente
recuperável e com cópia independente; essa rotação não é retenção de conversa.

Separar três conceitos:

1. **Histórico:** todos os eventos, mensagens, pensamentos recebidos, ferramentas,
   aprovações e metadados. Recuperação integral e verificável.
2. **Apresentação:** conversa ativa ou arquivada, prévia e paginação. Arquivar não
   apaga nem muda o dono.
3. **Execução:** processo do runner, estado nativo da CLI e workspace. Restaurar o
   histórico não prova que o processo ou os arquivos de trabalho foram preservados.

O dono permanente é `principal_id`; a credencial do app e o runner são vínculos
operacionais. Todas as consultas, prévias, downloads, restaurações e exclusões
devem verificar esse dono. Os domínios configurados no app continuam isolados.

## Comportamento proposto

Adicionar **Ativas** e **Arquivadas**, ambas com paginação por cursor e índice por
conta. Cada item mostra título, agente/modelo, última interação, tamanho e as três
últimas mensagens lógicas de usuário/assistente, com excertos de até 512 caracteres
por mensagem. Deltas de SSE precisam ser agrupados antes de gerar a prévia;
pensamentos e ferramentas continuam disponíveis dentro da conversa.

Ao abrir uma conversa, carregar primeiro as mensagens mais recentes. A rolagem
para cima busca páginas anteriores. Consultar a prévia ou navegar pelo arquivo
não desarquiva a conversa. **Retomar** é uma ação explícita; apresenta o estado da
restauração e só permite enviar um turno depois de validar histórico, runner,
workspace e estado nativo do agente. Uma restauração que falha mantém o arquivo
intacto e apresenta o erro, sem criar silenciosamente outra conversa.

Introduzir `last_interaction_at` e um índice de elegibilidade separado de
`updated_at`, `lease_expires_at` e `last_seen`. A atividade relevante inclui envio
de mensagem, resposta/conclusão do turno e decisão de aprovação; heartbeats,
reconexões, leitura de SSE e consultas de lista não renovam o prazo. O limite é
168 horas corridas em UTC, incluindo conversas que o usuário não abriu novamente.

Somente conversas sem turno, aprovação ou operação de workspace em andamento
podem ser arquivadas. Um runner desconectado não prova que seu processo parou;
uma conversa `orphaned` exige reconciliação. Arquivar uma sessão ainda vinculada a
um processo ocioso exige um protocolo explícito de suspensão e checkpoint antes
da liberação dos recursos. Não executar o encerramento atual que limpa um
workspace efêmero para simular suspensão.

## Armazenamento proposto

Manter no Bolt o índice por conta, metadados, três prévias e o manifesto da conversa.
O manifesto versionado inclui dono, conversa, geração, cursor final, intervalos de
eventos, offsets, tamanhos e SHA-256 dos blocos comprimidos e do conteúdo original.
Usar arquivos com blocos independentes e limites entre eventos. A escolha do codec
e do tamanho dos blocos deve seguir as medições abaixo.

O arquivo local fica em volume persistente. Para atender à retenção diante da
perda do volume ou servidor, manter também uma cópia externa verificável e um
backup consistente dos manifestos/índices. Compactar no mesmo disco não constitui
backup. Versionamento auxilia contra sobrescrita acidental, mas não substitui um
plano de restauração testado. Compressão precede criptografia; compressão e hashes
não fornecem confidencialidade.
As chaves de dados precisam ser versionadas e recuperáveis, independentes do JWT,
da credencial do runner e da senha da conta. Trocar a senha ou expirar um token
não pode inutilizar o acervo. O backup deve contemplar o material necessário à
descriptografia, com controle de acesso próprio.

Publicação segura:

1. Capturar dono, geração e cursor do snapshot; não manter o mutex global durante
   compressão ou transferência do arquivo.
2. Gravar arquivo temporário, sincronizar e verificar todos os blocos e intervalos.
   Confirmar também a cópia externa quando ela integrar a garantia de durabilidade.
3. Em uma transação curta, conferir novamente geração, cursor, elegibilidade e
   ausência de exclusão; publicar o manifesto e mudar o armazenamento canônico.
4. Somente após esse commit, liberar os registros quentes já preservados no arquivo.
   Antes do commit, uma falha conserva a origem. Esse rollback transacional não é
   um caminho alternativo de leitura.
5. Após reinício, reconciliar arquivos temporários e operações incompletas de forma
   idempotente. Arquivo ausente, hash inválido ou lacuna produz erro explícito.

A leitura arquivada deve acessar diretamente os blocos necessários. Retomar uma
conversa não precisa reinserir todos os eventos antigos no Bolt: prefixo arquivado
imutável e novos eventos quentes podem compor um único histórico com cursores
contíguos. Essa composição exige um contrato explícito; não é recuperação por
fallback.

Excluir registros do Bolt torna páginas reutilizáveis, mas não implica reduzir o
arquivo no sistema operacional. A compactação física do banco é uma manutenção
separada, com espaço temporário, backup e substituição segura do arquivo. Não usar
uma simples troca de arquivos com o banco aberto, especialmente no Windows.

## Contrato e migração propostos

Separar `conversation_id` de `session_id`: a conversa pertence à conta e pode
atravessar várias execuções. A classificação ativa/arquivada não deve ocupar os
estados `running`, `orphaned` ou `closed` da máquina de execução.

Os registros precisam guardar `principal_id`, `last_interaction_at`,
`archive_state`, `archive_generation`, cursor final, três prévias e disponibilidade
do checkpoint do agente/workspace. O índice de lista usa conta, categoria,
atividade e ID como desempate. Um índice por prazo agenda os candidatos a arquivo
sem varrer a coleção inteira a cada segundo.

O contrato deve oferecer listagem paginada por categoria, mensagens recentes e
anteriores por cursor, estado de restauração e ações explícitas de retomar e
apagar. O `DELETE /api/sessions/:id` atual encerra execução: não deve ser
apresentado como exclusão permanente do histórico.

Na migração, criar uma conversa por sessão já gravada, preservando dono, eventos
e cursores, com contagem e hashes antes de mudar a leitura canônica. Sessões sem
histórico registrado continuam marcadas como indisponíveis; não se inventam
mensagens ou checkpoints para preencher esse período. Isso também se aplica às
threads efêmeras que já perderam o processo original.

## Limites já encontrados no código

- `sessionBroker.list` varre todas as contas, ordena as sessões do titular e retorna
  somente 200. Isso limita a descoberta do histórico, mesmo sem apagar os dados.
- Todas as sessões permanecem no mapa global em memória. O custo cresce com a
  retenção total, incluindo contas que não estão ativas.
- `history` usa o mutex global durante a leitura e a decodificação da página.
- O app baixa todas as páginas antes de mostrar uma conversa. Paginar a API não
  basta para reduzir o tempo de abertura se o cliente drena todas as páginas.
- Cada delta confirma uma transação durável; desabilitar `fsync` para aumentar
  throughput violaria a garantia de não perder eventos já confirmados.
- Codex usa `thread/start` com `ephemeral: true`; Claude usa
  `--no-session-persistence`. O `CODEX_HOME`/diretório de controle é temporário.
  O transcript do pool não é um checkpoint completo desses agentes.

A documentação oficial do Codex prevê `thread/resume`, `thread/archive`,
`thread/unarchive` e paginação de turns. É necessário persistir o identificador e o
estado nativo correspondente antes de oferecer retomada após reiniciar o processo.
A documentação do Claude também distingue persistência local e `--resume`.
Os comandos disponíveis devem ser conferidos na versão instalada no runner.

## Metodologia

Os testes usam exclusivamente dados sintéticos determinísticos. Não há chamadas
de modelo nem leitura de conversas ou credenciais de usuários. O estudo não mede
o emulador, a rede móvel, S3 real ou a restauração do processo das CLIs.

- Corpora: 1.000, 10.000 e 100.000 eventos mistos; 10.000 eventos de alta entropia.
  Os dados mistos combinam vocabulário variado, entradas do usuário, raciocínio,
  ferramentas e respostas, com conteúdo pseudoaleatório em parte dos eventos.
  Não são uma estimativa estatística do uso real.
- Codec: gzip rápido/padrão e Zstandard rápido/padrão, em blocos de 256 KiB e 1 MiB.
  Construção inclui serialização, prévias e hashes; restauração valida hashes,
  sequência e conteúdo. SHA-256 é conferido contra um manifesto confiável.
- Caminho atual: leitura real de `session_events` e serialização de páginas de 256
  eventos; leitura completa inclui decodificação do JSON de cada página, sem rede.
- Listagem: 1, 10 e 100 contas, com 1.000 sessões por conta. Comparação entre a
  implementação atual, que devolve no máximo 200 sessões, e um protótipo de índice
  no Bolt que busca 50. As quantidades retornadas diferem intencionalmente.
- Concorrência: 1, 10 e 50 gravadores com 64 operações cada, com zero ou quatro
  leitores. Mesmos métodos de persistência do broker, fsync padrão, sem o transporte
  WS, autenticação ou fila de rede. Trata-se de carga saturada, sem pausas humanas.
- Prévia: três mensagens lógicas armazenadas separadamente, sem descompactar o chat.
- Disco: publicação de arquivo com `Sync`, releitura/verificação, exclusão dos
  eventos apenas da base sintética e compactação offline em outro banco.
- Medianas e percentis são de operações locais com caches aquecidos. Construção e
  restauração integral têm sete amostras; percentis altos com poucas amostras são
  apenas descritivos. Nenhuma medição representa capacidade garantida de produção.
- Operações de prévia e índice são medidas em lotes para resolver intervalos
  menores que a resolução do relógio. Nesses casos, os percentis representam a
  média por operação de cada lote, e não latência individual de requisição.
- Alocações são bytes acumulados por operação, não pico de RAM. O último bloco
  pode estar parcialmente ocupado; seu tamanho real aparece no JSON. O banco
  sintético inclui crescimento e páginas livres, portanto seu tamanho físico
  não representa apenas o espaço útil dos eventos.

## Resultados medidos

Dados completos e sanitizados em
[`bench/reports/session_history_capacity_20261024.json`](../bench/reports/session_history_capacity_20261024.json).
Foram executadas duas rodadas do estudo e três repetições do microbenchmark.
A primeira rodada revelou quantização do relógio nas consultas muito rápidas;
esses valores foram descartados na comparação de prévias e índices. A segunda
mede essas operações em lotes. As demais medições mostram variação local e não
justificam uma promessa de latência de produção.

### Conteúdo misto e recuperação

Medianas da segunda rodada, Zstandard rápido com blocos de 256 KiB. MiB usa
1.048.576 bytes. O tamanho comprimido inclui o manifesto e suas prévias.

| Eventos | JSONL original | Arquivo comprimido | Redução | Leitura atual completa | Recuperação do arquivo em memória | Prévia indexada |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1.000 | 0,58 MiB | 0,16 MiB | 72,5% | 11,9 ms | 6,9 ms | 10,3 µs |
| 10.000 | 5,83 MiB | 1,58 MiB | 72,9% | 116,4 ms | 63,4 ms | 10,4 µs |
| 100.000 | 58,37 MiB | 15,76 MiB | 73,0% | 1.443,7 ms | 878,4 ms | 13,7 µs |

A conversa de 100.000 eventos exigiu **391 páginas e 58,53 MiB de JSON** no
contrato atual. Entre as duas rodadas, a mediana local da leitura completa variou
de 1,16 a 1,44 segundo. A transferência desse payload a 10 Mbit/s teria um piso
teórico de aproximadamente 49 segundos, sem contar RTT, TLS ou processamento.
Isso é uma estimativa de volume, não um benchmark de rede.

A leitura completa acumulou aproximadamente 230 MiB de alocações por operação no
ensaio de 100.000 eventos. Esse número inclui serialização/decodificação de todas
as páginas e não mede o pico de memória do servidor nem a memória do app Kotlin.
O protótipo de arquivo também materializa o conteúdo para medição; a implementação
deve produzir e consumir blocos em streaming para limitar memória de trabalho.

Na amostra de alta entropia, os 10.000 eventos passaram de 5,80 MiB para cerca de
3,73 MiB: redução de **35,7%**, bem menor que no conteúdo misto. São campos JSON
com bytes pseudoaleatórios em base64, não dados criptografados ou mídia binária.
Planejar capacidade apenas com a economia de 73% seria incorreto.

### Escolha do codec e bloco

Resultados do corpus misto de 10.000 eventos, blocos de 256 KiB. O tempo de
construção abaixo é a mediana das três repetições do benchmark Go, com
serialização, prévias, compressão e hashes. O nível `-1` é o padrão do gzip Go.

| Codec | Tamanho/original | Construção | Recuperação integral no estudo |
| --- | ---: | ---: | ---: |
| gzip rápido (1) | 30,34% | 131,5 ms | 77,4 ms |
| gzip padrão (-1) | 25,50% | 234,3 ms | 74,9 ms |
| Zstandard rápido | 27,08% | 113,5 ms | 63,4 ms |
| Zstandard padrão | 26,70% | 115,5 ms | 62,9 ms |

**Escolha inicial: Zstandard rápido, blocos de 256 KiB.** No corpus de 10.000
eventos, o nível padrão economizou apenas 0,38 ponto percentual adicional do
original. Blocos de 1 MiB economizaram apenas 0,22 ponto percentual adicional no
Zstandard rápido. Blocos menores limitam a quantidade a descompactar por acesso.
O último bloco tem ocupação variável, registrada no JSON; seus tempos não são
uma comparação entre blocos de mesmo tamanho útil. A escolha deverá ser
reavaliada com métricas reais e eventual custo de requisições remotas.

O benchmark da construção Zstandard rápido acumulou cerca de 64 MiB de alocações
por operação para esse corpus de 5,83 MiB. Logo, a implementação não deve copiar
esse protótipo em memória integralmente para o caminho de produção.

### Listagem por conta

Medianas da segunda rodada. Cada conta tem 1.000 sessões.

| Contas | Total de sessões | Lista atual, limitada a 200 | Índice por conta, página de 50 |
| ---: | ---: | ---: | ---: |
| 1 | 1.000 | 0,520 ms | 0,234 ms |
| 10 | 10.000 | 0,688 ms | 0,236 ms |
| 100 | 100.000 | 3,458 ms | 0,236 ms |

O índice medido tem custo estável com o crescimento das outras contas. A comparação
também reduz o resultado de 200 para 50 itens; não é uma comparação de throughput
para respostas idênticas. A limitação funcional atual é mais urgente: **800 das
1.000 sessões do titular não aparecem na lista atual**. Paginação elimina esse
limite sem remover histórico.

### Gravação durável e concorrência

Segunda rodada; 64 gravações por worker. Leitores fazem requisições repetidas e
sem pausas, situação mais intensa que uma navegação humana comum. A duração total
inclui os leitores; eventos/s representa o workload misto inteiro.

| Gravadores | Leitores | Eventos/s | p95 da gravação | p95 da leitura |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 0 | 916 | 1,6 ms | — |
| 1 | 4 | 123 | 9,2 ms | 9,4 ms |
| 10 | 0 | 476 | 34,5 ms | — |
| 10 | 4 | 345 | 60,0 ms | 60,0 ms |
| 50 | 0 | 444 | 135,1 ms | — |
| 50 | 4 | 434 | 134,3 ms | 135,8 ms |

Houve variação entre rodadas: 50 gravadores sem leitores passaram de cerca de 706
para 444 eventos/s; com quatro leitores, de 635 para 434. Publicação do arquivo com
`fsync` também variou de 1,6 para 18,1 ms. São motivos para validar na implantação
real e observar fila/p95 antes de fixar metas.

Esses números não se traduzem diretamente em usuários cadastrados: importam os
agentes simultaneamente ativos e os deltas por segundo de cada um. Por exemplo,
20 agentes emitindo 20 deltas/s pediriam 400 commits/s no desenho atual, ordem de
grandeza já relevante neste ensaio saturado. Esse exemplo é aritmética de carga,
não uma capacidade certificada nem uma previsão do ritmo dos modelos.

Priorizar remoção de trabalho de leitura/decodificação do mutex global. Se a fila
de gravação crescer, estudar group commit com confirmação somente após o commit
durável, preservando ordem por conversa. Group commit e outros bancos **não foram
comparados** neste estudo; não há recomendação de migração baseada em velocidade
sem esse próximo ensaio.

### Disco e recuperação física

No ensaio de 10.000 eventos, o Bolt ocupava 16 MiB. Após publicar e verificar o
arquivo e remover os registros quentes sintéticos, continuou ocupando **16 MiB**.
A compactação offline gerou um banco de **64 KiB**, além do arquivo comprimido de
aproximadamente 1,57 MiB sem seu manifesto. A releitura do arquivo no filesystem,
com verificação e decodificação de todos os eventos, levou mediana de **76,0 ms**
na segunda rodada, com cache aquecido.

A compactação offline teve somente uma amostra por rodada e não incluiu o
protocolo operacional de substituição do banco. Não usar seu tempo como previsão
de manutenção de uma base real. Não foram simuladas falhas de energia, perda de
servidor, indisponibilidade de S3 ou restauração nativa das CLIs.

## Ordem recomendada para a implementação

1. Modelo permanente de conversa vinculado à conta, índice paginado e três prévias;
   carregar o chat por mensagens recentes, preservando cursores e o histórico todo.
2. Checkpoint persistente do agente e política de preservação do workspace para
   suspensão; validar retomada real sem depender do processo ainda estar vivo.
3. Arquivador de 168 horas, um worker inicial, compressão Zstandard em blocos,
   manifesto transacional e cópia independente. Ler arquivos sem reidratar a
   conversa inteira no banco. Validar falhas e restauração antes de ativar o job.
4. Medir na implantação real latência de lista/recuperação, commits/s, fila,
   backlog de arquivo, bytes por conta e integridade dos backups. O número de
   contas, isoladamente, não determina uma troca de banco.

Para o uso inicial, manter Bolt com índice e arquivo de histórico é uma evolução
menor que adicionar um banco servidor agora. Armazenamento S3 externo é uma opção
de serviço para a cópia independente. Um serviço compatível com S3 no mesmo Docker
pode organizar os objetos, mas não protege contra perda desse host. Redis não é
necessário para o desenho medido. Nenhum serviço ou mudança de produção foi
instalado pelo estudo.

## Reprodução

Na raiz do repositório, em PowerShell:

```powershell
go test . -run '^TestHistoryArchiveStudy' -count=1
$env:SESSION_HISTORY_STUDY_OUTPUT = "$PWD/tmp/session-history-study.json"
go test . -run '^TestSessionHistoryCapacityStudy$' -count=1 -timeout 15m -v
Remove-Item Env:SESSION_HISTORY_STUDY_OUTPUT
go test . -run '^$' -bench '^BenchmarkHistoryArchiveCodec$' -benchmem -benchtime=1s -count=3
```

O diretório `tmp/` precisa existir. O estudo de capacidade é opt-in; a suíte comum
executa somente os testes de integridade. O JSON do estudo não contém nome do host
nem especificações de hardware. A saída padrão do benchmark Go pode conter esses
dados e deve ser sanitizada antes de publicação.

## Critérios para implementação

- Testes de 167h59m/168h, timestamps UTC e exclusão de atividade de heartbeat.
- Isolamento entre contas e domínios em cada rota e em cada prévia.
- Paginação para mais de 200 conversas e carregamento recente no app.
- Restauração bit a bit dos eventos, incluindo pensamento e ferramentas.
- Falha explícita em arquivo ausente/corrompido, dono incorreto e lacunas.
- Escrita ou exclusão concorrente durante o arquivamento aborta a publicação
  quando a geração muda; restauração e arquivamento são idempotentes.
- Reinício em cada etapa de publicação; nenhum dado confirmado é descartado.
- Teste de restauração a partir da cópia externa e do backup do índice.
- Checkpoint da CLI e preservação do workspace para retomada de execução real.
- Exclusão explícita invalida cursores/gerações e alcança blocos, índices e prévias;
  o tratamento das cópias de backup deve ser definido e comunicado ao usuário.

## Fontes oficiais

- [bbolt 1.3.8: transações, cursores e backup](https://github.com/etcd-io/bbolt/blob/v1.3.8/README.md).
- [Zstandard: características e dados pequenos](https://github.com/facebook/zstd).
- [SQLite WAL: concorrência e durabilidade](https://www.sqlite.org/wal.html).
- [S3 Lifecycle: transição e expiração são ações diferentes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-lifecycle-mgmt.html).
- [S3 Intelligent-Tiering: prazos e objetos menores que 128 KB](https://docs.aws.amazon.com/AmazonS3/latest/userguide/intelligent-tiering-overview.html).
- [S3 Versioning](https://docs.aws.amazon.com/AmazonS3/latest/userguide/Versioning.html).
- [S3: verificação de integridade](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity.html).
- [OpenAI Docs: Codex App Server](https://learn.chatgpt.com/docs/app-server).
- [Claude Code: referência CLI](https://code.claude.com/docs/en/cli-reference).
