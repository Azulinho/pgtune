package main

import (
	"math"
	"strings"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

func mkSetting(name, value, unit, ctx string) Setting {
	return Setting{Name: name, Value: value, Unit: unit, Ctx: ctx}
}

func mkDB(settings ...Setting) *DBStats {
	db := &DBStats{Settings: map[string]Setting{}}
	for _, s := range settings {
		db.Settings[s.Name] = s
	}
	return db
}

func planItem(t *testing.T, plan []PlanItem, name string) PlanItem {
	t.Helper()
	for _, it := range plan {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("plan item %q not found (have: %s)", name, names(plan))
	return PlanItem{}
}

func names(plan []PlanItem) string {
	var b strings.Builder
	for i, it := range plan {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.Name)
	}
	return b.String()
}

// ---- parseWorkload ---------------------------------------------------------

func TestParseWorkload(t *testing.T) {
	cases := []struct {
		in   string
		want Workload
		ok   bool
	}{
		{"oltp", WorkloadOLTP, true},
		{"OLTP", WorkloadOLTP, true},
		{"web", WorkloadOLTP, true},
		{"transactional", WorkloadOLTP, true},
		{"olap", WorkloadOLAP, true},
		{"dw", WorkloadOLAP, true},
		{"analytics", WorkloadOLAP, true},
		{"mixed", WorkloadMixed, true},
		{"desktop", WorkloadDesktop, true},
		{"", Workload(""), false},
		{"nosql", Workload(""), false},
		{"oltp ", Workload(""), false}, // trailing space not trimmed
	}
	for _, c := range cases {
		got, err := parseWorkload(c.in)
		if (err == nil) != c.ok {
			t.Errorf("parseWorkload(%q) error = %v, want ok=%v", c.in, err, c.ok)
		}
		if err == nil && got != c.want {
			t.Errorf("parseWorkload(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- fmtBytes --------------------------------------------------------------

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1KB"},
		{1536, "1.5KB"},
		{34 * MiB, "34MB"},
		{int64ToUint(51.2 * float64(MiB)), "51.2MB"},
		{int64ToUint(17.2 * float64(MiB)), "17.2MB"},
		{512 * MiB, "512MB"},
		{GiB, "1GB"},
		{int64ToUint(3.2 * float64(GiB)), "3.2GB"},
		{6 * GiB, "6GB"},
	}
	for _, c := range cases {
		if got := fmtBytes(c.in); got != c.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func int64ToUint(f float64) uint64 { return uint64(f) }

// ---- parseBytes ------------------------------------------------------------

func TestParseBytes(t *testing.T) {
	cases := []struct {
		val, unit string
		want      float64
	}{
		{"34816", "kB", 34 * float64(MiB)},
		{"2048", "MB", 2048 * float64(MiB)},
		{"1", "GB", float64(GiB)},
		{"128", "8kB", 128 * 8192},
		{"0.5", "MB", 0.5 * float64(MiB)},
		{"100", "", 100},
		{"2", "16kB", 2 * 16384},
		{"4194304", "kB", 4 * float64(GiB)},
	}
	for _, c := range cases {
		got, err := parseBytes(c.val, c.unit)
		if err != nil {
			t.Errorf("parseBytes(%q,%q) error: %v", c.val, c.unit, err)
			continue
		}
		if math.Abs(got-c.want) > 1 {
			t.Errorf("parseBytes(%q,%q) = %v, want %v", c.val, c.unit, got, c.want)
		}
	}
	if _, err := parseBytes("abc", "kB"); err == nil {
		t.Error("parseBytes(\"abc\") should error")
	}
}

// ---- buildPlan: baseline ---------------------------------------------------

func TestBuildPlanBaselineOLTP(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB() // no settings at all -> Current "n/a", Changed false
	plan := buildPlan(sys, db, WorkloadOLTP, nil)

	expect := map[string]string{
		"shared_buffers":               "2GB",
		"effective_cache_size":         "6GB",
		"maintenance_work_mem":         "512MB",
		"wal_buffers":                  "16MB",
		"work_mem":                     "19MB",
		"max_wal_size":                 "8GB",
		"min_wal_size":                 "4GB",
		"checkpoint_completion_target": "0.9",
		"autovacuum_max_workers":       "3",
		"random_page_cost":             "1.1",
		"effective_io_concurrency":     "200",
	}
	for name, want := range expect {
		it := planItem(t, plan, name)
		if it.Proposed != want {
			t.Errorf("%s proposed = %q, want %q", name, it.Proposed, want)
		}
	}
	// without readable current values nothing must be marked changed
	for _, it := range plan {
		if it.Changed {
			t.Errorf("%s marked changed with no current value available", it.Name)
		}
	}
}

func TestBuildPlanOLAPUsesLargerSharedBuffers(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "hdd"}
	plan := buildPlan(sys, mkDB(), WorkloadOLAP, nil)
	if got := planItem(t, plan, "shared_buffers").Proposed; got != "3.2GB" {
		t.Errorf("OLAP shared_buffers = %q, want 3.2GB", got)
	}
	if got := planItem(t, plan, "random_page_cost").Proposed; got != "4" {
		t.Errorf("HDD random_page_cost = %q, want 4", got)
	}
	if got := planItem(t, plan, "effective_io_concurrency").Proposed; got != "2" {
		t.Errorf("HDD effective_io_concurrency = %q, want 2", got)
	}
}

func TestBuildPlanSmallRAMFloor(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 512 * MiB, NumCPU: 1, DiskType: "ssd"}
	plan := buildPlan(sys, mkDB(), WorkloadOLTP, nil)
	if got := planItem(t, plan, "shared_buffers").Proposed; got != "128MB" {
		t.Errorf("small-RAM shared_buffers = %q, want 128MB floor", got)
	}
	if got := planItem(t, plan, "maintenance_work_mem").Proposed; got != "64MB" {
		t.Errorf("small-RAM maintenance_work_mem = %q, want 64MB", got)
	}
}

// ---- buildPlan: evidence-based adjustments ---------------------------------

// Sort needs work_mem (34MB) + spill (17MB) = 51MB to fit in memory.
func TestBuildPlanWorkMemSpillEvidence(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("work_mem", "34816", "kB", "user")) // 34MB current
	qa := &QueryAnalysis{Findings: []Finding{
		{Kind: "disk-sort", Bytes: 17 * MiB},
	}}
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	it := planItem(t, plan, "work_mem")
	if it.Proposed != "51MB" {
		t.Errorf("work_mem proposed = %q, want 51MB (current 34MB + 17MB spill)", it.Proposed)
	}
	if !it.Changed {
		t.Error("work_mem should be marked changed")
	}
	if !strings.Contains(it.Reason, "hit disk") {
		t.Errorf("work_mem reason %q should mention the disk spill", it.Reason)
	}
	// the note must carry the numbers for the report
	if !strings.Contains(it.Reason, "sort needs ~51MB at current work_mem 34MB") {
		t.Errorf("work_mem reason %q should explain the math", it.Reason)
	}
}

// A huge spill must be capped by the hard RAM budget, never exceed it.
func TestBuildPlanWorkMemCappedByHardBudget(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("work_mem", "34816", "kB", "user")) // 34MB
	qa := &QueryAnalysis{Findings: []Finding{
		{Kind: "temp-spill", Bytes: 512 * MiB}, // unrealistic spill
	}}
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	// hardCap = (8GiB - 2GiB shared - 512MB mwm) / (100 conns + 3 workers) ~ 54MB
	it := planItem(t, plan, "work_mem")
	if it.Proposed != "54MB" {
		t.Errorf("work_mem proposed = %q, want hard cap 54MB", it.Proposed)
	}
}

// Without spill evidence, an operator-raised work_mem must be kept (no
// ping-pong downgrade back to the formula value).
func TestBuildPlanNeverDowngradesWorkMem(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("work_mem", "40960", "kB", "user")) // 40MB > formula 19MB
	qa := &QueryAnalysis{}                                   // no findings
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	it := planItem(t, plan, "work_mem")
	if it.Proposed != "40MB" {
		t.Errorf("work_mem proposed = %q, want 40MB (keep operator value)", it.Proposed)
	}
	if it.Changed {
		t.Error("work_mem should not be marked changed when already at target")
	}
}

// After work_mem was raised to fit a sort, a converged run (no spills) must
// keep the value and report "ok".
func TestBuildPlanConvergedNoPingPong(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("work_mem", "52224", "kB", "user")) // 51MB
	qa := &QueryAnalysis{}
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	it := planItem(t, plan, "work_mem")
	if it.Proposed != "51MB" || it.Changed {
		t.Errorf("work_mem = %q changed=%v, want 51MB unchanged", it.Proposed, it.Changed)
	}
}

func TestBuildPlanCheckpointPressureDoublesWAL(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(
		mkSetting("max_wal_size", "8388608", "kB", "sighup"), // 8GB
		mkSetting("min_wal_size", "4194304", "kB", "sighup"), // 4GB
	)
	db.CheckpointsReq = 5
	db.CheckpointsTimed = 2
	qa := &QueryAnalysis{} // non-nil so the evidence section runs
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	maxWal := planItem(t, plan, "max_wal_size")
	if maxWal.Proposed != "16GB" || !maxWal.Changed {
		t.Errorf("max_wal_size = %q changed=%v, want 16GB changed", maxWal.Proposed, maxWal.Changed)
	}
	if !strings.Contains(maxWal.Reason, "pressure") {
		t.Errorf("max_wal_size reason %q should mention checkpoint pressure", maxWal.Reason)
	}
	if got := planItem(t, plan, "min_wal_size").Proposed; got != "8GB" {
		t.Errorf("min_wal_size = %q, want 8GB (half of 16GB)", got)
	}
}

func TestBuildPlanBloatTightensAutovacuum(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	qa := &QueryAnalysis{Findings: []Finding{{Kind: "bloat"}}}
	plan := buildPlan(sys, mkDB(), WorkloadOLTP, qa)

	if got := planItem(t, plan, "autovacuum_vacuum_scale_factor").Proposed; got != "0.05" {
		t.Errorf("autovacuum_vacuum_scale_factor = %q, want 0.05", got)
	}
}

func TestBuildPlanNoBloatKeepsDefaultScaleFactor(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	plan := buildPlan(sys, mkDB(), WorkloadOLTP, &QueryAnalysis{})
	for _, it := range plan {
		if it.Name == "autovacuum_vacuum_scale_factor" {
			t.Error("autovacuum_vacuum_scale_factor should not appear without bloat evidence")
		}
	}
}

// Low cache hit ratio on a database that fits in RAM raises shared_buffers.
func TestBuildPlanLowCacheHitRaisesSharedBuffers(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("shared_buffers", "2097152", "kB", "postmaster")) // 2GB
	db.BlksHit = 900
	db.BlksRead = 100 // 90% hit
	db.DatabaseSizeBytes = int64(2 * GiB)
	qa := &QueryAnalysis{}
	plan := buildPlan(sys, db, WorkloadOLTP, qa)

	it := planItem(t, plan, "shared_buffers")
	if it.Proposed != "2.4GB" {
		t.Errorf("shared_buffers = %q, want 2.4GB (30%% of 8GB, raised for 90%% hit)", it.Proposed)
	}
	if !strings.Contains(it.Reason, "90.0%") {
		t.Errorf("shared_buffers reason %q should cite the hit ratio", it.Reason)
	}
}

// With a healthy cache the formula value stands.
func TestBuildPlanGoodCacheKeepsSharedBuffers(t *testing.T) {
	sys := &SysStats{TotalMemBytes: 8 * GiB, NumCPU: 4, DiskType: "ssd"}
	db := mkDB(mkSetting("shared_buffers", "2097152", "kB", "postmaster")) // 2GB
	db.BlksHit = 99900
	db.BlksRead = 100 // 99.9% hit
	db.DatabaseSizeBytes = int64(2 * GiB)
	plan := buildPlan(sys, db, WorkloadOLTP, &QueryAnalysis{})

	it := planItem(t, plan, "shared_buffers")
	if it.Proposed != "2GB" || it.Changed {
		t.Errorf("shared_buffers = %q changed=%v, want 2GB unchanged", it.Proposed, it.Changed)
	}
}

// ---- countFinding ----------------------------------------------------------

func TestCountFinding(t *testing.T) {
	if got := countFinding(nil, "bloat"); got != 0 {
		t.Errorf("countFinding(nil) = %d, want 0", got)
	}
	qa := &QueryAnalysis{Findings: []Finding{
		{Kind: "bloat"}, {Kind: "seq-scan"}, {Kind: "bloat"},
	}}
	if got := countFinding(qa, "bloat"); got != 2 {
		t.Errorf("countFinding(bloat) = %d, want 2", got)
	}
	if got := countFinding(qa, "disk-sort"); got != 0 {
		t.Errorf("countFinding(disk-sort) = %d, want 0", got)
	}
}

// ---- currentDisplay --------------------------------------------------------

func TestCurrentDisplay(t *testing.T) {
	it := PlanItem{kind: kindBytes}
	if got := currentDisplay(it, mkSetting("work_mem", "34816", "kB", "user")); got != "34MB" {
		t.Errorf("currentDisplay = %q, want 34MB", got)
	}
	if got := currentDisplay(it, mkSetting("", "", "", "")); got != "n/a" {
		t.Errorf("currentDisplay(empty) = %q, want n/a", got)
	}
	it2 := PlanItem{kind: kindFloat}
	if got := currentDisplay(it2, mkSetting("random_page_cost", "1.1", "", "user")); got != "1.1" {
		t.Errorf("currentDisplay(float) = %q, want 1.1", got)
	}
}
