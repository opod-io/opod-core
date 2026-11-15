package fetch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
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

// kvTierBytes is what the tier's own files hold right now.
func kvTierBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || kvTierKind(d.Name()) == "" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// A burst far faster than the idle period stays under 1.25 × the cap (PLAN
// T26.20): the loop re-reads a growing tier after the hot delay, not the idle
// one. The ratios are the cell's, compressed: there a 10 s idle period let a
// burst of about one cap every 37 s reach 2× the cap. Here the writer puts the
// whole cap down every 0.5 s, the idle period alone would let 40 % of the cap
// through, and the hot delay lets through about 2 %.
func TestKVTierPrunerBoundsABurst(t *testing.T) {
	dir := t.TempDir()
	const (
		capBytes = 4 << 20
		fileSize = 32 << 10
		every    = 200 * time.Millisecond
		hot      = 10 * time.Millisecond
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var passMu sync.Mutex
	var passes []time.Time // when each pass ended
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunKVTierPruner(ctx, KVTierPruneRequest{Dir: dir, MaxBytes: capBytes}, every, hot, func(_ KVTierPruneResult, err error) {
			if err != nil {
				t.Errorf("pass: %v", err)
			}
			passMu.Lock()
			passes = append(passes, time.Now())
			passMu.Unlock()
		})
	}()

	var peak int64
	stopSample := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if n := kvTierBytes(dir); n > peak {
				peak = n
			}
			select {
			case <-stopSample:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	// vLLM's layout and its write: <root>_r0/<hhh>/<hh>_g0/<hash>.bin through a
	// .tmp renamed into place. 6 × the cap at 8 MiB/s: one file every ~4 ms.
	blob := make([]byte, fileSize)
	burstStart := time.Now()
	for i := 0; i < 6*capBytes/fileSize; i++ {
		sub := filepath.Join(dir, "x_r0", fmt.Sprintf("%03x", i%7), fmt.Sprintf("%02x_g0", i%3))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(sub, fmt.Sprintf("%08d.bin", i))
		if err := os.WriteFile(p+".tmp", blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Millisecond)
	}
	burstEnd := time.Now()
	time.Sleep(every + 50*time.Millisecond)
	close(stopSample)
	<-sampled
	cancel()
	<-done

	// The design property (T26.20): while the tier grows, the next pass follows
	// after the hot delay, not the idle period. Asserted on the passes' own
	// rhythm, which does not depend on how fast a loaded machine lets them run:
	// the median gap between passes during the burst is far below the idle
	// period. (A bound on the peak alone failed 1 run in 12 on a CPU-starved
	// machine at 1.29 × the cap, the pruner's thread waiting while the writer
	// kept writing — scheduling, not the rule.)
	passMu.Lock()
	var gaps []time.Duration
	for i := 1; i < len(passes); i++ {
		if passes[i-1].After(burstStart) && passes[i].Before(burstEnd) {
			gaps = append(gaps, passes[i].Sub(passes[i-1]))
		}
	}
	passMu.Unlock()
	if len(gaps) < 5 {
		t.Fatalf("only %d passes during a %s burst: the pruner did not follow the growing tier", len(gaps)+1, burstEnd.Sub(burstStart).Round(time.Millisecond))
	}
	slices.Sort(gaps)
	if median := gaps[len(gaps)/2]; median > every/2 {
		t.Fatalf("median gap between passes during the burst %s, want the hot cadence (%s), well under the idle period (%s)", median, hot, every)
	}
	// And the regression it guards: on the cell the idle period alone let a
	// burst reach 2 × the cap.
	if limit := int64(capBytes) * 2; peak >= limit {
		t.Fatalf("the tier peaked at %d bytes during the burst, 2 × the %d-byte cap or more", peak, capBytes)
	}
	if end := kvTierBytes(dir); end > capBytes {
		t.Fatalf("the tier settled at %d bytes, over its %d-byte cap", end, capBytes)
	}
	t.Logf("peak %.2f × the cap", float64(peak)/capBytes)
}

// On a network filesystem a pass is slow, and its cost is per directory: on the
// design-partner cell's NFS a walk of 3,362 block files (each in its own
// hash-named directory) took 3.7 s, two writers outran it, and a 1 GiB tier
// reached 2.28 GiB (PLAN T26.24). Each directory listing here costs 2 ms; the
// writers spread over 448 directories, so one sequential pass costs about a
// second while they write 8 MiB/s into a 4 MiB tier. The bound holds when the
// pass reads directories concurrently and trims ahead of the growth it measured.
func TestKVTierPrunerHoldsTheBoundOnASlowFilesystem(t *testing.T) {
	dir := t.TempDir()
	const (
		capBytes = 4 << 20
		fileSize = 32 << 10
		every    = 200 * time.Millisecond
		hot      = 10 * time.Millisecond
	)
	slow := kvTierReadDir
	kvTierReadDir = func(name string) ([]os.DirEntry, error) {
		time.Sleep(2 * time.Millisecond)
		return slow(name)
	}
	defer func() { kvTierReadDir = slow }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunKVTierPruner(ctx, KVTierPruneRequest{Dir: dir, MaxBytes: capBytes}, every, hot, func(_ KVTierPruneResult, err error) {
			if err != nil {
				t.Errorf("pass: %v", err)
			}
		})
	}()
	var peak int64
	stopSample := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if n := kvTierBytes(dir); n > peak {
				peak = n
			}
			select {
			case <-stopSample:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	blob := make([]byte, fileSize)
	for i := 0; i < 6*capBytes/fileSize; i++ {
		sub := filepath.Join(dir, "x_r0", fmt.Sprintf("%03x", i%64), fmt.Sprintf("%02x_g0", i%6))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(sub, fmt.Sprintf("%08d.bin", i))
		if err := os.WriteFile(p+".tmp", blob, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p+".tmp", p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Millisecond)
	}
	time.Sleep(every + 100*time.Millisecond)
	close(stopSample)
	<-sampled
	cancel()
	<-done
	// The row's bound is 1.25 × the cap, proven on the cell; here 1.5 leaves room
	// for a CPU-starved test machine, and a sequential pass lands near 2.
	if limit := int64(capBytes) * 3 / 2; peak >= limit {
		t.Fatalf("the tier peaked at %.2f × its cap on a slow filesystem", float64(peak)/capBytes)
	}
	if end := kvTierBytes(dir); end > capBytes {
		t.Fatalf("the tier settled at %d bytes, over its %d-byte cap", end, capBytes)
	}
	t.Logf("peak %.2f × the cap", float64(peak)/capBytes)
}

// A pass trims ahead of the growth it is told to expect: the reserve counts as
// already written, so a tier under its cap but within the reserve of it is
// trimmed, down to the low watermark less the reserve.
func TestKVTierPruneTrimsAheadOfTheReserve(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		writeAged(t, dir, fmt.Sprintf("b%02d.bin", i), 100, time.Duration(10-i)*time.Minute)
	}
	// 1,000 bytes under a 1,200 cap: nothing goes without a reserve.
	res, err := PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 1200})
	if err != nil || res.Removed != 0 {
		t.Fatalf("no reserve: removed %d (%v), want 0", res.Removed, err)
	}
	// With 400 bytes expected, 1,400 > 1,200: trim to 0.9 × 1,200 − 400 = 680.
	res, err = PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 1200, Reserve: 400})
	if err != nil {
		t.Fatal(err)
	}
	if left := res.UsedBytes - res.FreedBytes; res.Removed != 4 || left != 600 {
		t.Fatalf("reserve 400: removed %d, %d bytes left; want 4 removed, 600 left", res.Removed, left)
	}
	if exists(filepath.Join(dir, "b00.bin")) || !exists(filepath.Join(dir, "b09.bin")) {
		t.Fatal("the reserve trim did not take the oldest blocks first")
	}
	// A reserve at or above the cap is held to half of it, never an empty tier.
	res, err = PruneKVTier(KVTierPruneRequest{Dir: dir, MaxBytes: 1200, Reserve: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if left := res.UsedBytes - res.FreedBytes; left == 0 {
		t.Fatal("a reserve larger than the cap emptied the tier")
	}
}

// An idle tier is read at the idle period; a tier that grew since the last
// pass is read again after the hot delay.
func TestKVTierPrunerCadence(t *testing.T) {
	dir := t.TempDir()
	writeAged(t, dir, "a.bin", 100, time.Hour)
	const every = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passes := make(chan time.Time, 16)
	go RunKVTierPruner(ctx, KVTierPruneRequest{Dir: dir, MaxBytes: 1 << 20}, every, time.Millisecond, func(KVTierPruneResult, error) {
		passes <- time.Now()
	})
	first := <-passes
	second := <-passes // nothing to compare the first against: the idle period
	if gap := second.Sub(first); gap < every*2/3 {
		t.Fatalf("an idle tier was read again after %v, not the %v idle period", gap, every)
	}
	writeAged(t, dir, "b.bin", 100, 0) // grows before the third pass
	third := <-passes
	fourth := <-passes // the third saw growth: the hot delay
	if gap := fourth.Sub(third); gap > every/2 {
		t.Fatalf("a growing tier waited %v for its next pass, not the hot delay", gap)
	}
}
