package main

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/models"
)

// parseModelAddArgs is the CLI-level parser; verify both orderings and the
// no-flag case behave correctly so users can type the words however they
// expect.
func TestParseModelAddArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantID     string
		wantForce  bool
		wantDryRun bool
		wantFrom   string
		wantNodes  string // comma-joined, for easy comparison
	}{
		{"id only", []string{"qwen3.6-27b"}, "qwen3.6-27b", false, false, "", ""},
		{"id then force", []string{"qwen3.6-27b", "--force"}, "qwen3.6-27b", true, false, "", ""},
		{"force then id", []string{"--force", "qwen3.6-27b"}, "qwen3.6-27b", true, false, "", ""},
		{"single-dash force", []string{"qwen3.6-27b", "-force"}, "qwen3.6-27b", true, false, "", ""},
		{"dry-run only", []string{"qwen3.6-27b", "--dry-run"}, "qwen3.6-27b", false, true, "", ""},
		{"dry-run + force", []string{"--dry-run", "qwen3.6-27b", "--force"}, "qwen3.6-27b", true, true, "", ""},
		{"dryrun spelling", []string{"qwen3.6-27b", "--dryrun"}, "qwen3.6-27b", false, true, "", ""},
		{"from path", []string{"--from", "./my.yaml"}, "", false, false, "./my.yaml", ""},
		{"from path equals", []string{"--from=./my.yaml"}, "", false, false, "./my.yaml", ""},
		{"from + force", []string{"--from", "x.yaml", "--force"}, "", true, false, "x.yaml", ""},
		{"empty", []string{}, "", false, false, "", ""},
		{"single node", []string{"qwen3.6-27b", "--node", "NodeA"}, "qwen3.6-27b", false, false, "", "NodeA"},
		{"node equals", []string{"qwen3.6-27b", "--node=NodeA"}, "qwen3.6-27b", false, false, "", "NodeA"},
		{"nodes csv", []string{"qwen3.6-27b", "--nodes", "NodeA,NodeB"}, "qwen3.6-27b", false, false, "", "NodeA,NodeB"},
		{"nodes equals csv", []string{"--nodes=NodeA,NodeB", "qwen3.6-27b"}, "qwen3.6-27b", false, false, "", "NodeA,NodeB"},
		{"node + force", []string{"qwen3.6-27b", "--node", "NodeA", "--force"}, "qwen3.6-27b", true, false, "", "NodeA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, force, dryRun, from, nodes := parseModelAddArgs(c.args)
			gotNodes := strings.Join(nodes, ",")
			if id != c.wantID || force != c.wantForce || dryRun != c.wantDryRun || from != c.wantFrom || gotNodes != c.wantNodes {
				t.Fatalf("got (%q, force=%v, dryRun=%v, from=%q, nodes=%q), want (%q, force=%v, dryRun=%v, from=%q, nodes=%q)",
					id, force, dryRun, from, gotNodes, c.wantID, c.wantForce, c.wantDryRun, c.wantFrom, c.wantNodes)
			}
		})
	}
}

// checkHardwareForModel is the actual gate; verify it refuses obviously
// under-spec installs and lets reasonable cases through. We can't easily
// fake the host's Detect() output, so these tests target the *catalog
// entry* side of the comparison using extreme values.
func TestCheckHardwareForModel_PassesWhenNoMinimums(t *testing.T) {
	entry := &models.Entry{
		ID: "trivial",
		// no Hardware fields set
	}
	if msg := checkHardwareForModel(entry); msg != "" {
		t.Fatalf("expected pass, got refusal: %s", msg)
	}
}

func TestCheckHardwareForModel_RefusesAbsurdRAMRequirement(t *testing.T) {
	// 999 TB of RAM is something no machine running this test has.
	// Worth checking: the function refuses with a clear message rather
	// than crashing or silently allowing.
	entry := &models.Entry{
		ID: "definitely-too-big",
		Hardware: models.HardwareSpec{
			MinRAMGB: 999_999,
		},
	}
	msg := checkHardwareForModel(entry)
	if msg == "" {
		t.Fatal("expected refusal for 999_999 GB RAM requirement, got pass")
	}
	if !strings.Contains(msg, "definitely-too-big") {
		t.Errorf("refusal message %q should name the model", msg)
	}
	if !strings.Contains(msg, "999999") && !strings.Contains(msg, "999_999") {
		t.Errorf("refusal message %q should mention the required size", msg)
	}
}

func TestCheckHardwareForModel_RefusesAbsurdVRAMRequirement(t *testing.T) {
	// If the host has any GPU at all, this refuses; if no GPU is
	// detected (most CI), the function falls through and passes —
	// either outcome is acceptable for this test, so we only check
	// the SHAPE of the refusal message when it occurs.
	entry := &models.Entry{
		ID: "needs-huge-gpu",
		Hardware: models.HardwareSpec{
			MinVRAMGB: 9999,
		},
	}
	msg := checkHardwareForModel(entry)
	if msg != "" && !strings.Contains(msg, "VRAM") {
		t.Errorf("refusal message %q should mention VRAM", msg)
	}
}
