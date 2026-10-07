package fetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// die drops a holder's flock the way a killed process would: the descriptor
// closes and the file stays.
func (l *lockFile) die() { _ = l.f.Close() }

func TestLockHolderGoneOnlyWhenItsFlockIsFree(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "m.gguf.lock")
	lf, ok, err := acquireLockFile(lock, "fetch")
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	b, _ := os.ReadFile(lock)
	if !strings.Contains(string(b), flockMarker) {
		t.Fatalf("the lock does not say it is flocked: %q", b)
	}
	if lockHolderGone(lock) {
		t.Fatal("a live holder was reported gone")
	}
	if _, ok, _ := acquireLockFile(lock, "fetch"); ok {
		t.Fatal("a second caller took a held lock")
	}
	lf.die()
	if !lockHolderGone(lock) {
		t.Fatal("a dead holder's lock was not recognised as abandoned")
	}
}

// A lock an older opod wrote carries no flock, so its holder cannot be proven
// dead: only the age rule may replace it.
func TestAnOldFormatLockIsNeverCalledDead(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "m.gguf.lock")
	if err := os.WriteFile(lock, []byte("pid 36 at 2026-10-06T17:02:54Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if lockHolderGone(lock) {
		t.Fatal("a lock with no flock marker was called dead: an old binary's live pull would be overwritten")
	}
}

func TestReleaseLeavesNoLock(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "m.gguf.lock")
	lf, _, err := acquireLockFile(lock, "fetch")
	if err != nil {
		t.Fatal(err)
	}
	lf.release()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("release left the lock file: %v", err)
	}
	if lockHolderGone(lock) {
		t.Fatal("a missing lock was reported as a dead holder")
	}
}

// The case the cell found (2026-10-06): an interrupted pull left its lock, and
// the next attempt sat behind it for LockWait (six hours) instead of resuming.
func TestTakeLockTakesOverFromADeadHolderAtOnce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "m.gguf")
	lf, _, err := acquireLockFile(target+".lock", "fetch")
	if err != nil {
		t.Fatal(err)
	}
	lf.die()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opt := Options{LockWait: 6 * time.Hour}.withDefaults()
	start := time.Now()
	release, err := takeLock(ctx, target+".lock", target, 16, true, opt)
	if err != nil {
		t.Fatalf("waited instead of taking over: %v", err)
	}
	if release == nil {
		t.Fatal("no lock returned")
	}
	defer release()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took over after %s; want well under a second", d)
	}
}

func TestPruneTakesOverADeadPullersLock(t *testing.T) {
	dir := t.TempDir()
	p := cached(t, dir, "orphan.gguf", 16, true, time.Now().Add(-100*time.Hour))
	lf, _, err := acquireLockFile(p+".lock", "fetch")
	if err != nil {
		t.Fatal(err)
	}
	lf.die()
	if _, err := Prune(PruneRequest{Dir: dir}); err != nil {
		t.Fatalf("a dead puller's lock still blocked the prune: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the file was not pruned")
	}
}
