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

	// An explicit -disk ssd/hdd wins over detection (useful for VMs and cloud
	// volumes whose virtual disk advertises rotational=1 despite sitting on
	// SSD/NVMe storage).
	if opts.Disk == "ssd" || opts.Disk == "hdd" {
		sys.DiskType = opts.Disk
		return sys, nil
	}

	// Disk detection runs on the database server host through pg_read_file /
	// pg_ls_dir. Only for a local connection is the client a valid fallback;
	// for a remote non-superuser, the client's disks describe a different
	// machine, so report unknown instead of guessing.
	sys.DiskType = detectDiskTypeServer(ctx, conn, sys.DataDir)
	if sys.DiskType == "" || (sys.DiskType == "unknown" && localHost(opts.Host)) {
		if local := detectDiskType(sys.DataDir); local != "" {
			sys.DiskType = local
		}
	}
	if sys.DiskType == "" {
		sys.DiskType = "unknown"
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

// serverListDir lists a directory on the database server host through
// pg_ls_dir (superuser-only, like pg_read_file). Used to enumerate
// /sys/block and its slaves/ directories.
func serverListDir(ctx context.Context, conn *pgx.Conn, path string) ([]string, error) {
	rows, err := conn.Query(ctx, "SELECT pg_ls_dir($1)", path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
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

// parseDiskOverride validates a -disk value. "auto" (or empty) keeps
// detection; "ssd"/"hdd" force the storage class regardless of sysfs.
func parseDiskOverride(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return "auto", nil
	case "ssd":
		return "ssd", nil
	case "hdd":
		return "hdd", nil
	}
	return "", fmt.Errorf("unknown disk type %q (want auto, ssd or hdd)", s)
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

// sysFS abstracts the /sys filesystem so the same storage-detection logic can
// run on the local client (os.ReadFile/os.ReadDir) or on the database server
// host (pg_read_file/pg_ls_dir). Paths are relative to /sys.
type sysFS interface {
	ReadFile(path string) (string, error)
	ListDir(path string) ([]string, error)
}

type localSysFS struct{}

func (localSysFS) ReadFile(path string) (string, error) {
	b, err := os.ReadFile("/sys/" + path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (localSysFS) ListDir(path string) ([]string, error) {
	entries, err := os.ReadDir("/sys/" + path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

type serverSysFS struct {
	ctx  context.Context
	conn *pgx.Conn
}

func (s serverSysFS) ReadFile(path string) (string, error) {
	return serverFile(s.ctx, s.conn, "/sys/"+path)
}

func (s serverSysFS) ListDir(path string) ([]string, error) {
	return serverListDir(s.ctx, s.conn, "/sys/"+path)
}

// mountEntry is one row of /proc/mounts.
type mountEntry struct {
	source string
	point  string
	fstype string
	opts   string
}

// parseMounts parses /proc/mounts content. Octal escapes in the device and
// mount-point fields (spaces, tabs, newlines, backslashes) are decoded.
func parseMounts(content string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		out = append(out, mountEntry{
			source: unescapeMountField(f[0]),
			point:  unescapeMountField(f[1]),
			fstype: f[2],
			opts:   f[3],
		})
	}
	return out
}

func unescapeMountField(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// mountForPath returns the most specific mount holding path. Root ("/")
// matches everything; other mount points match on a component boundary
// ("/var/lib" must not claim "/var/libx").
func mountForPath(path string, mounts []mountEntry) (mountEntry, bool) {
	if !strings.HasPrefix(path, "/") {
		return mountEntry{}, false
	}
	var best mountEntry
	bestLen := -1
	for _, m := range mounts {
		if m.point != "/" && path != m.point && !strings.HasPrefix(path, m.point+"/") {
			continue
		}
		if len(m.point) > bestLen {
			best, bestLen = m, len(m.point)
		}
	}
	if bestLen < 0 {
		return mountEntry{}, false
	}
	return best, true
}

// overlayUpperDir extracts upperdir= from an overlay mount's options.
func overlayUpperDir(opts string) string {
	for _, o := range strings.Split(opts, ",") {
		if strings.HasPrefix(o, "upperdir=") {
			return unescapeMountField(strings.TrimPrefix(o, "upperdir="))
		}
	}
	return ""
}

// isNetworkFS reports filesystems whose backing device cannot be described by
// a local /sys/block entry.
func isNetworkFS(fstype string) bool {
	switch fstype {
	case "nfs", "nfs4", "cifs", "smbfs", "ceph", "glusterfs", "9p",
		"afs", "lustre", "virtiofs":
		return true
	}
	return strings.HasPrefix(fstype, "nfs") ||
		strings.HasPrefix(fstype, "fuse") ||
		strings.HasPrefix(fstype, "smb")
}

const maxStorageDepth = 4

// storageClassForPath resolves path to a mount, then to the block device(s)
// behind it. It returns "" when no mount matches (the caller may fall back to
// scanning all devices) and "unknown" when a mount exists but its storage
// cannot be determined (overlay without upperdir, tmpfs, network, ...).
func storageClassForPath(fs sysFS, mounts []mountEntry, path string, depth int) string {
	if depth > maxStorageDepth {
		return "unknown"
	}
	m, ok := mountForPath(path, mounts)
	if !ok {
		return ""
	}
	switch m.fstype {
	case "overlay":
		upper := overlayUpperDir(m.opts)
		if upper == "" {
			return "unknown"
		}
		if c := storageClassForPath(fs, mounts, upper, depth+1); c != "" {
			return c
		}
		return "unknown"
	case "tmpfs", "ramfs", "devtmpfs":
		return "unknown"
	}
	if isNetworkFS(m.fstype) {
		return "unknown"
	}
	name, ok := deviceNameFromSource(fs, m.source)
	if !ok {
		return "unknown"
	}
	return storageClassForDevice(fs, name)
}

// deviceNameFromSource maps a /proc/mounts source to a /sys/block device name.
// It understands /dev/sda1, /dev/nvme0n1p1, /dev/dm-0, /dev/md0,
// /dev/mapper/<name> and /dev/<vg>/<lv> (LVM uses <vg>-<lv> as dm name).
func deviceNameFromSource(fs sysFS, source string) (string, bool) {
	base := strings.TrimPrefix(source, "/dev/")
	if base == source {
		return "", false
	}
	if strings.HasPrefix(base, "mapper/") {
		return dmDeviceByName(fs, strings.TrimPrefix(base, "mapper/"))
	}
	if strings.HasPrefix(base, "disk/") || strings.HasPrefix(base, "by-") {
		// /dev/disk/by-* symlinks need readlink, which the server-side
		// backend cannot do; treat as unresolved rather than guessing.
		return "", false
	}
	if strings.Contains(base, "/") {
		return dmDeviceByName(fs, strings.ReplaceAll(base, "/", "-"))
	}
	return deviceName(base), true
}

// dmDeviceByName finds the dm-N device whose /sys/block/dm-*/dm/name equals
// want (e.g. "vg-root").
func dmDeviceByName(fs sysFS, want string) (string, bool) {
	entries, err := fs.ListDir("block")
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e, "dm-") {
			continue
		}
		name, err := fs.ReadFile("block/" + e + "/dm/name")
		if err != nil {
			continue
		}
		if strings.TrimSpace(name) == want {
			return e, true
		}
	}
	return "", false
}

// storageClassForDevice follows device-mapper/md slaves down to the physical
// leaves and classifies their rotational flags.
func storageClassForDevice(fs sysFS, name string) string {
	return storageClassFromRotational(leafRotational(fs, name, map[string]bool{}))
}

func leafRotational(fs sysFS, name string, path map[string]bool) []int {
	if path[name] {
		return []int{-1} // cycle
	}
	path[name] = true
	defer delete(path, name)

	if slaves, err := fs.ListDir("block/" + name + "/slaves"); err == nil && len(slaves) > 0 {
		var vals []int
		for _, s := range slaves {
			vals = append(vals, leafRotational(fs, s, path)...)
		}
		return vals
	}
	if r, ok := rotationalOf(fs, name); ok {
		return []int{r}
	}
	return []int{-1}
}

// storageClassFromRotational maps rotational flags (0 = SSD, 1 = HDD, other /
// -1 = unknown) to a storage class. Mixed or partly unreadable devices are
// reported as unknown instead of assuming SSD.
func storageClassFromRotational(vals []int) string {
	if len(vals) == 0 {
		return "unknown"
	}
	sawSSD, sawHDD := false, false
	for _, v := range vals {
		switch v {
		case 0:
			sawSSD = true
		case 1:
			sawHDD = true
		default:
			return "unknown"
		}
	}
	switch {
	case sawSSD && !sawHDD:
		return "ssd"
	case sawHDD && !sawSSD:
		return "hdd"
	default:
		return "unknown"
	}
}

func rotationalOf(fs sysFS, name string) (int, bool) {
	content, err := fs.ReadFile("block/" + name + "/queue/rotational")
	if err != nil {
		return 0, false
	}
	return rotationalFromContent(content)
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

// scanAllDevices is the last-resort fallback when no mount maps the data
// directory. Virtual devices (loop, ram, zram, optical, floppy) are ignored;
// mixed or unreadable devices yield unknown.
func scanAllDevices(fs sysFS) string {
	entries, err := fs.ListDir("block")
	if err != nil {
		return "unknown"
	}
	var vals []int
	for _, e := range entries {
		if skipBlockDevice(e) {
			continue
		}
		vals = append(vals, leafRotational(fs, e, map[string]bool{})...)
	}
	return storageClassFromRotational(vals)
}

func skipBlockDevice(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "sr", "fd"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// detectDiskTypeWith classifies the storage behind dataDir from /proc/mounts
// content using the given /sys backend. Returns "ssd", "hdd" or "unknown".
func detectDiskTypeWith(fs sysFS, mountsContent, dataDir string) string {
	if dataDir != "" {
		if c := storageClassForPath(fs, parseMounts(mountsContent), dataDir, 0); c != "" {
			return c
		}
	}
	return scanAllDevices(fs)
}

// detectDiskType resolves the storage class on the client host.
func detectDiskType(dataDir string) string {
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return scanAllDevices(localSysFS{})
	}
	return detectDiskTypeWith(localSysFS{}, string(mounts), dataDir)
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
// (via pg_read_file/pg_ls_dir), so remote connections get the right ssd/hdd
// assumption. Returns "" when the server-side files are unreadable
// (non-superuser); callers then use the client-side fallback only for local
// connections.
func detectDiskTypeServer(ctx context.Context, conn *pgx.Conn, dataDir string) string {
	mounts, err := serverFile(ctx, conn, "/proc/mounts")
	if err != nil {
		return ""
	}
	return detectDiskTypeWith(serverSysFS{ctx: ctx, conn: conn}, mounts, dataDir)
}
