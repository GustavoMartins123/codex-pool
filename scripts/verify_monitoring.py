import base64
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.parse

ROOT = Path(__file__).resolve().parents[1]


def main():
    env = dict(os.environ)
    names = ("VERIFY_JWT_SECRET", "VERIFY_AUTH_KEY", "VERIFY_VAULT_KEY",
             "VERIFY_ADMIN_TOKEN", "MONITORING_METRICS_TOKEN", "GRAFANA_ADMIN_PASSWORD")
    env.update({name: secrets.token_hex(32) for name in names})
    env.update(VERIFY_VAULT_VERSION="1", VERIFY_VAULT_PREVIOUS="")
    project = "codex-pool-monitoring-test-" + secrets.token_hex(4)

    def run(args, *, check=True):
        result = subprocess.run(args, cwd=ROOT, env=env, text=True, capture_output=True,
                                timeout=300, creationflags=0x08000000 if os.name == "nt" else 0)
        if check and result.returncode:
            detail = result.stderr[-3000:]
            for name in names:
                detail = detail.replace(env[name], "<redacted>")
            raise RuntimeError("Monitoring verification command failed: " + detail)
        return result

    with tempfile.TemporaryDirectory(prefix="monitoring-test-") as temp:
        directory = Path(temp)
        empty_env = directory / ".env"
        empty_env.write_text("", encoding="utf-8")
        source = ["docker", "compose", "--env-file", str(empty_env), "-p", project,
                  "-f", str(ROOT / "docker-compose.verify.yml"), "-f", str(ROOT / "compose.monitoring.yml")]
        configured = json.loads(run(source + ["config", "--format", "json"]).stdout)
        configured["services"]["codex-pool"]["environment"]["MONITORING_METRICS_TOKEN"] = env["MONITORING_METRICS_TOKEN"]
        for name in ("prometheus", "grafana"):
            service = configured["services"][name]
            for port in service["ports"]:
                if port["host_ip"] != "127.0.0.1":
                    raise RuntimeError("Monitoring port is publicly exposed")
                port["published"] = "0"
        for network in configured["networks"].values():
            network["internal"] = True
        fixture = directory / "compose.json"
        fixture.write_text(json.dumps(configured), encoding="utf-8")
        command = ["docker", "compose", "--env-file", str(empty_env), "-p", project, "-f", str(fixture)]

        def compose(*args):
            return run(command + list(args)).stdout

        def request(url, authorization=None, expected=200):
            args = ["exec", "-T", "codex-pool", "curl", "--silent", "--show-error", "--max-time", "10", "--include"]
            if authorization:
                args += ["--header", "Authorization: " + authorization]
            response = compose(*args, url)
            header, body = response.split("\n\n", 1)
            status = int(header.splitlines()[0].split()[1])
            if status != expected:
                raise RuntimeError(f"Monitoring endpoint returned {status}, expected {expected}")
            for name in names:
                if env[name] in body:
                    raise RuntimeError("Monitoring response exposed a secret")
            return body

        try:
            compose("create")
            run(["docker", "run", "--rm", "--network", "none", "--user", "0", "--entrypoint", "sh",
                 "-v", project + "_pool:/app/pool", "-v", project + "_data:/app/data", "codex-pool:verify",
                 "-c", "chown -R 1000:1000 /app/pool /app/data; chmod 700 /app/pool /app/data"])
            compose("up", "-d", "--wait", "--wait-timeout", "120")
            request("http://127.0.0.1:8989/metrics", "Bearer " + env["MONITORING_METRICS_TOKEN"])
            request("http://127.0.0.1:8989/metrics", "Bearer invalid", 403)
            request("http://127.0.0.1:8989/admin/accounts", "Bearer " + env["MONITORING_METRICS_TOKEN"], 401)
            deadline = time.monotonic() + 45
            query = "http://prometheus:9090/api/v1/query?" + urllib.parse.urlencode({"query": 'up{job="codex-pool"}'})
            while time.monotonic() < deadline:
                result = json.loads(request(query))["data"]["result"]
                if result and result[0]["value"][1] == "1":
                    break
                time.sleep(0.5)
            else:
                raise RuntimeError("Authenticated Prometheus scrape did not become healthy")
            print("authenticated_scrape_and_scoped_credential: PASS", flush=True)
            request("http://grafana:3000/api/search", expected=401)
            credentials = base64.b64encode(("operator:" + env["GRAFANA_ADMIN_PASSWORD"]).encode()).decode()
            auth = "Basic " + credentials
            dashboard = json.loads(request("http://grafana:3000/api/dashboards/uid/codex-pool-operations", auth))
            if len(dashboard["dashboard"]["panels"]) != 6:
                raise RuntimeError("Operational dashboard was not provisioned")
            health = json.loads(request("http://grafana:3000/api/datasources/uid/codex-pool/health", auth))
            if health["status"] != "OK":
                raise RuntimeError("Grafana datasource is unavailable")
            print("grafana_authentication_dashboard_and_datasource: PASS", flush=True)
            compose("restart", "prometheus", "grafana")
            compose("up", "-d", "--wait", "--wait-timeout", "120")
            request("http://grafana:3000/api/dashboards/uid/codex-pool-operations", auth)
            for name in ("prometheus", "grafana"):
                if compose("exec", "-T", name, "id", "-u").strip() == "0":
                    raise RuntimeError("Monitoring service runs as root")
            print("persistent_nonroot_monitoring_restart: PASS", flush=True)
        finally:
            compose("down", "--volumes", "--remove-orphans")


if __name__ == "__main__":
    main()
