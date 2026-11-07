package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Workload shapes the tuning ratios (classic pgtune formula families).
type Workload string

const (
	WorkloadOLTP    Workload = "oltp"
	WorkloadOLAP    Workload = "olap"
	WorkloadMixed   Workload = "mixed"
	WorkloadDesktop Workload = "desktop"
)

const (
	KiB = uint64(1 << 10)
	MiB = uint64(1 << 20)
	GiB = uint64(1 << 30)
	TiB = uint64(1 << 40)
)

func parseWorkload(s string) (Workload, error) {
	switch strings.ToLower(s) {
	case "oltp", "web", "transactional":
		return WorkloadOLTP, nil
	case "olap", "dw", "datawarehouse", "analytics":
		return WorkloadOLAP, nil
	case "mixed":
		return WorkloadMixed, nil
	case "desktop":
		return WorkloadDesktop, nil
	}
	return "", fmt.Errorf("unknown workload %q (want oltp, olap, mixed or desktop)", s)
}

type PlanItem struct {
	Name     string
	Current  string // humanized current value
	Proposed string // value to set, e.g. "2048MB", "0.9", "200"
	Context  string // pg_settings.context
	Changed  bool
	Restart  bool // needs postgres restart (context = postmaster)
	Reason   string

	kind    int
	targetB uint64
	targetF float64
	targetI int64
}

const (
	kindBytes = iota
	kindFloat
	kindInt
)

func fmtBytes(b uint64) string {
	switch {
	case b >= TiB && b%TiB == 0:
		return fmt.Sprintf("%dTB", b/TiB)
	case b >= TiB:
		return fmt.Sprintf("%.1fTB", float64(b)/float64(TiB))
	case b >= GiB && b%GiB == 0:
		return fmt.Sprintf("%dGB", b/GiB)
	case b >= GiB:
		return fmt.Sprintf("%.1fGB", float64(b)/float64(GiB))
	case b >= MiB && b%MiB == 0:
		return fmt.Sprintf("%dMB", b/MiB)
	case b >= MiB:
		return fmt.Sprintf("%.1fMB", float64(b)/float64(MiB))
	case b >= KiB && b%KiB == 0:
		return fmt.Sprintf("%dKB", b/KiB)
	case b >= KiB:
		return fmt.Sprintf("%.1fKB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// parseBytes converts a pg_settings value (in its base unit) to bytes.
func parseBytes(setting, unit string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(setting), 64)
	if err != nil {
		return 0, err
	}
	switch strings.TrimSpace(unit) {
	case "B":
		return v, nil
	case "kB":
		return v * 1024, nil
	case "MB":
		return v * 1024 * 1024, nil
	case "GB":
		return v * 1024 * 1024 * 1024, nil
	case "TB":
		return v * 1024 * 1024 * 1024 * 1024, nil
	case "8kB":
		return v * 8192, nil
	case "16kB":
		return v * 16384, nil
	}
	return v, nil
}

// buildPlan computes the tuning plan from system + database stats and
// workload evidence (query analysis findings).
func buildPlan(sys *SysStats, db *DBStats, wl Workload, qa *QueryAnalysis) []PlanItem {
	ram := sys.TotalMemBytes
	if ram == 0 {
		ram = 8 * GiB
	}

	sharedRatio := 0.25
	if wl == WorkloadOLAP {
		sharedRatio = 0.4
	}
	shared := uint64(float64(ram) * sharedRatio)
	if shared < 128*MiB {
		shared = 128 * MiB
	}

	effective := uint64(float64(ram) * 0.75)

	mwm := uint64(64 * MiB)
	switch {
	case ram < 1*GiB:
		mwm = 64 * MiB
	case ram < 4*GiB:
		mwm = 128 * MiB
	case ram < 8*GiB:
		mwm = 256 * MiB
	case ram < 16*GiB:
		mwm = 512 * MiB
	case ram < 32*GiB:
		mwm = 1 * GiB
	default:
		mwm = 2 * GiB
	}

	wal := shared / 32
	if wal < 8*MiB {
		wal = 8 * MiB
	}
	if wal > 16*MiB {
		wal = 16 * MiB
	}

	maxConn, _ := strconv.Atoi(settingValue(db.Settings, "max_connections", "100"))
	if maxConn < 10 {
		maxConn = 100
	}

	avWorkers := int64(3)
	if cores := sys.NumCPU; cores > 0 {
		av := int64(cores+1) / 2
		if av < 3 {
			av = 3
		}
		if av > 8 {
			av = 8
		}
		avWorkers = av
	}

	// work_mem = spare RAM spread across all connections (3x safety factor)
	workMem := uint64(16 * MiB)
	if denom := uint64(maxConn+int(avWorkers)) * 3; denom > 0 && ram > shared {
		w := (ram - shared) / denom
		w = w / MiB * MiB // round down to whole MB
		if w < 4*MiB {
			w = 4 * MiB
		}
		if w > 64*MiB {
			w = 64 * MiB
		}
		workMem = w
	}

	maxWal := uint64(1 * GiB)
	switch {
	case ram < 1*GiB:
		maxWal = 1 * GiB
	case ram < 4*GiB:
		maxWal = 2 * GiB
	case ram < 8*GiB:
		maxWal = 4 * GiB
	case ram < 16*GiB:
		maxWal = 8 * GiB
	case ram < 32*GiB:
		maxWal = 16 * GiB
	default:
		maxWal = 32 * GiB
	}
	minWal := maxWal / 2

	// ---- evidence-based adjustments on top of the formulas ----
	// notes[param] = reason, merged into the item's Reason for the report.
	notes := map[string]string{}
	spills := 0
	if qa != nil {
		count := func(k string) int {
			n := 0
			for _, f := range qa.Findings {
				if f.Kind == k {
					n++
				}
			}
			return n
		}

		// 1. disk sorts / temp spills -> work_mem too small. Raise it within a
		// hard budget (spare RAM across connections, 1x factor instead of 3x)
		// so evidence can only tighten the safety margin, never blow past RAM.
		spills = count("disk-sort") + count("temp-spill")
		if spills > 0 {
			var maxSort uint64
			for _, f := range qa.Findings {
				if (f.Kind == "disk-sort" || f.Kind == "temp-spill") && f.Bytes > maxSort {
					maxSort = f.Bytes
				}
			}
			// The reported spill is only the part that overflowed the *current*
			// work_mem; the sort needs work_mem + spill to fit in memory.
			// E.g. work_mem=34MB + 17MB spilled = sort needs ~51MB.
			curWorkMem := uint64(0)
			if s, ok := db.Settings["work_mem"]; ok {
				if cur, err := parseBytes(s.Value, s.Unit); err == nil && uint64(cur) > 0 {
					curWorkMem = uint64(cur)
				}
			}
			needed := maxSort + curWorkMem // in-memory portion + what spilled
			hardCap := uint64(0)
			if ram > shared+mwm {
				hardCap = (ram - shared - mwm) / uint64(maxConn+int(avWorkers))
			}
			if hardCap > 64*MiB {
				hardCap = 64 * MiB
			}
			ev := needed // already includes headroom (the in-memory portion)
			if ev > hardCap {
				ev = hardCap
			}
			ev = ev / MiB * MiB
			if ev < 4*MiB {
				ev = 4 * MiB
			}
			if ev > workMem {
				workMem = ev
				notes["work_mem"] = fmt.Sprintf("%d sort(s)/spill(s) hit disk (sort needs ~%s at current work_mem %s)", spills, fmtBytes(needed), fmtBytes(curWorkMem))
			}
		}

		// 2. low cache hit ratio -> shared_buffers too small (only when the
		// database fits in RAM, i.e. a bigger cache can actually help).
		hit := 0.0
		if tot := db.BlksHit + db.BlksRead; tot > 0 {
			hit = float64(db.BlksHit) / float64(tot)
		}
		if hit > 0 && hit < 0.98 && uint64(db.DatabaseSizeBytes) <= ram {
			ratio := 0.30
			if wl == WorkloadOLAP {
				ratio = 0.45
			}
			if ns := uint64(float64(ram) * ratio); ns > shared {
				shared = ns
				notes["shared_buffers"] = fmt.Sprintf("cache hit ratio only %.1f%%", hit*100)
			}
		} else if hit > 0 && hit < 0.98 {
			// raise was considered but is pointless: the database cannot fit
			// in RAM no matter how big the cache is
			notes["shared_buffers"] = fmt.Sprintf(
				"cache hit ratio only %.1f%% but database (%s) exceeds RAM (%s) - a bigger cache cannot help",
				hit*100, fmtBytes(uint64(db.DatabaseSizeBytes)), fmtBytes(ram))
		}

		// 3. checkpoint pressure (requested >= timed) -> WAL too small.
		if db.CheckpointsReq > 0 && db.CheckpointsReq >= db.CheckpointsTimed {
			if nw := maxWal * 2; nw <= 64*GiB {
				maxWal = nw
				minWal = maxWal / 2
				notes["max_wal_size"] = fmt.Sprintf("%d requested vs %d timed checkpoints (pressure)",
					db.CheckpointsReq, db.CheckpointsTimed)
				notes["min_wal_size"] = "follows max_wal_size"
			}
		}

		// 4. dead-tuple bloat -> autovacuum should run more eagerly (see below).
	}

	// Never downgrade work_mem from a lack of evidence: if a previous run (or
	// the operator) raised it above the formula, keep the higher value so the
	// instance does not ping-pong between two values.
	if spills == 0 {
		if s, ok := db.Settings["work_mem"]; ok {
			if cur, err := parseBytes(s.Value, s.Unit); err == nil && uint64(cur) > workMem {
				workMem = uint64(cur)
			}
		}
	}

	items := []PlanItem{}

	addBytes := func(name, ctx, reason string, target uint64) {
		if r, ok := notes[name]; ok {
			reason += " (evidence: " + r + ")"
		}
		item := PlanItem{
			Name: name, Proposed: fmtBytes(target), Context: ctx,
			Restart: ctx == "postmaster", Reason: reason,
			kind: kindBytes, targetB: target,
		}
		if s, ok := db.Settings[name]; ok {
			item.Current = currentDisplay(item, s)
			if cur, err := parseBytes(s.Value, s.Unit); err == nil {
				item.Changed = math.Abs(cur-float64(target)) > 0.5*float64(MiB)
			}
		} else {
			item.Current = "n/a"
			item.Changed = false // cannot read current value -> don't touch
		}
		items = append(items, item)
	}

	addFloat := func(name, ctx, reason string, target float64) {
		item := PlanItem{
			Name: name, Proposed: strconv.FormatFloat(target, 'f', -1, 64),
			Context: ctx, Restart: ctx == "postmaster", Reason: reason,
			kind: kindFloat, targetF: target,
		}
		if s, ok := db.Settings[name]; ok {
			item.Current = s.Value
			if cur, err := strconv.ParseFloat(s.Value, 64); err == nil {
				item.Changed = math.Abs(cur-target) > 1e-9
			}
		} else {
			item.Current = "n/a"
			item.Changed = false
		}
		items = append(items, item)
	}

	addInt := func(name, ctx, reason string, target int64) {
		item := PlanItem{
			Name: name, Proposed: strconv.FormatInt(target, 10),
			Context: ctx, Restart: ctx == "postmaster", Reason: reason,
			kind: kindInt, targetI: target,
		}
		if s, ok := db.Settings[name]; ok {
			item.Current = s.Value
			if cur, err := strconv.ParseInt(s.Value, 10, 64); err == nil {
				item.Changed = cur != target
			}
		} else {
			item.Current = "n/a"
			item.Changed = false
		}
		items = append(items, item)
	}

	addBytes("shared_buffers", "postmaster", "fraction of RAM by workload", shared)
	addBytes("effective_cache_size", "sighup", "75% of RAM", effective)
	addBytes("maintenance_work_mem", "sighup", "RAM size tier", mwm)
	addBytes("wal_buffers", "postmaster", "shared_buffers/32", wal)
	addBytes("work_mem", "user", "spare RAM / (connections x 3)", workMem)
	addBytes("max_wal_size", "sighup", "RAM size tier", maxWal)
	addBytes("min_wal_size", "sighup", "half of max_wal_size", minWal)
	addFloat("checkpoint_completion_target", "sighup", "spread checkpoint IO", 0.9)
	addInt("autovacuum_max_workers", "sighup", "based on CPU count", avWorkers)
	if qa != nil && countFinding(qa, "bloat") > 0 {
		addFloat("autovacuum_vacuum_scale_factor", "sighup", "dead-tuple bloat detected", 0.05)
	}

	// Cost constants follow the detected storage class. Unknown storage leaves
	// them at the PostgreSQL defaults rather than guessing.
	switch sys.DiskType {
	case "ssd":
		addFloat("random_page_cost", "user", "SSD storage", 1.1)
		addFloat("seq_page_cost", "user", "SSD storage", 1.0)
		addInt("effective_io_concurrency", "user", "SSD storage", 200)
		addInt("maintenance_io_concurrency", "sighup", "SSD storage", 100)
	case "hdd":
		addFloat("random_page_cost", "user", "HDD storage", 4.0)
		addFloat("seq_page_cost", "user", "HDD storage", 1.0)
		addInt("effective_io_concurrency", "user", "HDD storage", 2)
		addInt("maintenance_io_concurrency", "sighup", "HDD storage", 2)
	}

	return items
}

// countFinding counts findings of the given kind (nil-safe).
func countFinding(qa *QueryAnalysis, kind string) int {
	if qa == nil {
		return 0
	}
	n := 0
	for _, f := range qa.Findings {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// currentDisplay renders a pg_settings value in human-friendly form.
func currentDisplay(it PlanItem, s Setting) string {
	if s.Value == "" {
		return "n/a"
	}
	switch it.kind {
	case kindBytes:
		if b, err := parseBytes(s.Value, s.Unit); err == nil {
			return fmtBytes(uint64(b))
		}
		return s.Value
	default:
		return s.Value
	}
}
