package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

func humanDuration(sec float64) string {
	d := time.Duration(sec) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func printReport(out io.Writer, sys *SysStats, db *DBStats, wl Workload, plan []PlanItem, qa *QueryAnalysis) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)

	fmt.Fprintln(w, "\n=== pgtune report ===")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "PostgreSQL\t: %s\n", db.Version)
	fmt.Fprintf(w, "Database\t: %s (%s)\n", db.Database, fmtBytes(uint64(db.DatabaseSizeBytes)))
	fmt.Fprintf(w, "Uptime\t: %s\n", humanDuration(db.UptimeSec))
	fmt.Fprintf(w, "System\t: %s RAM, %d CPU, %s storage\n",
		fmtBytes(sys.TotalMemBytes), sys.NumCPU, strings.ToUpper(sys.DiskType))
	fmt.Fprintf(w, "Workload\t: %s\n", wl)
	fmt.Fprintf(w, "Superuser connection\t: %v\n", db.IsSuperuser)
	w.Flush()

	printLiveStats(w, "--- live stats ---", db)
	printTuningPlan(w, plan)
	printQueryAnalysis(w, qa)
}

// printAllReport prints one instance-level view (aggregated live stats and the
// tuning plan) followed by a per-database deep dive (query analysis, findings,
// index recommendations) for every analyzable database.
func printAllReport(out io.Writer, sys *SysStats, agg *DBStats, wl Workload, plan []PlanItem, perDBs []*PerDB) {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)

	fmt.Fprintln(w, "\n=== pgtune report (all databases) ===")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "PostgreSQL\t: %s\n", agg.Version)
	fmt.Fprintf(w, "Databases\t: %s\n", agg.Database)
	fmt.Fprintf(w, "Uptime\t: %s\n", humanDuration(agg.UptimeSec))
	fmt.Fprintf(w, "System\t: %s RAM, %d CPU, %s storage\n",
		fmtBytes(sys.TotalMemBytes), sys.NumCPU, strings.ToUpper(sys.DiskType))
	fmt.Fprintf(w, "Workload\t: %s\n", wl)
	fmt.Fprintf(w, "Superuser connection\t: %v\n", agg.IsSuperuser)
	w.Flush()

	printLiveStats(w, "--- live stats (all databases) ---", agg)
	printTuningPlan(w, plan)

	skipped := 0
	for _, p := range perDBs {
		if p.SkipReason != "" {
			skipped++
			continue
		}
		fmt.Fprintf(w, "\n=== database: %s ===\n", p.DB.Database)
		printDBCompact(w, p.DB)
		printQueryAnalysis(w, p.QA)
	}
	if skipped > 0 {
		fmt.Fprintln(w, "\nSkipped databases:")
		for _, p := range perDBs {
			if p.SkipReason != "" {
				fmt.Fprintf(w, "  %s: %s\n", p.DB.Database, p.SkipReason)
			}
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "note: run as a role with pg_read_all_stats (plus table privileges for EXPLAIN) to see all databases' statements; per-database changes apply with -analyze / -apply-index")
	w.Flush()
}

// printDBCompact shows the per-database subset of live stats that actually
// differs between databases on the same server.
func printDBCompact(w *tabwriter.Writer, db *DBStats) {
	hit := 0.0
	if total := db.BlksHit + db.BlksRead; total > 0 {
		hit = float64(db.BlksHit) / float64(total) * 100
	}
	idx := 0.0
	if total := db.SeqScans + db.IdxScans; total > 0 {
		idx = float64(db.IdxScans) / float64(total) * 100
	}
	fmt.Fprintf(w, "Database\t: %s (%s)\n", db.Database, fmtBytes(uint64(db.DatabaseSizeBytes)))
	fmt.Fprintf(w, "Cache hit ratio\t: %.2f%%\n", hit)
	fmt.Fprintf(w, "Transactions\t: %d commits / %d rollbacks\n", db.Commits, db.Rollbacks)
	fmt.Fprintf(w, "Temp spill\t: %d files / %s\n", db.TempFiles, fmtBytes(uint64(db.TempBytes)))
	fmt.Fprintf(w, "Index scan ratio\t: %.1f%% (seq=%d idx=%d)\n", idx, db.SeqScans, db.IdxScans)
	w.Flush()
}

func printLiveStats(w *tabwriter.Writer, title string, db *DBStats) {
	hit := 0.0
	if total := db.BlksHit + db.BlksRead; total > 0 {
		hit = float64(db.BlksHit) / float64(total) * 100
	}
	idx := 0.0
	if total := db.SeqScans + db.IdxScans; total > 0 {
		idx = float64(db.IdxScans) / float64(total) * 100
	}

	fmt.Fprintln(w, "\n"+title)
	fmt.Fprintf(w, "Cache hit ratio\t: %.2f%%\n", hit)
	fmt.Fprintf(w, "Transactions\t: %d commits / %d rollbacks\n", db.Commits, db.Rollbacks)
	fmt.Fprintf(w, "Deadlocks\t: %d\n", db.Deadlocks)
	fmt.Fprintf(w, "Temp spill\t: %d files / %s\n", db.TempFiles, fmtBytes(uint64(db.TempBytes)))
	fmt.Fprintf(w, "Checkpoints\t: %d timed / %d requested (requested = checkpoint pressure)\n",
		db.CheckpointsTimed, db.CheckpointsReq)
	fmt.Fprintf(w, "Buffers chkpt/clean\t: %d / %d\n",
		db.BuffersCheckpoint, db.BuffersClean)
	fmt.Fprintf(w, "Index scan ratio\t: %.1f%% (seq=%d idx=%d)\n", idx, db.SeqScans, db.IdxScans)
	fmt.Fprintf(w, "Connections\t: %d active / %s max\n",
		db.ActiveConns, settingValue(db.Settings, "max_connections", "?"))
	fmt.Fprintf(w, "I/O timing\t: read %.0f ms / write %.0f ms (0 = track_io_timing off)\n",
		db.BlkReadTimeMS, db.BlkWriteTimeMS)
	w.Flush()
}

func printTuningPlan(w *tabwriter.Writer, plan []PlanItem) {
	fmt.Fprintln(w, "\n--- tuning plan ---")
	fmt.Fprintln(w, "PARAMETER\tCURRENT\tPROPOSED\tCONTEXT\tSTATUS")
	for _, it := range plan {
		status := "ok"
		switch {
		case !it.Changed:
			status = "ok"
		case it.Restart:
			status = "restart"
		default:
			status = "reload"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			it.Name, it.Current, it.Proposed, it.Context, status)
	}
	w.Flush()

	// evidence-based adjustments get called out under the table (also when the
	// item was NOT changed, so an "ok" row can explain itself, e.g. a low
	// cache hit ratio that cannot be fixed because the DB exceeds RAM)
	printed := false
	for _, it := range plan {
		if strings.Contains(it.Reason, "evidence") {
			if !printed {
				fmt.Fprintln(w, "\nEvidence-based adjustments:")
				printed = true
			}
			fmt.Fprintf(w, "  %s: %s\n", it.Name, it.Reason)
		}
	}
	w.Flush()
}

func printQueryAnalysis(w *tabwriter.Writer, qa *QueryAnalysis) {
	if qa == nil {
		return
	}
	fmt.Fprintln(w, "\n--- query analysis ---")
	if !qa.Available {
		fmt.Fprintf(w, "pg_stat_statements\t: %s\n", qa.Note)
		w.Flush()
		return
	}
	if qa.RestartNeeded {
		fmt.Fprintf(w, "pg_stat_statements\t: %s\n", qa.Note)
		w.Flush()
		return
	}
	fmt.Fprintf(w, "pg_stat_statements\t: capturing (preloaded)\n")
	if qa.Note != "" {
		fmt.Fprintf(w, "note\t: %s\n", qa.Note)
	}
	w.Flush()

	if len(qa.Top) > 0 {
		fmt.Fprintln(w, "Top queries (by total execution time):")
		for i, q := range qa.Top {
			display := truncate(q.Query, 90)
			params := ""
			if q.HasParams {
				params = " (parameterized)"
			}
			fmt.Fprintf(w, "  #%d total=%.1fs mean=%.1fms calls=%d rows=%d%s\n",
				i+1, q.TotalMS/1000, q.MeanMS, q.Calls, q.Rows, params)
			fmt.Fprintf(w, "     %s\n", display)
		}
		w.Flush()
	}

	if len(qa.LongRunning) > 0 {
		fmt.Fprintf(w, "Long-running queries (>%.0fs):\n", qa.LongRunning[0].DurationS)
		_ = qa.LongRunning
		for _, l := range qa.LongRunning {
			fmt.Fprintf(w, "  pid=%d %s %.0fs state=%s %s\n",
				l.Pid, l.User, l.DurationS, l.State, truncate(l.Query, 70))
		}
		w.Flush()
	}

	if len(qa.Tables) > 0 {
		fmt.Fprintln(w, "Largest tables:")
		fmt.Fprintln(w, "  TABLE\tSIZE\tHIT%\tSEQ/IDX\tDEAD%\tLAST ANALYZE")
		for _, t := range qa.Tables {
			hit := 0.0
			if tot := t.BlksHit + t.BlksRead; tot > 0 {
				hit = float64(t.BlksHit) / float64(tot) * 100
			}
			last := "never"
			if t.LastAutoAnalyze != nil {
				last = humanAge(*t.LastAutoAnalyze)
			} else if t.LastAnalyze != nil {
				last = humanAge(*t.LastAnalyze)
			}
			fmt.Fprintf(w, "  %s.%s\t%s\t%.1f%%\t%d/%d\t%.1f%%\t%s\n",
				t.Schema, t.Table, fmtBytes(uint64(t.SizeBytes)), hit,
				t.SeqScan, t.IdxScan, t.DeadPct*100, last)
		}
		w.Flush()
	}

	if len(qa.Findings) > 0 {
		fmt.Fprintln(w, "Findings & recommendations:")
		for _, f := range qa.Findings {
			where := ""
			if f.QueryID != 0 {
				where = fmt.Sprintf(" [query #%d]", f.QueryID)
			} else if f.Table != "" && !strings.Contains(f.Detail, f.Table) {
				where = " [" + f.Table + "]"
			}
			fmt.Fprintf(w, "  ! %s%s - %s\n", f.Detail, where, f.Suggestion)
			if f.IndexDDL != "" {
				if est, total, ok := indexSelectivity(&f, qa.Tables); ok {
					pct := est / total * 100
					if pct <= 10 {
						fmt.Fprintf(w, "      index: %s\n", f.IndexDDL)
						fmt.Fprintf(w, "      matches ~%.0f of %.0f rows (%.3f%% of table) - create it with -apply-index\n", est, total, pct)
					} else {
						fmt.Fprintf(w, "      matches ~%.0f of %.0f rows (%.1f%% of table) - low selectivity, index may not help\n", est, total, pct)
					}
				} else {
					fmt.Fprintf(w, "      index: %s\n", f.IndexDDL)
				}
			}
		}
		w.Flush()
	} else {
		fmt.Fprintln(w, "No problems detected in the current workload.")
		w.Flush()
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
