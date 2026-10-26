# Runner

The runner is a standalone binary that maintains a WebSocket connection to
codex-pool and runs Codex CLI or Claude Code on the authorized machine. The app
creates sessions, displays events, and handles approvals.

## Releases

Each `runner-v*` tag publishes `runnerd` for Windows, Linux, and macOS as ZIP or
TAR.GZ archives, together with `SHA256SUMS`. Verify the SHA-256 checksum before
running a downloaded binary. The `.sh` files are build scripts; the executable
is `runnerd` or `runnerd.exe`.

## Pair from the app

The same `runnerd` binary works with any pool: it knows none until the app
tells it. Run it without arguments on the machine:

```bash
./runnerd
```

On first use it prints a QR. In the app, open **Runners**, choose the option to
connect a runner, scan the QR, check the machine name and authorize it. The
runner saves its credential and connects to the pool in the same process; later
starts skip the QR and connect directly.

How it works:

- The QR lists the machine's private IPv4 LAN addresses, a one-time secret and
  the hash of a throwaway TLS certificate. It names no pool.
- The app connects to the machine over TLS pinned to that certificate, sends
  the pool origin of the account that scanned the QR, and receives the pairing
  request the runner opened at that pool. The app's login never reaches the
  runner, and the command key never reaches the pool.
- The app stores the command key with that account, then approves the request.
  The pool sends the credential only to the runner, which saves it before
  reporting success. The app shows the runner online once the pool sees it.
- The LAN service answers only this flow and closes when pairing ends. A QR
  expires after 5 minutes and a new one replaces it; each QR can bind one
  pairing only.

Requirements and limits:

- The phone must reach the computer directly on the LAN while pairing. Being
  on the same Wi-Fi is not enough when the network isolates clients. Allow
  `runnerd` through the computer's firewall if needed. IPv6-only networks are
  not supported yet.
- Treat the QR as a secret: whoever scans it before you can start a pairing.
  Nothing is authorized until you approve it in the app.
- Pairing from the app is available on Android. On iOS, use the launch
  configuration below.
- Without arguments, `runnerd` keeps its state, workspaces and any agent it
  installed in the user configuration directory. Use a launch configuration
  for other paths, labels, allowed workspace paths or `allow_auto`.
- The pool must support recoverable credential delivery. Older pools are
  refused with a message asking to update them.

### Agents

Without arguments, `runnerd` looks for each agent in this order and uses the
first one that is a native build for this machine (or, outside Windows, a
script) and answers `--version` within 10 seconds:

1. Codex installed by `runnerd` itself.
2. Claude Code from its native installer, in `~/.local/bin`.
3. `PATH`, ignoring relative entries and the current directory. An npm
   install of Codex runs its native build directly, so Node is not needed.
   On Windows only `.exe` files are run, never `.cmd`, `.ps1` or `.bat`.

If it finds none and you are at the terminal, it offers to install Codex. The
prompt shows the version and size, and nothing is downloaded unless you answer
`y`. To install without the prompt:

```bash
./runnerd install codex
```

The download comes from the npm registry and is checked against a SHA-512
compiled into `runnerd`; only the platform's native files are unpacked, into
the runner's own directory. `PATH` and your own Codex settings are not
changed, and no administrator rights are needed. Each runner release pins one
Codex release; a newer `runnerd` replaces it. Maintainers update the pin with
`go run ./cmd/agentcatalog -codex <version>`, which checks the npm registry's
signature on every platform package before writing the catalog.

The runner still pairs when no agent is available; it stops with
`paired, but no agent is available` and connects once one is installed.

If the runner stops after the app approves but before it saves the
credential, start it again within about 10 minutes: it finishes the same
pairing from its journal without a new QR. Later than that, revoke the unused
runner in the app and pair again.

## Launch configuration

Create `runner.launch.json` outside the repository. It contains paths and
options; the token is stored in a separate state file.

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

On Windows, escape backslashes:

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

`state`, `workspaces`, executable paths, and `allow_paths` must be absolute.
At least one of `codex` or `claude` must point to an existing executable.
On Windows, executables must be native `.exe` files; shell shims are rejected.

## Permission modes

Each session runs in one of two permission modes, chosen in the app:

- **Gated** (default): the agent asks for approval in the app before running
  commands, and Codex stays inside its `workspace-write` sandbox.
- **Auto**: the agent runs without approvals and without a filesystem sandbox
  (Codex `danger-full-access`, Claude Code `bypassPermissions`). It can read
  and change anything the runner's user can, not only the session workspace.

The runner refuses auto sessions unless the machine owner enables them in the
launch configuration:

```json
{
  "allow_auto": true
}
```

Without this setting, an auto session ends before any credential is fetched or
workspace is created: the app shows it closed with `auto_permissions_disabled`,
and the runner logs `auto permissions are disabled`.
Path workspaces always require gated permissions.

## Signed commands

Pairing and rotation generate a command key on the runner. The key travels
only inside the QR code: the app that scans it stores the key with its
credentials, and the pool never receives it. The QR image saved with `--qr` or
`qr` is deleted when pairing ends.

Every runner has a command key and refuses to start without one. It accepts a
prompt only when the paired device signed it for that session, its agent, its permission mode and its workspace,
and accepts an approval only when the device signed it for the exact command
the runner asked about. A compromised pool, or someone holding a stolen app
token, can therefore no longer make the runner execute anything. Denials,
cancellation and closing stay unsigned because they are always safe.

- Only the device that scanned the QR can send prompts and approvals to that
  runner. To use another device, run `runnerd rotate` and scan the new QR
  there.
- Signed prompts must reach the runner within 10 minutes of being sent, and
  each must be newer than the last one the session accepted. Keep the device
  and runner clocks in sync.
- The app keeps a new session's first message on the device and sends it as
  a signed turn once the runner is ready. The pool refuses unsigned prompts
  and approvals, and runners that do not announce a command key.

## Pair and run

Extract the binary and pair the machine:

```bash
./runnerd pair --config /absolute/path/runner.launch.json
```

```powershell
.\runnerd.exe pair --config C:\Users\me\runner.launch.json
```

The terminal displays a QR code and a verification code. In the app, open
**Runners**, choose the option to connect a runner, scan the QR code, verify the
machine, and authorize it using the verification code.

Then start the runner using the same configuration file:

```bash
./runnerd run --config /absolute/path/runner.launch.json
```

```powershell
.\runnerd.exe run --config C:\Users\me\runner.launch.json
```

The process runs in the foreground and reconnects with backoff. There is no
built-in service installer; use systemd, launchd, or Task Scheduler with this
command if you need automatic startup.

To rotate the credential while preserving the runner identity:

```bash
./runnerd rotate --config /absolute/path/runner.launch.json
```

## Workspaces, history, and resume

Ephemeral workspaces are stored in `workspaces/<session-id>` and preserved when
execution ends so sessions can resume. Codex uses `thread/resume`; Claude uses
`--resume` with the same UUID. Resuming requires the original runner to be
online, the workspace and native transcript to be present, and checkpoint
checksums to match.

If the checkpoint is missing or corrupted, history remains available, but
execution cannot resume and no new thread is created. Permanent deletion is a
separate action: it removes the ephemeral workspace, checkpoint receipt, and
native agent state. User path and repository workspaces are preserved.

## Troubleshooting

- `no private IPv4 LAN address found`: connect the computer to the phone's
  network, or use a launch configuration with `pair --url`.
- The app cannot reach the computer: check that both are on the same network
  without client isolation and that the firewall allows `runnerd`.
- `paired, but no agent is available`: the lines above it say why each
  candidate was skipped. Run `runnerd install codex`, install Codex or Claude
  Code yourself, or start with `runnerd run --config`.
- `does not match the pinned SHA-512`: the download was altered or corrupted.
  Nothing was installed; try again on another network.
- `invalid pairing journal`: the file next to the runner state is damaged.
  Move it away, revoke any unused runner in the app, and start again.
- `runner state unavailable`: pair the machine or correct the `state` path.
- `runner has no command key`: the machine was paired before command keys
  existed. Run `runnerd rotate` and scan the new QR in the app.
- `runner credential rejected`: the credential was revoked, rotated, or belongs
  to another pool.
- `agent executable unavailable`: correct the absolute executable path in JSON.
- `workspace path is not in the runner allowlist`: add the existing directory
  to `allow_paths`.
- `auto permissions are disabled`: the session asked for auto permissions on a
  runner without `"allow_auto": true`. Use gated permissions, or enable auto
  sessions in the launch configuration.
- `command is not signed by the device paired with this runner`: the prompt or
  approval came from a device without this runner's key, or from a pool that
  altered it. Run `runnerd rotate` and scan the new QR on the device you use.
- `outside the accepted time window`: the device or runner clock is off by
  more than 10 minutes, or the pool delivered the command late.
- `original runner must be online`: resuming and deleting require the machine
  that holds the workspace and native state.
- `checkpoint unavailable` or `checkpoint checksum mismatch`: history remains
  available, but resuming fails explicitly.

## Build

All targets:

```bash
./scripts/build-runner.sh
./scripts/build-runner-release.sh
```

PowerShell:

```powershell
.\scripts\build-runner.ps1
.\scripts\build-runner-release.ps1
```

The `.github/workflows/runner-release.yml` workflow builds binaries from
`runner-v*` tags. Generated binaries are not tracked in the repository.
