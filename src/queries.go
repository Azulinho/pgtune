package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// TopQuery is one entry from pg_stat_statements.
type TopQuery struct {
	UserID    int64
	QueryID   int64
	Calls     int64
	TotalMS   float64
	MeanMS    float64
	Rows      int64
	Query     string
	HasParams bool
}

// ExplainMode says how a captured statement may be explored.
type ExplainMode int

const (
	ExplainSkip ExplainMode = iota
	ExplainPlanOnly
	ExplainAnalyze
)

var lockingClauseRe = regexp.MustCompile(`(?i)\bfor\s+(?:(?:no\s+)?key\s+)?(?:update|share)\b`)

// cteDMLRe catches data-modifying CTE bodies, which start with WITH but still
// write rows. selectIntoRe catches SELECT ... INTO, which creates a table.
var (
	cteDMLRe     = regexp.MustCompile(`(?i)\b(insert|update|delete|merge)\b`)
	selectIntoRe = regexp.MustCompile(`(?i)\binto\b`)
)

// explainModeFor decides how a captured statement may be explored. Only
// statements run by the connected role are executed; everything else gets a
// plan-only EXPLAIN. DML, data-modifying CTEs, SELECT INTO, non-SELECT
// statements and statements containing SQL comments are never executed.
// Locking clauses are also never executed, regardless of ownership.
func explainModeFor(q TopQuery, currentUserID int64) ExplainMode {
	sql := strings.TrimRight(strings.TrimSpace(q.Query), "; \t\n")
	if sql == "" || dmlRe.MatchString(sql) {
		return ExplainSkip
	}
	low := strings.ToLower(sql)
	if !(strings.HasPrefix(low, "select") || strings.HasPrefix(low, "with")) {
		return ExplainSkip
	}
	// Comments can hide a locking clause or a DML keyword from the regexes
	// below. Treat any commented statement as plan-only rather than risk
	// executing something the classifier did not understand.
	if strings.Contains(sql, "/*") || strings.Contains(sql, "--") {
		return ExplainPlanOnly
	}
	if lockingClauseRe.MatchString(sql) {
		return ExplainPlanOnly
	}
	if strings.HasPrefix(low, "with") && cteDMLRe.MatchString(sql) {
		return ExplainPlanOnly
	}
	if selectIntoRe.MatchString(sql) {
		return ExplainPlanOnly
	}
	if currentUserID == 0 || q.UserID != currentUserID {
		return ExplainPlanOnly
	}
	return ExplainAnalyze
}

// LongRunning is a query currently running longer than the threshold.
type LongRunning struct {
	Pid       int32
	User      string
	DB        string
	State     string
	DurationS float64
	Query     string
}

// TableStat is one row of pg_stat_user_tables joined with pg_statio.
type TableStat struct {
	Schema, Table     string
	SeqScan, IdxScan  int64
	BlksHit, BlksRead int64
	Live, Dead        int64
	Upd, Del          int64
	DeadPct           float64
	LastVacuum        *time.Time
	LastAutoVacuum    *time.Time
	LastAnalyze       *time.Time
	LastAutoAnalyze   *time.Time
	SizeBytes         int64
}

// Finding is a detected problem with an actionable suggestion.
type Finding struct {
	Kind       string // seq-scan, table-seq-scan, disk-sort, temp-spill, estimate-skew, bloat, stale-stats, long-running, explain-timeout
	QueryID    int64
	Table      string
	Bytes      uint64
	Detail     string
	Suggestion string
	IndexDDL   string // suggested CREATE INDEX statement (seq-scan findings)
	EstRows    int64  // planner estimate of rows matching the filter
}

// QueryAnalysis is the collected workload intelligence.
type QueryAnalysis struct {
	Available     bool // extension exists on server
	Installed     bool // extension created in this database
	Preloaded     bool // shared_preload_libraries contains pg_stat_statements
	RestartNeeded bool
	Note          string
	Top           []TopQuery
	LongRunning   []LongRunning
	Tables        []TableStat
	Findings      []Finding
}

var dmlRe = regexp.MustCompile(`(?i)^\s*(insert|update|delete|merge|create|alter|drop|truncate|grant|revoke|copy|vacuum|analyze|call|do|reindex|cluster|refresh)\b`)

const (
	// findings thresholds (heuristics; see README "How it works")
	staleStatsCutoff = 7 * 24 * time.Hour // ANALYZE older than this counts as stale
	findingMinTable  = 8 * MiB            // tables below this size are not flagged
	seqScanMinScans  = 10                 // at least this many seq scans to flag a table
	seqScanRatio     = 10                 // seq scans must exceed idx scans by this factor
	bloatDeadPct     = 0.25               // dead-tuple share that counts as bloat
	bloatMinLive     = 1000               // tables with fewer live rows are ignored
	tempSpillDBMin   = 10 * GiB           // cumulative temp spill worth reporting
)

// isStaleAnalyze reports whether a table's statistics are stale: the most
// recent ANALYZE (manual or auto) is older than the cutoff, or the table was
// never analyzed at all. A table that was only ever auto-analyzed is NOT stale
// as long as that run is recent.
func isStaleAnalyze(lastA, lastAA *time.Time, now time.Time, cutoff time.Duration) bool {
	last := lastA
	if last == nil || (lastAA != nil && lastAA.After(*last)) {
		last = lastAA
	}
	if last == nil {
		return true // never analyzed in any way
	}
	return last.Before(now.Add(-cutoff))
}

// selfQueryPatterns are ILIKE patterns identifying the app's own diagnostic
// statements so they do not pollute the pg_stat_statements top list.
func selfQueryPatterns() []string {
	return []string{
		"EXPLAIN%",
		"ALTER SYSTEM%",
		"%pg_available_extensions%",
		"%pg_stat_statements%",
		"%pg_stat_statements_reset%",
		"%pg_stat_user_tables%",
		"%pg_stat_activity%",
		"%pg_stat_database%",
		"%pg_stat_checkpointer%",
		"%pg_stat_bgwriter%",
		"%pg_extension%",
		"%pg_settings%",
		"%pg_database_size%",
		"%pg_postmaster_start_time%",
		"%pg_roles%",
		"%pg_reload_conf%",
		"%pg_stats%",
		"%pg_type WHERE typname%", // pgx driver type resolution on connect
		"SET %",                   // framework per-connection setup, not workload
		"%set_config(%",
	}
}

func checkPgss(ctx context.Context, conn *pgx.Conn) (available, installed, preloaded bool, err error) {
	var n int
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_available_extensions WHERE name = 'pg_stat_statements'").Scan(&n); err != nil {
		return false, false, false, err
	}
	available = n > 0
	_ = conn.QueryRow(ctx,
		"SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'").Scan(&n)
	installed = n > 0
	// SHOW shared_preload_libraries is restricted to pg_read_all_settings, so
	// the authoritative test is whether the view actually works: it errors with
	// "must be loaded via shared_preload_libraries" when not preloaded.
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_statements").Scan(&n); err == nil {
		preloaded = true
	}
	return available, installed, preloaded, nil
}

func collectQueryAnalysis(ctx context.Context, conn *pgx.Conn, opts Options, db *DBStats) (*QueryAnalysis, error) {
	qa := &QueryAnalysis{}

	avail, installed, preloaded, err := checkPgss(ctx, conn)
	if err != nil {
		return nil, err
	}
	qa.Available, qa.Installed, qa.Preloaded = avail, installed, preloaded

	if !avail {
		qa.Note = "pg_stat_statements is not available on this server (postgresql-contrib missing) - query analysis skipped"
		return qa, nil
	}
	if !preloaded {
		qa.RestartNeeded = true
		if installed {
			qa.Note = "pg_stat_statements is installed but not in shared_preload_libraries - it captures stats only after a restart"
		} else {
			qa.Note = "pg_stat_statements is not installed and not preloaded - run with -apply to install it (one restart required)"
		}
		return qa, nil
	}

	// The connected role's OID decides which statements may be executed under
	// EXPLAIN ANALYZE. If it cannot be read, 0 keeps every statement plan-only.
	var currentUserID int64
	if err := conn.QueryRow(ctx,
		`SELECT oid::bigint FROM pg_roles WHERE rolname = current_user`).Scan(&currentUserID); err != nil {
		currentUserID = 0
	}

	rows, err := conn.Query(ctx, `
		SELECT COALESCE(s.userid,0)::bigint, COALESCE(s.queryid,0), s.calls, s.total_exec_time, s.mean_exec_time, s.rows, s.query
		FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database()
		  AND s.query <> '<insufficient privilege>'
		  AND NOT (s.query ILIKE ANY($2))
		ORDER BY s.total_exec_time DESC
		LIMIT $1`, opts.TopN, selfQueryPatterns())
	if err != nil {
		qa.Note = "could not read pg_stat_statements: " + err.Error()
		return qa, nil
	}
	for rows.Next() {
		var t TopQuery
		if err := rows.Scan(&t.UserID, &t.QueryID, &t.Calls, &t.TotalMS, &t.MeanMS, &t.Rows, &t.Query); err != nil {
			rows.Close()
			return nil, err
		}
		t.HasParams = strings.Contains(t.Query, "$1")
		qa.Top = append(qa.Top, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(qa.Top) == 0 {
		qa.Note = "no statements captured in pg_stat_statements yet - run a workload (make load) and re-run"
	}

	// long-running queries
	lr, err := conn.Query(ctx, `
		SELECT pid, usename, datname, state,
		       extract(epoch FROM now() - query_start), query
		FROM pg_stat_activity
		WHERE state <> 'idle'
		  AND query NOT ILIKE '%pg_stat_activity%'
		  AND now() - query_start > make_interval(secs => $1)
		ORDER BY query_start`, opts.LongRunningSec)
	if err == nil {
		for lr.Next() {
			var l LongRunning
			if err := lr.Scan(&l.Pid, &l.User, &l.DB, &l.State, &l.DurationS, &l.Query); err == nil {
				qa.LongRunning = append(qa.LongRunning, l)
			}
		}
		lr.Close()
	}
	if len(qa.LongRunning) > 0 {
		qa.Findings = append(qa.Findings, Finding{
			Kind:       "long-running",
			Detail:     fmt.Sprintf("%d query(s) running longer than %.0fs", len(qa.LongRunning), opts.LongRunningSec),
			Suggestion: "investigate (possible missing index, lock contention, or runaway query)",
		})
	}

	// largest tables + their usage stats
	tb, err := conn.Query(ctx, `
		SELECT n.nspname, c.relname,
		       COALESCE(s.seq_scan,0), COALESCE(s.idx_scan,0),
		       COALESCE(st.heap_blks_hit,0), COALESCE(st.heap_blks_read,0),
		       COALESCE(s.n_live_tup,0), COALESCE(s.n_dead_tup,0),
		       COALESCE(s.n_tup_upd,0), COALESCE(s.n_tup_del,0),
		       s.last_vacuum, s.last_autovacuum, s.last_analyze, s.last_autoanalyze,
		       pg_total_relation_size(c.oid)
		FROM pg_stat_user_tables s
		JOIN pg_statio_user_tables st ON st.relid = s.relid
		JOIN pg_class c ON c.oid = s.relid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r','p')
		ORDER BY pg_total_relation_size(c.oid) DESC
		LIMIT $1`, opts.TablesN)
	if err == nil {
		for tb.Next() {
			var t TableStat
			var lv, lav, la, laa *time.Time
			if err := tb.Scan(&t.Schema, &t.Table, &t.SeqScan, &t.IdxScan,
				&t.BlksHit, &t.BlksRead, &t.Live, &t.Dead, &t.Upd, &t.Del,
				&lv, &lav, &la, &laa, &t.SizeBytes); err != nil {
				tb.Close()
				return nil, err
			}
			t.LastVacuum, t.LastAutoVacuum, t.LastAnalyze, t.LastAutoAnalyze = lv, lav, la, laa
			if t.Live+t.Dead > 0 {
				t.DeadPct = float64(t.Dead) / float64(t.Live+t.Dead)
			}
			qa.Tables = append(qa.Tables, t)
		}
		tb.Close()
	}

	// derive table-level findings
	now := time.Now()
	for _, t := range qa.Tables {
		if t.SizeBytes > int64(findingMinTable) && t.SeqScan > seqScanMinScans && t.SeqScan > seqScanRatio*max(t.IdxScan, 1) {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "table-seq-scan",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: %d sequential scans vs %d index scans - reads are walking the whole table",
					t.Schema, t.Table, t.SeqScan, t.IdxScan),
				Suggestion: "an index on the columns used by common WHERE filters would cut I/O dramatically",
			})
		}
		if t.DeadPct > bloatDeadPct && t.Live > bloatMinLive {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "bloat",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: %.1f%% dead tuples - table is bloating",
					t.Schema, t.Table, t.DeadPct*100),
				Suggestion: "check autovacuum (more workers / lower scale factor) or run VACUUM",
			})
		}
		stale := isStaleAnalyze(t.LastAnalyze, t.LastAutoAnalyze, now, staleStatsCutoff)
		if stale && t.SizeBytes > int64(findingMinTable) {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "stale-stats",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: no ANALYZE in the last 7 days - planner is guessing row counts",
					t.Schema, t.Table),
				Suggestion: "run ANALYZE (the app can do it with -analyze)",
			})
		}
	}

	// aggregate temp-file pressure that per-query EXPLAINs cannot see (the
	// top queries are parameterized and only get a non-executing EXPLAIN).
	// Cumulative since the last stats reset - advisory, not per-sort evidence.
	if db != nil && db.TempBytes > int64(tempSpillDBMin) {
		qa.Findings = append(qa.Findings, Finding{
			Kind:       "temp-spill-db",
			Detail:     fmt.Sprintf("%d temp file(s), %s total spilled to disk since stats reset", db.TempFiles, fmtBytes(uint64(db.TempBytes))),
			Suggestion: "sorts exceed work_mem; add covering indexes on ORDER BY/filter columns, or raise maintenance_work_mem for index builds",
		})
	}

	// EXPLAIN ANALYZE / plan-only EXPLAIN on the most expensive eligible queries.
	if err := explainTopQueries(ctx, conn, qa, opts, currentUserID); err != nil {
		return nil, err
	}

	return qa, nil
}

// explainTopQueries runs EXPLAIN on the top queries. Only statements run by
// the connected role are executed with EXPLAIN ANALYZE; statements from other
// roles, and any locking clause, get a plain EXPLAIN that never executes the
// query. Parameterized statements also get a plain EXPLAIN with probe values,
// because executing them with dummy values would misrepresent the workload.
// DML is skipped. A context deadline (per-DB budget exhausted) is returned as
// an error so the caller records the database as skipped; all other failures
// stay explain-error findings.
func explainTopQueries(ctx context.Context, conn *pgx.Conn, qa *QueryAnalysis, opts Options, currentUserID int64) error {
	if conn == nil || opts.ExplainN <= 0 {
		return nil
	}
	done := 0
	planOnly := 0
	for i := range qa.Top {
		if done >= opts.ExplainN {
			break
		}
		q := &qa.Top[i]
		sql := strings.TrimRight(strings.TrimSpace(q.Query), "; \t\n")
		mode := explainModeFor(*q, currentUserID)
		if mode == ExplainSkip {
			continue
		}
		if mode == ExplainPlanOnly {
			planOnly++
		}

		var out string
		if mode == ExplainPlanOnly || q.HasParams {
			// Plain EXPLAIN (not ANALYZE, not GENERIC_PLAN): no execution, but
			// the planner uses bound values, giving realistic row estimates
			// (GENERIC_PLAN assumes ~1/3 selectivity for every predicate).
			// Probe values come from pg_stats histograms when the WHERE clause
			// targets a plain column; otherwise dummy 1 is bound.
			args := make([]any, 0)
			probed := false
			if q.HasParams {
				args = make([]any, countParams(sql))
				for i := range args {
					args[i] = 1
				}
				var probe map[int]any
				probe, probed = probeValues(ctx, conn, sql)
				if probed {
					for k, v := range probe {
						if k >= 1 && k <= len(args) {
							args[k-1] = v
						}
					}
				}
			}
			if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&out); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				// when the probe succeeded the failure is real (e.g. permission
				// denied); when we shot blind with dummy 1 it is usually just a
				// type mismatch of the placeholder, which says nothing about the
				// query itself - stay quiet in that case
				if probed {
					qa.Findings = append(qa.Findings, Finding{
						Kind:       "explain-error",
						QueryID:    q.QueryID,
						Detail:     fmt.Sprintf("EXPLAIN failed: %v", err),
						Suggestion: "query may be slow or not executable in this session",
					})
					done++
				}
				continue
			}
		} else {
			// EXPLAIN ANALYZE executes the query for real, so cap it with a
			// statement_timeout scoped to a transaction (SET LOCAL): it cannot
			// leak into later statements on the shared connection, and it is
			// reliable under connection pooling. 0 disables the cap.
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", opts.ExplainTimeout.Milliseconds())); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&out); err != nil {
				_ = tx.Rollback(ctx)
				if errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				qa.Findings = append(qa.Findings, Finding{
					Kind:       "explain-error",
					QueryID:    q.QueryID,
					Detail:     fmt.Sprintf("EXPLAIN ANALYZE failed: %v", err),
					Suggestion: "query may be slow or not executable in this session",
				})
				done++
				continue
			}
			// EXPLAIN ANALYZE really executed the statement. Roll it back so
			// side effects from anything the classifier missed do not persist;
			// locks taken during execution are released here too.
			_ = tx.Rollback(ctx)
		}

		var expl []jsonExplain
		if err := json.Unmarshal([]byte(out), &expl); err != nil {
			continue
		}
		for _, e := range expl {
			qa.Findings = append(qa.Findings, walkPlan(e.Plan, *q)...)
		}
		done++
	}
	if planOnly > 0 {
		note := fmt.Sprintf("%d statement(s) were planned only (not executed)", planOnly)
		if qa.Note != "" {
			qa.Note += "; " + note
		} else {
			qa.Note = note
		}
	}
	return nil
}

// countParams returns the highest $n placeholder number in a query.
func countParams(q string) int {
	max := 0
	rest := q
	for {
		i := strings.Index(rest, "$")
		if i < 0 {
			return max
		}
		j := i + 1
		n := 0
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			n = n*10 + int(rest[j]-'0')
			j++
		}
		if n > max {
			max = n
		}
		rest = rest[j:]
	}
}

var (
	probeWhereRe   = regexp.MustCompile(`(?i)\bwhere\s+([a-zA-Z_][a-zA-Z0-9_$\.]*?)\s*(=|<>|!=|<=|>=|<|>)\s*\$(\d+)`)
	probeBetweenRe = regexp.MustCompile(`(?i)\bwhere\s+([a-zA-Z_][a-zA-Z0-9_$\.]*?)\s+between\s+\$(\d+)\s+and\s+\$(\d+)`)
	probeFromRe    = regexp.MustCompile(`(?i)\bfrom\s+([a-zA-Z_][a-zA-Z0-9_$]*(?:\.[a-zA-Z_][a-zA-Z0-9_$]*)?)`)
	probeNumericRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
)

// probeValues picks realistic probe values for the $n placeholders of a
// parameterized query, derived from the target column's pg_stats histogram.
// This lets EXPLAIN estimate selectivity with a representative constant
// instead of an arbitrary dummy (e.g. for `abalance > $1` a low-end value
// would make the predicate look unselective). Returns a map of parameter
// number -> Go value; ok=false when no plain-column WHERE predicate is found
// or the column has no statistics.
func probeValues(ctx context.Context, conn *pgx.Conn, sql string) (map[int]any, bool) {
	out := map[int]any{}
	var colExpr, op string
	var p1, p2 int
	if m := probeBetweenRe.FindStringSubmatch(sql); m != nil {
		colExpr, op = m[1], "between"
		fmt.Sscanf(m[2], "%d", &p1)
		fmt.Sscanf(m[3], "%d", &p2)
	} else if m := probeWhereRe.FindStringSubmatch(sql); m != nil {
		colExpr, op = m[1], m[2]
		fmt.Sscanf(m[3], "%d", &p1)
	} else {
		return nil, false
	}

	col := colExpr
	if i := strings.LastIndex(col, "."); i >= 0 {
		col = col[i+1:]
	}
	col = strings.Trim(col, `"`)
	fm := probeFromRe.FindStringSubmatch(sql)
	if fm == nil {
		return nil, false
	}
	tbl := fm[1]
	schema, table := "public", tbl
	if i := strings.Index(tbl, "."); i >= 0 {
		schema, table = tbl[:i], tbl[i+1:]
	}

	var boundsTxt, mcvsTxt string
	err := conn.QueryRow(ctx,
		`SELECT COALESCE(array_to_string(histogram_bounds, '|'), ''),
		        COALESCE(array_to_string(most_common_vals, '|'), '')
		 FROM pg_stats WHERE schemaname = $1 AND tablename = $2 AND attname = $3`,
		schema, table, col).Scan(&boundsTxt, &mcvsTxt)
	if err != nil {
		return nil, false
	}
	bounds := []string{}
	for _, b := range strings.Split(boundsTxt, "|") {
		if s := strings.TrimSpace(b); s != "" {
			bounds = append(bounds, s)
		}
	}
	if len(bounds) < 2 {
		// no histogram (e.g. unique column): MCV is meaningless, fall back
		return nil, false
	}

	pick := func(idx int) any {
		if idx < 0 {
			idx = 0
		}
		if idx >= len(bounds) {
			idx = len(bounds) - 1
		}
		v := bounds[idx]
		if probeNumericRe.MatchString(v) {
			if strings.ContainsAny(v, ".eE") {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					return f
				}
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
		return v
	}

	lo, hi := 1, len(bounds)-2
	if hi < lo {
		hi = lo
	}
	switch op {
	case "<", "<=":
		out[p1] = pick(lo)
	case ">", ">=":
		out[p1] = pick(hi)
	case "between":
		out[p1] = pick((lo + hi) / 2)
		out[p2] = pick(hi)
	default: // "=", "<>", "!=": probe the median - representative, not extreme
		out[p1] = pick((lo + hi) / 2)
	}
	return out, true
}

type jsonExplain struct {
	Plan          jsonPlan `json:"Plan"`
	PlanningTime  float64  `json:"Planning Time"`
	ExecutionTime float64  `json:"Execution Time"`
}

type jsonPlan struct {
	NodeType          string     `json:"Node Type"`
	RelationName      string     `json:"Relation Name"`
	Schema            string     `json:"Schema"`
	Filter            string     `json:"Filter"`
	IndexName         string     `json:"Index Name"`
	SortMethod        string     `json:"Sort Method"`
	SortSpaceUsed     int64      `json:"Sort Space Used"`
	SortSpaceType     string     `json:"Sort Space Type"`
	PlanRows          float64    `json:"Plan Rows"`
	ActualRows        float64    `json:"Actual Rows"`
	ActualTotalTime   float64    `json:"Actual Total Time"`
	TempReadBlocks    int64      `json:"Temp Read Blocks"`
	TempWrittenBlocks int64      `json:"Temp Written Blocks"`
	SharedHitBlocks   int64      `json:"Shared Hit Blocks"`
	SharedReadBlocks  int64      `json:"Shared Read Blocks"`
	Plans             []jsonPlan `json:"Plans"`
}

// walkPlan recursively inspects an EXPLAIN JSON tree for problems.
func walkPlan(n jsonPlan, q TopQuery) []Finding {
	var out []Finding
	switch n.NodeType {
	case "Seq Scan", "Parallel Seq Scan":
		if n.Filter != "" {
			tbl := n.RelationName
			if n.Schema != "" {
				tbl = n.Schema + "." + n.RelationName
			}
			f := Finding{
				Kind:       "seq-scan",
				QueryID:    q.QueryID,
				Table:      tbl,
				EstRows:    int64(n.PlanRows),
				Detail:     fmt.Sprintf("seq scan on %s with filter %q", tbl, n.Filter),
				Suggestion: "an index supporting this filter would let the planner use an index scan",
			}
			if ddl := indexDDLForFilter(tbl, n.Filter); ddl != "" {
				f.IndexDDL = ddl
				f.Suggestion = "an index on the filtered column would let the planner use an index scan"
			}
			out = append(out, f)
		}
	case "Sort", "Incremental Sort":
		if n.SortSpaceType == "Disk" || strings.Contains(n.SortMethod, "external") {
			// Sort Space Used is reported in kB.
			out = append(out, Finding{
				Kind:       "disk-sort",
				QueryID:    q.QueryID,
				Bytes:      uint64(n.SortSpaceUsed) * 1024,
				Detail:     fmt.Sprintf("sort spilled to disk (%s)", fmtBytes(uint64(n.SortSpaceUsed)*1024)),
				Suggestion: "increase work_mem (or shrink the sort with an index on the ORDER BY columns)",
			})
		}
	}
	if n.TempWrittenBlocks > 0 {
		b := uint64(n.TempWrittenBlocks) * 8192
		out = append(out, Finding{
			Kind:       "temp-spill",
			QueryID:    q.QueryID,
			Bytes:      b,
			Detail:     fmt.Sprintf("query spilled %s to temporary files", fmtBytes(b)),
			Suggestion: "increase work_mem (or temp_buffers for hash-heavy queries)",
		})
	}
	if n.ActualRows >= 100 && n.PlanRows > 0 {
		r := n.ActualRows / n.PlanRows
		if r > 10 || n.PlanRows/n.ActualRows > 10 {
			off := r
			if r < 1 {
				off = n.PlanRows / n.ActualRows
			}
			out = append(out, Finding{
				Kind:       "estimate-skew",
				QueryID:    q.QueryID,
				Detail:     fmt.Sprintf("planner estimated %.0f rows, actual %.0f (%.0fx off)", n.PlanRows, n.ActualRows, off),
				Suggestion: "statistics are stale or missing - run ANALYZE",
			})
		}
	}
	for _, c := range n.Plans {
		out = append(out, walkPlan(c, q)...)
	}
	return out
}

// splitTable splits "schema.table" into its parts (schema defaults to public).
func splitTable(tbl string) (schema, table string) {
	if i := strings.LastIndex(tbl, "."); i >= 0 {
		return tbl[:i], tbl[i+1:]
	}
	return "public", tbl
}

// quoteIdent renders one or more identifiers safely for DDL.
func quoteIdent(parts ...string) string {
	return pgx.Identifier(parts).Sanitize()
}

// indexName keeps generated index names within PostgreSQL's 63-byte identifier
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

// filterColumn extracts the plain column name from a filter predicate such as
// "(abalance < 1)" or "t.col BETWEEN 10 AND 20". Returns "" for expressions
// (functions, casts) that would need an expression index instead.
func filterColumn(filter string) string {
	f := strings.TrimSpace(filter)
	for strings.HasPrefix(f, "(") && strings.HasSuffix(f, ")") {
		f = strings.TrimSpace(f[1 : len(f)-1])
	}
	re := regexp.MustCompile(`^("?[A-Za-z_][A-Za-z0-9_$]*"?)(?:\s*\.\s*("?[A-Za-z_][A-Za-z0-9_$]*"?))?\s*(=|<>|!=|<=|>=|<|>|IN|BETWEEN|LIKE|ILIKE)`)
	m := re.FindStringSubmatch(f)
	if m == nil {
		return ""
	}
	col := m[1]
	if m[2] != "" {
		col = m[2]
	}
	return strings.Trim(col, `"`)
}

// indexDDLForFilter builds a CREATE INDEX statement for the column a filter
// predicate targets, or "" if no plain column can be extracted.
func indexDDLForFilter(tbl, filter string) string {
	schema, table := splitTable(tbl)
	col := filterColumn(filter)
	if table == "" || col == "" {
		return ""
	}
	return fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s);",
		quoteIdent(indexName(table, col)), quoteIdent(schema, table), quoteIdent(col))
}

// indexSelectivity reports the planner's estimated rows matching the filter
// versus live rows in the table (from pg_stat_user_tables).
func indexSelectivity(f *Finding, tables []TableStat) (est, total float64, ok bool) {
	schema, table := splitTable(f.Table)
	for _, t := range tables {
		if t.Schema == schema && t.Table == table {
			if t.Live <= 0 || f.EstRows < 0 {
				return 0, 0, false
			}
			return float64(f.EstRows), float64(t.Live), true
		}
	}
	return 0, 0, false
}
