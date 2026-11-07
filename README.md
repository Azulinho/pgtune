# pgtune

pgtune tunes a PostgreSQL server for you, and can also load-test it with
pgbench.

It connects to your database, looks at what is really happening, and works out
a set of settings. If you ask it to, it applies those settings for you.

pgtune never restarts PostgreSQL. If a change needs a restart, it tells you and
you do it yourself. You can run it by hand, from cron, or from a systemd timer.

## Requirements

- Linux. pgtune reads system details from `/proc` on the database host.
- PostgreSQL 13 or newer. Older servers work, but you get less query analysis.
- macOS and Windows are not supported.

## Install

Every release publishes `.deb` and `.rpm` packages for `amd64`/`x86_64` and
`arm64`/`aarch64`. Plain `linux/amd64` and `linux/arm64` binaries are there
too. You can find them on the
[Releases](https://github.com/Azulinho/pgtune/releases) page.

The same packages are also in signed APT and RPM repositories. This means
`apt`, `dnf` and `zypper` can install and upgrade pgtune for you.

The package contains one static binary. It has no runtime dependencies. It
installs `/usr/bin/pgtune` and docs in `/usr/share/doc/pgtune/`.

The repositories always hold the latest release. Older versions stay on the
Releases page.

### Debian and Ubuntu (APT)

First add the signing key and the repository:

```bash
curl -fsSL https://azulinho.github.io/pgtune/deb/pgtune-archive-keyring.gpg \
  | sudo tee /usr/share/keyrings/pgtune-archive-keyring.gpg >/dev/null

echo "deb [signed-by=/usr/share/keyrings/pgtune-archive-keyring.gpg] \
https://azulinho.github.io/pgtune/deb stable main" \
  | sudo tee /etc/apt/sources.list.d/pgtune.list
```

Then install it:

```bash
sudo apt update
sudo apt install pgtune                 # first install
sudo apt install --only-upgrade pgtune  # later upgrades
```

The repository has `amd64` and `arm64` packages. APT picks the right one for
your machine.

The signing key fingerprint is:

```
D158 CA36 D983 0B4F 3357 8654 305D CC1B FAD4 2079
```

To remove pgtune and its repository:

```bash
sudo apt remove pgtune
sudo rm /etc/apt/sources.list.d/pgtune.list \
  /usr/share/keyrings/pgtune-archive-keyring.gpg
sudo apt update
```

### Fedora and RHEL (dnf)

Download the repo file and install:

```bash
sudo curl -fsSL -o /etc/yum.repos.d/pgtune.repo \
  https://azulinho.github.io/pgtune/rpm/pgtune.repo
sudo dnf install pgtune
sudo dnf upgrade pgtune  # later upgrades
```

dnf imports the signing key the first time you use the repo. The metadata and
the packages are both GPG-signed.

To remove pgtune and its repository:

```bash
sudo dnf remove pgtune
sudo rm /etc/yum.repos.d/pgtune.repo
```

### openSUSE (zypper)

```bash
sudo rpm --import https://azulinho.github.io/pgtune/rpm/RPM-GPG-KEY-pgtune.asc
sudo zypper addrepo --refresh https://azulinho.github.io/pgtune/rpm/ pgtune
sudo zypper install pgtune
sudo zypper update pgtune  # later upgrades
```

To remove pgtune and its repository:

```bash
sudo zypper remove pgtune
sudo zypper removerepo pgtune
```

The RPM repository serves `x86_64` and `aarch64` from one URL. The package
manager picks the right one.

### Manual install

```bash
# Debian / Ubuntu
sudo dpkg -i pgtune_<version>-1_amd64.deb

# RHEL / Fedora / openSUSE
sudo rpm -U pgtune-<version>-1.x86_64.rpm
```

Every push to `main` creates the next `0.<YYYYMM>.<N>` tag and publishes new
packages for it.

## Two ways to run pgtune

There are two ways to use pgtune. Pick the one that matches what you are
doing.

| How | Use it for | Connection |
|---|---|---|
| `make <target>` | Development in this repository | Fixed to `postgres/postgres` over TCP 127.0.0.1:5432 |
| `pgtune <flags>` | Real servers, cron and systemd timers | Unix socket as your OS user, or `PG*` variables and flags |

The `make` targets are a convenience for working on this project. They are not
for production.

The installed binary is the tool you run on a server. It works like `psql` and
is safe to run on a schedule.

## Development: the make targets

Run these from a clone of this repository:

```bash
make build          # compile
make tune-dry       # read stats, show the plan, change nothing
make tune-restart   # install pg_stat_statements, apply, ANALYZE
make load           # generate a workload, then run pgtune again
```

`make help` lists every target.

These targets connect to `postgres/postgres` over TCP on 127.0.0.1:5432. Use
them only for local development.

`make tune-restart` needs one manual restart of PostgreSQL the first time. That
is because `pg_stat_statements` is loaded at server start.

## On a server: run pgtune directly

After you install the package, run `pgtune` itself. Do not use the Makefile on
a server.

```bash
sudo -u postgres pgtune                      # show the plan, change nothing
sudo -u postgres pgtune -apply -analyze      # apply the plan and ANALYZE
sudo -u postgres pgtune -all-dbs -apply      # every database
```

By default pgtune uses the unix socket and connects as your current OS user.
There is no password. This is the same as a local `psql`.

It also reads the usual `PG*` variables when set: `PGHOST`, `PGPORT`,
`PGUSER`, `PGPASSWORD`, `PGDATABASE` and `PGSSLMODE`.

To tune a remote server, use `-host` and `-port`, plus `-user` and
`-password`. You can use the environment variables instead.

### Run it from a systemd timer

A timer keeps the settings in check without you logging in. Create two units.

```ini
# /etc/systemd/system/pgtune.service
[Unit]
Description=pgtune PostgreSQL auto-tune
After=postgresql.service

[Service]
Type=oneshot
User=postgres
ExecStart=/usr/bin/pgtune -apply -analyze
```

```ini
# /etc/systemd/system/pgtune.timer
[Unit]
Description=Run pgtune every day

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

Then enable the timer:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now pgtune.timer
```

`Persistent=true` means a missed run starts after boot.

For a remote database, add flags to `ExecStart`, or use an environment file:

```ini
# /etc/pgtune.env
PGHOST=db.example.com
PGUSER=pgtune
PGPASSWORD=secret
```

```ini
[Service]
EnvironmentFile=/etc/pgtune.env
```

Keep that file private with `chmod 600`.

In an unattended run, think twice before adding `-apply-index`. It creates
indexes, which takes time and adds load.

Also think about `-install-pgss`. It is on by default. It needs
`shared_preload_libraries` and one restart. Pass `-install-pgss=false` if you
do not want pgtune to touch that.

## How it works

### Connect

pgtune connects like a local `psql`. It uses the unix socket, your OS user, and
peer authentication.

If you run it as the `postgres` OS user, it is the same as
`sudo -u postgres psql`.

For a remote server or TCP, use `-host`, `-port`, `-user` and `-password`.

### Read the system

pgtune reads RAM, CPU and disk type from the **database host**, not from the
machine you run it on. It uses `pg_read_file` and `pg_ls_dir` to read
`/proc/meminfo`, `/proc/cpuinfo`, `/proc/mounts` and `/sys/block/*`.

A local `psql` session can read the same files. So the numbers are still
correct when you connect remotely.

- LVM, `md` and device-mapper disks are traced down to the real physical disk.
- Overlay mounts are resolved through `upperdir`.
- Mixed or unknown storage is reported as `UNKNOWN`. The cost settings then
  stay at PostgreSQL defaults.
- Reading these files needs superuser. Without it, RAM and CPU come from your
  local machine instead, with a warning for remote connections. Disk detection
  falls back only for local connections.
- `-ram` sets RAM yourself. `-disk ssd` or `-disk hdd` sets the disk type.

### Read the database

pgtune looks at:

- `pg_stat_database`: cache hit ratio, commits, temp spills, deadlocks.
- `pg_stat_checkpointer` and `pg_stat_bgwriter`: checkpoint pressure.
- `pg_stat_user_tables`: scans, dead tuples, last ANALYZE.
- `pg_stat_activity`: long-running queries.
- `pg_settings` and the size of each database.

### Look at queries

pgtune uses `pg_stat_statements`. If it is missing, pgtune installs it.

- It lists the slowest queries by total time, with call counts and average
  latency.
- It runs `EXPLAIN (ANALYZE, BUFFERS)` on the most expensive SELECTs. These are
  real runs, so they cost time. DML is never executed.
- Queries with parameters get a plain `EXPLAIN`, using sample values from
  `pg_stats` so the planner estimate is realistic.
- It reports things like full table scans with filters, sorts spilling to disk,
  wrong planner estimates, table bloat, stale statistics and long queries.
- pgtune ignores its own diagnostic queries and setup statements from drivers
  and frameworks.

For a filtered full scan on a plain column, pgtune suggests a
`CREATE INDEX CONCURRENTLY` statement. It skips predicates that match more than
10% of the table. It skips columns that are already indexed. It warns you when
the table gets many writes.

With `-apply-index`, pgtune creates the index and runs `EXPLAIN` again to check
that the planner uses it.

SQL parsing is best effort. If pgtune does not understand a query, it skips it.
It never guesses.

### Make a plan

pgtune starts from classic pgtune formulas for the workload you choose: `oltp`
(the default), `olap`, `mixed` or `desktop`.

Then it adjusts based on what it found:

- `work_mem` goes up when sorts spill to disk, within a fixed RAM budget.
- `shared_buffers` goes up when the cache hit ratio is low and the database
  fits in RAM.
- `max_wal_size` and `min_wal_size` go up under checkpoint pressure.
- `autovacuum_vacuum_scale_factor` goes down when dead tuples pile up.
- Cost settings follow the disk type:

| Disk | `random_page_cost` | `seq_page_cost` | `effective_io_concurrency` | `maintenance_io_concurrency` |
|---|---|---|---|---|
| SSD | 1.1 | 1.0 | 200 | 100 |
| HDD | 4.0 | 1.0 | 2 | 2 |

If the disk type is unknown, all four stay at PostgreSQL defaults.

### Apply the plan

pgtune runs `ALTER SYSTEM SET ...` for each changed parameter, then reloads the
configuration.

`ALTER SYSTEM` needs a superuser. If you connect as a normal user, pgtune
prints the SQL so you can run it yourself.

Some parameters need a full restart, such as `shared_buffers`, `wal_buffers`
and `shared_preload_libraries`. pgtune lists them and shows how to restart on
common systems. It never restarts the server.

pgtune is safe to run again. If nothing needs to change, it changes nothing.
It will not lower memory settings just because it found no problem, so values
do not bounce up and down.

## About pg_stat_statements

Many people find `pg_stat_statements` confusing. It has two parts.

1. **The library.** Setting `shared_preload_libraries = 'pg_stat_statements'`
   loads it for the whole server. It starts with the postmaster and collects
   query statistics for every database. This needs one restart.
2. **The extension.** Running `CREATE EXTENSION pg_stat_statements` creates a
   view in **one** database. You need to run it in every database where you
   want to read the statistics.

pgtune installs the extension in the database you point it at. With
`-all-dbs -apply` it installs it in every database. It does not create a helper
database for this.

The report shows the state of each database:

| Report line | What it means |
|---|---|
| `capturing (preloaded)` | The library is loaded and the view exists here. You can read stats. |
| `installed but not preloaded` | The view exists, but the library is not loaded. Stats start after a restart. |
| `not installed and not preloaded` | The view does not exist in this database. Run `-apply` or `-all-dbs -apply` to create it. |

It is normal for one database to show `capturing (preloaded)` while another
shows `not installed and not preloaded`. The library is shared, but each
database needs its own view.

## Options

These flags are the same for a local build and for the installed binary.

```
pgtune [flags]
  -host string          PostgreSQL host; PGHOST (default: local unix socket,
                        like psql; a socket directory path also works)
  -port int             PostgreSQL port; PGPORT (default 5432)
  -user string          PostgreSQL user; PGUSER (default: current OS user)
  -password string      PostgreSQL password; PGPASSWORD (default "")
  -db string            database to analyze; PGDATABASE (default "postgres")
  -sslmode string       disable | allow | prefer | require | verify-ca |
                        verify-full; PGSSLMODE (default "prefer")
  -ram string           total RAM override, e.g. 32GB
  -disk string          auto | ssd | hdd (default "auto")
  -workload string      oltp | olap | mixed | desktop (default "oltp")
  -apply                apply the plan (ALTER SYSTEM + reload; needs superuser)
  -apply-index          create indexes found by seq-scan analysis
  -analyze              run ANALYZE when statistics are stale (needs -apply)
  -install-pgss         install pg_stat_statements when missing (default true)
  -top int              number of top queries to show (default 8)
  -explain int          how many queries to EXPLAIN, 0 disables (default 3)
  -explain-timeout int  seconds allowed for EXPLAIN ANALYZE (default 5)
  -long-running float   flag queries running longer than this (default 5)
  -timeout int          seconds allowed per database operation (default 60)
  -tables int           number of largest tables to analyze (default 10)
  -all-dbs              analyze every database, with per-database advice
```

Examples:

```bash
pgtune                                      # local socket as your OS user
PGPASSWORD=postgres pgtune -host 127.0.0.1 -user postgres   # TCP
pgtune -workload olap -apply -analyze       # analytics auto-tune
pgtune -disk ssd                            # force SSD on a VM or cloud disk
pgtune -apply-index                         # create recommended indexes
pgtune -all-dbs                             # every database on the server
```

To skip `EXPLAIN` and run faster, use `-explain 0`.

## Several databases on one server

Tuning settings such as `shared_buffers`, `work_mem`, WAL and autovacuum apply
to the whole instance. So pgtune makes one plan, using evidence from all
databases together.

Query analysis is per database. Table stats, `pg_stat_statements`, `EXPLAIN`
and index advice are all per database. So each database gets its own section
and its own findings.

One database may show no problems while another needs a new index.

```bash
pgtune -all-dbs          # analysis only
pgtune -all-dbs -apply   # instance plan + install pg_stat_statements everywhere
pgtune -all-dbs -analyze -apply-index   # per-database ANALYZE + indexes
```

### Permissions

With a `postgres` superuser connection, you see everything with no extra
setup. The `make` targets use this by default.

As a normal user, PostgreSQL 15 and later only shows you your own statements in
`pg_stat_statements` and your own tables in `pg_stats`. So a plain `app` user
gets an incomplete picture.

For the full view, use a role with `pg_read_all_stats`. Give it table
privileges too, so `EXPLAIN` can plan the queries.

```sql
CREATE ROLE pgtune LOGIN PASSWORD 'pgtune';
GRANT pg_read_all_stats TO pgtune;
-- optionally, per database:
-- GRANT SELECT ON ALL TABLES IN SCHEMA public TO pgtune;
```

Then run pgtune as that role:

```bash
PGUSER=pgtune PGPASSWORD=pgtune pgtune -all-dbs
```

Databases you cannot connect to are listed as skipped. The `postgres`
maintenance database is analyzed like any other.

## Load testing

Load testing uses the `make` targets and `pgbench`. It is a development tool,
not part of a server setup.

```bash
make load                 # 20 clients, 8 threads, 60s, creates the schema
make load CLIENTS=100 DURATION=120
make load-readonly        # SELECT-only
make load-custom F=my.sql
```

To compare before and after tuning:

```bash
make load DURATION=30     # before
make tune
make load DURATION=30     # after
```

`make pgss-reset` clears `pg_stat_statements`. This is useful before a
benchmark, so the stats start from zero.

## Project files

| File | Purpose |
|---|---|
| `main.go` | CLI entry and orchestration |
| `stats.go` | System and live PostgreSQL statistics |
| `queries.go` | pg_stat_statements, long queries, EXPLAIN, findings |
| `alldbs.go` | Database discovery, per-DB collection, aggregation |
| `tune.go` | Tuning formulas and evidence-based adjustments |
| `apply.go` | ALTER SYSTEM, reload, pg_stat_statements install, ANALYZE |
| `report.go` | Terminal report |
| `*_test.go` | Unit tests for plan math, EXPLAIN and DDL parsing |
| `Makefile` | Development build, tune and load-test targets |
| `README.md` | This file |

`make test` runs the unit tests. It does not need a database.

`make help` lists every target.
