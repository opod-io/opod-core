package fetch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAged writes size bytes at dir/rel with the given age.
func writeAged(t *testing.T, dir, rel string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Over the cap the oldest blocks go first, down to the low watermark; a fresh
// write in flight stays, a crashed one goes, and nothing that is not the
// tier's own file is touched.
func TestKVTierPruneTakesTheOldestBlocksFirst(t *testing.T) {
	dir := t.TempDir()
	base := "m_abc_r0/aaa/bb_g0/"
	oldest := writeAged(t, dir, base+"1.bin", 100, 4*time.Hour)
	older := writeAged(t, dir, base+"2.bin", 100, 3*time.Hour)
	newer := writeAged(t, dir, base+"3.bin", 100, 2*time.Hour)
	newest := writeAged(t, dir, base+"4.bin", 100, time.Hour)
	inFlight := writeAged(t, dir, base+"5.bin.tmp", 50, time.Minute)
	crashed := writeAged(t, dir, base+"6.bin.tmp", 50, time.Hour)
	cfg := writeAged(t, dir, "m_abc/config.json", 10, 5*time.Hour)
	foreign := writeAged(t, dir, "notes.txt", 1000, 9*time.Hour)

	// 400 blocks + 100 tmp + 10 config = 510 used; the crashed write goes
	// (460), then blocks until at most 0.9 × 300 = 270: the two oldest.
	res, err := PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 300, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.UsedBytes != 510 || res.Removed != 3 || res.FreedBytes != 250 || !res.DryRun {
		t.Fatalf("dry run %+v", res)
	}
	if !exists(oldest) || !exists(crashed) {
		t.Fatal("a dry run removed a file")
	}
	if _, err = PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 300}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{oldest: false, older: false, crashed: false, newer: true, newest: true, inFlight: true, cfg: true, foreign: true} {
		if exists(p) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), !want, want)
		}
	}
}

// Under the cap nothing goes but a crashed write.
func TestKVTierPruneUnderTheCapKeepsEveryBlock(t *testing.T) {
	dir := t.TempDir()
	b := writeAged(t, dir, "m_r0/a/b_g0/1.bin", 100, 48*time.Hour)
	res, err := PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 1 << 20})
	if err != nil || res.Removed != 0 || !exists(b) {
		t.Fatalf("res %+v err %v", res, err)
	}
}

// A cap of zero empties the tier but for the kept entries, and removes the
// directories it left empty, the tier's own directory included.
func TestKVTierPruneZeroEmptiesAllButTheKept(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ep_1")
	gone := writeAged(t, dir, "old-layout/m_r0/a/b_g0/1.bin", 100, time.Hour)
	writeAged(t, dir, "old-layout/m/config.json", 10, time.Hour)
	kept := writeAged(t, dir, "current/m_r0/a/b_g0/2.bin", 100, time.Hour)
	if _, err := PruneKVTier(KVTierPruneRequest{Dir: dir, Keep: []string{"current"}}); err != nil {
		t.Fatal(err)
	}
	if exists(gone) || exists(filepath.Join(dir, "old-layout")) || !exists(kept) {
		t.Fatalf("revision sweep: gone=%v old dir=%v kept=%v", exists(gone), exists(filepath.Join(dir, "old-layout")), exists(kept))
	}
	if _, err := PruneKVTier(KVTierPruneRequest{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if exists(dir) {
		t.Fatal("an emptied tier leaves its directory behind")
	}
	// A foreign file stops the directory going, and survives.
	foreign := writeAged(t, dir, "x/owner.dat", 5, time.Hour)
	writeAged(t, dir, "x/1.bin", 5, time.Hour)
	if _, err := PruneKVTier(KVTierPruneRequest{Dir: dir}); err != nil || !exists(foreign) {
		t.Fatalf("foreign file: exists=%v err=%v", exists(foreign), err)
	}
}

// A tier the engine has not created yet is empty, not an error; a relative
// directory is refused.
func TestKVTierPruneEdges(t *testing.T) {
	if res, err := PruneKVTier(KVTierPruneRequest{Dir: filepath.Join(t.TempDir(), "none"), MaxBytes: 1}); err != nil || res.UsedBytes != 0 {
		t.Fatalf("missing dir: %+v %v", res, err)
	}
	if _, err := PruneKVTier(KVTierPruneRequest{Dir: "rel/dir"}); err == nil {
		t.Fatal("a relative directory must be refused")
	}
}
