package main

import (
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "0s"},
		{30, "30s"},
		{90, "1m 30s"},
		{3600, "1h 0m"},
		{3660, "1h 1m"},
		{90000, "25h 0m"},
	}
	for _, c := range cases {
		if got := humanDuration(c.sec); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "hello..."},
		{"a\nb", 10, "a b"},          // newlines flattened to spaces
		{"  padded  ", 10, "padded"}, // leading/trailing whitespace trimmed
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.n); got != c.want {
			t.Errorf("truncate(%q,%d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "30s ago"},
		{2 * time.Minute, "2m ago"},
		{5 * time.Hour, "5h ago"},
		{10 * 24 * time.Hour, "10d ago"},
	}
	for _, c := range cases {
		if got := humanAge(now.Add(-c.ago)); got != c.want {
			t.Errorf("humanAge(%v ago) = %q, want %q", c.ago, got, c.want)
		}
	}
}
