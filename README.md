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

1. **Connect** — as the configured role (default `postgres/postgres` direct on
   port 5432, i.e. a superuser connection; pass `-port 6432 -user app -password
   app -db app` to go through pgbouncer as the application role).

2. **Collect stats**
   - System: RAM, CPU, disk type (SSD/HDD) from `/proc`.
   - Server: `pg_stat_database` (hit ratio, commits, temp spill, deadlocks),
     `pg_stat_checkpointer`/`pg_stat_bgwriter` (checkpoint pressure),
     `pg_stat_user_tables` (seq vs index scans, dead tuples, last ANALYZE),
     `pg_stat_activity` (long-running queries), `pg_settings`, database size.

3. **Query analysis** (`pg_stat_statements`, auto-installed if missing)
   - Top queries by total execution time, with call counts and mean latency.
   - `EXPLAIN (ANALYZE, BUFFERS)` on the most expensive SELECTs — real
     execution. Parameterized queries get a plain `EXPLAIN` (no execution)
     with probe values derived from `pg_stats` histograms, so the planner
     estimates selectivity realistically (pgss normalizes literals to `$n`;
     GENERIC_PLAN would assume ~1/3 selectivity for every predicate). DML is
     never executed.
   - Findings: seq scans with filters (→ **concrete CREATE INDEX
     recommendation**), sorts spilling to disk / temp spills (→ work_mem),
     planner estimate skew (→ ANALYZE), table bloat, stale statistics
     (manual or auto ANALYZE older than 7 days), aggregate temp-file pressure
     (e.g. 113GB spilled since stats reset), checkpoint pressure,
     long-running queries. The app's own diagnostic queries and driver/framework
     setup statements are excluded from the pg_stat_statements top list.
   - Index recommendations: for a filtered seq scan on a plain column, the app
     emits `CREATE INDEX CONCURRENTLY` DDL, estimates selectivity with the
     histogram probe (skips predicates matching >10% of the table), skips
     columns already covered by an index, warns when the table is write-heavy,
     and with `-apply-index` creates the index and re-runs EXPLAIN to verify
     the planner switched to it.

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
  -port int             PostgreSQL port: 5432 direct / 6432 pgbouncer (default 5432)
  -user string          (default "postgres")
  -password string      (default "postgres")
  -db string            database to analyze (default "postgres")
  -workload string      oltp | olap | mixed | desktop (default "oltp")
  -apply                apply the plan (ALTER SYSTEM + reload)
  -apply-index          create indexes recommended by seq-scan findings
                        (CONCURRENTLY; skips unselective & covered cases; verifies)
  -restart              restart postgres via sudo systemctl if required
  -analyze              run ANALYZE when stale statistics are detected (apply mode)
  -sudo                 allow sudo -u postgres fallback for apply (default true)
  -install-pgss         install pg_stat_statements when missing (default true)
  -top int              number of top queries from pg_stat_statements (default 8)
  -explain int          how many top queries to EXPLAIN, 0 disables (default 3)
  -long-running float   flag queries running longer than this many seconds (default 5)
  -tables int           number of largest tables to analyze (default 10)
  -all-dbs              analyze every database on the server, one instance
                        plan + per-database recommendations
```

Examples:

```bash
./pgtune                                    # defaults: postgres/postgres@5432/postgres
./pgtune -port 6432 -user app -password app -db app   # via pgbouncer as the app role
./pgtune -workload olap -apply -restart -analyze   # full analytics auto-tune
./pgtune -apply-index                             # create recommended indexes
./pgtune -sudo=false                               # print SQL instead of applying
./pgtune -explain 0                                # skip EXPLAIN (faster, read-only)
./pgtune -all-dbs                                  # every database on the server
```

## Multiple databases on one server

Tuning parameters (`shared_buffers`, `work_mem`, WAL, autovacuum, ...) are
**instance-wide**: the plan is computed once, with evidence (disk sorts, temp
spills, cache hit ratio, bloat) aggregated across all databases. Query analysis
is **per-database** (table stats, pg_stat_statements, EXPLAIN, and index
recommendations are inherently per-database), so each database gets its own
section with its own findings - a converged OLTP database may show "no
problems" while a second database recommends a new index.

```bash
./pgtune -all-dbs          # analysis only
./pgtune -all-dbs -apply   # instance plan + install pg_stat_statements in every DB
./pgtune -all-dbs -analyze -apply-index   # per-database ANALYZE + index creation
```

### Permissions

With the default `postgres` superuser login you see every database's
statements, table stats and histograms with no extra setup - `make tune-all`
just works. If you run as a non-superuser instead, note that since PostgreSQL
15 a non-superuser can only see their *own* statements in
`pg_stat_statements` and their own tables' `pg_stats` histograms, so a plain
`app` user gets an incomplete picture of other databases. For the full
cross-database view as a non-superuser, use a role with `pg_read_all_stats`
(plus table privileges so EXPLAIN can plan the queries):

```sql
CREATE ROLE pgtune LOGIN PASSWORD 'pgtune';
GRANT pg_read_all_stats TO pgtune;
-- optionally, per database:  GRANT SELECT ON ALL TABLES IN SCHEMA public TO pgtune;
```

```bash
make tune-all PGPORT=5432 PGUSER=pgtune PGPASSWORD=pgtune   # non-superuser variant
```

Databases you cannot connect to are listed as skipped. The maintenance
`postgres` database is analyzed like any other; the `pg_stat_statements`
extension must be installed in each database you want captured (the app does
this automatically with `-apply`).

## Load testing

```bash
make load                      # 20 clients / 8 threads / 60s (auto-inits schema)
make load CLIENTS=100 DURATION=120
make load-readonly             # SELECT-only
make load PGPORT=6432 PGUSER=app PGPASSWORD=app PGDATABASE=app   # through pgbouncer
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
| `alldbs.go` | multi-database discovery, per-DB collection, instance aggregation |
| `tune.go` | tuning formulas + evidence-based adjustments |
| `apply.go` | ALTER SYSTEM / reload / pg_stat_statements install / ANALYZE / restart |
| `report.go` | terminal report rendering |
| `*_test.go` | unit tests for plan math, EXPLAIN analysis, and DDL parsing (`make test`; no database needed) |
| `Makefile` | build, tune, and load-test targets |
