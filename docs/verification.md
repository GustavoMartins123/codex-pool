# Verification

Use Go 1.26.8 and Node 24.21.0, matching the pinned container build and CI.
Build embedded assets before running Go checks:

```sh
npm --prefix web ci
npm --prefix web run build
npm --prefix web test
go vet ./...
go test -race -count=1 ./...
bash scripts/check-dependencies.sh
```

The dependency script fails on npm vulnerabilities at any severity and on
govulncheck findings in called Go code. It also writes Go and frontend CycloneDX
SBOMs to `tmp/supply-chain`. CI archives only these four dependency reports,
fixes external Actions to commit SHAs, and runs vet/race tests on Linux and Windows.
Windows requires MSYS2 UCRT64 GCC with CGO enabled.

As of 2026-09-29, govulncheck v1.8.0 reported GO-2026-5932 only at the module
level for `golang.org/x/crypto v0.56.0`. The affected `openpgp` package is not
imported or called. No finding is suppressed; every CI run repeats the scan.
Project maintainers should revisit this triage when dependencies or imports change.

## Authenticated regression coverage

`TestAuthenticatedHandoffTransportAndProviderIsolation` uses real local HTTP/SSE
and WebSocket connections with authenticated Alice/Bob identities sharing an
external conversation ID. It exercises Codex WS → Claude SSE → Antigravity SSE
→ Codex WS, including Antigravity recovery, and verifies question/answer
sentinels and preserved history in upstream requests and scoped stores.

`TestAuthenticatedWebSocketRevocationStopsNextTurn` covers client revocation and
principal suspension on an already-open socket, denial of new HTTP/WS requests,
and continued access for the other principal. WebSockets revalidate their
handshake credential on each client message. Revocation blocks the next message;
it does not retract messages already forwarded to an upstream.

## Disposable Docker/vault acceptance

With Docker Desktop using Linux containers, or a Linux Docker daemon:

```sh
docker build --target verify -t codex-pool:verify-tests .
docker build -t codex-pool:verify .
python scripts/verify_docker.py
```

The verify stage runs Linux vet/race checks as a regular user, so permission
tests exercise real restrictions. The runtime remains the default final stage.
The Python script requires Python 3.10+ and Docker Compose with `up --wait`.

The script creates a unique Compose project with generated keys, one synthetic
upstream credential, two named volumes, no published ports, and an internal
network. It checks the nonroot UID, health, credential migration, authenticated
operator surfaces and empty conversation inspector, credential verification,
paired Bolt/DuckDB backup sizes and SHA-256 digests, restore/restart/login state,
wrong/missing-key rejection without mutation, and key rotation followed by restart
without the previous key. HTTP probes run through curl inside the container.
Providers remain unreachable from this internal network.

It writes results under `tmp/verification` and removes only its own project
and volumes, including on test failure. Images and build cache remain available.
This acceptance run does not deploy the production Compose stack or validate
live paid providers. Production container changes still deploy through
`scripts/deploy.sh`.
