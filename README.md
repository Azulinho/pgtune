# pgtune

Automatic PostgreSQL tuning for a single instance, plus pgbench load testing.

Requirements: Linux (system stats come from `/proc` on the database host) and
PostgreSQL 13 or newer (for `pg_stat_statements` column names; older servers
report reduced query analysis). macOS and Windows are not supported.

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
                    # ANALYZE stale stats; restart postgres manually once
make load           # generate workload, then re-run pgtune to see analysis
```

> The `Makefile` targets below are local-development conveniences: they pin
> `postgres/postgres` over TCP 127.0.0.1:5432 on this host. The `pgtune`
> binary itself behaves like a local `psql`: it connects over the unix socket
> as the current OS user (peer authentication, no password), reads standard
> `PG*` environment variables (`PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`,
> `PGDATABASE`, `PGSSLMODE`) when set, and has no default password - adjust
> the variables for your platform. For a remote server use `-host`/`-port`
> plus `-user`/`-password` (or the env vars).

## Packages

Release tags publish `.deb` and `.rpm` packages for `amd64`/`x86_64` and
`arm64`/`aarch64`, plus raw `linux/amd64` and `linux/arm64` binaries, on the
[Releases](https://github.com/Azulinho/pgtune/releases) page.

The same packages are available from signed APT and RPM repositories hosted on
GitHub Pages, so `apt`, `dnf`, and `zypper` can install and upgrade `pgtune`
directly. The packages contain a static binary with no runtime dependencies and
install `/usr/bin/pgtune` plus docs in `/usr/share/doc/pgtune/`.

The repositories always contain the latest release; older versions remain
available on the Releases page.

### APT repository (Debian, Ubuntu)

Install the repository signing key and add the source:

```bash
curl -fsSL https://azulinho.github.io/pgtune/deb/pgtune-archive-keyring.gpg \
  | sudo tee /usr/share/keyrings/pgtune-archive-keyring.gpg >/dev/null

echo "deb [signed-by=/usr/share/keyrings/pgtune-archive-keyring.gpg] \
https://azulinho.github.io/pgtune/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/pgtune.list
```

Then install or upgrade:

```bash
sudo apt update
sudo apt install pgtune                 # first install
sudo apt install --only-upgrade pgtune  # later upgrades
```

The repository contains `amd64` and `arm64` packages; APT selects the one that
matches the host. To inspect the signing key:

```bash
gpg --show-keys --with-fingerprint \
  /usr/share/keyrings/pgtune-archive-keyring.gpg
```

The current signing key fingerprint is:

```
D158 CA36 D983 0B4F 3357 8654 305D CC1B FAD4 2079
```

Remove the package and the repository configuration:

```bash
sudo apt remove pgtune
sudo rm /etc/apt/sources.list.d/pgtune.list \
  /usr/share/keyrings/pgtune-archive-keyring.gpg
sudo apt update
```

### RPM repository (RHEL, Fedora, openSUSE)

On Fedora, RHEL and compatible distributions, download the repo file and
install:

```bash
sudo curl -fsSL -o /etc/yum.repos.d/pgtune.repo \
  https://azulinho.github.io/pgtune/rpm/pgtune.repo
sudo dnf install pgtune
sudo dnf upgrade pgtune  # later upgrades
```

dnf imports the signing key on first use and prints its fingerprint. The
repository metadata and the packages are both GPG-signed, and the repo file
enables `gpgcheck` and `repo_gpgcheck`.

On openSUSE, import the key and add the repository with zypper:

```bash
sudo rpm --import https://azulinho.github.io/pgtune/rpm/RPM-GPG-KEY-pgtune.asc
sudo zypper addrepo --refresh https://azulinho.github.io/pgtune/rpm/ pgtune
sudo zypper install pgtune
sudo zypper update pgtune  # later upgrades
```

The RPM repository serves `x86_64` and `aarch64` packages from one URL; the
package manager selects the matching architecture.

Remove the package and the repository:

```bash
# Fedora / RHEL
sudo dnf remove pgtune
sudo rm /etc/yum.repos.d/pgtune.repo

# openSUSE
sudo zypper remove pgtune
sudo zypper removerepo pgtune
```

### Manual install

```bash
# Debian / Ubuntu
sudo dpkg -i pgtune_<version>-1_amd64.deb

# RHEL / Fedora / openSUSE
sudo rpm -U pgtune-<version>-1.x86_64.rpm
```

Every push to `main` creates the next `0.<YYYYMM>.<N>` tag and publishes the
packages for it automatically.

## How it works

1. **Connect** — like a local `psql`: unix socket, role = current OS user,
   peer authentication (no password). Running the binary as the `postgres`
   OS user is the exact equivalent of `sudo -u postgres psql`. For TCP or a
   remote server: `-host`/`-port`/`-user`/`-password` or `PGHOST`/`PGPORT`/
   `PGUSER`/`PGPASSWORD`. SSL mode defaults to `prefer` (ignored on sockets).
   Pass `-port 5432 -user app -password app -db app` to run as the
   application role.

2. **Collect stats**
   - System: RAM, CPU and storage class (SSD/HDD) read from the **database
     server host** via `pg_read_file`/`pg_ls_dir` (`/proc/meminfo`,
     `/proc/cpuinfo`, `/proc/mounts`, `/sys/block/*`) — the same file access
     a local postgres user's psql session has, so tuning is correct even
     over a remote connection. Device-mapper/LVM/md devices are followed
     through their `slaves/` to the physical disks, and overlay mounts are
     resolved through `upperdir`. Mixed or undetectable storage is reported
     as `UNKNOWN` and leaves the cost settings at PostgreSQL defaults. These
     functions need superuser; for RAM/CPU, restricted roles fall back to the
     client host's `/proc` (warned about when the connection is not local) —
     disk detection does so only for a local connection, otherwise it reports
     `UNKNOWN`. Use `-ram` to be explicit, and `-disk ssd|hdd` to override
     detection (e.g. virtio/cloud disks that report `rotational=1` even when
     backed by SSD/NVMe).
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
     (e.g. tens of GB spilled since stats reset), checkpoint pressure,
     long-running queries. The app's own diagnostic queries and driver/framework
     setup statements are excluded from the pg_stat_statements top list.
     SQL parsing (filter columns, `$n` placeholders, index definitions) is
     best-effort: query shapes it does not recognize are skipped, never guessed.
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
   - cost/I/O constants by detected storage class: SSD →
     `random_page_cost 1.1`, `seq_page_cost 1.0`,
     `effective_io_concurrency 200`, `maintenance_io_concurrency 100`;
     HDD → `4.0 / 1.0 / 2 / 2`. Unknown storage leaves all four at the
     PostgreSQL defaults rather than guessing.

5. **Apply** — `ALTER SYSTEM SET ...` for changed parameters, then reload.
   `ALTER SYSTEM` needs a superuser connection: with a non-superuser role the
   app prints the exact SQL for manual execution. Parameters with `postmaster`
   context (shared_buffers, wal_buffers, shared_preload_libraries) require a
   restart; the app reports which ones and how to restart on common platforms,
   but never restarts the server itself.

6. **pg_stat_statements bootstrap** — on first run the app creates the
   extension and adds it to `shared_preload_libraries` (both need superuser;
   one manual restart), after which every run reports real query statistics.
   `make tune-restart` does this automatically; use `-install-pgss=false` to
   opt out.

### pg_stat_statements: one server-wide library, per-database views

`pg_stat_statements` is easy to confuse because it has two separate parts:

1. **The library** (`shared_preload_libraries = 'pg_stat_statements'`) is
   **server-wide**: it loads once at postmaster start and captures statement
   statistics in shared memory for every database on the cluster. This is the
   part pgtune enables during bootstrap (`-apply`, one manual restart).
2. **The extension** (`CREATE EXTENSION pg_stat_statements`) is
   **per-database**: it creates the `pg_stat_statements` view object in one
   specific database. The view has to be created separately in each database
   where you want to read the captured statistics.

pgtune installs the extension in the database(s) it is pointed at: the `-db`
target, or every database with `-all-dbs -apply`. It never creates a helper
database for this and does not need one - the shared library is the capture
engine, the per-database view is just a window into it.

The per-database report tells you which state each database is in:

| Report line | What it means |
|---|---|
| `capturing (preloaded)` | library preloaded **and** the view exists in this database - stats are readable here |
| `installed but not preloaded` | the extension exists here but the library is not in `shared_preload_libraries` - capturing starts only after a restart |
| `not installed and not preloaded` | the view does not exist in this database (the extension was never created here). The "not preloaded" wording is per-database and can be misleading: the library may already be preloaded server-wide - the fix is `-apply` / `-all-dbs -apply` to create the extension in this database too |

So it is normal for one database to show `capturing (preloaded)` while a
sibling database on the same server shows `not installed and not preloaded`:
the library is shared, but each database needs its own `CREATE EXTENSION`.
`./pgtune -all-dbs -apply` closes the gap automatically.

The app is idempotent: re-running applies nothing once the instance matches the
plan, and it never downgrades memory parameters based on a lack of evidence
(no ping-pong between values).

## CLI reference

```
pgtune [flags]
  -host string          PostgreSQL host; PGHOST (default: local unix socket,
                        like psql; a socket directory path also works)
  -port int             PostgreSQL port; PGPORT (default 5432)
  -user string          PostgreSQL user; PGUSER (default: current OS user,
                        peer-authenticated over the socket, like psql)
  -password string      PostgreSQL password; PGPASSWORD (default "")
  -db string            database to analyze; PGDATABASE (default "postgres")
  -sslmode string       SSL mode: disable | allow | prefer | require |
                        verify-ca | verify-full; PGSSLMODE (default "prefer")
  -ram string           total RAM override, e.g. 32GB (default: read from the
                        database server's /proc/meminfo)
  -disk string          auto | ssd | hdd (default "auto": detect the data
                        directory's disk; ssd/hdd override VMs and cloud
                        volumes that misreport their rotational flag)
  -workload string      oltp | olap | mixed | desktop (default "oltp")
  -apply                apply the plan (ALTER SYSTEM + reload; needs superuser)
  -apply-index          create indexes recommended by seq-scan findings
                        (CONCURRENTLY; skips unselective & covered cases; verifies)
  -analyze              run ANALYZE when stale statistics are detected (apply mode)
  -install-pgss         install pg_stat_statements when missing (default true)
  -top int              number of top queries from pg_stat_statements (default 8)
  -explain int          how many top queries to EXPLAIN, 0 disables (default 3)
  -explain-timeout int  seconds allowed for EXPLAIN ANALYZE execution, 0 = no limit (default 5)
  -long-running float   flag queries running longer than this many seconds (default 5)
  -timeout int          seconds allowed per database operation (default 60)
  -tables int           number of largest tables to analyze (default 10)
  -all-dbs              analyze every database on the server, one instance
                        plan + per-database recommendations
```

Examples:

```bash
./pgtune                                   # local socket as your OS user (like psql)
PGPASSWORD=postgres ./pgtune -host 127.0.0.1 -user postgres   # TCP, postgres superuser
./pgtune -password secret                  # password as a flag
./pgtune -sslmode require                  # TLS to the server
./pgtune -port 5432 -user app -password app -db app   # as the app role
./pgtune -workload olap -apply -analyze    # full analytics auto-tune
./pgtune -disk ssd                         # force SSD cost model (VM/cloud disk)
./pgtune -apply-index                      # create recommended indexes
./pgtune -apply                            # non-superuser: prints the SQL to run
./pgtune -explain 0                        # skip EXPLAIN (faster, read-only)
./pgtune -all-dbs                          # every database on the server
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

With a `postgres` superuser connection (the Makefile default) you see every
database's statements, table stats and histograms with no extra setup -
`make tune-all` just works. If you run as a non-superuser instead, note that
since PostgreSQL 15 a non-superuser can only see their *own* statements in
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
make load PGPORT=5432 PGUSER=app PGPASSWORD=app PGDATABASE=app   # as the app role
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
| `apply.go` | ALTER SYSTEM / reload / pg_stat_statements install / ANALYZE |
| `report.go` | terminal report rendering |
| `*_test.go` | unit tests for plan math, EXPLAIN analysis, and DDL parsing (`make test`; no database needed) |
| `Makefile` | build, tune, and load-test targets |
