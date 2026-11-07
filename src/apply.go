package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// applyIndexes creates the indexes recommended by seq-scan findings. Only
// selective predicates are applied (filter matches <= 10% of the table);
// existing covering indexes are skipped. After creation it ANALYZEs the table
// and re-runs EXPLAIN to verify the planner actually switched to the index.
// Creating an index requires table ownership (not superuser): when the
// connected role cannot do it, the DDL is printed for manual execution.
func applyIndexes(ctx context.Context, conn *pgx.Conn, qa *QueryAnalysis) error {
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
			fmt.Printf("skip index on %s: filter matches %.1f%% of the table - not selective enough\n", f.Table, est/total*100)
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
			fmt.Printf("index on %s.%s (%s) already covered by %s - skipping\n", schema, table, col, name)
			continue
		}
		if writeHeavy := tableWriteHeavy(f, qa.Tables); writeHeavy {
			fmt.Printf("warning: %s.%s is write-heavy - the new index adds per-row UPDATE/DELETE overhead\n", schema, table)
		}
		if _, err := conn.Exec(ctx, f.IndexDDL); err != nil {
			fmt.Printf("warning: could not create index (run manually as a table owner):\n  %s\n  (%v)\n", f.IndexDDL, err)
			if name := indexNameFromDDL(f.IndexDDL); name != "" {
				fmt.Printf("  a cancelled build can leave an INVALID index; check and drop it with:\n  DROP INDEX CONCURRENTLY IF EXISTS %s;\n", quoteIdent(schema, name))
			}
			continue
		}
		fmt.Printf("created index on %s.%s (%s)\n", schema, table, col)
		// refresh planner statistics so the new path is considered accurately
		if _, err := conn.Exec(ctx, fmt.Sprintf("ANALYZE %s;", quoteIdent(schema, table))); err != nil {
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
		if t.Schema == schema && t.Table == table {
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

var indexNameRe = regexp.MustCompile(`(?i)^CREATE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?("[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)`)

// indexNameFromDDL returns the index name from a CREATE INDEX statement, or ""
// when the statement cannot be parsed.
func indexNameFromDDL(ddl string) string {
	m := indexNameRe.FindStringSubmatch(strings.TrimSpace(ddl))
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"`)
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
	if qa == nil {
		return nil
	}
	for i := range qa.Top {
		if qa.Top[i].QueryID == id {
			return &qa.Top[i]
		}
	}
	return nil
}

// applySettings pushes the changed settings to the server via ALTER SYSTEM and
// reloads. ALTER SYSTEM requires superuser: when the connected role is not a
// superuser, the statements are printed for manual execution instead. Returns
// the names of parameters needing a restart.
func applySettings(ctx context.Context, conn *pgx.Conn, db *DBStats, plan []PlanItem) ([]string, error) {
	var changed []PlanItem
	for _, it := range plan {
		if it.Changed {
			changed = append(changed, it)
		}
	}
	if len(changed) == 0 {
		fmt.Println("No configuration changes needed - the instance already matches the plan.")
		return nil, nil
	}

	fmt.Printf("Applying %d setting(s)...\n", len(changed))

	var stmts []string
	for _, it := range changed {
		stmts = append(stmts, fmt.Sprintf("ALTER SYSTEM SET %s = '%s';", it.Name, it.Proposed))
	}
	stmts = append(stmts, "SELECT pg_reload_conf();")

	if !db.IsSuperuser {
		fmt.Println("Connected user is not a superuser - ALTER SYSTEM requires superuser privileges.")
		fmt.Println("Run the following as a superuser (e.g. sudo -u postgres psql):")
		for _, s := range stmts {
			fmt.Println("  " + s)
		}
		return nil, fmt.Errorf("cannot apply: not a superuser; run the printed SQL manually")
	}

	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return nil, fmt.Errorf("exec %q: %w", s, err)
		}
	}
	fmt.Println("Applied via superuser connection.")

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
// database and adds it to shared_preload_libraries. Both need superuser
// privileges; when the connected role cannot read the current
// shared_preload_libraries value, the statements are printed for manual
// execution. Returns restart reasons.
func applyPgss(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	avail, installed, preloaded, err := checkPgss(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !avail {
		fmt.Println("pg_stat_statements not available on this server (postgresql-contrib missing) - skipping.")
		return nil, nil
	}

	var stmts []string
	if !installed {
		stmts = append(stmts, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements;")
	}

	var restart []string
	if !preloaded {
		// SHOW shared_preload_libraries is restricted to superusers (and
		// pg_read_all_settings). preloaded (from view existence) is false
		// whenever the extension is missing from THIS database, even if the
		// library is already preloaded server-wide - so only a genuine preload
		// change needs a restart.
		var cur string
		if err := conn.QueryRow(ctx, "SHOW shared_preload_libraries").Scan(&cur); err != nil {
			msg := "cannot read shared_preload_libraries (needs superuser). Run manually as a superuser:\n" +
				"  SHOW shared_preload_libraries; -- note the current value\n"
			if !installed {
				msg += "  CREATE EXTENSION pg_stat_statements;\n"
			}
			msg += "  ALTER SYSTEM SET shared_preload_libraries = '<current value>, pg_stat_statements';\n" +
				"  SELECT pg_reload_conf();\n" +
				"  -- then restart PostgreSQL once"
			return nil, fmt.Errorf("%s", msg)
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
			restart = append(restart, "shared_preload_libraries (pg_stat_statements)")
		}
	}

	if len(stmts) == 0 {
		fmt.Println("pg_stat_statements already configured.")
		return nil, nil
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return nil, fmt.Errorf("exec %q: %w", s, err)
		}
	}
	fmt.Println("Installed pg_stat_statements (extension + shared_preload_libraries).")
	return restart, nil
}

// applyAnalyze runs ANALYZE when stale statistics were detected. Plain ANALYZE
// processes every table the connected role may analyze; a non-superuser only
// covers their own tables, so failures are reported rather than fatal.
func applyAnalyze(ctx context.Context, conn *pgx.Conn, opts Options, dbName string, qa *QueryAnalysis) error {
	if qa == nil {
		return nil
	}
	need := countFinding(qa, "stale-stats") > 0 || countFinding(qa, "estimate-skew") > 0
	if !need {
		if !opts.AllDBs {
			fmt.Println("No stale statistics detected - skipping ANALYZE.")
		}
		return nil
	}
	fmt.Printf("Running ANALYZE on database %s (stale statistics detected)...\n", dbName)
	if _, err := conn.Exec(ctx, "ANALYZE;"); err != nil {
		return fmt.Errorf("ANALYZE: %w (run manually as a table owner or superuser)", err)
	}
	return nil
}

// noteRestart reports settings that need a postmaster restart. The app never
// restarts PostgreSQL itself; the operator does that with their platform's
// service manager (systemctl, pg_ctlcluster, pg_ctl, ...).
func noteRestart(reasons []string) {
	if len(reasons) == 0 {
		return
	}
	fmt.Printf("Settings requiring a restart: %s\n", strings.Join(reasons, ", "))
	fmt.Println("Restart PostgreSQL to apply them (the app does not restart the server), e.g.:")
	fmt.Println("  sudo systemctl restart postgresql        # Debian/Ubuntu default cluster")
	fmt.Println("  sudo systemctl restart postgresql-16     # RHEL/Fedora named cluster")
	fmt.Println("  pg_ctlcluster 16 main restart            # Debian multi-cluster")
}
