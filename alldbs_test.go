package main

import (
	"strings"
	"testing"
)

func mkStats(name string, blksHit, blksRead, size int64) *DBStats {
	return &DBStats{
		Database:          name,
		DatabaseSizeBytes: size,
		BlksHit:           blksHit,
		BlksRead:          blksRead,
		Commits:           10,
		TempFiles:         3,
		TempBytes:         int64(4 * MiB),
		SeqScans:          7,
		IdxScans:          9,
	}
}

func TestAggregateDBStats(t *testing.T) {
	primary := mkStats("app", 900, 100, int64(100*MiB))
	primary.Version = "PostgreSQL 17"
	primary.CheckpointsTimed = 50
	primary.ActiveConns = 3
	primary.IsSuperuser = false
	primary.Settings = map[string]Setting{"work_mem": {Name: "work_mem", Value: "51MB", Unit: "MB"}}

	other := mkStats("app2", 800, 200, int64(50*MiB))
	skipped := &PerDB{DB: &DBStats{Database: "postgres"}, SkipReason: "no CONNECT privilege"}

	agg := aggregateDBStats([]*PerDB{{DB: primary, QA: &QueryAnalysis{}}, {DB: other}, skipped})

	// per-database counters are summed
	if agg.BlksHit != 1700 || agg.BlksRead != 300 {
		t.Errorf("aggregate blks = %d/%d, want 1700/300", agg.BlksHit, agg.BlksRead)
	}
	if agg.TempFiles != 6 {
		t.Errorf("aggregate temp files = %d, want 6", agg.TempFiles)
	}
	if agg.DatabaseSizeBytes != int64(150*MiB) {
		t.Errorf("aggregate size = %d, want %d", agg.DatabaseSizeBytes, int64(150*MiB))
	}
	// instance-level fields come from the primary and are NOT summed
	if agg.CheckpointsTimed != 50 || agg.ActiveConns != 3 || agg.IsSuperuser {
		t.Error("instance-level fields must come from the primary database, not be summed")
	}
	if agg.Settings["work_mem"].Value != "51MB" {
		t.Error("settings must be carried from the primary")
	}
	if !strings.Contains(agg.Database, "app (100MB)") || !strings.Contains(agg.Database, "app2 (50MB)") {
		t.Errorf("Database summary %q should contain per-db sizes", agg.Database)
	}
	// skipped databases contribute nothing
	if strings.Contains(agg.Database, "postgres") {
		t.Error("skipped databases must not be aggregated")
	}
}

func TestAggregateQA(t *testing.T) {
	perDBs := []*PerDB{
		{DB: &DBStats{Database: "app"}, QA: &QueryAnalysis{
			Findings: []Finding{{Kind: "disk-sort", Detail: "sort spilled"}, {Kind: "bloat", Detail: "bloat"}},
		}},
		{DB: &DBStats{Database: "app2"}, QA: &QueryAnalysis{
			Findings: []Finding{{Kind: "disk-sort", Detail: "sort spilled"}, {Kind: "stale-stats", Detail: "stale"}},
		}},
		{DB: &DBStats{Database: "postgres"}, SkipReason: "no access"},
		{DB: &DBStats{Database: "empty"}, QA: nil},
	}
	agg := aggregateQA(perDBs)

	// duplicate findings across databases are deduped
	if len(agg.Findings) != 3 {
		t.Errorf("aggregate findings = %d, want 3 (deduped): %+v", len(agg.Findings), agg.Findings)
	}
	if countFinding(agg, "disk-sort") != 1 || countFinding(agg, "stale-stats") != 1 {
		t.Error("kind counts must reflect deduped findings")
	}
}

func TestDedupe(t *testing.T) {
	got := dedupe([]string{"a", "b", "a", "c", "b"})
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("dedupe = %v, want [a b c]", got)
	}
}
