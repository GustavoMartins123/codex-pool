#!/usr/bin/env bash
set -euo pipefail

# Rebuild the image and recreate the container.
#
# Do NOT build on the host and `docker cp` the binary into the container:
# host builds are linked against a newer glibc and crash inside the
# debian:bookworm-slim runtime image with
# "version `GLIBC_2.38' not found".

docker compose build codex-pool
docker compose up -d codex-pool
