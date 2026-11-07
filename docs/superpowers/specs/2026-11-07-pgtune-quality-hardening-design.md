# pgtune quality hardening — design

## Intent

pgtune is published on GitHub and may be run by strangers as a PostgreSQL
superuser on production servers. This work removes the failure modes most
likely to hurt those users, then strengthens the feedback loop so future
changes stay safe.

## Goals

- Never execute another role's SQL as the connected (possibly superuser) role.
- Generate valid, safely quoted DDL for any schema, table, or column name.
- Make index builds survive long-running `CREATE INDEX CONCURRENTLY` calls and
  tell the operator how to clean up an `INVALID` index.
- Build connection strings without hand-escaping, and give each collection
  phase its own timeout budget.
- Remove a latent unsigned underflow in the work_mem budget.
- Add a real-PostgreSQL integration test and stronger CI checks.

## Non-goals

- No new tuning formulas or features such as JSON output or drift detection.
- No package restructuring into `internal/`.
- No automatic dropping of invalid indexes (destructive; the operator decides).

## Constraints

- Keep the single `main` package and existing file layout.
- Behavior flags stay compatible except the documented EXPLAIN ownership rule.
- Go 1.25, pgx v5, Linux, PostgreSQL 13+.

## Approaches considered

1. **Safety-first hardening (chosen)** — localized fixes to the dangerous
   paths, each covered by a regression test, then CI hardening.
2. **Verification-first** — build the integration harness and machine-readable
   output before changing behavior. Safer for a large rewrite, but delays the
   risk reduction.
3. **Restructure-first** — split into internal packages and a DB interface.
   Rejected: high churn for a 5k-line codebase that is still easy to navigate.

## EXPLAIN policy (decided)

`ANALYZE` stays on by default. A captured statement is executed with
`EXPLAIN (ANALYZE, BUFFERS)` only when all of these hold:

- it is a SELECT or WITH statement;
- it is not DML;
- it has no locking clause (`FOR UPDATE`, `FOR SHARE`, `FOR NO KEY UPDATE`,
  `FOR KEY SHARE`);
- its `pg_stat_statements.userid` equals the connected role's OID.

Every other eligible statement gets a plain `EXPLAIN` (no execution). The
report says how many statements were planned only and why. `-explain 0` still
disables query planning entirely.

This is the middle ground the maintainer chose: other roles' statements still
produce planner estimates and index advice, but are never run.

## What is left to the builder

Exact helper names and test layout are specified in the implementation plan.
No open product decisions remain.
