package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PerDB is one database's snapshot: live stats and query analysis. SkipReason
// is set when the database could not be analyzed (e.g. no CONNECT privilege).
type PerDB struct {
	DB         *DBStats
	QA         *QueryAnalysis
	SkipReason string
}

// dsnFor builds a connection string for the given database.
func dsnFor(opts Options, db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		url.QueryEscape(opts.User), url.QueryEscape(opts.Password), opts.Host, opts.Port, url.QueryEscape(db), opts.SslMode)
}

// listDatabases returns the names of non-template, connectable databases.
// Templates (template0/template1) are excluded; the maintenance database
// "postgres" is included like any other.
func listDatabases(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx, `
		SELECT datname FROM pg_database
		WHERE datistemplate = false AND datallowconn = true
		ORDER BY datname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// timeoutReason converts a context deadline into an actionable skip reason;
// other errors pass through verbatim.
func timeoutReason(opts Options, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s - server busy or unresponsive; raise with -timeout", opts.Timeout)
	}
	return err.Error()
}

// collectAllDBs analyzes every database on the server. The primary database
// (the one the process is connected to) comes first and reuses the already
// collected stats; the others get their own connections. Databases that
// cannot be connected to are recorded with a SkipReason, not errors.
// Collection stays serial; each database gets its own -timeout budget, so a
// slow database skips itself (with a truthful reason) without starving the rest.
func collectAllDBs(ctx context.Context, conn *pgx.Conn, opts Options, primaryDB *DBStats, primaryQA *QueryAnalysis) []*PerDB {
	names, err := listDatabases(ctx, conn)
	if err != nil {
		return []*PerDB{{SkipReason: "cannot list databases: " + err.Error()}}
	}
	if len(names) == 0 {
		return []*PerDB{{SkipReason: "no connectable databases found"}}
	}
	// primary first, then alphabetical
	sort.Strings(names)
	ordered := []string{primaryDB.Database}
	for _, n := range names {
		if n != primaryDB.Database {
			ordered = append(ordered, n)
		}
	}

	out := []*PerDB{}
	for _, name := range ordered {
		if name == primaryDB.Database {
			out = append(out, &PerDB{DB: primaryDB, QA: primaryQA})
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, opts.Timeout)
		c, err := pgx.Connect(dctx, dsnFor(opts, name))
		if err != nil {
			cancel()
			out = append(out, &PerDB{DB: &DBStats{Database: name}, SkipReason: timeoutReason(opts, err)})
			continue
		}
		db, err := collectDBStats(dctx, c)
		if err != nil {
			c.Close(dctx)
			cancel()
			out = append(out, &PerDB{DB: &DBStats{Database: name}, SkipReason: "stats: " + timeoutReason(opts, err)})
			continue
		}
		qa, err := collectQueryAnalysis(dctx, c, opts, db)
		c.Close(dctx)
		cancel()
		if err != nil {
			out = append(out, &PerDB{DB: db, SkipReason: "query analysis: " + timeoutReason(opts, err)})
			continue
		}
		out = append(out, &PerDB{DB: db, QA: qa})
	}
	return out
}

// aggregateDBStats merges per-database counters into an instance-wide view.
// Fields that are instance-level (uptime, checkpoints, buffers, active
// connections, settings, superuser status) come from the primary database;
// everything in pg_stat_database is summed across databases.
func aggregateDBStats(perDBs []*PerDB) *DBStats {
	if len(perDBs) == 0 {
		return &DBStats{Settings: map[string]Setting{}}
	}
	agg := &DBStats{Settings: map[string]Setting{}}
	names := []string{}
	for _, p := range perDBs {
		if p.SkipReason != "" || p.DB == nil {
			continue
		}
		d := p.DB
		if agg.Version == "" {
			agg.Version = d.Version
			agg.UptimeSec = d.UptimeSec
			agg.CheckpointsTimed = d.CheckpointsTimed
			agg.CheckpointsReq = d.CheckpointsReq
			agg.BuffersCheckpoint = d.BuffersCheckpoint
			agg.BuffersClean = d.BuffersClean
			agg.MaxwrittenClean = d.MaxwrittenClean
			agg.ActiveConns = d.ActiveConns
			agg.IsSuperuser = d.IsSuperuser
			agg.Settings = d.Settings
		}
		agg.Commits += d.Commits
		agg.Rollbacks += d.Rollbacks
		agg.BlksHit += d.BlksHit
		agg.BlksRead += d.BlksRead
		agg.Deadlocks += d.Deadlocks
		agg.TempFiles += d.TempFiles
		agg.TempBytes += d.TempBytes
		agg.BlkReadTimeMS += d.BlkReadTimeMS
		agg.BlkWriteTimeMS += d.BlkWriteTimeMS
		agg.SeqScans += d.SeqScans
		agg.IdxScans += d.IdxScans
		agg.DatabaseSizeBytes += d.DatabaseSizeBytes
		names = append(names, fmt.Sprintf("%s (%s)", d.Database, fmtBytes(uint64(d.DatabaseSizeBytes))))
	}
	if agg.Database == "" {
		agg.Database = strings.Join(names, ", ")
	}
	return agg
}

// aggregateQA unions the findings of all analyzed databases so the instance
// tuning plan sees evidence (disk sorts, temp spills, bloat, hit ratios)
// gathered from every database, not just the primary one.
func aggregateQA(perDBs []*PerDB) *QueryAnalysis {
	agg := &QueryAnalysis{Available: true}
	seen := map[string]bool{}
	notes := []string{}
	for _, p := range perDBs {
		if p.QA == nil {
			continue
		}
		for _, f := range p.QA.Findings {
			key := f.Kind + "|" + f.Detail
			if !seen[key] {
				seen[key] = true
				agg.Findings = append(agg.Findings, f)
			}
		}
		if p.QA.Note != "" && p.QA.Available && !p.QA.RestartNeeded {
			notes = append(notes, p.DB.Database+": "+p.QA.Note)
		}
	}
	agg.Note = strings.Join(notes, "; ")
	return agg
}
