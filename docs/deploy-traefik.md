# Deploy behind Traefik

This deployment keeps the application, data, and credentials in one codex-pool
container. Traefik terminates HTTPS on a shared external Docker network. The
repository contains no machine-specific domain, Docker network name, or secret.

## Prepare the target host

1. Point the chosen DNS name at the VPS. If Cloudflare proxies it, use **Full
   (strict)** TLS and ensure Traefik has a valid origin certificate for the name.
2. Install Docker Engine with the Compose plugin and clone this repository.
3. Create the external Docker network used by Traefik if it does not already
   exist. The codex-pool container must join that same network.
4. Copy `.env.example` to `.env` on the VPS. Set `POOL_DOMAIN`,
   `TRAEFIK_NETWORK`, `TRAEFIK_ENTRYPOINT`, and `TRAEFIK_CERTRESOLVER` to the
   values configured in that Traefik installation. Set independent, strong
   `ADMIN_TOKEN`, `POOL_AUTH_ENCRYPTION_KEY`, `POOL_JWT_SECRET`, and
   `POOL_CREDENTIAL_KEY`. Keep `.env` outside Git and readable only by the
   deployment account.

`ADMIN_TOKEN` is required to create the first operator. An installation
without it denies operator creation. Keep all four secrets stable across
restarts and redeployments. Changing the Passport or JWT secret invalidates
existing stored or issued credentials; changing the credential-vault key
without its documented rotation procedure makes encrypted account files
unreadable.

The base `docker-compose.yml` binds port 8989 only to the host loopback
interface. `compose.traefik.yml` adds the HTTPS route; it does not publish
another port. Do not open port 8989 in the VPS firewall.

## Move an existing installation

Stop the source instance before copying its files so the databases and account
tokens form one consistent snapshot. Copy `pool/` and `data/` over SSH to the
same paths beside the cloned repository. Copy the existing secret values used
by that instance into the target `.env`, especially
`POOL_AUTH_ENCRYPTION_KEY`, `POOL_JWT_SECRET`, and `POOL_CREDENTIAL_KEY` if set.
Copy any relevant configuration from `config.toml` into the target's
environment or mount that file explicitly. The default Compose file does not
mount `config.toml`.

On Linux, the container runs as UID/GID 1000. Give that user ownership of the
copied `pool/` and `data/` trees, and restrict credential files to owner access.
Do not add the data, credentials, `.env`, or a transfer archive to Git. Start
only the target instance after the transfer; running two instances against
copies of the same refresh tokens can invalidate one another.

## Validate and start

From the repository root on the VPS:

```sh
export COMPOSE_FILE=docker-compose.yml:compose.traefik.yml
docker compose config --quiet
./scripts/deploy.sh
docker compose ps
```

Open `https://$POOL_DOMAIN/` and create the first operator with the configured
admin token if the copied database has none. Confirm that the dashboard loads,
an authenticated client request succeeds, and an unauthenticated request is
rejected. Keep the source stopped once the target is serving real traffic.

Back up `pool/`, `data/`, and the secret values together to encrypted storage.
Stop the container or use a database-aware snapshot so Bolt, SQLite, and
DuckDB files remain consistent.
