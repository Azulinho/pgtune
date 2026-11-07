// pgtune - connect to PostgreSQL, gather stats, compute a tuning plan,
// and apply it to the server configuration (ALTER SYSTEM + reload).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// version is stamped at build time via -ldflags "-X main.version=...";
// local builds report "dev".
var version = "dev"

type Options struct {
	Host           string
	Port           int
	User           string
	Password       string
	DB             string
	SslMode        string
	Workload       string
	Apply          bool
	TopN           int
	ExplainN       int
	ExplainTimeout time.Duration
	LongRunningSec float64
	Timeout        time.Duration
	TablesN        int
	InstallPgss    bool
	Analyze        bool
	ApplyIndex     bool
	AllDBs         bool
	Ram            string
	Disk           string
}

// envOr returns the named environment variable, or def when unset/empty.
// The standard libpq environment variables (PGHOST, PGPORT, PGUSER,
// PGPASSWORD, PGDATABASE, PGSSLMODE) are honored as flag defaults.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envPort(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// defaultUser mirrors psql: the connecting role defaults to the current OS
// user, which peer authentication over the local unix socket accepts without
// a password (running as the postgres OS user gives the postgres role).
func defaultUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "postgres"
}

func main() {
	opts := Options{}
	var timeoutSec int
	var explainTimeoutSec int
	flag.StringVar(&opts.Host, "host", envOr("PGHOST", ""), "PostgreSQL host (default: local unix socket, like psql)")
	flag.IntVar(&opts.Port, "port", envPort("PGPORT", 5432), "PostgreSQL port")
	flag.StringVar(&opts.User, "user", envOr("PGUSER", defaultUser()), "PostgreSQL user (default: current OS user, like psql)")
	flag.StringVar(&opts.Password, "password", envOr("PGPASSWORD", ""), "PostgreSQL password (also read from PGPASSWORD)")
	flag.StringVar(&opts.DB, "db", envOr("PGDATABASE", "postgres"), "Database to analyze")
	flag.StringVar(&opts.SslMode, "sslmode", envOr("PGSSLMODE", "prefer"), "SSL mode: disable, allow, prefer, require, verify-ca, verify-full")
	flag.StringVar(&opts.Ram, "ram", "", "Total RAM override, e.g. 32GB (default: read from /proc/meminfo)")
	flag.StringVar(&opts.Disk, "disk", "auto", "Storage type override: auto, ssd, hdd (default: detect the data directory's disk)")
	flag.StringVar(&opts.Workload, "workload", "oltp", "Workload type: oltp, olap, mixed, desktop")
	flag.BoolVar(&opts.Apply, "apply", false, "Apply the tuning plan (ALTER SYSTEM + reload)")
	flag.IntVar(&opts.TopN, "top", 8, "number of top queries to report from pg_stat_statements")
	flag.IntVar(&opts.ExplainN, "explain", 3, "how many top queries to EXPLAIN (0 disables)")
	flag.IntVar(&explainTimeoutSec, "explain-timeout", 5, "seconds allowed for EXPLAIN ANALYZE execution (0 = no limit)")
	flag.Float64Var(&opts.LongRunningSec, "long-running", 5.0, "flag queries running longer than this many seconds")
	flag.IntVar(&timeoutSec, "timeout", 60, "seconds allowed per database operation (connect, stats, query analysis, apply step)")
	flag.IntVar(&opts.TablesN, "tables", 10, "number of largest tables to analyze")
	flag.BoolVar(&opts.InstallPgss, "install-pgss", true, "Install pg_stat_statements (extension + preload) when missing")
	flag.BoolVar(&opts.Analyze, "analyze", false, "Run ANALYZE when stale statistics are detected (apply mode)")
	flag.BoolVar(&opts.ApplyIndex, "apply-index", false, "Create indexes recommended by seq-scan findings (CONCURRENTLY, skips unselective ones)")
	flag.BoolVar(&opts.AllDBs, "all-dbs", false, "Analyze every database on the server (per-database recommendations)")
	var showVersion bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("pgtune %s\n", version)
		os.Exit(0)
	}

	opts.Timeout = time.Duration(timeoutSec) * time.Second
	opts.ExplainTimeout = time.Duration(explainTimeoutSec) * time.Second
	if opts.Timeout <= 0 {
		fmt.Fprintf(os.Stderr, "error: -timeout must be > 0\n")
		os.Exit(1)
	}

	disk, err := parseDiskOverride(opts.Disk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	opts.Disk = disk

	ctx := context.Background()

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(opts.User), url.QueryEscape(opts.Password), opts.Host, opts.Port, url.QueryEscape(opts.DB), opts.SslMode)

	cctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	conn, err := pgx.Connect(cctx, dsn)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot connect to %s:%d as %s: %v\n", opts.Host, opts.Port, opts.User, timeoutReason(opts, err))
		os.Exit(1)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(cctx)
	}()

	dctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	sys, err := collectSysStats(dctx, conn, opts)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error: system stats: %v\n", timeoutReason(opts, err))
		os.Exit(1)
	}
	db, err := collectDBStats(dctx, conn)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error: database stats: %v\n", timeoutReason(opts, err))
		os.Exit(1)
	}

	wl, err := parseWorkload(opts.Workload)
	if err != nil {
		cancel()
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	qa, err := collectQueryAnalysis(dctx, conn, opts, db)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query analysis: %v\n", timeoutReason(opts, err))
		os.Exit(1)
	}

	if opts.AllDBs {
		runAllDBs(ctx, conn, opts, sys, db, qa)
		return
	}

	plan := buildPlan(sys, db, wl, qa)
	printReport(os.Stdout, sys, db, wl, plan, qa)

	if opts.Apply {
		var reasons []string

		actx, cancel := context.WithTimeout(ctx, opts.Timeout)
		r, err := applySettings(actx, conn, db, plan)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: apply: %v\n", timeoutReason(opts, err))
			os.Exit(1)
		}
		reasons = append(reasons, r...)

		if opts.InstallPgss {
			actx, cancel := context.WithTimeout(ctx, opts.Timeout)
			r, err = applyPgss(actx, conn)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: pg_stat_statements: %v\n", timeoutReason(opts, err))
				os.Exit(1)
			}
			reasons = append(reasons, r...)
		}

		if opts.Analyze {
			actx, cancel := context.WithTimeout(ctx, opts.Timeout)
			err = applyAnalyze(actx, conn, opts, db.Database, qa)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: analyze: %v\n", timeoutReason(opts, err))
				os.Exit(1)
			}
		}

		noteRestart(dedupe(reasons))
	} else {
		fmt.Println("\nRun with -apply to apply these changes (ALTER SYSTEM + reload).")
		if qa != nil && qa.RestartNeeded {
			fmt.Println("note: pg_stat_statements needs -apply once, plus a manual PostgreSQL restart, to start capturing query stats.")
		}
	}

	if opts.ApplyIndex {
		actx, cancel := context.WithTimeout(ctx, opts.Timeout)
		err := applyIndexes(actx, conn, qa)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: index: %v\n", timeoutReason(opts, err))
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
	printAllReport(os.Stdout, sys, agg, wl, plan, perDBs)

	// per-database connections for apply steps (the primary connection is
	// reused only for the instance-wide settings)
	connectDB := func(name string) (*pgx.Conn, bool) {
		actx, cancel := context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
		c, err := pgx.Connect(actx, dsnFor(opts, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot connect to %s for apply: %v\n", name, timeoutReason(opts, err))
			return nil, false
		}
		return c, true
	}

	if opts.Apply {
		actx, cancel := context.WithTimeout(ctx, opts.Timeout)
		reasons, err := applySettings(actx, conn, agg, plan)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: apply: %v\n", timeoutReason(opts, err))
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
				actx, cancel := context.WithTimeout(ctx, opts.Timeout)
				r, err := applyPgss(actx, c)
				c.Close(actx)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: pg_stat_statements on %s: %v\n", p.DB.Database, timeoutReason(opts, err))
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
				c, ok := connectDB(p.DB.Database)
				if !ok {
					continue
				}
				actx, cancel := context.WithTimeout(ctx, opts.Timeout)
				err := applyAnalyze(actx, c, opts, p.DB.Database, p.QA)
				c.Close(actx)
				cancel()
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: ANALYZE on %s: %v\n", p.DB.Database, timeoutReason(opts, err))
				}
			}
		}

		noteRestart(dedupe(reasons))
	} else {
		fmt.Println("\nRun with -apply to apply the instance plan (ALTER SYSTEM + reload); -analyze / -apply-index apply per database.")
		if aggQA != nil && aggQA.RestartNeeded {
			fmt.Println("note: pg_stat_statements needs -apply once, plus a manual PostgreSQL restart, to start capturing query stats.")
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
			actx, cancel := context.WithTimeout(ctx, opts.Timeout)
			err := applyIndexes(actx, c, p.QA)
			c.Close(actx)
			cancel()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: index on %s: %v\n", p.DB.Database, timeoutReason(opts, err))
			}
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
