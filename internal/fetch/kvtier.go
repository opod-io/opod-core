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
// lookup reads as a miss (tiering/manager.py complete_write, read on the
// pinned vLLM 0.27.1).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KVTierStaleWrite is how old an in-flight write (`*.tmp`) must be before a
// prune reads it as a crashed one and removes it: the engine renames a write
// into place the moment it finishes, so one this old has no writer left.
const KVTierStaleWrite = 10 * time.Minute

// kvTierLowWatermark is the share of the cap a prune over it trims down to.
const kvTierLowWatermark = 0.9

// KVTierPruneRequest is one pass over a disk tier directory.
type KVTierPruneRequest struct {
	Dir string
	// MaxBytes caps the tier's files; 0 removes all of them.
	MaxBytes int64
	// Keep names top-level entries of Dir that are never touched.
	Keep   []string
	DryRun bool
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
	var files []kvTierFile
	err := filepath.WalkDir(req.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed under us (the engine renamed a write)
			}
			return err
		}
		if filepath.Dir(p) == req.Dir && keep[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		kind := kvTierKind(d.Name())
		if kind == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // gone between the listing and the stat
		}
		files = append(files, kvTierFile{path: p, size: info.Size(), mod: info.ModTime(), kind: kind})
		res.UsedBytes += info.Size()
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("kv tier prune: walk %s: %w", req.Dir, err)
	}
	var drop []kvTierFile
	if req.MaxBytes == 0 {
		drop = files
	} else {
		now := time.Now()
		used := res.UsedBytes
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
		if used > req.MaxBytes {
			target := int64(float64(req.MaxBytes) * kvTierLowWatermark)
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
	for _, f := range drop {
		if !req.DryRun {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, fmt.Errorf("kv tier prune: remove %s: %w", f.path, err)
			}
		}
		res.Removed++
		res.FreedBytes += f.size
	}
	if req.MaxBytes == 0 && !req.DryRun {
		removeEmptyDirs(req.Dir, keep)
	}
	return res, nil
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
