# Runner

The runner is a standalone binary that maintains a WebSocket connection to
codex-pool and runs Codex CLI or Claude Code on the authorized machine. The app
creates sessions, displays events, and handles approvals.

## Releases

Each `runner-v*` tag publishes `runnerd` for Windows, Linux, and macOS as ZIP or
TAR.GZ archives, together with `SHA256SUMS`. Verify the SHA-256 checksum before
running a downloaded binary. The `.sh` files are build scripts; the executable
is `runnerd` or `runnerd.exe`.

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

A runner with a command key accepts a prompt only when the paired device
signed it for that session, its agent, its permission mode and its workspace,
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
- With a signing device, the app sends a new session's first message as a
  signed turn once the runner is ready, instead of with the session request.
- Runners paired before command keys existed keep accepting unsigned commands
  and log how to upgrade. Run `runnerd rotate` on each of them and scan the
  new QR to require signatures.
- Update the pool before runners and the app: a runner with a command key and
  the app's signed requests use protocol fields that older pools reject.

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

- `runner state unavailable`: pair the machine or correct the `state` path.
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
