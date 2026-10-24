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
