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

// collectSysStats gathers system facts for the tuning plan. System stats are
// read from the database server host itself through pg_read_file - the same
// server-side file access a local postgres user's psql session has - so the
// plan stays correct even when the connection is remote. pg_read_file is
// superuser-only; roles without it fall back to this client's own /proc
// (authoritative for local connections, warned about otherwise). -ram always
// wins over both.
func collectSysStats(ctx context.Context, conn *pgx.Conn, opts Options) (*SysStats, error) {
	sys := &SysStats{NumCPU: runtime.NumCPU()}

	// data_directory is only readable by superusers; when the connected role
	// cannot read it, leave the data dir unknown and let disk detection fall
	// back to scanning /sys/block (no platform-specific default path).
	var dd string
	if err := conn.QueryRow(ctx, "SELECT setting FROM pg_settings WHERE name = 'data_directory'").Scan(&dd); err == nil && dd != "" {
		sys.DataDir = dd
	}

	if opts.Ram != "" {
		ram, err := parseRam(opts.Ram)
		if err != nil {
			return nil, fmt.Errorf("-ram %q: %w", opts.Ram, err)
		}
		sys.TotalMemBytes = ram
	}

	serverSide := false
	if opts.Ram == "" {
		if content, err := serverFile(ctx, conn, "/proc/meminfo"); err == nil {
			if kb, err := memTotalFrom(content); err == nil {
				sys.TotalMemBytes = kb * 1024
				serverSide = true
			}
		}
	}
	if content, err := serverFile(ctx, conn, "/proc/cpuinfo"); err == nil {
		if n := cpuCountFrom(content); n > 0 {
			sys.NumCPU = n
			serverSide = true
		}
	}
	if !serverSide {
		if opts.Ram == "" {
			if kb, err := readMemTotalKB(); err == nil {
				sys.TotalMemBytes = kb * 1024
			} else {
				fmt.Fprintf(os.Stderr, "warning: cannot read /proc/meminfo (%v) - assuming 8 GiB RAM; use -ram to override\n", err)
				sys.TotalMemBytes = 8 * GiB
			}
		}
		if !localHost(opts.Host) {
			fmt.Fprintf(os.Stderr, "warning: system stats (RAM/CPU) read from the client host, not the database server (pg_read_file needs superuser) - run on the server host or pass -ram for accurate tuning\n")
		}
	}

	if sys.DiskType = detectDiskTypeServer(ctx, conn, sys.DataDir); sys.DiskType == "" {
		sys.DiskType = detectDiskType(sys.DataDir) // client-side fallback
	}
	return sys, nil
}

// serverFile reads a file on the database server host through pg_read_file,
// exactly like a local postgres user's psql session could. Only superusers
// can execute pg_read_file; restricted roles get an error here and callers
// fall back to client-side reads.
func serverFile(ctx context.Context, conn *pgx.Conn, path string) (string, error) {
	var content string
	if err := conn.QueryRow(ctx, "SELECT pg_read_file($1)", path).Scan(&content); err != nil {
		return "", err
	}
	return content, nil
}

// memTotalFrom parses MemTotal (in kB) from /proc/meminfo content.
func memTotalFrom(content string) (uint64, error) {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				return strconv.ParseUint(f[1], 10, 64)
			}
		}
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// cpuCountFrom counts the "processor" entries in /proc/cpuinfo content.
func cpuCountFrom(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

// localHost reports whether the connection target is the machine this process
// runs on (unix socket or loopback) - i.e. this client's /proc reads describe
// the database server. A socket path (leading "/") is always local.
func localHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "/") {
		return true
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

var ramSuffixes = []struct {
	suffix string
	mult   uint64
}{
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

// parseRam converts a -ram value ("32GB", "16384MB", "16777216") to bytes.
func parseRam(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty value")
	}
	upper := strings.ToUpper(s)
	for _, rs := range ramSuffixes {
		if strings.HasSuffix(upper, rs.suffix) {
			num := strings.TrimSpace(s[:len(s)-len(rs.suffix)])
			v, err := strconv.ParseUint(num, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("cannot parse %q (use e.g. 32GB, 16384MB, or plain bytes)", s)
			}
			return v * rs.mult, nil
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot parse %q (use e.g. 32GB, 16384MB, or plain bytes)", s)
	}
	return v, nil
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
	return memTotalFrom(string(b))
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
	return rotationalFromContent(string(b))
}

// rotationalFromContent parses a /sys/block/<dev>/queue/rotational file
// (0 = SSD, 1 = HDD).
func rotationalFromContent(content string) (int, bool) {
	switch strings.TrimSpace(content) {
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
	return deviceForPathFromMounts(path, string(mounts))
}

// deviceForPathFromMounts maps a path to its backing device name (e.g.
// "nvme0n1") from /proc/mounts content. Returns "" when it cannot be resolved.
func deviceForPathFromMounts(path, mountsContent string) string {
	bestDev, bestLen := "", 0
	for _, line := range strings.Split(mountsContent, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		dev, mnt := f[0], f[1]
		// match on a component boundary ("/var/lib" must not claim
		// "/var/libx"); the root mount "/" legitimately matches everything
		if mnt != "/" {
			if !strings.HasPrefix(path, mnt+"/") && path != mnt {
				continue
			}
		}
		if len(mnt) <= bestLen {
			continue // longest mount wins
		}
		if !strings.HasPrefix(dev, "/dev/") {
			continue // skip overlay, tmpfs, proc, ...
		}
		bestDev, bestLen = dev, len(mnt)
	}
	if bestDev == "" {
		return ""
	}
	return deviceName(bestDev)
}

// deviceName strips the /dev/ prefix and any partition suffix from a block
// device path: /dev/sda1 -> sda, /dev/nvme0n1p1 -> nvme0n1,
// /dev/mmcblk0p1 -> mmcblk0. Unrecognized shapes pass through unchanged
// (the subsequent /sys/block lookup simply fails for them).
func deviceName(dev string) string {
	base := strings.TrimPrefix(dev, "/dev/")
	if i := strings.IndexAny(base, "0123456789"); i > 0 {
		stem := base[:i]
		if strings.HasPrefix(stem, "sd") || strings.HasPrefix(stem, "vd") || strings.HasPrefix(stem, "xvd") {
			return stem
		}
	}
	if strings.HasPrefix(base, "nvme") || strings.HasPrefix(base, "mmc") {
		// nvme0n1p1 / mmcblk0p1: cut the trailing "pN" partition suffix
		if i := strings.LastIndex(base, "p"); i > 0 && isDigits(base[i+1:]) {
			return base[:i]
		}
		return base
	}
	return base
}

// isDigits reports whether s is non-empty and all decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// detectDiskTypeServer resolves the storage class from the server host itself
// (via pg_read_file), so remote connections get the right ssd/hdd assumption.
// Returns "" when the server-side files are unreadable (non-superuser) or the
// data directory cannot be mapped to a device; callers fall back to the
// client-side detection.
func detectDiskTypeServer(ctx context.Context, conn *pgx.Conn, dataDir string) string {
	mounts, err := serverFile(ctx, conn, "/proc/mounts")
	if err != nil {
		return ""
	}
	dev := deviceForPathFromMounts(dataDir, mounts)
	if dev == "" {
		return ""
	}
	if content, err := serverFile(ctx, conn, "/sys/block/"+dev+"/queue/rotational"); err == nil {
		if r, ok := rotationalFromContent(content); ok {
			if r == 0 {
				return "ssd"
			}
			return "hdd"
		}
	}
	return ""
}
