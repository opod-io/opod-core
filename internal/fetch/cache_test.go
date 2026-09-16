package fetch

// ADR-046 · a prune is safe only because of what it refuses to do. Each rule
// is a test, because the cost of getting one wrong is a customer's weights.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cached(t *testing.T, dir, name string, size int, ours bool, lastUsed time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if ours {
		if err := WriteMarker(p, "org/repo", name, int64(size)); err != nil {
			t.Fatal(err)
		}
		if !lastUsed.IsZero() {
			m, _ := readMarker(p)
			m.LastUsedAt = lastUsed
			b, _ := os.ReadFile(markerPath(p))
			_ = b
			if err := writeMarkerRaw(p, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	return p
}

func TestPruneNeverTouchesAFileWeDidNotWrite(t *testing.T) {
	dir := t.TempDir()
	theirs := cached(t, dir, "someone-elses.gguf", 16, false, time.Time{})
	res, err := Prune(PruneRequest{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Fatalf("pruned %v — a file without our marker belongs to the owner", res.Removed)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatalf("the file is gone: %v", err)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("the skipped file was not named: %+v", res)
	}
}

func TestPruneKeepsWhatTheControlPlaneReferences(t *testing.T) {
	dir := t.TempDir()
	keep := cached(t, dir, "in-use.gguf", 16, true, time.Now().Add(-100*time.Hour))
	gone := cached(t, dir, "orphan.gguf", 16, true, time.Now().Add(-100*time.Hour))
	res, err := Prune(PruneRequest{Dir: dir, Keep: []string{"in-use.gguf"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("a referenced file was pruned: an endpoint would restart into an empty cache")
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatal("an unreferenced file survived a prune with no constraints")
	}
	if len(res.Removed) != 1 || res.Removed[0].File != "orphan.gguf" {
		t.Fatalf("prune removed %+v", res.Removed)
	}
}

func TestPruneRespectsTheMinimumAge(t *testing.T) {
	dir := t.TempDir()
	fresh := cached(t, dir, "just-used.gguf", 16, true, time.Now().Add(-1*time.Hour))
	if _, err := Prune(PruneRequest{Dir: dir, MinAge: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a file used an hour ago was pruned: a rollback inside the window would re-download it")
	}
}

func TestPruneWaitsForAPullInFlight(t *testing.T) {
	dir := t.TempDir()
	p := cached(t, dir, "pulling.gguf", 16, true, time.Now().Add(-100*time.Hour))
	// A download holds the lock.
	lock, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close(); _ = os.Remove(p + ".lock") }()

	if _, err := Prune(PruneRequest{Dir: dir}); err == nil {
		t.Fatal("a prune deleted a file another process was pulling")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the file went anyway")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	p := cached(t, dir, "orphan.gguf", 32, true, time.Now().Add(-100*time.Hour))
	res, err := Prune(PruneRequest{Dir: dir, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.FreedBytes != 32 {
		t.Fatalf("a dry run must still say what it would free: %+v", res)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a dry run deleted the file")
	}
}

func TestSweepClearsCrashedPullsOnly(t *testing.T) {
	dir := t.TempDir()
	good := cached(t, dir, "weights.gguf", 16, true, time.Now())
	stale := filepath.Join(dir, "weights.gguf.partial-123")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)
	n, err := SweepPartials(dir, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("swept %d leftovers (err %v), want 1", n, err)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatal("the sweep took a real file")
	}
}
