# pgtune repo
#
# Load testing (pgbench) and automatic PostgreSQL tuning (Go app).
#
# Common targets:
#   make help           - list all targets
#   make build          - compile the pgtune binary
#   make test           - run the unit tests
#   make tune           - auto-tune: gather stats + apply tuning plan (as postgres superuser)
#   make tune-index     - create indexes recommended by seq-scan findings
#   make tune-restart   - full config tune incl. pg_stat_statements install + ANALYZE
#                         (restart PostgreSQL manually after the first run)
#   make tune-full      - everything: tune + ANALYZE + recommended indexes (restart manually)
#   make tune-dry       - gather stats and show the plan without applying
#   make tune-olap      - same as tune but for an analytics workload
#   make pgss-reset     - clear pg_stat_statements (run before a benchmark)
#   make load           - run the pgbench read/write benchmark
#   make load-readonly  - SELECT-only benchmark
#   make load-custom F=script.sql - custom pgbench script
#
# Workflow: make load ; make tune-full ; make load   (measure -> tune -> measure)

# --- pgtune app ---
GO      ?= go
# The binary defaults to the local unix socket as your OS user (like psql);
# the targets below pin the classic local-dev setup: postgres superuser over
# TCP 127.0.0.1 with PGPASSWORD. (= defers $(PGUSER) expansion to recipe time)
PGTUNE  = ./pgtune -host 127.0.0.1 -user $(PGUSER)
WORKLOAD ?= oltp

# --- load test (pgbench) ---
# The Makefile pins these for both pgbench and pgtune: postgres superuser,
# direct TCP (127.0.0.1). The pgtune binary itself defaults to the local
# unix socket as the current OS user (peer auth), exactly like psql.
# For an app role: make load PGPORT=5432 PGUSER=app PGPASSWORD=app PGDATABASE=app
PGHOST    ?= 127.0.0.1
PGPORT    ?= 5432
PGUSER    ?= postgres
PGPASSWORD?= postgres
PGDATABASE?= postgres
export PGPASSWORD

CLIENTS   ?= 20
THREADS   ?= 8
DURATION  ?= 60
SCALE     ?= 10
PROGRESS  ?= 5
# pgbench flags: CLIENTS=-c concurrent clients, THREADS=-j threads,
# DURATION=-T seconds, SCALE=-s schema scale, PROGRESS=-P report interval

PG_CONN := PGPASSWORD=$(PGPASSWORD) pgbench -h $(PGHOST) -p $(PGPORT) -U $(PGUSER) -d $(PGDATABASE)

.PHONY: help build test tune tune-index tune-restart tune-full tune-dry tune-olap tune-all pgss-reset load load-init load-readonly load-custom load-init-if-needed

help:
	@echo "pgtune targets:"
	@echo "  make build          compile the pgtune binary"
	@echo "  make test           run the unit tests (no database needed)"
	@echo "  make tune           auto-tune postgres (stats + apply, workload=$(WORKLOAD))"
	@echo "  make tune-index     create indexes recommended by seq-scan findings"
	@echo "  make tune-restart   full config tune: pg_stat_statements + ANALYZE (restart postgres manually)"
	@echo "  make tune-full      everything: tune + ANALYZE + recommended indexes (restart postgres manually)"
	@echo "  make tune-dry       show stats and plan without applying"
	@echo "  make tune-olap      auto-tune for an analytics workload"
	@echo "  make tune-all       analyze every database on the server (per-DB recommendations)"
	@echo "  make pgss-reset     clear pg_stat_statements (before/after benchmark)"
	@echo "  make load           run pgbench read/write benchmark (defaults: $(CLIENTS) clients, $(DURATION)s)"
	@echo "  make load-readonly  SELECT-only benchmark"
	@echo "  make load-custom F=script.sql   run a custom pgbench script"

## Compile the pgtune binary.
build:
	cd src && $(GO) build -o ../pgtune .

## Run the unit tests (pure logic: plan math, EXPLAIN analysis, DDL parsing).
test:
	cd src && $(GO) test -count=1 ./...

## Auto-tune: gather stats, apply plan, reload (restarts only if required).
tune: build
	$(PGTUNE) -apply -workload $(WORKLOAD)

## Create the indexes recommended by seq-scan findings (CONCURRENTLY,
## skips unselective predicates and existing covering indexes, verifies).
tune-index: build
	$(PGTUNE) -apply-index

## Full auto-tune: install pg_stat_statements, apply plan, ANALYZE stale
## statistics. Restart PostgreSQL manually once after the first run (the app
## never restarts the server itself).
tune-restart: build
	$(PGTUNE) -apply -analyze -workload $(WORKLOAD)

## One-shot: config tune + ANALYZE + recommended indexes.
## Restart PostgreSQL manually once if the plan requires it.
tune-full: build
	$(PGTUNE) -apply -analyze -apply-index -workload $(WORKLOAD)

## Show stats and tuning plan without changing anything.
tune-dry: build
	$(PGTUNE) -workload $(WORKLOAD)

## Auto-tune for an analytics/warehouse workload.
tune-olap: build
	$(PGTUNE) -apply -workload olap

## Analyze every database on the server: one instance-level tuning plan
## (evidence aggregated across databases) plus per-database deep dives with
## their own recommendations (indexes, ANALYZE, etc.).
## Best run as a role with pg_read_all_stats: see README "Multiple databases".
tune-all: build
	$(PGTUNE) -all-dbs -user $(PGUSER) -password $(PGPASSWORD) -port $(PGPORT)

## Clear pg_stat_statements so query stats start from zero (as the configured
## superuser; requires pg_stat_statements preloaded).
pgss-reset:
	PGPASSWORD=$(PGPASSWORD) psql -h $(PGHOST) -p $(PGPORT) -U $(PGUSER) -d $(PGDATABASE) -c "SELECT pg_stat_statements_reset();"

## Run the default TPC-B-like read/write workload.
## Initializes the schema first if the pgbench_accounts table is missing.
load: load-init-if-needed
	$(PG_CONN) -c $(CLIENTS) -j $(THREADS) -T $(DURATION) -P $(PROGRESS)

## (Re)initialize the pgbench schema (drops existing pgbench_* tables).
load-init:
	$(PG_CONN) -i -s $(SCALE)

## Run a SELECT-only (read) workload — good for pure pool/latency testing.
load-readonly:
	$(PG_CONN) -S -c $(CLIENTS) -j $(THREADS) -T $(DURATION) -P $(PROGRESS)

## Run a custom pgbench script: make load-custom F=/path/to/script.sql
load-custom:
	test -n "$(F)" || (echo "Usage: make load-custom F=script.sql"; exit 1)
	$(PG_CONN) -f $(F) -c $(CLIENTS) -j $(THREADS) -T $(DURATION) -P $(PROGRESS)

# Check if schema exists; run init only when missing.
load-init-if-needed:
	@if ! PGPASSWORD=$(PGPASSWORD) psql -h $(PGHOST) -p $(PGPORT) -U $(PGUSER) -d $(PGDATABASE) \
		-tAc "SELECT to_regclass('public.pgbench_accounts') IS NOT NULL" | grep -q t; then \
		echo "==> initializing pgbench schema (scale $(SCALE))"; \
		$(PG_CONN) -i -s $(SCALE); \
	else \
		echo "==> pgbench schema already present, skipping init"; \
	fi
