package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
)

// The startup banner is what an operator audits. It once read
// "ui on · egress on · callbacks on" on a binary with no UI, no vendor egress
// and no callback sinks, and named an OPOD_PROTOCOLS nothing parses.
func TestStartupBannerNamesNoSurfaceThatDoesNotExist(t *testing.T) {
	for _, managed := range []bool{false, true} {
		cfg := config.Default()
		cfg.Surfaces.Managed = managed
		var out bytes.Buffer
		printNetworkPosture(&out, cfg)
		banner := out.String()
		for _, gone := range []string{"ui on", "ui off", "egress", "callbacks", "OPOD_UI", "OPOD_EGRESS", "OPOD_CALLBACKS", "OPOD_PROTOCOLS", "Surfaces:"} {
			if strings.Contains(banner, gone) {
				t.Errorf("managed=%v: the banner still says %q:\n%s", managed, gone, banner)
			}
		}
		want := "Mode:            standalone"
		if managed {
			want = "Mode:            managed  (OPOD_MANAGED=1"
		}
		if !strings.Contains(banner, want) {
			t.Errorf("managed=%v: the banner does not say %q:\n%s", managed, want, banner)
		}
		if !strings.Contains(banner, "Telemetry:     none") {
			t.Errorf("the network posture lost its telemetry line:\n%s", banner)
		}
	}
}
