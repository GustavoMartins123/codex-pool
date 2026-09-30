#!/usr/bin/env python3
"""Homologate a disposable, network-isolated stack with synthetic credentials.

Build the runtime image first: docker build -t codex-pool:verify .
Never uses the production Compose project, bind mounts, or credentials.
"""
import hashlib
import http.cookies
import json
import os
from pathlib import Path
import secrets
import subprocess

ROOT = Path(__file__).resolve().parents[1]
PROJECT = "codex-pool-verify-" + secrets.token_hex(4)
ENV = dict(os.environ)
ENV.update({name: secrets.token_hex(32) for name in (
    "VERIFY_JWT_SECRET", "VERIFY_AUTH_KEY", "VERIFY_VAULT_KEY", "VERIFY_ADMIN_TOKEN")})
ENV.update(VERIFY_VAULT_VERSION="1", VERIFY_VAULT_PREVIOUS="")
COMPOSE = ["docker", "compose", "--project-name", PROJECT, "--file",
           str(ROOT / "docker-compose.verify.yml")]
REPORT = ROOT / "tmp/verification/docker-homologation.json"
SENTINEL = "VERIFY_TEST_UPSTREAM_SENTINEL"
RESULTS = []


def run(args, *, data=None, expect=0):
    result = subprocess.run(args, cwd=ROOT, env=ENV, input=data,
                            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=180)
    if expect == 0 and result.returncode != 0:
        raise RuntimeError("Docker operation failed: " + result.stderr[-3000:])
    if expect != 0:
        if result.returncode != expect:
            raise RuntimeError("Fail-closed check returned an unexpected exit code")
        return result.stdout + result.stderr
    return result.stdout


def compose(*args, **kwargs):
    return run(COMPOSE + list(args), **kwargs)


def passed(name):
    RESULTS.append({"check": name, "passed": True})
    print(name + ": PASS", flush=True)


def start():
    compose("up", "-d", "--wait", "--wait-timeout", "90")
    return "http://127.0.0.1:8989"


class Client:
    def __init__(self, base):
        self.base, self.cookies, self.csrf = base, {}, ""

    def request(self, path, body=None, admin=False, status=200):
        headers = {"Content-Type": "application/json"}
        if admin:
            headers["X-Admin-Token"] = ENV["VERIFY_ADMIN_TOKEN"]
        if self.cookies:
            headers["Cookie"] = "; ".join(k + "=" + v for k, v in self.cookies.items())
        if self.csrf:
            headers["X-CSRF-Token"] = self.csrf
        args = ["exec", "-T", "codex-pool", "curl", "--silent", "--show-error", "--include",
                "--max-time", "15"]
        for key, value in headers.items():
            args += ["--header", key + ": " + value]
        if body is not None:
            args += ["--data-binary", json.dumps(body)]
        response = compose(*args, self.base + path)
        head, raw = response.split("\n\n", 1)
        actual_status = int(head.splitlines()[0].split()[1])
        if actual_status != status:
            raise RuntimeError(f"{path}: expected {status}, received {actual_status}: {raw[:500]}")
        if SENTINEL in raw:
            raise RuntimeError("Upstream credential leaked in HTTP response")
        for line in head.splitlines()[1:]:
            key, value = line.split(":", 1)
            if key.lower() == "set-cookie":
                jar = http.cookies.SimpleCookie()
                jar.load(value.strip())
                self.cookies.update({k: v.value for k, v in jar.items()})
        value = json.loads(raw)
        if isinstance(value, dict) and "csrf" in value:
            self.csrf = value["csrf"]
        return value


def main():
    failure = None
    try:
        compose("create")
        run(["docker", "run", "--rm", "--network", "none", "--user", "0",
             "--entrypoint", "sh", "-v", PROJECT + "_pool:/app/pool",
             "-v", PROJECT + "_data:/app/data", "codex-pool:verify", "-c",
             "mkdir -p /app/pool/codex; printf '%s' '{\"tokens\":{\"access_token\":\""
             + SENTINEL + "\"}}' > /app/pool/codex/one.json; "
             "chown -R 1000:1000 /app/pool /app/data; chmod 700 /app/pool /app/pool/codex /app/data; "
             "chmod 600 /app/pool/codex/one.json"])
        base = start()
        uid = compose("exec", "-T", "codex-pool", "id", "-u").strip()
        if uid != "1000":
            raise RuntimeError("Runtime is not nonroot")
        network = run(["docker", "network", "inspect", PROJECT + "_isolated", "--format", "{{.Internal}}"])
        if network.strip() != "true":
            raise RuntimeError("Verification network permits external traffic")
        passed("nonroot_internal_network_health")
        raw = compose("exec", "-T", "codex-pool", "cat", "/app/pool/codex/one.json")
        if json.loads(raw).get("cpvault") != 1 or SENTINEL in raw:
            raise RuntimeError("Synthetic credential was not encrypted")
        passed("plaintext_migration_at_rest")
        client = Client(base)
        password = secrets.token_urlsafe(32)
        client.request("/api/setup/operator", {"username": "verifyoperator", "email": "verify@example.test",
                       "password": password, "display_name": "Verification Operator"}, admin=True)
        client.request("/api/auth/me")
        accounts = client.request("/admin/accounts")
        if not any(entry["id"] == "one" for entry in accounts):
            raise RuntimeError("Synthetic credential was not loaded into the pool")
        for path in ("/api/pool/stats", "/api/console/principals", "/api/console/audit",
                     "/api/console/analytics-health", "/admin/transitions"):
            client.request(path)
        client.request("/admin/debug/conversations/verify-empty", status=404)
        before = client.request("/api/me/clients", {"label": "before-backup"})["id"]
        passed("operator_authenticated_surfaces_no_secret_leak")
        compose("stop")
        compose("run", "--rm", "codex-pool", "-check-credentials")
        compose("run", "--rm", "codex-pool", "-backup-dir", "/app/data/backups")
        manifest_path = compose("run", "--rm", "--entrypoint", "sh", "codex-pool", "-c",
                                "ls /app/data/backups/passport-backup-*.json").strip()
        manifest = json.loads(compose("run", "--rm", "--entrypoint", "cat", "codex-pool", manifest_path))
        for component in ("bolt", "duckdb"):
            entry = manifest[component]
            content = subprocess.run(COMPOSE + ["run", "--rm", "--entrypoint", "cat", "codex-pool",
                                     "/app/data/backups/" + entry["name"]], cwd=ROOT, env=ENV,
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True, timeout=60).stdout
            if len(content) != entry["bytes"] or hashlib.sha256(content).hexdigest() != entry["sha256"]:
                raise RuntimeError("Paired backup digest mismatch")
        passed("offline_vault_check_paired_backup_digests")
        client.base = start()
        after = client.request("/api/me/clients", {"label": "after-backup"})["id"]
        compose("stop")
        compose("run", "--rm", "codex-pool", "-restore-manifest", manifest_path)
        client.base = start()
        client.request("/api/auth/login", {"email": "verify@example.test", "password": password})
        ids = {entry["id"] for entry in client.request("/api/me/clients")}
        if before not in ids or after in ids:
            raise RuntimeError("Restore did not recover the exact backed-up client state")
        passed("paired_restore_restart_login_state")
        compose("stop")
        wrong = compose("run", "--rm", "-e", "POOL_CREDENTIAL_KEY=" + secrets.token_hex(32),
                        "codex-pool", expect=1)
        missing = compose("run", "--rm", "-e", "POOL_CREDENTIAL_KEY=", "codex-pool", expect=1)
        if "authentication failed" not in wrong or "POOL_CREDENTIAL_KEY is required" not in missing:
            raise RuntimeError("Startup failed for a reason other than vault key validation")
        current = compose("run", "--rm", "--entrypoint", "cat", "codex-pool", "/app/pool/codex/one.json")
        if current != raw:
            raise RuntimeError("Rejected startup changed vault credentials")
        passed("wrong_missing_key_startup_fail_closed_unchanged")
        start()
        passed("healthy_restart_after_rejected_keys")
        compose("stop")
        ENV.update(VERIFY_VAULT_PREVIOUS=ENV["VERIFY_VAULT_KEY"],
                   VERIFY_VAULT_KEY=secrets.token_hex(32), VERIFY_VAULT_VERSION="2")
        start()
        rotated = compose("exec", "-T", "codex-pool", "cat", "/app/pool/codex/one.json")
        if json.loads(rotated).get("kv") != 2 or SENTINEL in rotated:
            raise RuntimeError("Previous-key credential did not rotate to the current key")
        compose("stop")
        ENV["VERIFY_VAULT_PREVIOUS"] = ""
        compose("run", "--rm", "codex-pool", "-check-credentials")
        start()
        passed("key_rotation_restart_without_previous_key")
    except Exception as error:
        failure = str(error)
        raise
    finally:
        try:
            compose("down", "--volumes", "--remove-orphans")
            passed("disposable_project_cleanup")
        except Exception as error:
            failure = (failure or "") + "; cleanup failed: " + str(error)
            raise
        finally:
            REPORT.parent.mkdir(parents=True, exist_ok=True)
            REPORT.write_text(json.dumps({"project": PROJECT, "checks": RESULTS, "failure": failure}, indent=2))


if __name__ == "__main__":
    main()
