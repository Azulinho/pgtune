package main

import (
	"strings"
	"testing"
)

// ---- ddlColumn -------------------------------------------------------------

func TestDDLColumn(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pgbench_accounts_abalance ON public.pgbench_accounts (abalance);", "abalance"},
		{"CREATE INDEX idx ON public.t (a, b);", "a, b"},
		{"CREATE INDEX idx ON public.t (\"weird\");", "weird"},
		{"no parens here", ""},
	}
	for _, c := range cases {
		if got := ddlColumn(c.in); got != c.want {
			t.Errorf("ddlColumn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- idxLeadingCol ---------------------------------------------------------

func TestIdxLeadingCol(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"CREATE INDEX idx ON public.t USING btree (a)", "a"},
		{"CREATE INDEX idx ON public.t USING btree (a, b DESC)", "a"},
		{"CREATE INDEX idx ON public.t USING btree (a text_pattern_ops)", "a"},
		{`CREATE INDEX idx ON public.t USING btree ("weird")`, "weird"},
		{"CREATE INDEX idx ON public.t USING btree (lower(x))", "lower(x"}, // expression: first ')' cuts it; caller compares to plain col -> no match
		{"garbage without parens", ""},
	}
	for _, c := range cases {
		if got := idxLeadingCol(c.in); got != c.want {
			t.Errorf("idxLeadingCol(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- tableWriteHeavy -------------------------------------------------------

func TestTableWriteHeavy(t *testing.T) {
	tables := []TableStat{
		{Schema: "public", Table: "accounts", Live: 1000, Upd: 600}, // updates > half the table
		{Schema: "public", Table: "orders", Live: 1000, Upd: 100},   // light
		{Schema: "public", Table: "empty", Live: 0, Upd: 50},        // no live rows -> false
		{Schema: "other", Table: "accounts", Live: 1000, Upd: 900},  // different schema
	}
	cases := []struct {
		table string
		want  bool
	}{
		{"public.accounts", true},
		{"public.orders", false},
		{"public.empty", false},
		{"public.missing", false},
		{"other.accounts", true}, // same-named table in another schema
	}
	for _, c := range cases {
		f := &Finding{Table: c.table}
		if got := tableWriteHeavy(f, tables); got != c.want {
			t.Errorf("tableWriteHeavy(%q) = %v, want %v", c.table, got, c.want)
		}
	}
}

// ---- topQueryByID ----------------------------------------------------------

func TestTopQueryByID(t *testing.T) {
	qa := &QueryAnalysis{Top: []TopQuery{
		{QueryID: 1, Query: "a"},
		{QueryID: 2, Query: "b"},
	}}
	q := topQueryByID(qa, 2)
	if q == nil || q.Query != "b" {
		t.Errorf("topQueryByID(2) = %+v, want query b", q)
	}
	if q := topQueryByID(qa, 99); q != nil {
		t.Errorf("topQueryByID(99) = %+v, want nil", q)
	}
	if q := topQueryByID(nil, 1); q != nil {
		t.Errorf("topQueryByID(nil) = %+v, want nil", q)
	}
}

// ---- coveringIndex helper (idxLeadingCol round trip) -----------------------

func TestIndexDDLRoundTrip(t *testing.T) {
	// the DDL generated for a finding must parse back to the same column
	ddl := indexDDLForFilter("public.pgbench_accounts", "(abalance < 1)")
	if ddlColumn(ddl) != "abalance" {
		t.Errorf("round trip failed: %q -> %q", ddl, ddlColumn(ddl))
	}
	if !strings.Contains(ddl, "CONCURRENTLY IF NOT EXISTS") {
		t.Errorf("DDL should be safe to re-run: %q", ddl)
	}
}

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
