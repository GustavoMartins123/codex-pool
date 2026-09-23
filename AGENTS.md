# Contribution Guidelines

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
