# pgtune repo
#
# Load testing (pgbench) and automatic PostgreSQL tuning (Go app).
#
# Common targets:
#   make help           - list all targets
#   make build          - compile the pgtune binary
#   make tune           - auto-tune: gather stats + apply tuning plan (needs sudo for ALTER SYSTEM)
#   make tune-restart   - full auto-tune incl. pg_stat_statements install + restart + ANALYZE
#   make tune-dry       - gather stats and show the plan without applying
#   make tune-olap      - same as tune but for an analytics workload
#   make load           - run the pgbench read/write benchmark
#   make load-readonly  - SELECT-only benchmark
#   make load-custom F=script.sql - custom pgbench script

# --- pgtune app ---
GO      ?= go
PGTUNE  := ./pgtune
WORKLOAD ?= oltp

# --- load test (pgbench) ---
PGHOST    ?= 127.0.0.1
PGPORT    ?= 6432          # pgbouncer; use 5432 to hit postgres directly
PGUSER    ?= app
PGPASSWORD?= app
PGDATABASE?= app

CLIENTS   ?= 20            # -c  concurrent clients
THREADS   ?= 8             # -j  threads
DURATION  ?= 60            # -T  seconds
SCALE     ?= 10            # -s  benchmark schema scale factor
PROGRESS  ?= 5             # -P  progress report interval (s)

PG_CONN := PGPASSWORD=$(PGPASSWORD) pgbench -h $(PGHOST) -p $(PGPORT) -U $(PGUSER) -d $(PGDATABASE)

.PHONY: help build tune tune-restart tune-dry tune-olap load load-init load-readonly load-custom load-init-if-needed

help:
	@echo "pgtune targets:"
	@echo "  make build          compile the pgtune binary"
	@echo "  make tune           auto-tune postgres (stats + apply, workload=$(WORKLOAD))"
	@echo "  make tune-restart   full auto-tune: pg_stat_statements install + restart + ANALYZE"
	@echo "  make tune-dry       show stats and plan without applying"
	@echo "  make tune-olap      auto-tune for an analytics workload"
	@echo "  make load           run pgbench read/write benchmark (defaults: $(CLIENTS) clients, $(DURATION)s)"
	@echo "  make load-readonly  SELECT-only benchmark"
	@echo "  make load-custom F=script.sql   run a custom pgbench script"

## Compile the pgtune binary.
build:
	$(GO) build -o pgtune .

## Auto-tune: gather stats, apply plan, reload (restarts only if required).
tune: build
	$(PGTUNE) -apply -workload $(WORKLOAD)

## Full auto-tune: install pg_stat_statements, apply plan, ANALYZE stale
## statistics, restart once if required. Use this on a fresh instance.
tune-restart: build
	$(PGTUNE) -apply -restart -analyze -workload $(WORKLOAD)

## Show stats and tuning plan without changing anything.
tune-dry: build
	$(PGTUNE) -workload $(WORKLOAD)

## Auto-tune for an analytics/warehouse workload.
tune-olap: build
	$(PGTUNE) -apply -workload olap

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
