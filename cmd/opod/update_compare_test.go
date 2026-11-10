package main

import "testing"

// T24.80: the release is ordered against this build by semver. As strings,
// "v0.21.0-dev" vs "v0.4.5" read the older release as an update.
func TestCompareBuildOrdersBySemver(t *testing.T) {
	for _, c := range []struct {
		current, target string
		want            buildCompare
	}{
		{"0.4.5", "v0.4.5", buildSame},
		{"v0.4.4", "v0.4.5", buildNewer},
		{"0.4.10", "v0.4.9", buildOlder},
		{"0.10.0", "v0.9.9", buildOlder},
		{"0.4.5-rc1", "v0.4.5", buildNewer},
		{"0.21.0-dev", "v0.4.5", buildDev},
		{"0.0.0-dev", "v0.4.5", buildDev},
		{"1a2b3c4-dirty", "v0.4.5", buildDev},
	} {
		if got := compareBuild(c.current, c.target); got != c.want {
			t.Errorf("compareBuild(%q, %q) = %d, want %d", c.current, c.target, got, c.want)
		}
	}
}
