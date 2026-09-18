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

// snapshotCached lays down a complete snapshot directory the way Snapshot does:
// every file with its marker, the manifest last.
func snapshotCached(t *testing.T, dir, repo, rev string, lastUsed time.Time, files map[string]int) string {
	t.Helper()
	sdir := SnapshotDir(dir, repo, rev)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	man := SnapshotManifest{Repo: repo, Revision: rev, CompletedAt: time.Now().UTC()}
	for name, size := range files {
		cached(t, sdir, name, size, true, lastUsed)
		man.Files = append(man.Files, SnapshotFile{Name: name, Size: int64(size)})
	}
	if err := writeManifest(sdir, man); err != nil {
		t.Fatal(err)
	}
	return sdir
}

func entryNamed(t *testing.T, entries []Entry, file string) Entry {
	t.Helper()
	for _, e := range entries {
		if e.File == file {
			return e
		}
	}
	t.Fatalf("no entry %q in %+v", file, entries)
	return Entry{}
}

// What lives under <repo>@<rev>/ used to be invisible: never listed, never
// reclaimed. A pinned GGUF is a file like any other; a snapshot is one entry.
func TestListSeesRevisionDirectories(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-100 * time.Hour)
	cached(t, dir, "top.gguf", 8, true, old)
	rdir := RevisionDir(dir, "org/repo", "v1")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	cached(t, rdir, "pinned.gguf", 16, true, old)
	sdir := snapshotCached(t, dir, "org/llama", "abc123", old, map[string]int{"model.safetensors": 64, "config.json": 4})
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil { // someone else's directory
		t.Fatal(err)
	}

	entries, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want the top file, the pinned file and ONE snapshot entry, got %+v", entries)
	}
	if e := entryNamed(t, entries, "org-repo@v1/pinned.gguf"); !e.Ours || e.Revision != "v1" || e.Size != 16 || e.Snapshot {
		t.Fatalf("pinned file: %+v", e)
	}
	man, err := os.Stat(filepath.Join(sdir, SnapshotManifestName))
	if err != nil {
		t.Fatal(err)
	}
	e := entryNamed(t, entries, "org-llama@abc123")
	if !e.Snapshot || !e.Ours || e.Files != 2 || e.Size != 68+man.Size() || e.Repo != "org/llama" || e.Revision != "abc123" || e.Path != sdir {
		t.Fatalf("snapshot entry: %+v", e)
	}
	if e.LastUsedAt.IsZero() {
		t.Fatal("a snapshot's last use is the latest use of any of its files")
	}
}

func TestPruneReclaimsAPinnedRevisionFile(t *testing.T) {
	dir := t.TempDir()
	rdir := RevisionDir(dir, "org/repo", "v1")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * time.Hour)
	kept := cached(t, rdir, "kept.gguf", 16, true, old)
	gone := cached(t, rdir, "gone.gguf", 16, true, old)
	foreign := cached(t, rdir, "theirs.gguf", 16, false, time.Time{})

	res, err := Prune(PruneRequest{Dir: dir, Keep: []string{"kept.gguf"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0].File != "org-repo@v1/gone.gguf" || res.Kept != 1 {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "org-repo@v1/theirs.gguf" {
		t.Fatalf("a file we did not write must be named, not removed: %+v", res.Skipped)
	}
	for _, p := range []string{kept, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s went: %v", p, err)
		}
	}
	if _, err := os.Stat(gone); err == nil {
		t.Fatal("the unreferenced pinned file survived")
	}

	// Once its last file goes, so does the directory.
	rdir2 := RevisionDir(dir, "org/repo", "v2")
	if err := os.MkdirAll(rdir2, 0o755); err != nil {
		t.Fatal(err)
	}
	cached(t, rdir2, "only.gguf", 16, true, old)
	if _, err := Prune(PruneRequest{Dir: dir, Keep: []string{filepath.Join(rdir, "kept.gguf")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rdir2); err == nil {
		t.Fatal("an emptied revision directory was left behind")
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal("a keep given as a path did not keep the file")
	}
}

// snapshotIntact fails unless the manifest and every file are still there.
func snapshotIntact(t *testing.T, sdir string, files map[string]int) {
	t.Helper()
	if _, ok := SnapshotPath(filepath.Dir(sdir), "org/llama", "abc123"); !ok {
		t.Fatalf("the snapshot in %s is no longer complete", sdir)
	}
	for name := range files {
		if _, err := os.Stat(filepath.Join(sdir, name)); err != nil {
			t.Fatalf("%s went: %v", name, err)
		}
	}
}

// A snapshot is one unit: every rule that spares a file spares all of it, and
// when it goes, all of it goes.
func TestPruneTakesASnapshotWholeOrNotAtAll(t *testing.T) {
	files := map[string]int{"model-00001-of-00002.safetensors": 64, "model-00002-of-00002.safetensors": 64, "config.json": 4}
	old := time.Now().Add(-100 * time.Hour)

	t.Run("a pull holds one file's lock", func(t *testing.T) {
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		lock := filepath.Join(sdir, "config.json.lock")
		if err := os.WriteFile(lock, []byte("pull"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Prune(PruneRequest{Dir: dir}); err == nil {
			t.Fatal("a prune took a snapshot while one of its files was being pulled")
		}
		snapshotIntact(t, sdir, files)
		for name := range files {
			if name == "config.json" {
				continue
			}
			if _, err := os.Stat(filepath.Join(sdir, name+".lock")); err == nil {
				t.Fatalf("the refused prune left its own lock on %s", name)
			}
		}
	})

	t.Run("one file is not ours", func(t *testing.T) {
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		cached(t, sdir, "notes.txt", 4, false, time.Time{})
		res, err := Prune(PruneRequest{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Removed) != 0 || len(res.Skipped) != 1 || res.Skipped[0] != "org-llama@abc123" {
			t.Fatalf("result: %+v", res)
		}
		snapshotIntact(t, sdir, files)
	})

	t.Run("one file was used recently", func(t *testing.T) {
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		Touch(filepath.Join(sdir, "config.json"))
		res, err := Prune(PruneRequest{Dir: dir, MinAge: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Removed) != 0 || res.Kept != 1 {
			t.Fatalf("result: %+v", res)
		}
		snapshotIntact(t, sdir, files)
	})

	t.Run("the keep list names it", func(t *testing.T) {
		for _, keep := range []string{"org-llama@abc123", "org-llama@abc123/config.json"} {
			dir := t.TempDir()
			sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
			res, err := Prune(PruneRequest{Dir: dir, Keep: []string{keep}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Removed) != 0 || res.Kept != 1 {
				t.Fatalf("keep %q: %+v", keep, res)
			}
			snapshotIntact(t, sdir, files)
		}
		// Every snapshot holds a config.json: a bare name must not pin them all.
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		if _, err := Prune(PruneRequest{Dir: dir, Keep: []string{"config.json"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(sdir); err == nil {
			t.Fatal("a bare file name kept a whole snapshot")
		}
	})

	t.Run("a dry run says the whole size and removes nothing", func(t *testing.T) {
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		res, err := Prune(PruneRequest{Dir: dir, DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Removed) != 1 || !res.Removed[0].Snapshot || res.FreedBytes < 132 {
			t.Fatalf("result: %+v", res)
		}
		snapshotIntact(t, sdir, files)
	})

	t.Run("unreferenced and old: all of it goes", func(t *testing.T) {
		dir := t.TempDir()
		sdir := snapshotCached(t, dir, "org/llama", "abc123", old, files)
		res, err := Prune(PruneRequest{Dir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Removed) != 1 || res.Removed[0].Files != 3 {
			t.Fatalf("result: %+v", res)
		}
		if _, err := os.Stat(sdir); err == nil {
			left, _ := os.ReadDir(sdir)
			t.Fatalf("the snapshot directory survived with %d entries", len(left))
		}
	})
}

func TestSweepReachesRevisionDirectories(t *testing.T) {
	dir := t.TempDir()
	sdir := snapshotCached(t, dir, "org/llama", "abc123", time.Now(), map[string]int{"model.safetensors": 8})
	stale := filepath.Join(sdir, "model.safetensors.partial-9")
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, past, past)
	if n, err := SweepPartials(dir, 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("swept %d leftovers (err %v), want 1", n, err)
	}
	if _, ok := SnapshotPath(dir, "org/llama", "abc123"); !ok {
		t.Fatal("the sweep damaged the snapshot")
	}
}
