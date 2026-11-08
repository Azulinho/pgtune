package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- countParams -----------------------------------------------------------

func TestCountParams(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"SELECT * FROM t WHERE a = $1", 1},
		{"SELECT $1, $2, $10", 10},
		{"$1 AND $2 AND $2", 2},
		{"SELECT 1", 0},
		{"UPDATE t SET a = $1 WHERE b = $2 AND c = $3", 3},
		{"$1$x", 1}, // non-digit after $ stops the scan
		{"", 0},
	}
	for _, c := range cases {
		if got := countParams(c.in); got != c.want {
			t.Errorf("countParams(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---- splitTable ------------------------------------------------------------

func TestSplitTable(t *testing.T) {
	cases := []struct {
		in, schema, table string
	}{
		{"public.pgbench_accounts", "public", "pgbench_accounts"},
		{"pgbench_accounts", "public", "pgbench_accounts"},
		{"app.orders", "app", "orders"},
		{"a.b.c", "a.b", "c"}, // last dot wins
	}
	for _, c := range cases {
		schema, table := splitTable(c.in)
		if schema != c.schema || table != c.table {
			t.Errorf("splitTable(%q) = (%q,%q), want (%q,%q)", c.in, schema, table, c.schema, c.table)
		}
	}
}

// ---- filterColumn ----------------------------------------------------------

func TestFilterColumn(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"(abalance < 1)", "abalance"},
		{"abalance > 8028", "abalance"},
		{"(t.col BETWEEN 10 AND 20)", "col"},
		{"status = 'pending'", "status"},
		{`t."weird name" = 5`, ""}, // quoted identifiers with spaces unsupported
		{"lower(name) = 'x'", ""},  // expression -> would need an expression index
		{"x::int = 5", ""},         // cast expression
		{"(a = 1 AND b = 2)", "a"}, // AND chain: recommend on the first column
		{"(x LIKE 'a%')", "x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := filterColumn(c.in); got != c.want {
			t.Errorf("filterColumn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- indexDDLForFilter -----------------------------------------------------

func TestIndexDDLForFilter(t *testing.T) {
	ddl := indexDDLForFilter("public.pgbench_accounts", "(abalance < 1)")
	want := `CREATE INDEX CONCURRENTLY IF NOT EXISTS "idx_pgbench_accounts_abalance" ON "public"."pgbench_accounts" ("abalance");`
	if ddl != want {
		t.Errorf("indexDDLForFilter = %q, want %q", ddl, want)
	}
	// unqualified table still gets the public schema in the DDL
	ddl = indexDDLForFilter("pgbench_accounts", "(abalance < 1)")
	if !strings.Contains(ddl, `ON "public"."pgbench_accounts"`) {
		t.Errorf("unqualified DDL = %q, want public schema", ddl)
	}
	// expression filters must not produce a plain index
	if ddl := indexDDLForFilter("public.t", "(lower(x) = 'a')"); ddl != "" {
		t.Errorf("expression filter produced DDL %q, want empty", ddl)
	}
}

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

// ---- indexSelectivity ------------------------------------------------------

func TestIndexSelectivity(t *testing.T) {
	tables := []TableStat{
		{Schema: "public", Table: "pgbench_accounts", Live: 1_000_000},
		{Schema: "public", Table: "other", Live: 10},
	}
	f := &Finding{Table: "public.pgbench_accounts", EstRows: 1968}
	est, total, ok := indexSelectivity(f, tables)
	if !ok || est != 1968 || total != 1_000_000 {
		t.Errorf("indexSelectivity = (%v, %v, %v), want (1968, 1000000, true)", est, total, ok)
	}
	// unknown table -> not ok
	f2 := &Finding{Table: "public.missing", EstRows: 1}
	if _, _, ok := indexSelectivity(f2, tables); ok {
		t.Error("indexSelectivity for unknown table should return ok=false")
	}
	// zero live rows -> not ok (avoid div by zero downstream)
	f3 := &Finding{Table: "public.empty", EstRows: 0}
	tables = append(tables, TableStat{Schema: "public", Table: "empty", Live: 0})
	if _, _, ok := indexSelectivity(f3, tables); ok {
		t.Error("indexSelectivity with Live=0 should return ok=false")
	}
}

// ---- dmlRe -----------------------------------------------------------------

func TestDMLRe(t *testing.T) {
	for _, in := range []string{
		"UPDATE pgbench_branches SET bbalance = bbalance + $1 WHERE bid = $2",
		"  INSERT INTO t VALUES (1)",
		"DELETE FROM t",
		"CREATE INDEX i ON t (a)",
		"ANALYZE t",
	} {
		if !dmlRe.MatchString(in) {
			t.Errorf("dmlRe should match %q", in)
		}
	}
	for _, in := range []string{
		"SELECT count(*) FROM t",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"select a from t",
	} {
		if dmlRe.MatchString(in) {
			t.Errorf("dmlRe should NOT match %q", in)
		}
	}
}

// ---- probe regexes ---------------------------------------------------------

func TestProbeWhereRe(t *testing.T) {
	m := probeWhereRe.FindStringSubmatch(
		"SELECT count(*) FROM pgbench_accounts WHERE abalance > $1")
	if m == nil {
		t.Fatal("probeWhereRe did not match")
	}
	if m[1] != "abalance" || m[2] != ">" || m[3] != "1" {
		t.Errorf("groups = (%q,%q,%q), want (abalance,>,1)", m[1], m[2], m[3])
	}
	// qualified column
	m = probeWhereRe.FindStringSubmatch("SELECT * FROM t WHERE t.a <= $2")
	if m == nil || m[1] != "t.a" || m[2] != "<=" || m[3] != "2" {
		t.Errorf("qualified match = %v", m)
	}
	// literal predicate (no placeholder) must not match
	if m := probeWhereRe.FindStringSubmatch("SELECT * FROM t WHERE a = 5"); m != nil {
		t.Errorf("literal predicate matched: %v", m)
	}
}

func TestProbeBetweenRe(t *testing.T) {
	m := probeBetweenRe.FindStringSubmatch("SELECT * FROM t WHERE x BETWEEN $1 AND $2")
	if m == nil {
		t.Fatal("probeBetweenRe did not match")
	}
	if m[1] != "x" || m[2] != "1" || m[3] != "2" {
		t.Errorf("groups = (%q,%q,%q), want (x,1,2)", m[1], m[2], m[3])
	}
}

func TestProbeFromRe(t *testing.T) {
	m := probeFromRe.FindStringSubmatch("SELECT * FROM public.pgbench_accounts WHERE abalance > $1")
	if m == nil || m[1] != "public.pgbench_accounts" {
		t.Errorf("from match = %v", m)
	}
	m = probeFromRe.FindStringSubmatch("SELECT * FROM pgbench_accounts a WHERE a.balance > $1")
	if m == nil || m[1] != "pgbench_accounts" {
		t.Errorf("unqualified from match = %v", m)
	}
}

func TestProbeNumericRe(t *testing.T) {
	for _, in := range []string{"7705", "-12", "0", "3.14", "-0.5", "1000000"} {
		if !probeNumericRe.MatchString(in) {
			t.Errorf("probeNumericRe should match %q", in)
		}
	}
	for _, in := range []string{"abc", "1e3", "+5", "1,000", ""} {
		if probeNumericRe.MatchString(in) {
			t.Errorf("probeNumericRe should NOT match %q", in)
		}
	}
}

// ---- walkPlan --------------------------------------------------------------

func TestWalkPlanSeqScanWithFilter(t *testing.T) {
	n := jsonPlan{
		NodeType:     "Seq Scan",
		Schema:       "public",
		RelationName: "pgbench_accounts",
		Filter:       "(abalance > 8028)",
		PlanRows:     2161,
	}
	findings := walkPlan(n, TopQuery{QueryID: -42})
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Kind != "seq-scan" || f.QueryID != -42 || f.Table != "public.pgbench_accounts" || f.EstRows != 2161 {
		t.Errorf("unexpected finding: %+v", f)
	}
	if !strings.Contains(f.IndexDDL, "idx_pgbench_accounts_abalance") {
		t.Errorf("finding should carry index DDL, got %q", f.IndexDDL)
	}
}

func TestWalkPlanSeqScanWithoutFilter(t *testing.T) {
	n := jsonPlan{NodeType: "Seq Scan", RelationName: "t"} // no Filter
	if findings := walkPlan(n, TopQuery{}); len(findings) != 0 {
		t.Errorf("full-table scan without filter should not be a finding: %+v", findings)
	}
}

func TestWalkPlanDiskSort(t *testing.T) {
	n := jsonPlan{
		NodeType:      "Sort",
		SortMethod:    "external merge",
		SortSpaceUsed: 17624, // kB per EXPLAIN
		SortSpaceType: "Disk",
	}
	findings := walkPlan(n, TopQuery{})
	if len(findings) != 1 || findings[0].Kind != "disk-sort" {
		t.Fatalf("got %+v, want one disk-sort finding", findings)
	}
	if want := uint64(17624) * 1024; findings[0].Bytes != want {
		t.Errorf("disk-sort Bytes = %d, want %d (kB -> bytes)", findings[0].Bytes, want)
	}
}

func TestWalkPlanTempSpill(t *testing.T) {
	n := jsonPlan{NodeType: "Hash Join", TempWrittenBlocks: 2204} // 2204 x 8kB
	findings := walkPlan(n, TopQuery{})
	if len(findings) != 1 || findings[0].Kind != "temp-spill" {
		t.Fatalf("got %+v, want one temp-spill finding", findings)
	}
	if want := uint64(2204) * 8192; findings[0].Bytes != want {
		t.Errorf("temp-spill Bytes = %d, want %d", findings[0].Bytes, want)
	}
}

func TestWalkPlanEstimateSkew(t *testing.T) {
	// actual >> estimate
	n := jsonPlan{NodeType: "Seq Scan", PlanRows: 1000, ActualRows: 50000}
	findings := walkPlan(n, TopQuery{})
	if len(findings) != 1 || findings[0].Kind != "estimate-skew" {
		t.Fatalf("got %+v, want one estimate-skew finding", findings)
	}
	// estimate >> actual
	n2 := jsonPlan{NodeType: "Seq Scan", PlanRows: 500000, ActualRows: 100}
	findings = walkPlan(n2, TopQuery{})
	if len(findings) != 1 || findings[0].Kind != "estimate-skew" {
		t.Fatalf("got %+v, want one estimate-skew finding", findings)
	}
	// within 10x -> no finding
	n3 := jsonPlan{NodeType: "Seq Scan", PlanRows: 10000, ActualRows: 20000}
	if findings := walkPlan(n3, TopQuery{}); len(findings) != 0 {
		t.Errorf("10x-ish estimates should not be a finding: %+v", findings)
	}
	// small actual row counts are exempt (avoid noise on tiny scans)
	n4 := jsonPlan{NodeType: "Seq Scan", PlanRows: 10, ActualRows: 50}
	if findings := walkPlan(n4, TopQuery{}); len(findings) != 0 {
		t.Errorf("small scan should be exempt from estimate-skew: %+v", findings)
	}
}

func TestWalkPlanRecurses(t *testing.T) {
	// parent node has no issue itself, but the child seq scan does
	n := jsonPlan{
		NodeType: "Aggregate",
		Plans: []jsonPlan{{
			NodeType:     "Seq Scan",
			RelationName: "accounts",
			Filter:       "(balance < 0)",
			PlanRows:     50,
		}},
	}
	findings := walkPlan(n, TopQuery{})
	if len(findings) != 1 || findings[0].Kind != "seq-scan" {
		t.Fatalf("recursion failed: %+v", findings)
	}
}

func TestWalkPlanFromRealExplainJSON(t *testing.T) {
	// a realistic EXPLAIN (FORMAT JSON) blob as the server would return it
	raw := `[{"Plan": {
		"Node Type": "Aggregate",
		"Strategy": "Plain",
		"Plans": [{
			"Node Type": "Seq Scan",
			"Relation Name": "pgbench_accounts",
			"Schema": "public",
			"Alias": "pgbench_accounts",
			"Filter": "(abalance > 8028)",
			"Plan Rows": 2161,
			"Actual Rows": 0,
			"Actual Total Time": 210.5,
			"Shared Hit Blocks": 5,
			"Shared Read Blocks": 190
		}]
	}}]`
	var expl []jsonExplain
	if err := json.Unmarshal([]byte(raw), &expl); err != nil {
		t.Fatal(err)
	}
	findings := walkPlan(expl[0].Plan, TopQuery{QueryID: -7})
	if len(findings) != 1 || findings[0].Kind != "seq-scan" {
		t.Fatalf("got %+v, want one seq-scan finding", findings)
	}
	if findings[0].EstRows != 2161 {
		t.Errorf("EstRows = %d, want 2161", findings[0].EstRows)
	}
}

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
		{"data-modifying cte delete", TopQuery{UserID: me, Query: "WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d"}, me, ExplainPlanOnly},
		{"data-modifying cte update", TopQuery{UserID: me, Query: "WITH u AS (UPDATE t SET a = 1 RETURNING *) SELECT count(*) FROM u"}, me, ExplainPlanOnly},
		{"select into", TopQuery{UserID: me, Query: "SELECT * INTO new_t FROM t"}, me, ExplainPlanOnly},
		{"locking clause with comment", TopQuery{UserID: me, Query: "SELECT * FROM t FOR /* lock */ UPDATE"}, me, ExplainPlanOnly},
		{"line comment", TopQuery{UserID: me, Query: "SELECT 1 -- note"}, me, ExplainPlanOnly},
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
