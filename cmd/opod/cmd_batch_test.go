package main

import "testing"

// PLAN T34.1: --concurrency is a number of at least 1, or auto (start at 8).
func TestParseConcurrency(t *testing.T) {
	for _, tc := range []struct {
		in        string
		start     int
		auto, err bool
	}{
		{"8", 8, false, false},
		{" 32 ", 32, false, false},
		{"auto", 8, true, false},
		{"0", 0, false, true},
		{"-3", 0, false, true},
		{"many", 0, false, true},
		{"", 0, false, true},
	} {
		start, auto, err := parseConcurrency(tc.in)
		if (err != nil) != tc.err || start != tc.start || auto != tc.auto {
			t.Errorf("parseConcurrency(%q) = %d, %v, %v; want %d, %v, error %v", tc.in, start, auto, err, tc.start, tc.auto, tc.err)
		}
	}
}
