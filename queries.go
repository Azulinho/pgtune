package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TopQuery is one entry from pg_stat_statements.
type TopQuery struct {
	QueryID   int64
	Calls     int64
	TotalMS   float64
	MeanMS    float64
	Rows      int64
	Query     string
	HasParams bool
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
	Schema, Table                     string
	SeqScan, IdxScan                  int64
	BlksHit, BlksRead                 int64
	Live, Dead                        int64
	DeadPct                           float64
	LastVacuum                        *time.Time
	LastAutoVacuum                    *time.Time
	LastAnalyze                       *time.Time
	LastAutoAnalyze                   *time.Time
	SizeBytes                         int64
}

// Finding is a detected problem with an actionable suggestion.
type Finding struct {
	Kind       string // seq-scan, table-seq-scan, disk-sort, temp-spill, estimate-skew, bloat, stale-stats, long-running, explain-timeout
	QueryID    int64
	Table      string
	Bytes      uint64
	Detail     string
	Suggestion string
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

func collectQueryAnalysis(ctx context.Context, conn *pgx.Conn, opts Options) (*QueryAnalysis, error) {
	qa := &QueryAnalysis{}

	avail, installed, preloaded, err := checkPgss(ctx, conn)
	if err != nil {
		return nil, err
	}
	qa.Available, qa.Installed, qa.Preloaded = avail, installed, preloaded

	if !avail {
		qa.Note = "pg_stat_statements is not available on this server (postgresql-contrib missing) — query analysis skipped"
		return qa, nil
	}
	if !preloaded {
		qa.RestartNeeded = true
		if installed {
			qa.Note = "pg_stat_statements is installed but not in shared_preload_libraries — it captures stats only after a restart"
		} else {
			qa.Note = "pg_stat_statements is not installed and not preloaded — run with -apply to install it (one restart required)"
		}
		return qa, nil
	}

	rows, err := conn.Query(ctx, `
		SELECT COALESCE(s.queryid,0), s.calls, s.total_exec_time, s.mean_exec_time, s.rows, s.query
		FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database()
		  AND s.query NOT ILIKE 'EXPLAIN%'
		ORDER BY s.total_exec_time DESC
		LIMIT $1`, opts.TopN)
	if err != nil {
		qa.Note = "could not read pg_stat_statements: " + err.Error()
		return qa, nil
	}
	for rows.Next() {
		var t TopQuery
		if err := rows.Scan(&t.QueryID, &t.Calls, &t.TotalMS, &t.MeanMS, &t.Rows, &t.Query); err != nil {
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
		qa.Note = "no statements captured in pg_stat_statements yet — run a workload (make load) and re-run"
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
				&t.BlksHit, &t.BlksRead, &t.Live, &t.Dead,
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
	for _, t := range qa.Tables {
		if t.SizeBytes > int64(8*MiB) && t.SeqScan > 10 && t.SeqScan > 10*max(t.IdxScan, 1) {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "table-seq-scan",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: %d sequential scans vs %d index scans — reads are walking the whole table",
					t.Schema, t.Table, t.SeqScan, t.IdxScan),
				Suggestion: "an index on the columns used by common WHERE filters would cut I/O dramatically",
			})
		}
		if t.DeadPct > 0.25 && t.Live > 1000 {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "bloat",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: %.1f%% dead tuples — table is bloating",
					t.Schema, t.Table, t.DeadPct*100),
				Suggestion: "check autovacuum (more workers / lower scale factor) or run VACUUM",
			})
		}
		stale := t.LastAnalyze == nil || t.LastAutoAnalyze == nil
		if !stale {
			cut := time.Now().Add(-7 * 24 * time.Hour)
			stale = t.LastAnalyze.Before(cut) && t.LastAutoAnalyze.Before(cut)
		}
		if stale && t.SizeBytes > int64(8*MiB) {
			qa.Findings = append(qa.Findings, Finding{
				Kind:  "stale-stats",
				Table: t.Schema + "." + t.Table,
				Detail: fmt.Sprintf("%s.%s: no ANALYZE in 7+ days — planner is guessing row counts",
					t.Schema, t.Table),
				Suggestion: "run ANALYZE (the app can do it with -analyze)",
			})
		}
	}

	// EXPLAIN ANALYZE / GENERIC_PLAN on the most expensive eligible queries.
	explainTopQueries(ctx, conn, qa, opts.ExplainN)

	return qa, nil
}

// explainTopQueries runs EXPLAIN on the top queries. SELECT-only queries are
// safe to ANALYZE; parameterized ones get a GENERIC_PLAN (plan shape, no
// execution) with dummy bound values — in generic mode PostgreSQL ignores the
// actual values. DML is skipped (EXPLAIN ANALYZE would execute it).
func explainTopQueries(ctx context.Context, conn *pgx.Conn, qa *QueryAnalysis, explainN int) {
	if conn == nil || explainN <= 0 {
		return
	}
	done := 0
	for i := range qa.Top {
		if done >= explainN {
			break
		}
		q := &qa.Top[i]
		sql := strings.TrimRight(strings.TrimSpace(q.Query), "; \t\n")
		if dmlRe.MatchString(sql) {
			continue
		}
		low := strings.ToLower(sql)
		if !(strings.HasPrefix(low, "select") || strings.HasPrefix(low, "with")) {
			continue
		}

		var out string
		if q.HasParams {
			// generic plan: bind dummy values, PostgreSQL ignores them in
			// GENERIC_PLAN mode but needs them for protocol-level binding.
			args := make([]any, countParams(sql))
			for i := range args {
				args[i] = 1
			}
			if err := conn.QueryRow(ctx, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+sql, args...).Scan(&out); err != nil {
				continue
			}
		} else {
			if _, err := conn.Exec(ctx, "SET statement_timeout = 5000"); err != nil {
				continue
			}
			if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&out); err != nil {
				qa.Findings = append(qa.Findings, Finding{
					Kind:    "explain-error",
					QueryID: q.QueryID,
					Detail:  fmt.Sprintf("EXPLAIN ANALYZE failed: %v", err),
					Suggestion: "query may be slow or not executable in this session",
				})
				done++
				continue
			}
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
			out = append(out, Finding{
				Kind:       "seq-scan",
				QueryID:    q.QueryID,
				Table:      tbl,
				Detail:     fmt.Sprintf("seq scan on %s with filter %q", tbl, n.Filter),
				Suggestion: "an index supporting this filter would let the planner use an index scan",
			})
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
				Suggestion: "statistics are stale or missing — run ANALYZE",
			})
		}
	}
	for _, c := range n.Plans {
		out = append(out, walkPlan(c, q)...)
	}
	return out
}
