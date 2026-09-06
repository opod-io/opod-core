package main

import (
	"testing"
	"time"
)

func TestParseFlexibleDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"45s", 45 * time.Second, false},
		{"", 0, true},
		{"0d", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseFlexibleDuration(c.in)
			if c.err {
				if err == nil {
					t.Errorf("expected error for %q, got %v", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("%q: got %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestParseFlexibleDate(t *testing.T) {
	t1, err := parseFlexibleDate("2026-07-01")
	if err != nil {
		t.Fatalf("date: %v", err)
	}
	if t1.Year() != 2026 || t1.Month() != 7 || t1.Day() != 1 {
		t.Errorf("date parsed wrong: %v", t1)
	}
	if t1.Location().String() != "UTC" {
		t.Errorf("date should be UTC, got %v", t1.Location())
	}
	if _, err := parseFlexibleDate("not a date"); err == nil {
		t.Error("expected error for garbage input")
	}
}
