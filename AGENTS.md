# Contribution Guidelines

## Documentation and Reports

Do not create or modify documentation or reports, including performance reports,
bug reports, and audits, unless explicitly requested or authorized by the user.
Ask for authorization before doing so. Authorization for code changes does not
automatically authorize documentation or reports.

Do not include the user's hardware specifications or private local environment
details in versioned or published artifacts without explicit authorization.

## Incremental Commits

Create one commit for each coherent part of a change. Keep commits small and
ordered so that each one is easy to review and revert independently. A typical
implementation should separate domain or backend policy, API contract changes,
frontend behavior, and tests or documentation when those parts can stand alone.

Only stage files and hunks created for the current task. Preserve unrelated
working tree changes and do not include generated build artifacts in commits.

Commit messages should be short, imperative, and written in English. Before
finishing, report the commit hashes and the verification performed for each
part.

## Deploying Container Changes

Never build on the host and `docker cp` the binary into the container
(`docker cp codex-pool-bin codex-pool:/app/codex-pool`). Host builds link
against a newer glibc than the `debian:bookworm-slim` runtime image and crash
on start with "version `GLIBC_2.38' not found". Deploy with:

    scripts/deploy.sh

which runs `docker compose build` and `docker compose up -d`.
