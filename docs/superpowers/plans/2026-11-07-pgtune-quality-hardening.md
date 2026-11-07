# pgtune Quality Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make pgtune safe for strangers to run as a superuser on production servers, and leave a regression test for every failure mode fixed.

**Architecture:** Localized changes in the existing `main` package: a pure EXPLAIN-policy classifier, identifier-safe DDL generation, a URL-based connection builder, separate per-phase budgets, and a DB-backed integration test. No package restructuring.

**Tech Stack:** Go 1.25, pgx v5, PostgreSQL 13+, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-11-07-pgtune-quality-hardening-design.md`

## Global Constraints

- Keep the single `main` package and existing file layout.
- Go 1.25, pgx v5, Linux, PostgreSQL 13+.
- Do not add runtime dependencies.
- Every behavior change gets a test that was watched fail before the code changed.
- Run all Go commands from `src/` unless a step says otherwise.

## Review Focus

- A non-superuser role's query in `pg_stat_statements` must never be executed as the connected role.
- `CREATE INDEX` DDL must stay valid for mixed-case, reserved-word, dotted, and over-63-byte identifiers.
- A cancelled `CREATE INDEX CONCURRENTLY` must tell the operator how to find and drop the invalid index.
- Passwords, database names, unix-socket paths, and IPv6 hosts must round-trip through the connection builder.
- The integration test must run real SQL against PostgreSQL, not a mock.

---

### Task 1: Never execute another role's statements

**Files:**
- Modify: `src/queries.go`
- Modify: `src/queries_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: existing `TopQuery`, `QueryAnalysis`, `dmlRe`.
- Produces: `type ExplainMode int` with `ExplainSkip`, `ExplainPlanOnly`, `ExplainAnalyze`; `func explainModeFor(q TopQuery, currentUserID int64) ExplainMode`; new `TopQuery.UserID int64` field; `explainTopQueries(ctx, conn, qa, opts, currentUserID int64) error`.

- [ ] **Step 1: Write the failing tests**

Add to `src/queries_test.go`:

```go
func TestExplainModeFor(t *testing.T) {
	const me, other = int64(10), int64(20)
	cases := []struct {
		name string
		q    TopQuery
		user int64
		want ExplainMode
	}{
		{"owned select", TopQuery{UserID: me, Query: "SELECT 1"}, me, ExplainAnalyze},
		{"other role select", TopQuery{UserID: other, Query: "SELECT 1"}, me, ExplainPlanOnly},
		{"unknown current role", TopQuery{UserID: other, Query: "SELECT 1"}, 0, ExplainPlanOnly},
		{"owned cte", TopQuery{UserID: me, Query: "WITH x AS (SELECT 1) SELECT * FROM x"}, me, ExplainAnalyze},
		{"owned for update", TopQuery{UserID: me, Query: "SELECT * FROM t FOR UPDATE"}, me, ExplainPlanOnly},
		{"owned for share", TopQuery{UserID: me, Query: "SELECT * FROM t FOR SHARE"}, me, ExplainPlanOnly},
		{"owned for no key update", TopQuery{UserID: me, Query: "SELECT * FROM t FOR NO KEY UPDATE"}, me, ExplainPlanOnly},
		{"owned for key share", TopQuery{UserID: me, Query: "SELECT * FROM t FOR KEY SHARE"}, me, ExplainPlanOnly},
		{"insert", TopQuery{UserID: me, Query: "INSERT INTO t VALUES (1)"}, me, ExplainSkip},
		{"update", TopQuery{UserID: me, Query: "UPDATE t SET a = 1"}, me, ExplainSkip},
		{"show", TopQuery{UserID: me, Query: "SHOW work_mem"}, me, ExplainSkip},
		{"empty", TopQuery{UserID: me, Query: "  ; "}, me, ExplainSkip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := explainModeFor(c.q, c.user); got != c.want {
				t.Errorf("explainModeFor(%q, %d) = %v, want %v", c.q.Query, c.user, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run TestExplainModeFor ./...`
Expected: FAIL — `undefined: ExplainMode`, `undefined: explainModeFor`, unknown field `UserID`.

- [ ] **Step 3: Implement the policy and wire it in**

In `src/queries.go`:

1. Add `UserID int64` to `TopQuery`.
2. Add:

```go
type ExplainMode int

const (
	ExplainSkip ExplainMode = iota
	ExplainPlanOnly
	ExplainAnalyze
)

var lockingClauseRe = regexp.MustCompile(`(?i)\bfor\s+(?:(?:no\s+)?key\s+)?(?:update|share)\b`)

// explainModeFor decides how a captured statement may be explored. Only
// statements run by the connected role are executed; everything else gets a
// plan-only EXPLAIN. DML and non-SELECT statements are skipped, and locking
// clauses are never executed, regardless of ownership.
func explainModeFor(q TopQuery, currentUserID int64) ExplainMode {
	sql := strings.TrimRight(strings.TrimSpace(q.Query), "; \t\n")
	if sql == "" || dmlRe.MatchString(sql) {
		return ExplainSkip
	}
	low := strings.ToLower(sql)
	if !(strings.HasPrefix(low, "select") || strings.HasPrefix(low, "with")) {
		return ExplainSkip
	}
	if lockingClauseRe.MatchString(sql) {
		return ExplainPlanOnly
	}
	if currentUserID == 0 || q.UserID != currentUserID {
		return ExplainPlanOnly
	}
	return ExplainAnalyze
}
```

3. In `collectQueryAnalysis`, select `COALESCE(s.userid,0)::bigint` as the first column and scan it into `t.UserID`.
4. Also in `collectQueryAnalysis`, read the connected role's OID:

```go
var currentUserID int64
if err := conn.QueryRow(ctx,
	`SELECT oid::bigint FROM pg_roles WHERE rolname = current_user`).Scan(&currentUserID); err != nil {
	currentUserID = 0 // conservative: never execute anything
}
```

5. Change the call to `explainTopQueries(ctx, conn, qa, opts, currentUserID)` and its signature.
6. In the loop, replace the `q.HasParams` branch with `mode := explainModeFor(*q, currentUserID)`:
   - `ExplainSkip`: `continue`.
   - `ExplainPlanOnly`: the existing plain `EXPLAIN (FORMAT JSON)` path, passing probe args when `q.HasParams`.
   - `ExplainAnalyze`: the existing transaction + `SET LOCAL statement_timeout` path.
7. Count plan-only statements. After the loop, append a note:

```go
if planOnly > 0 {
	note := fmt.Sprintf("%d statement(s) from other roles were planned only (never executed as the connected role)", planOnly)
	if qa.Note != "" {
		qa.Note += "; " + note
	} else {
		qa.Note = note
	}
}
```

Keep the existing `done` limit and error handling. In `README.md`, under "Look at queries", replace the `EXPLAIN (ANALYZE, BUFFERS)` sentence with: "It runs `EXPLAIN (ANALYZE, BUFFERS)` only on statements the connected role ran. Statements from other roles get a plain `EXPLAIN` and are never executed. `FOR UPDATE`/`FOR SHARE` statements are never executed either."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestExplainModeFor|TestDMLRe|TestWalkPlan' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/queries.go src/queries_test.go README.md
git commit -m "fix: never execute other roles' statements under EXPLAIN ANALYZE"
```

---

### Task 2: Quote identifiers and match table schemas correctly

**Files:**
- Modify: `src/queries.go`
- Modify: `src/apply.go`
- Modify: `src/queries_test.go`
- Modify: `src/apply_test.go`

**Interfaces:**
- Consumes: existing `splitTable`, `filterColumn`.
- Produces: `func quoteIdent(parts ...string) string`; `func indexName(table, col string) string`; `indexDDLForFilter` now emits quoted identifiers.

- [ ] **Step 1: Update and add failing tests**

In `src/queries_test.go`:
- Change `TestIndexDDLForFilter`'s expected string to:

```go
want := `CREATE INDEX CONCURRENTLY IF NOT EXISTS "idx_pgbench_accounts_abalance" ON "public"."pgbench_accounts" ("abalance");`
```

- Add:

```go
func TestIndexDDLQuotesSpecialIdentifiers(t *testing.T) {
	ddl := indexDDLForFilter("app.my-table", "(status = 'x')")
	for _, want := range []string{`"app"."my-table"`, `("status")`, `"idx_my-table_status"`} {
		if !strings.Contains(ddl, want) {
			t.Errorf("indexDDLForFilter = %q, want it to contain %q", ddl, want)
		}
	}
}

func TestIndexNameFitsPostgresLimit(t *testing.T) {
	name := indexName(strings.Repeat("t", 80), strings.Repeat("c", 80))
	if len(name) > 63 {
		t.Errorf("indexName length = %d, want <= 63 (%q)", len(name), name)
	}
}

func TestIndexSelectivityNonPublicSchema(t *testing.T) {
	tables := []TableStat{{Schema: "app", Table: "orders", Live: 1000}}
	f := &Finding{Table: "app.orders", EstRows: 20}
	est, total, ok := indexSelectivity(f, tables)
	if !ok || est != 20 || total != 1000 {
		t.Errorf("indexSelectivity = (%v, %v, %v), want (20, 1000, true)", est, total, ok)
	}
}
```

In `src/apply_test.go`, change the `{"other.accounts", false}` case in `TestTableWriteHeavy` to expect `true`, because `Schema: "other"` is a real match once the bug is fixed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestIndexDDLForFilter|TestIndexDDLQuotesSpecialIdentifiers|TestIndexNameFitsPostgresLimit|TestIndexSelectivity|TestTableWriteHeavy|TestIndexDDLRoundTrip' ./...`
Expected: FAIL — unquoted DDL, missing `indexName`, and `other.accounts` reported false.

- [ ] **Step 3: Implement quoting and schema matching**

In `src/queries.go`:

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

func quoteIdent(parts ...string) string {
	return pgx.Identifier(parts).Sanitize()
}

// indexName keeps generated names within PostgreSQL's 63-byte identifier
// limit and appends a short hash when truncation is needed.
func indexName(table, col string) string {
	name := "idx_" + table + "_" + col
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:4])
	prefix := name[:63-len(suffix)]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + suffix
}
```

Replace `indexDDLForFilter`'s `fmt.Sprintf` with:

```go
return fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s);",
	quoteIdent(indexName(table, col)), quoteIdent(schema, table), quoteIdent(col))
```

In `indexSelectivity` and `tableWriteHeavy`, match `t.Schema == schema && t.Table == table` instead of calling `splitTable(t.Table)`.

In `src/apply.go`, quote the ANALYZE target:

```go
conn.Exec(ctx, fmt.Sprintf("ANALYZE %s;", quoteIdent(schema, table)))
```

- [ ] **Step 4: Run the full unit suite**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/queries.go src/queries_test.go src/apply.go src/apply_test.go
git commit -m "fix: quote generated identifiers and match non-public schemas"
```

---

### Task 3: Build connection strings without hand-escaping, and split phase budgets

**Files:**
- Create: `src/conn.go`
- Create: `src/conn_test.go`
- Create: `src/main_test.go`
- Modify: `src/main.go`
- Modify: `src/alldbs.go`
- Modify: `src/tune.go`
- Modify: `src/tune_test.go`

**Interfaces:**
- Consumes: `Options`, `dsnFor` callers.
- Produces: `func buildDSN(opts Options, db string) string`; `func opContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)`.

- [ ] **Step 1: Write the failing tests**

Create `src/conn_test.go`:

```go
package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBuildDSNRoundTrip(t *testing.T) {
	opts := Options{
		User:     "app user",
		Password: "p a+s s/word@x",
		Host:     "/var/run/postgresql",
		Port:     5433,
		SslMode:  "require",
	}
	cfg, err := pgx.ParseConfig(buildDSN(opts, "my db"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.User != opts.User {
		t.Errorf("user = %q, want %q", cfg.User, opts.User)
	}
	if cfg.Password != opts.Password {
		t.Errorf("password = %q, want %q", cfg.Password, opts.Password)
	}
	if cfg.Host != opts.Host {
		t.Errorf("host = %q, want %q", cfg.Host, opts.Host)
	}
	if cfg.Port != 5433 {
		t.Errorf("port = %d, want 5433", cfg.Port)
	}
	if cfg.Database != "my db" {
		t.Errorf("database = %q, want %q", cfg.Database, "my db")
	}
}

func TestBuildDSNIPv6(t *testing.T) {
	cfg, err := pgx.ParseConfig(buildDSN(Options{User: "u", Host: "::1", Port: 5432}, "postgres"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Host != "::1" {
		t.Errorf("host = %q, want ::1", cfg.Host)
	}
}
```

Create `src/main_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestOpContextZeroHasNoDeadline(t *testing.T) {
	ctx, cancel := opContext(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("opContext(0) should not impose a deadline")
	}
}

func TestOpContextPositiveHasDeadline(t *testing.T) {
	ctx, cancel := opContext(context.Background(), time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Error("opContext(time.Second) should impose a deadline")
	}
}
```

Add to `src/tune_test.go`:

```go
func TestBuildPlanTinyRAMSpillDoesNotOvercommit(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 128 * MiB, NumCPU: 2, DiskType: "ssd"}
	db := mkDB(mkSetting("work_mem", "4096", "kB", "user"))
	qa := &QueryAnalysis{Findings: []Finding{{Kind: "temp-spill", Bytes: 1 * GiB}}}
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	it := planItem(t, plan, "work_mem")
	if it.Proposed != "16MB" {
		t.Errorf("work_mem proposed = %q, want 16MB when RAM leaves no spare budget", it.Proposed)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestBuildDSN|TestOpContext|TestBuildPlanTinyRAM' ./...`
Expected: FAIL — `undefined: buildDSN`, `undefined: opContext`, and `work_mem` proposed as `64MB`.

- [ ] **Step 3: Implement**

Create `src/conn.go`:

```go
package main

import (
	"net/url"
	"strconv"
)

// buildDSN builds a libpq URL without hand-escaping values: url.UserPassword
// and url.Values encode userinfo and settings correctly. Host and port travel
// as query parameters so unix socket paths and IPv6 literals work.
func buildDSN(opts Options, db string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(opts.User, opts.Password),
		Path:   "/" + db,
	}
	q := url.Values{}
	if opts.Host != "" {
		q.Set("host", opts.Host)
	}
	if opts.Port > 0 {
		q.Set("port", strconv.Itoa(opts.Port))
	}
	if opts.SslMode != "" {
		q.Set("sslmode", opts.SslMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
```

In `src/alldbs.go`, make `dsnFor` delegate: `func dsnFor(opts Options, db string) string { return buildDSN(opts, db) }` and remove the now-unused `net/url` import.

In `src/main.go`, replace the inline DSN with `dsn := dsnFor(opts, opts.DB)` and remove the `net/url` import. Add:

```go
// opContext applies an operation budget; d <= 0 means no deadline.
func opContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, d)
}
```

Replace the single `dctx` with three independent budgets: one around `collectSysStats`, one around `collectDBStats`, and one around `collectQueryAnalysis`. Keep `opts.Timeout` validation.

In `src/tune.go`, guard the hard-cap subtraction:

```go
hardCap := uint64(0)
if ram > shared+mwm {
	hardCap = (ram - shared - mwm) / uint64(maxConn+int(avWorkers))
}
if hardCap > 64*MiB {
	hardCap = 64 * MiB
}
```

- [ ] **Step 4: Run the full unit suite**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/conn.go src/conn_test.go src/main.go src/main_test.go src/alldbs.go src/tune.go src/tune_test.go
git commit -m "fix: escape connection values correctly and budget each collection phase"
```

---

### Task 4: Give index builds their own budget and report invalid indexes

**Files:**
- Modify: `src/main.go`
- Modify: `src/apply.go`
- Modify: `src/apply_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `opContext`, `quoteIdent`, existing `applyIndexes`.
- Produces: new `Options.IndexTimeout time.Duration`; `func indexNameFromDDL(ddl string) string`.

- [ ] **Step 1: Write the failing test**

Add to `src/apply_test.go`:

```go
func TestIndexNameFromDDL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`CREATE INDEX CONCURRENTLY IF NOT EXISTS "idx_t_a" ON "public"."t" ("a");`, "idx_t_a"},
		{`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_t_a ON public.t (a);`, "idx_t_a"},
		{"garbage", ""},
	}
	for _, c := range cases {
		if got := indexNameFromDDL(c.in); got != c.want {
			t.Errorf("indexNameFromDDL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestIndexNameFromDDL ./...`
Expected: FAIL — `undefined: indexNameFromDDL`.

- [ ] **Step 3: Implement**

In `src/main.go`:
- Add `IndexTimeout time.Duration` to `Options`.
- Add a flag: `flag.IntVar(&indexTimeoutSec, "index-timeout", 0, "seconds allowed per CREATE INDEX (0 = no limit)")`.
- Set `opts.IndexTimeout = time.Duration(indexTimeoutSec) * time.Second`.
- In both `-apply-index` call sites (single DB and `runAllDBs`), use `opContext(ctx, opts.IndexTimeout)` instead of `opts.Timeout`.

In `src/apply.go`, add:

```go
var indexNameRe = regexp.MustCompile(`(?i)^CREATE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)`)

func indexNameFromDDL(ddl string) string {
	m := indexNameRe.FindStringSubmatch(strings.TrimSpace(ddl))
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"`)
}
```

On a failed `CREATE INDEX`, after the existing warning, add:

```go
if name := indexNameFromDDL(f.IndexDDL); name != "" {
	fmt.Printf("  a cancelled build can leave an INVALID index; check and drop it with:\n  DROP INDEX CONCURRENTLY IF EXISTS %s;\n", quoteIdent(schema, name))
}
```

In `README.md` Options block, add `-index-timeout int   seconds allowed per CREATE INDEX, 0 = no limit (default 0)`.

- [ ] **Step 4: Run the full unit suite**

Run: `go test -count=1 ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add src/main.go src/apply.go src/apply_test.go README.md
git commit -m "fix: give index builds their own timeout and report invalid indexes"
```

---

### Task 5: Strengthen CI and dependency updates

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `.github/dependabot.yml`

**Interfaces:**
- Consumes: existing CI job under `working-directory: src`.
- Produces: gofmt, race, and gomod Dependabot coverage.

- [ ] **Step 1: Update CI**

In `.github/workflows/ci.yml`, add a formatting step before `vet` and run tests with the race detector:

```yaml
      - name: gofmt
        run: test -z "$(gofmt -l .)"
      - name: vet
        run: go vet ./...
      - name: test
        run: go test -race -count=1 ./...
      - name: build
        run: go build -o /dev/null ./...
```

- [ ] **Step 2: Update Dependabot**

Append to `.github/dependabot.yml`:

```yaml
  - package-ecosystem: gomod
    directory: /src
    schedule:
      interval: weekly
```

- [ ] **Step 3: Verify locally**

Run from `src/`: `gofmt -l .`
Expected: no output.

Run from `src/`: `go vet ./... && go test -race -count=1 ./...`
Expected: PASS.

Run from the repository root: `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yml")); yaml.safe_load(open(".github/dependabot.yml")); print("yaml ok")'`
Expected: `yaml ok`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml .github/dependabot.yml
git commit -m "ci: check formatting, run tests with -race, track Go modules"
```

---

### Task 6: Add a real-PostgreSQL integration test

**Files:**
- Create: `src/integration_test.go`
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `collectDBStats`, `collectQueryAnalysis`, `Options`.
- Produces: `TestIntegrationCollectStats`, gated by `PGTUNE_TEST_DSN`.

- [ ] **Step 1: Write the integration test**

Create `src/integration_test.go`:

```go
package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIntegrationCollectStats(t *testing.T) {
	dsn := os.Getenv("PGTUNE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGTUNE_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	db, err := collectDBStats(ctx, conn)
	if err != nil {
		t.Fatalf("collectDBStats: %v", err)
	}
	if db.Version == "" || db.Database == "" {
		t.Fatalf("incomplete stats: version=%q database=%q", db.Version, db.Database)
	}
	if len(db.Settings) == 0 {
		t.Fatal("no pg_settings rows collected")
	}

	opts := Options{TopN: 5, ExplainN: 0, LongRunningSec: 5, TablesN: 5}
	qa, err := collectQueryAnalysis(ctx, conn, opts, db)
	if err != nil {
		t.Fatalf("collectQueryAnalysis: %v", err)
	}
	if qa == nil {
		t.Fatal("collectQueryAnalysis returned nil")
	}
}
```

- [ ] **Step 2: Verify the test skips without a DSN**

Run: `go test -run TestIntegrationCollectStats -v ./...`
Expected: SKIP with `PGTUNE_TEST_DSN not set`.

- [ ] **Step 3: Run it against a live PostgreSQL**

Build the test binary and run it as the `postgres` OS user over the local unix socket:

```bash
go test -c -o /tmp/pgtune-integration.test .
sudo -u postgres env PGTUNE_TEST_DSN='postgres:///postgres?host=/var/run/postgresql' \
  /tmp/pgtune-integration.test -test.run TestIntegrationCollectStats -test.v
```

Expected: PASS. If the local server differs, use any DSN that the test process can connect with.

- [ ] **Step 4: Add the CI service-container job**

Append to `.github/workflows/ci.yml`:

```yaml
  integration:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: src
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_PASSWORD: postgres
        ports:
          - 5432:5432
        options: >-
          --health-cmd pg_isready
          --health-interval 5s
          --health-timeout 5s
          --health-retries 10
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v7.0.0
        with:
          go-version: "1.25"
      - name: integration test
        env:
          PGTUNE_TEST_DSN: postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
        run: go test -count=1 -run TestIntegrationCollectStats -v ./...
```

- [ ] **Step 5: Verify YAML and commit**

Run from the repository root: `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/ci.yml")); print("yaml ok")'`
Expected: `yaml ok`.

```bash
git add src/integration_test.go .github/workflows/ci.yml
git commit -m "test: exercise stats collection against real PostgreSQL"
```

---

## Final verification

Run from `src/`:

```bash
gofmt -l .
go vet ./...
go test -race -count=1 ./...
go build -o /dev/null ./...
```

Expected: no gofmt output, no vet findings, all tests pass, build succeeds.
