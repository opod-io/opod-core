package fetch

// The prune reports the volume it is standing on.
//
// A manager that has to get "how full is this node's cache disk" from a node
// probe reports 0 for the one node where it matters: a full disk is the first
// thing that stops the probe being admitted, so the node that is out of space
// reported no space at all (a design-partner node, 2026-09-21). The prune runs
// ON that filesystem, so it answers the question itself.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPruneReportsTheVolumeItRanOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.gguf"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Prune(PruneRequest{Dir: dir, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.VolumeErr != "" {
		t.Fatalf("statfs failed on a temp dir: %s", res.VolumeErr)
	}
	if res.TotalBytes <= 0 || res.FreeBytes <= 0 || res.FreeBytes > res.TotalBytes {
		t.Errorf("want a measured volume, got total=%d free=%d", res.TotalBytes, res.FreeBytes)
	}

	// A directory that is not there is a named failure, never a silent zero.
	res, err = Prune(PruneRequest{Dir: filepath.Join(dir, "absent"), DryRun: true})
	if err == nil && res.VolumeErr == "" {
		t.Error("a cache directory that does not exist must be said, not reported as 0 bytes free")
	}
}
