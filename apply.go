package main

import (
	"context"
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
