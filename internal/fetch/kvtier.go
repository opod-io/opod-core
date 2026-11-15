package fetch

// The disk KV tier's directory, and how it is kept bounded (PLAN T26.9).
//
// vLLM's fs tier (TieringOffloadingSpec, secondary tier `fs`) writes one file
// per offloaded block under its root_dir and deletes nothing: it has no
// capacity and no eviction. Left alone it fills the volume, and on a node the
// kubelet watches that is DiskPressure — every pod on the node is evicted, and
// the cleanup Job cannot be admitted to the node that needs it. So whoever
// names the directory bounds it, and this is the mechanism:
//
//   - only the tier's own files are ever touched: block files (`*.bin`), its
//     in-flight writes (`*.tmp`, written then renamed), and the `config.json`
//     each layout writes beside its blocks. Nothing else under the directory
//     is read as the tier's, whatever it is;
//   - over the cap, the OLDEST blocks go first (by modification time — a block
//     is written once, so its age is how long the engine has gone without
//     writing it again), down to a low watermark below the cap so a busy tier
//     is not trimmed by one file at a time on every pass;
//   - a cap of zero empties the tier (an endpoint deleted, a layout no longer
//     served), keeping only the top-level entries the caller names.
//
// A block removed while the engine reads it is safe: the open handle keeps the
// inode alive until it closes. A block removed between the engine's lookup and
// its read fails that one block's promotion into the CPU tier, which the next
// lookup reads as a miss (tiering/manager.py complete_store, read on vLLM
// 0.27.1 and 0.33.0).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// KVTierStaleWrite is how old an in-flight write (`*.tmp`) must be before a
// prune reads it as a crashed one and removes it: the engine renames a write
// into place the moment it finishes, so one this old has no writer left.
const KVTierStaleWrite = 10 * time.Minute

// kvTierLowWatermark is the share of the cap a prune over it trims down to.
const kvTierLowWatermark = 0.9

// KVTierHotDelay is how soon the next pass follows one that found the tier
// growing or had to trim it. The engine writes a burst far faster than an idle
// period allows for: on the design-partner cell a 4 GiB tier took ≈7 GB in a
// minute (≈120 MB/s), so a fixed 10 s period let it reach twice its cap (PLAN
// T26.20). One second of writes is a few percent of any sensible cap.
const KVTierHotDelay = time.Second

// KVTierPruneRequest is one pass over a disk tier directory.
type KVTierPruneRequest struct {
	Dir string
	// MaxBytes caps the tier's files; 0 removes all of them.
	MaxBytes int64
	// Keep names top-level entries of Dir that are never touched.
	Keep []string
	// Reserve is how much the tier is expected to grow before the next pass
	// reads it (the loop measures it as the growth between its last two
	// passes). A pass counts it as already written, so it trims before a burst
	// crosses the cap rather than after; it is held to half the cap, so a
	// burst larger than the tier never empties it.
	Reserve int64
	DryRun  bool
}

// KVTierPruneResult is what one pass found and did.
type KVTierPruneResult struct {
	Dir        string `json:"dir"`
	DryRun     bool   `json:"dryRun"`
	UsedBytes  int64  `json:"usedBytes"` // the tier's files before the pass
	Removed    int    `json:"removed"`
	FreedBytes int64  `json:"freedBytes"`
	TotalBytes int64  `json:"totalBytes,omitempty"`
	FreeBytes  int64  `json:"freeBytes,omitempty"`
	VolumeErr  string `json:"volumeErr,omitempty"`
	// WalkMs and RemoveMs are how long the pass spent listing the tier and
	// deleting from it: a pass much longer than the loop's period is what
	// lets a burst outrun the cap (PLAN T26.20).
	WalkMs   int64 `json:"walkMs"`
	RemoveMs int64 `json:"removeMs"`
}

type kvTierFile struct {
	path string
	size int64
	mod  time.Time
	kind string // bin | tmp | config
}

// kvTierKind is which of the tier's own files a name is, or "".
func kvTierKind(name string) string {
	switch {
	case strings.HasSuffix(name, ".bin"):
		return "bin"
	case strings.HasSuffix(name, ".tmp"):
		return "tmp"
	case name == "config.json":
		return "config"
	}
	return ""
}

// PruneKVTier makes one pass over a disk tier directory. A directory that does
// not exist yet is an empty tier, not an error: the engine creates it on its
// first write.
func PruneKVTier(req KVTierPruneRequest) (KVTierPruneResult, error) {
	res := KVTierPruneResult{Dir: req.Dir, DryRun: req.DryRun}
	if req.Dir == "" || !filepath.IsAbs(req.Dir) {
		return res, fmt.Errorf("kv tier prune: --dir must be an absolute directory, not %q", req.Dir)
	}
	if req.MaxBytes < 0 {
		return res, fmt.Errorf("kv tier prune: the cap must be 0 or more, not %d bytes", req.MaxBytes)
	}
	if _, err := os.Stat(req.Dir); errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if total, free, err := volumeBytes(req.Dir); err == nil {
		res.TotalBytes, res.FreeBytes = int64(total), int64(free)
	} else {
		res.VolumeErr = err.Error()
	}
	keep := map[string]bool{}
	for _, k := range req.Keep {
		if k = strings.TrimSpace(k); k != "" {
			keep[filepath.Base(k)] = true
		}
	}
	walkStart := time.Now()
	files, err := walkKVTier(req.Dir, keep, kvTierWalkers)
	if err != nil {
		return res, fmt.Errorf("kv tier prune: walk %s: %w", req.Dir, err)
	}
	for _, f := range files {
		res.UsedBytes += f.size
	}
	res.WalkMs = time.Since(walkStart).Milliseconds()
	var drop []kvTierFile
	if req.MaxBytes == 0 {
		drop = files
	} else {
		now := time.Now()
		used := res.UsedBytes
		reserve := min(max(req.Reserve, 0), req.MaxBytes/2)
		var blocks []kvTierFile
		for _, f := range files {
			switch {
			case f.kind == "tmp" && now.Sub(f.mod) > KVTierStaleWrite:
				drop = append(drop, f)
				used -= f.size
			case f.kind == "bin":
				blocks = append(blocks, f)
			}
		}
		if used+reserve > req.MaxBytes {
			target := int64(float64(req.MaxBytes)*kvTierLowWatermark) - reserve
			sort.Slice(blocks, func(i, j int) bool { return blocks[i].mod.Before(blocks[j].mod) })
			for _, b := range blocks {
				if used <= target {
					break
				}
				drop = append(drop, b)
				used -= b.size
			}
		}
	}
	removeStart := time.Now()
	for _, f := range drop {
		if !req.DryRun {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				res.RemoveMs = time.Since(removeStart).Milliseconds()
				return res, fmt.Errorf("kv tier prune: remove %s: %w", f.path, err)
			}
		}
		res.Removed++
		res.FreedBytes += f.size
	}
	res.RemoveMs = time.Since(removeStart).Milliseconds()
	if req.MaxBytes == 0 && !req.DryRun {
		removeEmptyDirs(req.Dir, keep)
	}
	return res, nil
}

// kvTierWalkers is how many directories a pass lists at once. A network
// filesystem answers each listing and each stat in a round trip, and vLLM puts
// almost every block in a hash-named directory of its own, so a sequential
// walk costs about a millisecond per block (3,362 blocks in 3.7 s on the
// design-partner cell's NFS) and a burst outruns it. Concurrent listings hide
// the round trips; on a local disk they cost nothing.
const kvTierWalkers = 32

// kvTierReadDir lists one directory; a test slows it to stand in for a network
// filesystem.
var kvTierReadDir = os.ReadDir

// walkKVTier lists the tier's own files under root with up to workers
// directory listings in flight, skipping the kept top-level entries. A
// directory or file removed under the walk is skipped (the engine renames its
// writes, and a pruner removes); any other error ends the walk. The queue is a
// slice, not a goroutine per directory, so a tier of a million directories
// holds a fixed number of goroutines.
func walkKVTier(root string, keep map[string]bool, workers int) ([]kvTierFile, error) {
	var (
		mu       sync.Mutex
		wake     = sync.NewCond(&mu)
		queue    = []string{root}
		busy     int
		files    []kvTierFile
		firstErr error
		wg       sync.WaitGroup
	)
	list := func(dir string) ([]string, []kvTierFile, error) {
		entries, err := kvTierReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, err
		}
		var subdirs []string
		var found []kvTierFile
		for _, d := range entries {
			if dir == root && keep[d.Name()] {
				continue
			}
			p := filepath.Join(dir, d.Name())
			if d.IsDir() {
				subdirs = append(subdirs, p)
				continue
			}
			kind := kvTierKind(d.Name())
			if !d.Type().IsRegular() || kind == "" {
				continue
			}
			info, err := d.Info()
			if err != nil {
				continue // gone between the listing and the stat
			}
			found = append(found, kvTierFile{path: p, size: info.Size(), mod: info.ModTime(), kind: kind})
		}
		return subdirs, found, nil
	}
	for range max(workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			for {
				for len(queue) == 0 && busy > 0 && firstErr == nil {
					wake.Wait()
				}
				if firstErr != nil || len(queue) == 0 {
					wake.Broadcast()
					return
				}
				dir := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				busy++
				mu.Unlock()
				subdirs, found, err := list(dir)
				mu.Lock()
				busy--
				if err != nil && firstErr == nil {
					firstErr = err
				}
				queue = append(queue, subdirs...)
				files = append(files, found...)
				wake.Broadcast()
			}
		}()
	}
	wg.Wait()
	return files, firstErr
}

// removeEmptyDirs removes every directory under dir (and dir itself) left
// empty, deepest first, skipping the kept top-level entries. os.Remove refuses
// a directory that still holds anything, so nothing but empty directories go.
func removeEmptyDirs(dir string, keep map[string]bool) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if filepath.Dir(p) == dir && keep[d.Name()] {
			return filepath.SkipDir
		}
		dirs = append(dirs, p)
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}

// RunKVTierPruner makes passes over a disk tier until ctx ends, reporting each
// one. The idle period is every; a pass that trimmed the tier, or found it
// larger than the previous pass left it, is followed after hot instead — so
// the tier is re-read every second for as long as the engine writes to it and
// every idle period once it stops. A pass in progress is never interrupted,
// and the delay counts from its end. A failed pass is reported and the next
// one tries again at the idle period.
func RunKVTierPruner(ctx context.Context, req KVTierPruneRequest, every, hot time.Duration, report func(KVTierPruneResult, error)) {
	if hot <= 0 || hot > every {
		hot = every
	}
	left := int64(-1) // the tier's bytes after the previous pass; -1 = none yet
	for {
		res, err := PruneKVTier(req)
		report(res, err)
		delay := every
		if err == nil {
			now := res.UsedBytes - res.FreedBytes
			if (res.Removed > 0 && !req.DryRun) || (left >= 0 && now > left) {
				delay = hot
			}
			// What the engine wrote between the previous pass and this one is
			// what it will likely write before the next: the next pass trims
			// that much ahead. A slow pass (a network filesystem) measures a
			// larger growth, so it trims further ahead — the bound holds
			// while one pass's growth is under the cap's headroom.
			req.Reserve = 0
			if left >= 0 && res.UsedBytes > left {
				req.Reserve = res.UsedBytes - left
			}
			left = now
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
