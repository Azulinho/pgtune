package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
)

// sudoPSQL runs statements as the postgres superuser via passwordless sudo.
// db is the database to connect to ("" = default).
func sudoPSQL(ctx context.Context, db string, stmts ...string) error {
	args := []string{"-n", "-u", "postgres", "psql", "-v", "ON_ERROR_STOP=1", "-q"}
	if db != "" {
		args = append(args, "-d", db)
	}
	for _, s := range stmts {
		args = append(args, "-c", s)
	}
	cmd := exec.CommandContext(ctx, "sudo", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo -u postgres psql failed: %v\n%s", err, out)
	}
	return nil
}

// applyIndexes creates the indexes recommended by seq-scan findings. Only
// selective predicates are applied (filter matches <= 10% of the table);
// existing covering indexes are skipped. After creation it ANALYZEs the table
// and re-runs EXPLAIN to verify the planner actually switched to the index.
func applyIndexes(ctx context.Context, conn *pgx.Conn, opts Options, qa *QueryAnalysis) error {
	if qa == nil {
		return nil
	}
	var toCreate []*Finding
	seen := map[string]bool{}
	for i := range qa.Findings {
		f := &qa.Findings[i]
		if f.Kind != "seq-scan" || f.IndexDDL == "" || seen[f.IndexDDL] {
			continue
		}
		seen[f.IndexDDL] = true
		if est, total, ok := indexSelectivity(f, qa.Tables); ok && total > 0 && est/total > 0.10 {
			fmt.Printf("skip index on %s: filter matches %.1f%% of the table — not selective enough\n", f.Table, est/total*100)
			continue
		}
		toCreate = append(toCreate, f)
	}
	if len(toCreate) == 0 {
		fmt.Println("No index recommendations to apply.")
		return nil
	}
	for _, f := range toCreate {
		schema, table := splitTable(f.Table)
		col := ddlColumn(f.IndexDDL)
		if col == "" {
			continue
		}
		if name, ok := coveringIndex(ctx, conn, schema, table, col); ok {
			fmt.Printf("index on %s.%s (%s) already covered by %s — skipping\n", schema, table, col, name)
			continue
		}
		if writeHeavy := tableWriteHeavy(f, qa.Tables); writeHeavy {
			fmt.Printf("warning: %s.%s is write-heavy — the new index adds per-row UPDATE/DELETE overhead\n", schema, table)
		}
		if !opts.UseSudo {
			fmt.Printf("apply with -sudo (or run manually): %s\n", f.IndexDDL)
			continue
		}
		if err := sudoPSQL(ctx, opts.DB, f.IndexDDL); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
		fmt.Printf("created index on %s.%s (%s)\n", schema, table, col)
		// refresh planner statistics so the new path is considered accurately
		if err := sudoPSQL(ctx, opts.DB, fmt.Sprintf("ANALYZE %s.%s;", schema, table)); err != nil {
			fmt.Printf("warning: ANALYZE %s.%s failed: %v\n", schema, table, err)
		}
		if q := topQueryByID(qa, f.QueryID); q != nil {
			fmt.Printf("  verify: %s\n", verifyIndexPlan(ctx, conn, *q, schema, table))
		}
	}
	return nil
}

// tableWriteHeavy reports whether a table has seen significant UPDATE/DELETE
// traffic relative to its live row count (indexes add write amplification).
func tableWriteHeavy(f *Finding, tables []TableStat) bool {
	schema, table := splitTable(f.Table)
	for _, t := range tables {
		ts, tt := splitTable(t.Table)
		if ts == schema && tt == table {
			if t.Live <= 0 {
				return false
			}
			return t.Upd+t.Del > t.Live/2
		}
	}
	return false
}

// ddlColumn extracts the indexed column from a generated CREATE INDEX DDL.
func ddlColumn(ddl string) string {
	i := strings.LastIndex(ddl, "(")
	if i < 0 {
		return ""
	}
	col := strings.TrimSpace(ddl[i+1:])
	col = strings.TrimSuffix(col, ");")
	return strings.Trim(strings.TrimSpace(col), `"`)
}

// coveringIndex reports the name of an existing index whose first column is
// col (best-effort parse of pg_indexes.indexdef).
func coveringIndex(ctx context.Context, conn *pgx.Conn, schema, table, col string) (string, bool) {
	rows, err := conn.Query(ctx,
		`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`,
		schema, table)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return "", false
		}
		if idxLeadingCol(def) == col {
			return name, true
		}
	}
	return "", false
}

// idxLeadingCol extracts the leading indexed column from an indexdef such as
// "... USING btree (col1, col2)" (strips opclasses and sort directions).
func idxLeadingCol(def string) string {
	i := strings.Index(def, " (")
	if i < 0 {
		return ""
	}
	rest := def[i+2:]
	if j := strings.IndexByte(rest, ')'); j >= 0 {
		rest = rest[:j]
	}
	col := strings.TrimSpace(strings.Split(rest, ",")[0])
	if k := strings.IndexAny(col, " \t"); k >= 0 {
		col = col[:k]
	}
	return strings.Trim(col, `"`)
}

// verifyIndexPlan re-runs EXPLAIN (FORMAT JSON, no execution) on a query after
// index creation and reports whether the filtered seq scan is gone. Plain
// EXPLAIN with bound values is used so the planner sees the real constants
// (GENERIC_PLAN would assume ~1/3 selectivity and may stick to a seq scan).
func verifyIndexPlan(ctx context.Context, conn *pgx.Conn, q TopQuery, schema, table string) string {
	args := make([]any, countParams(q.Query))
	for i := range args {
		args[i] = 1
	}
	if probe, ok := probeValues(ctx, conn, q.Query); ok {
		for k, v := range probe {
			if k >= 1 && k <= len(args) {
				args[k-1] = v
			}
		}
	}
	rows, err := conn.Query(ctx, "EXPLAIN (FORMAT JSON) "+q.Query, args...)
	if err != nil {
		return fmt.Sprintf("EXPLAIN failed: %v", err)
	}
	defer rows.Close()
	var raw string
	if !rows.Next() {
		return "no plan returned"
	}
	if err := rows.Scan(&raw); err != nil {
		return fmt.Sprintf("EXPLAIN scan failed: %v", err)
	}
	var jr []jsonExplain
	if err := json.Unmarshal([]byte(raw), &jr); err != nil || len(jr) == 0 {
		return "unparsable plan"
	}
	seqStill, indexUsed := false, ""
	var walk func(n jsonPlan)
	walk = func(n jsonPlan) {
		if (n.NodeType == "Seq Scan" || n.NodeType == "Parallel Seq Scan") && n.RelationName == table {
			if n.Filter != "" {
				seqStill = true
			}
		}
		if n.IndexName != "" && n.RelationName == table && indexUsed == "" {
			indexUsed = n.IndexName
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(jr[0].Plan)
	switch {
	case indexUsed != "" && !seqStill:
		return "plan now uses index " + indexUsed
	case indexUsed != "":
		return fmt.Sprintf("plan uses %s but still seq-scans with filter", indexUsed)
	default:
		return "plan unchanged (filtered seq scan remains)"
	}
}

// topQueryByID returns the TopQuery with the given queryid.
func topQueryByID(qa *QueryAnalysis, id int64) *TopQuery {
	for i := range qa.Top {
		if qa.Top[i].QueryID == id {
			return &qa.Top[i]
		}
	}
	return nil
}

// applySettings pushes the changed settings to the server via ALTER SYSTEM and
// reloads. ALTER SYSTEM requires superuser: when the connected role is not a
// superuser it falls back to `sudo -u postgres psql`, or prints the statements
// for manual execution. Returns the names of parameters needing a restart.
func applySettings(ctx context.Context, conn *pgx.Conn, db *DBStats, plan []PlanItem, opts Options) ([]string, error) {
	var changed []PlanItem
	for _, it := range plan {
		if it.Changed {
			changed = append(changed, it)
		}
	}
	if len(changed) == 0 {
		fmt.Println("No configuration changes needed — the instance already matches the plan.")
		return nil, nil
	}

	fmt.Printf("Applying %d setting(s)...\n", len(changed))

	var stmts []string
	for _, it := range changed {
		stmts = append(stmts, fmt.Sprintf("ALTER SYSTEM SET %s = '%s';", it.Name, it.Proposed))
	}
	stmts = append(stmts, "SELECT pg_reload_conf();")

	switch {
	case db.IsSuperuser:
		for _, s := range stmts {
			if _, err := conn.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("exec %q: %w", s, err)
			}
		}
		fmt.Println("Applied via superuser connection.")
	case opts.UseSudo:
		if err := sudoPSQL(ctx, "", stmts...); err != nil {
			return nil, err
		}
		fmt.Println("Applied via sudo -u postgres psql.")
	default:
		fmt.Println("Connected user is not a superuser and sudo apply is disabled (-sudo=false).")
		fmt.Println("Run the following as a superuser (e.g. sudo -u postgres psql):")
		for _, s := range stmts {
			fmt.Println("  " + s)
		}
		return nil, fmt.Errorf("cannot apply: not superuser and sudo apply disabled")
	}

	var restart []string
	for _, it := range changed {
		if it.Restart {
			restart = append(restart, it.Name)
		}
	}
	for _, it := range changed {
		if it.Context == "user" {
			fmt.Printf("note: %s (user context) applies to new sessions only\n", it.Name)
		}
	}
	return restart, nil
}

// applyPgss bootstraps pg_stat_statements: creates the extension in the target
// database and adds it to shared_preload_libraries. Returns restart reasons.
func applyPgss(ctx context.Context, conn *pgx.Conn, opts Options, dbName string) ([]string, error) {
	avail, installed, preloaded, err := checkPgss(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !avail {
		fmt.Println("pg_stat_statements not available on this server (postgresql-contrib missing) — skipping.")
		return nil, nil
	}

	var stmts []string
	if !installed {
		stmts = append(stmts, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;")
	}

	var restart []string
	if !preloaded {
		// The current shared_preload_libraries value is only readable as a
		// superuser, so fetch it through sudo and merge pg_stat_statements in.
		cur, err := sudoShow(ctx, "SHOW shared_preload_libraries")
		if err != nil {
			return nil, err
		}
		if !strings.Contains(cur, "pg_stat_statements") {
			var parts []string
			for _, p := range strings.Split(cur, ",") {
				if s := strings.TrimSpace(p); s != "" {
					parts = append(parts, s)
				}
			}
			parts = append(parts, "pg_stat_statements")
			stmts = append(stmts, fmt.Sprintf(
				"ALTER SYSTEM SET shared_preload_libraries = '%s';", strings.Join(parts, ", ")))
		}
		restart = append(restart, "shared_preload_libraries (pg_stat_statements)")
	}

	if len(stmts) == 0 {
		fmt.Println("pg_stat_statements already configured.")
		return nil, nil
	}
	if err := sudoPSQL(ctx, dbName, stmts...); err != nil {
		return nil, err
	}
	fmt.Println("Installed pg_stat_statements (extension + shared_preload_libraries).")
	return restart, nil
}

// applyAnalyze runs ANALYZE when stale statistics were detected.
func applyAnalyze(ctx context.Context, opts Options, db *DBStats, qa *QueryAnalysis) error {
	if qa == nil {
		return nil
	}
	need := countFinding(qa, "stale-stats") > 0 || countFinding(qa, "estimate-skew") > 0
	if !need {
		fmt.Println("No stale statistics detected — skipping ANALYZE.")
		return nil
	}
	fmt.Printf("Running ANALYZE on database %s (stale statistics detected)...\n", db.Database)
	return sudoPSQL(ctx, db.Database, "ANALYZE;")
}

// sudoShow runs a single statement as the postgres superuser and returns the
// first column of the first row (used for settings that are not readable by
// non-superusers).
func sudoShow(ctx context.Context, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "sudo", "-n", "-u", "postgres", "psql", "-tAc", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sudo %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// doRestart restarts PostgreSQL if required settings were changed and the
// -restart flag is set; otherwise it tells the user how to restart.
func doRestart(ctx context.Context, opts Options, reasons []string) error {
	if len(reasons) == 0 {
		return nil
	}
	fmt.Printf("Settings requiring a restart: %s\n", strings.Join(reasons, ", "))
	if !opts.Restart {
		fmt.Println("Restart with: sudo systemctl restart postgresql  (or re-run with -restart)")
		return nil
	}
	fmt.Println("Restarting PostgreSQL (sudo systemctl restart postgresql)...")
	if out, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "restart", "postgresql").CombinedOutput(); err != nil {
		return fmt.Errorf("restart failed: %v\n%s", err, out)
	}
	fmt.Println("PostgreSQL restarted.")
	return nil
}
