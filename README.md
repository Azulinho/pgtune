# pgtune

Automatic PostgreSQL tuning for a single instance, plus pgbench load testing.

- **`pgtune`** — a Go CLI that connects to PostgreSQL, collects live statistics,
  analyzes the workload (slow queries, `pg_stat_statements`, `EXPLAIN ANALYZE`),
  computes a tuning plan, and applies it to the server configuration via
  `ALTER SYSTEM` + `pg_reload_conf()`.
- **`make load`** — pgbench benchmarks against the same instance.

## Quick start

```bash
make build          # compile
make tune-dry       # collect stats + show the tuning plan (changes nothing)
make tune-restart   # full auto-tune: install pg_stat_statements, apply plan,
                    # ANALYZE stale stats, restart once (first run only)
make load           # generate workload, then re-run pgtune to see analysis
```

## How it works

1. **Connect** — as the configured role (default `app/app` through pgbouncer on
   port 6432; use `-port 5432` to go direct).

2. **Collect stats**
   - System: RAM, CPU, disk type (SSD/HDD) from `/proc`.
   - Server: `pg_stat_database` (hit ratio, commits, temp spill, deadlocks),
     `pg_stat_checkpointer`/`pg_stat_bgwriter` (checkpoint pressure),
     `pg_stat_user_tables` (seq vs index scans, dead tuples, last ANALYZE),
     `pg_stat_activity` (long-running queries), `pg_settings`, database size.

3. **Query analysis** (`pg_stat_statements`, auto-installed if missing)
   - Top queries by total execution time, with call counts and mean latency.
   - `EXPLAIN (ANALYZE, BUFFERS)` on the most expensive SELECTs — real
     execution. Parameterized queries get `EXPLAIN (GENERIC_PLAN)` (plan shape
     without executing; pgss normalizes literals to `$n`, so ANALYZE can only
     run on constant-free queries). DML is never executed.
   - Findings: seq scans with filters (→ index), sorts spilling to disk /
     temp spills (→ work_mem), planner estimate skew (→ ANALYZE), table bloat,
     stale statistics, checkpoint pressure, long-running queries.

4. **Compute the plan** — classic pgtune-style formulas per workload
   (`oltp` default, `olap`, `mixed`, `desktop`) **plus evidence-based
   adjustments**:
   - work_mem raised when sorts spill to disk (within a hard RAM budget)
   - shared_buffers raised when the cache hit ratio is low and the DB fits in RAM
   - max_wal_size / min_wal_size raised under checkpoint pressure
   - autovacuum_vacuum_scale_factor tightened when dead-tuple bloat is found

5. **Apply** — `ALTER SYSTEM SET ...` for changed parameters, then reload.
   `ALTER SYSTEM` needs superuser: the app falls back to `sudo -u postgres psql`
   (passwordless sudo on this host) or prints the SQL with `-sudo=false`.
   Parameters with `postmaster` context (shared_buffers, wal_buffers,
   shared_preload_libraries) require a restart; the app reports them and
   restarts when given `-restart`.

6. **pg_stat_statements bootstrap** — on first run the app creates the
   extension and adds it to `shared_preload_libraries` (one restart), after
   which every run reports real query statistics. `make tune-restart` does this
   automatically; use `-install-pgss=false` to opt out.

The app is idempotent: re-running applies nothing once the instance matches the
plan, and it never downgrades memory parameters based on a lack of evidence
(no ping-pong between values).

## CLI reference

```
pgtune [flags]
  -host string          PostgreSQL host (default "127.0.0.1")
  -port int             PostgreSQL port: 6432 pgbouncer / 5432 direct (default 6432)
  -user string          (default "app")
  -password string      (default "app")
  -db string            database to analyze (default "app")
  -workload string      oltp | olap | mixed | desktop (default "oltp")
  -apply                apply the plan (ALTER SYSTEM + reload)
  -restart              restart postgres via sudo systemctl if required
  -analyze              run ANALYZE when stale statistics are detected (apply mode)
  -sudo                 allow sudo -u postgres fallback for apply (default true)
  -install-pgss         install pg_stat_statements when missing (default true)
  -top int              number of top queries from pg_stat_statements (default 8)
  -explain int          how many top queries to EXPLAIN, 0 disables (default 3)
  -long-running float   flag queries running longer than this many seconds (default 5)
  -tables int           number of largest tables to analyze (default 10)
```

Examples:

```bash
./pgtune -workload olap -apply -restart -analyze   # full analytics auto-tune
./pgtune -port 5432 -apply                         # connect direct to postgres
./pgtune -sudo=false                               # print SQL instead of applying
./pgtune -explain 0                                # skip EXPLAIN (faster, read-only)
```

## Load testing

```bash
make load                      # 20 clients / 8 threads / 60s (auto-inits schema)
make load CLIENTS=100 DURATION=120
make load-readonly             # SELECT-only
make load-custom F=my.sql
```

Compare TPS/latency before and after tuning:

```bash
make load DURATION=30          # before
make tune
make load DURATION=30          # after
```

## Layout

| File | Purpose |
|---|---|
| `main.go` | CLI entry, orchestration |
| `stats.go` | system + live PostgreSQL statistics collection |
| `queries.go` | pg_stat_statements, long-running queries, EXPLAIN analysis, findings |
| `tune.go` | tuning formulas + evidence-based adjustments |
| `apply.go` | ALTER SYSTEM / reload / pg_stat_statements install / ANALYZE / restart |
| `report.go` | terminal report rendering |
| `Makefile` | build, tune, and load-test targets |
