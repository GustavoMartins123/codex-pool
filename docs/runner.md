# Runner

O runner é um binário independente que mantém uma conexão WebSocket com o
codex-pool e executa Codex CLI ou Claude Code na máquina autorizada. O app não
executa os agentes; ele cria sessões, mostra eventos e decide aprovações.

## Releases

Cada tag `runner-v*` publica `runnerd` para Windows, Linux e macOS, nos formatos
ZIP ou TAR.GZ, além de `SHA256SUMS`. Confirme o SHA-256 antes de executar um
binário baixado. O arquivo `.sh` é script de compilação; o processo executado é
`runnerd` ou `runnerd.exe`.

## Arquivo único de configuração

Crie `runner.launch.json` fora do repositório. Ele contém apenas caminhos e
opções; o token fica em um arquivo de estado separado.

```json
{
  "state": "/absolute/path/runner-state/runner.json",
  "pool_url": "https://pool.example",
  "name": "desktop-dev",
  "workspaces": "/absolute/path/runner-workspaces",
  "codex": "/absolute/path/codex",
  "claude": "/absolute/path/claude",
  "allow_paths": ["/absolute/path/repos"],
  "labels": ["desktop", "linux"]
}
```

No Windows, use barras invertidas escapadas:

```json
{
  "state": "C:\\Users\\me\\AppData\\Local\\codex-pool\\runner.json",
  "pool_url": "https://pool.example",
  "name": "desktop-windows",
  "workspaces": "C:\\Users\\me\\codex-workspaces",
  "codex": "C:\\Users\\me\\AppData\\Roaming\\npm\\node_modules\\@openai\\codex\\node_modules\\@openai\\codex-win32-x64\\vendor\\x86_64-pc-windows-msvc\\bin\\codex.exe",
  "claude": "C:\\Users\\me\\.local\\bin\\claude.exe",
  "allow_paths": ["C:\\Users\\me\\repos"],
  "labels": ["desktop", "windows"]
}
```

`state`, `workspaces`, executáveis e `allow_paths` precisam ser caminhos
absolutos. Pelo menos um entre `codex` e `claude` deve existir. No Windows, os
executáveis precisam ser `.exe` nativos; shell shims são rejeitados.

## Parear e executar

Extraia o binário e pareie a máquina:

```bash
./runnerd pair --config /absolute/path/runner.launch.json
```

```powershell
.\runnerd.exe pair --config C:\Users\me\runner.launch.json
```

O terminal mostra QR e código. No app, abra **Runners → Conectar runner**, leia o
QR, confira a máquina e autorize usando o código.

Depois, o runner inicia com um único arquivo:

```bash
./runnerd run --config /absolute/path/runner.launch.json
```

```powershell
.\runnerd.exe run --config C:\Users\me\runner.launch.json
```

O processo fica em foreground e reconecta com backoff. Ainda não há instalador
de serviço embutido; use systemd, launchd ou Task Scheduler apontando para esse
comando se precisar de inicialização automática.

Para rotacionar a credencial mantendo a identidade do runner:

```bash
./runnerd rotate --config /absolute/path/runner.launch.json
```

## Workspaces, histórico e retomada

Workspaces efêmeros ficam em `workspaces/<session-id>` e são preservados quando
a execução termina para permitir retomada. O Codex usa `thread/resume`; Claude
usa `--resume` com o mesmo UUID. A retomada exige o runner original online, o
workspace, o transcript nativo e os hashes do checkpoint íntegros.

Se o checkpoint faltar ou estiver corrompido, o histórico continua consultável,
mas a execução não é retomada e nenhuma thread nova é criada. A exclusão
permanente é uma ação separada: remove workspace efêmero, recibo e controle
nativo. Workspaces por caminho e repositórios do usuário não são removidos.

## Diagnóstico

- `runner state unavailable`: pareie a máquina ou corrija o caminho `state`.
- `runner credential rejected`: a credencial foi revogada, rotacionada ou pertence
  a outro pool.
- `agent executable unavailable`: corrija o caminho absoluto no JSON.
- `workspace path is not in the runner allowlist`: adicione o diretório existente
  em `allow_paths`.
- `original runner must be online`: retomada e exclusão dependem da máquina que
  possui o workspace e o estado nativo.
- `checkpoint unavailable` ou `checkpoint checksum mismatch`: a consulta do
  histórico continua disponível, mas a retomada falha explicitamente.

## Compilar

Todos os alvos:

```bash
./scripts/build-runner.sh
./scripts/build-runner-release.sh
```

PowerShell:

```powershell
.\scripts\build-runner.ps1
.\scripts\build-runner-release.ps1
```

O workflow `.github/workflows/runner-release.yml` gera os binários a partir de
tags `runner-v*`; eles não são versionados no repositório.
