package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// SysStats describes the host running PostgreSQL.
type SysStats struct {
	TotalMemBytes uint64
	NumCPU        int
	DiskType      string // "ssd", "hdd" or "unknown"
	DataDir       string
}

// Setting is one row of pg_settings.
type Setting struct {
	Name           string
	Value          string // value in the parameter's base unit
	Unit           string
	Ctx            string
	VarType        string
	Source         string
	PendingRestart bool
}

// DBStats is a snapshot of live PostgreSQL statistics.
type DBStats struct {
	Version           string
	UptimeSec         float64
	Database          string
	DatabaseSizeBytes int64
	Commits           int64
	Rollbacks         int64
	BlksHit           int64
	BlksRead          int64
	Deadlocks         int64
	TempFiles         int64
	TempBytes         int64
	BlkReadTimeMS     float64
	BlkWriteTimeMS    float64
	CheckpointsTimed  int64
	CheckpointsReq    int64
	BuffersCheckpoint int64 // buffers_written by checkpoints
	BuffersClean      int64
	MaxwrittenClean   int64
	SeqScans          int64
	IdxScans          int64
	ActiveConns       int64
	IsSuperuser       bool
	Settings          map[string]Setting
}

func collectSysStats(ctx context.Context, conn *pgx.Conn) (*SysStats, error) {
	sys := &SysStats{NumCPU: runtime.NumCPU()}
	if kb, err := readMemTotalKB(); err == nil {
		sys.TotalMemBytes = kb * 1024
	} else {
		sys.TotalMemBytes = 8 * GiB
	}

	sys.DataDir = "/var/lib/postgresql"
	var dd string
	if err := conn.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name = 'data_directory'").Scan(&dd); err == nil && dd != "" {
		sys.DataDir = dd
	}
	sys.DiskType = detectDiskType(sys.DataDir)
	return sys, nil
}

func collectDBStats(ctx context.Context, conn *pgx.Conn) (*DBStats, error) {
	db := &DBStats{Settings: map[string]Setting{}}

	if err := conn.QueryRow(ctx,
		"SELECT version(), extract(epoch FROM now() - pg_postmaster_start_time())").
		Scan(&db.Version, &db.UptimeSec); err != nil {
		return nil, fmt.Errorf("server info: %w", err)
	}

	if err := conn.QueryRow(ctx,
		"SELECT current_database(), pg_database_size(current_database())").
		Scan(&db.Database, &db.DatabaseSizeBytes); err != nil {
		return nil, fmt.Errorf("database size: %w", err)
	}

	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(xact_commit,0), COALESCE(xact_rollback,0),
		       COALESCE(blks_hit,0), COALESCE(blks_read,0),
		       COALESCE(deadlocks,0), COALESCE(temp_files,0), COALESCE(temp_bytes,0),
		       COALESCE(blk_read_time,0), COALESCE(blk_write_time,0)
		FROM pg_stat_database WHERE datname = current_database()`).
		Scan(&db.Commits, &db.Rollbacks, &db.BlksHit, &db.BlksRead, &db.Deadlocks,
			&db.TempFiles, &db.TempBytes, &db.BlkReadTimeMS, &db.BlkWriteTimeMS); err != nil {
		return nil, fmt.Errorf("pg_stat_database: %w", err)
	}

	// PG 17+: checkpoint stats live in pg_stat_checkpointer (num_timed/num_requested,
	// buffers_written); older servers keep them in pg_stat_bgwriter with the
	// checkpoints_timed/checkpoints_req names.
	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(num_timed,0), COALESCE(num_requested,0), COALESCE(buffers_written,0)
		FROM pg_stat_checkpointer`).
		Scan(&db.CheckpointsTimed, &db.CheckpointsReq, &db.BuffersCheckpoint); err != nil {
		_ = conn.QueryRow(ctx, `
			SELECT COALESCE(checkpoints_timed,0), COALESCE(checkpoints_req,0),
			       COALESCE(buffers_checkpoint,0)
			FROM pg_stat_bgwriter`).
			Scan(&db.CheckpointsTimed, &db.CheckpointsReq, &db.BuffersCheckpoint)
	}

	if err := conn.QueryRow(ctx, `
		SELECT COALESCE(buffers_clean,0), COALESCE(maxwritten_clean,0)
		FROM pg_stat_bgwriter`).
		Scan(&db.BuffersClean, &db.MaxwrittenClean); err != nil {
		return nil, fmt.Errorf("pg_stat_bgwriter: %w", err)
	}

	if err := conn.QueryRow(ctx,
		"SELECT COALESCE(SUM(seq_scan),0)::bigint, COALESCE(SUM(idx_scan),0)::bigint FROM pg_stat_user_tables").
		Scan(&db.SeqScans, &db.IdxScans); err != nil {
		return nil, fmt.Errorf("scan stats: %w", err)
	}

	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity").Scan(&db.ActiveConns); err != nil {
		return nil, fmt.Errorf("pg_stat_activity: %w", err)
	}

	// ALTER SYSTEM needs superuser; detect whether the current role can do it.
	if err := conn.QueryRow(ctx,
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&db.IsSuperuser); err != nil {
		db.IsSuperuser = false
	}

	rows, err := conn.Query(ctx, `
		SELECT name, setting, COALESCE(unit, ''), context, vartype, source, pending_restart FROM pg_settings`)
	if err != nil {
		return nil, fmt.Errorf("pg_settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Name, &s.Value, &s.Unit, &s.Ctx, &s.VarType, &s.Source, &s.PendingRestart); err != nil {
			return nil, err
		}
		db.Settings[s.Name] = s
	}
	return db, rows.Err()
}

func settingValue(s map[string]Setting, name, def string) string {
	if v, ok := s[name]; ok {
		return v.Value
	}
	return def
}

func readMemTotalKB() (uint64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return strconv.ParseUint(f[1], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// detectDiskType resolves the block device that hosts the data directory and
// reports whether it is rotational (HDD) or not (SSD). Falls back to scanning
// all block devices when the mapping is not resolvable (e.g. overlay mounts).
func detectDiskType(dataDir string) string {
	if dev := deviceForPath(dataDir); dev != "" {
		if r, ok := rotationalOf(dev); ok {
			if r == 0 {
				return "ssd"
			}
			return "hdd"
		}
	}
	hasSSD, hasHDD := false, false
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return "unknown"
	}
	for _, e := range entries {
		if r, ok := rotationalOf(e.Name()); ok {
			if r == 0 {
				hasSSD = true
			} else {
				hasHDD = true
			}
		}
	}
	switch {
	case hasSSD && !hasHDD:
		return "ssd"
	case hasHDD && !hasSSD:
		return "hdd"
	case hasSSD && hasHDD:
		return "ssd" // mixed storage: prefer SSD assumptions
	default:
		return "unknown"
	}
}

func rotationalOf(name string) (int, bool) {
	b, err := os.ReadFile("/sys/block/" + name + "/queue/rotational")
	if err != nil {
		return 0, false
	}
	switch strings.TrimSpace(string(b)) {
	case "0":
		return 0, true
	case "1":
		return 1, true
	}
	return 0, false
}

// deviceForPath maps a path to its backing device name (e.g. "nvme0n1")
// using /proc/mounts. Returns "" when it cannot be resolved.
func deviceForPath(path string) string {
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return ""
	}
	bestDev, bestLen := "", 0
	for _, line := range strings.Split(string(mounts), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		dev, mnt := f[0], f[1]
		if !strings.HasPrefix(path, mnt) || len(mnt) <= bestLen {
			continue
		}
		if !strings.HasPrefix(dev, "/dev/") {
			continue // skip overlay, tmpfs, proc, ...
		}
		bestDev, bestLen = dev, len(mnt)
	}
	if bestDev == "" {
		return ""
	}
	base := strings.TrimPrefix(bestDev, "/dev/")
	// strip partitions: nvme0n1p1 -> nvme0n1, sda1 -> sda
	if i := strings.IndexAny(base, "0123456789"); i > 0 {
		stem := base[:i]
		if strings.HasPrefix(stem, "sd") || strings.HasPrefix(stem, "vd") || strings.HasPrefix(stem, "xvd") {
			return stem
		}
		if strings.HasPrefix(stem, "nvme") || strings.HasPrefix(stem, "mmc") {
			// nvme0n1p1: cut before the trailing "pN" partition suffix
			if j := strings.LastIndex(stem, "p"); j > 0 {
				return stem[:j]
			}
			return stem
		}
	}
	return base
}
