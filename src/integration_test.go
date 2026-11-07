package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIntegrationCollectStats(t *testing.T) {
	dsn := os.Getenv("PGTUNE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGTUNE_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	db, err := collectDBStats(ctx, conn)
	if err != nil {
		t.Fatalf("collectDBStats: %v", err)
	}
	if db.Version == "" || db.Database == "" {
		t.Fatalf("incomplete stats: version=%q database=%q", db.Version, db.Database)
	}
	if len(db.Settings) == 0 {
		t.Fatal("no pg_settings rows collected")
	}

	opts := Options{TopN: 5, ExplainN: 0, LongRunningSec: 5, TablesN: 5}
	qa, err := collectQueryAnalysis(ctx, conn, opts, db)
	if err != nil {
		t.Fatalf("collectQueryAnalysis: %v", err)
	}
	if qa == nil {
		t.Fatal("collectQueryAnalysis returned nil")
	}
}
