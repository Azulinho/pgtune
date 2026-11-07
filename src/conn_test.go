package main

import (
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestBuildDSNRoundTrip(t *testing.T) {
	opts := Options{
		User:     "app user",
		Password: "p a+s s/word@x",
		Host:     "/var/run/postgresql",
		Port:     5433,
		SslMode:  "require",
	}
	cfg, err := pgx.ParseConfig(buildDSN(opts, "my db"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.User != opts.User {
		t.Errorf("user = %q, want %q", cfg.User, opts.User)
	}
	if cfg.Password != opts.Password {
		t.Errorf("password = %q, want %q", cfg.Password, opts.Password)
	}
	if cfg.Host != opts.Host {
		t.Errorf("host = %q, want %q", cfg.Host, opts.Host)
	}
	if cfg.Port != 5433 {
		t.Errorf("port = %d, want 5433", cfg.Port)
	}
	if cfg.Database != "my db" {
		t.Errorf("database = %q, want %q", cfg.Database, "my db")
	}
}

func TestBuildDSNIPv6(t *testing.T) {
	cfg, err := pgx.ParseConfig(buildDSN(Options{User: "u", Host: "::1", Port: 5432}, "postgres"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Host != "::1" {
		t.Errorf("host = %q, want ::1", cfg.Host)
	}
}
