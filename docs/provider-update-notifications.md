# Idea: hosted provider update notifications for clients

Proposed by Gustavo on 2026-09-29, right after the gpt-6.1-sol launch
incident. Explicitly flagged as a future, non-trivial system ("vai ser
um sistema complexo").

## Scope

This is about the **remote/hosted** deployment model only: an operator
runs codex-pool as a service and clients connect to it from other
machines. Local instances already pick up changes without any of this
(fsnotify hot-reloads accounts and config, the model poller syncs in
process, the CLI reads its own catalog files). The unsolved problem is
the hosted case, where the operator updates or redeploys the service
and every connected client is out of date with no signal.

## Problem

A hosted pool silently serves whatever its current state happens to
hold, and remote clients find out about changes only by accident:

- The pool's codex fingerprint refreshes from the appcast every 72h, so
  a pool that booted before a model launch serves a stale catalog for
  up to three days (this is exactly what delayed gpt-6.1-sol).
- Client machines run the generated `model_sync.sh`, which re-fetches
  `/backend-api/codex/models` every 15 minutes but has no way to learn
  that the *provider-side* contract changed (new models, removed
  models, fingerprint bumps, required client updates).
- A deploy or restart of the hosted pool changes what clients see with
  no signal back to them; the first symptom is usually "model not
  found" or a missing entry in the /model picker.

## Idea

When codex-pool runs as a hosted provider, the operator should be able
to notify connected clients after a service update: new models, catalog
changes, availability flips, fingerprint/client-version requirements,
and planned restarts. The update always originates on the provider
side; clients are passive listeners on other machines.

## Hooks that already exist

- `/api/pool/models` aggregates static + discovered models with
  per-model availability (`SupportingAccounts` / `AvailableNow`) — a
  catalog hash/version derived from this payload would be the update
  signal.
- The generated `model_sync.sh` already runs as a long-lived MCP server
  next to the CLI and declares MCP capabilities (currently
  `tools.listChanged: false` — a natural place to flip to true and push
  a notification).
- The pool already has SSE and websocket plumbing plus a heartbeat
  layer, so a `/api/updates/stream` (SSE) or long-poll
  `/api/updates?since=<version>` endpoint is incremental work.
- Federation nodes already poll `/api/pool/models` on an interval
  (federation.go) and could relay notifications downstream.

## Possible shape

1. Pool computes a monotonically increasing `catalog_version` (hash of
   the effective model descriptors + fingerprint + protocol version).
2. Exposes it cheaply (`/status`, `ETag` on `/api/pool/models`).
3. Clients (sync script, federation nodes, passport sessions) subscribe
   or long-poll; on change they refresh the local catalog and surface a
   human-visible notice ("gpt-6.1-sol available — restart to pick it
   up").
4. MCP `listChanged` notification for clients that host the pool's MCP
   server.

## Open questions

- What counts as an update worth notifying: additions only, or removals
  and availability flips too?
- Auth for the push channel (pool JWT is a given, but passport sessions
  and federation nodes have different lifetimes).
- Backward compatibility: older CLIs only poll; notifications must
  degrade to the current 15-minute loop.
- Rate/throttle policy for many clients behind one pool.
