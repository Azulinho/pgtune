// pgtune - connect to PostgreSQL, gather stats, compute a tuning plan,
// and apply it to the server configuration (ALTER SYSTEM + reload).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

type Options struct {
	Host           string
	Port           int
	User           string
	Password       string
	DB             string
	Workload       string
	Apply          bool
	Restart        bool
	UseSudo        bool
	TopN           int
	ExplainN       int
	LongRunningSec float64
	TablesN        int
	InstallPgss    bool
	Analyze        bool
	ApplyIndex     bool
	AllDBs         bool
}

func main() {
	opts := Options{}
	flag.StringVar(&opts.Host, "host", "127.0.0.1", "PostgreSQL host")
	flag.IntVar(&opts.Port, "port", 6432, "PostgreSQL port (6432 = pgbouncer, 5432 = direct)")
	flag.StringVar(&opts.User, "user", "app", "PostgreSQL user")
	flag.StringVar(&opts.Password, "password", "app", "PostgreSQL password")
	flag.StringVar(&opts.DB, "db", "app", "Database to analyze")
	flag.StringVar(&opts.Workload, "workload", "oltp", "Workload type: oltp, olap, mixed, desktop")
	flag.BoolVar(&opts.Apply, "apply", false, "Apply the tuning plan (ALTER SYSTEM + reload)")
	flag.BoolVar(&opts.Restart, "restart", false, "Restart postgres (via sudo systemctl) if the plan requires it")
	flag.BoolVar(&opts.UseSudo, "sudo", true, "Use `sudo -u postgres psql` to apply changes when the connected user lacks superuser rights")
	flag.IntVar(&opts.TopN, "top", 8, "number of top queries to report from pg_stat_statements")
	flag.IntVar(&opts.ExplainN, "explain", 3, "how many top queries to EXPLAIN (0 disables)")
	flag.Float64Var(&opts.LongRunningSec, "long-running", 5.0, "flag queries running longer than this many seconds")
	flag.IntVar(&opts.TablesN, "tables", 10, "number of largest tables to analyze")
	flag.BoolVar(&opts.InstallPgss, "install-pgss", true, "Install pg_stat_statements (extension + preload) when missing")
	flag.BoolVar(&opts.Analyze, "analyze", false, "Run ANALYZE when stale statistics are detected (apply mode)")
	flag.BoolVar(&opts.ApplyIndex, "apply-index", false, "Create indexes recommended by seq-scan findings (CONCURRENTLY, skips unselective ones)")
	flag.BoolVar(&opts.AllDBs, "all-dbs", false, "Analyze every database on the server (per-database recommendations)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		url.QueryEscape(opts.User), url.QueryEscape(opts.Password), opts.Host, opts.Port, url.QueryEscape(opts.DB))

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot connect to %s:%d as %s: %v\n", opts.Host, opts.Port, opts.User, err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	sys, err := collectSysStats(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: system stats: %v\n", err)
		os.Exit(1)
	}
	db, err := collectDBStats(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database stats: %v\n", err)
		os.Exit(1)
	}

	wl, err := parseWorkload(opts.Workload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	qa, err := collectQueryAnalysis(ctx, conn, opts, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query analysis: %v\n", err)
		os.Exit(1)
	}

	if opts.AllDBs {
		runAllDBs(ctx, conn, opts, sys, db, qa)
		return
	}

	plan := buildPlan(sys, db, wl, qa)
	printReport(sys, db, wl, plan, qa)

	if opts.Apply {
		var reasons []string

		r, err := applySettings(ctx, conn, db, plan, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: apply: %v\n", err)
			os.Exit(1)
		}
		reasons = append(reasons, r...)

		if opts.InstallPgss {
			r, err = applyPgss(ctx, conn, opts, db.Database)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: pg_stat_statements: %v\n", err)
				os.Exit(1)
			}
			reasons = append(reasons, r...)
		}

		if opts.Analyze {
			if err := applyAnalyze(ctx, opts, db.Database, qa); err != nil {
				fmt.Fprintf(os.Stderr, "error: analyze: %v\n", err)
				os.Exit(1)
			}
		}

		if err := doRestart(ctx, opts, reasons); err != nil {
			fmt.Fprintf(os.Stderr, "error: restart: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Println("\nRun with -apply to apply these changes (ALTER SYSTEM + reload).")
		if qa != nil && qa.RestartNeeded {
			fmt.Println("note: pg_stat_statements needs -apply -restart once to start capturing query stats.")
		}
	}

	if opts.ApplyIndex {
		if err := applyIndexes(ctx, conn, opts, opts.DB, qa); err != nil {
			fmt.Fprintf(os.Stderr, "error: index: %v\n", err)
			os.Exit(1)
		}
	}
}

// runAllDBs analyzes every database on the server, prints one instance-level
// plan (evidence aggregated across databases) and per-database deep dives, and
// applies changes: settings are instance-wide, ANALYZE and index creation run
// against each database that needs them.
func runAllDBs(ctx context.Context, conn *pgx.Conn, opts Options, sys *SysStats, primaryDB *DBStats, primaryQA *QueryAnalysis) {
	wl, err := parseWorkload(opts.Workload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	perDBs := collectAllDBs(ctx, conn, opts, primaryDB, primaryQA)
	agg := aggregateDBStats(perDBs)
	aggQA := aggregateQA(perDBs)
	plan := buildPlan(sys, agg, wl, aggQA)
	printAllReport(sys, agg, wl, plan, perDBs)

	// per-database connections for apply steps (the primary connection is
	// reused only for the instance-wide settings)
	connectDB := func(name string) (*pgx.Conn, bool) {
		c, err := pgx.Connect(ctx, dsnFor(opts, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot connect to %s for apply: %v\n", name, err)
			return nil, false
		}
		return c, true
	}

	if opts.Apply {
		reasons, err := applySettings(ctx, conn, agg, plan, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: apply: %v\n", err)
			os.Exit(1)
		}

		if opts.InstallPgss {
			for _, p := range perDBs {
				if p.SkipReason != "" || p.DB == nil {
					continue
				}
				c, ok := connectDB(p.DB.Database)
				if !ok {
					continue
				}
				r, err := applyPgss(ctx, c, opts, p.DB.Database)
				c.Close(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: pg_stat_statements on %s: %v\n", p.DB.Database, err)
					continue
				}
				reasons = append(reasons, r...)
			}
		}

		if opts.Analyze {
			for _, p := range perDBs {
				if p.SkipReason != "" || p.DB == nil {
					continue
				}
				if err := applyAnalyze(ctx, opts, p.DB.Database, p.QA); err != nil {
					fmt.Fprintf(os.Stderr, "warning: ANALYZE on %s: %v\n", p.DB.Database, err)
				}
			}
		}

		if err := doRestart(ctx, opts, dedupe(reasons)); err != nil {
			fmt.Fprintf(os.Stderr, "error: restart: %v\n", err)
			os.Exit(1)
		}
	} else {
		fmt.Println("\nRun with -apply to apply the instance plan (ALTER SYSTEM + reload); -analyze / -apply-index apply per database.")
		if aggQA != nil && aggQA.RestartNeeded {
			fmt.Println("note: pg_stat_statements needs -apply -restart once to start capturing query stats.")
		}
	}

	if opts.ApplyIndex {
		for _, p := range perDBs {
			if p.SkipReason != "" || p.DB == nil {
				continue
			}
			c, ok := connectDB(p.DB.Database)
			if !ok {
				continue
			}
			if err := applyIndexes(ctx, c, opts, p.DB.Database, p.QA); err != nil {
				fmt.Fprintf(os.Stderr, "error: index on %s: %v\n", p.DB.Database, err)
			}
			c.Close(ctx)
		}
	}
}

func dedupe(s []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
