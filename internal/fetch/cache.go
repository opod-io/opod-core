package fetch

// The weight cache, and the only safe way to delete from it (ADR-046).
//
// `/data/models` is a hostPath every pod on a node shares. Nothing has ever
// deleted from it, so a full node refuses new work until someone clears it by
// hand — and a hand that does not know which files are still referenced is
// exactly what D10 was written to prevent.
//
// Two facts make a delete safe, and both live here:
//
//   - a MARKER beside every file this package wrote (`<file>.opod`), so a
//     prune can only ever consider files we fetched. Anything else on the
//     volume belongs to the owner and is never touched, whatever it looks like;
//   - the same per-file LOCK a download takes, so a prune and a pull can never
//     race: whoever gets the lock goes first and the other waits.
//
// Deleting renames to `.trash-<stamp>` and then unlinks, so a reader that
// opened the file a microsecond earlier keeps reading a file that is no longer
// in anyone's way (Linux keeps the inode alive until the last handle closes).
//
// What the control plane decides — which files are still referenced by a plan,
// a job, a prefetch or a staged version — is passed in as the keep list. This
// package never guesses that.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MarkerSuffix is the sidecar this package writes beside each fetched file.
const MarkerSuffix = ".opod"

// Marker is what we know about a file we pulled.
type Marker struct {
	Repo       string    `json:"repo"`
	File       string    `json:"file"`
	Size       int64     `json:"size"`
	FetchedAt  time.Time `json:"fetchedAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

// Entry is one cached file as `opod cache ls` reports it.
type Entry struct {
	Path       string    `json:"path"`
	File       string    `json:"file"`
	Repo       string    `json:"repo,omitempty"`
	Size       int64     `json:"size"`
	Ours       bool      `json:"ours"` // has our marker: the only files a prune may touch
	FetchedAt  time.Time `json:"fetchedAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
}

func markerPath(target string) string { return target + MarkerSuffix }

// WriteMarker records a fetched file. A failure here is not fatal to the
// fetch — a file without a marker is simply one the cache will never delete.
func WriteMarker(target, repo, file string, size int64) error {
	m := Marker{Repo: repo, File: file, Size: size, FetchedAt: time.Now().UTC(), LastUsedAt: time.Now().UTC()}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(target), b, 0o644)
}

// Touch records that something used the file, for least-recently-used pruning.
// Missing marker = a file we did not write: left alone.
func Touch(target string) {
	m, err := readMarker(target)
	if err != nil {
		return
	}
	m.LastUsedAt = time.Now().UTC()
	if b, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(markerPath(target), b, 0o644)
	}
}

func readMarker(target string) (Marker, error) {
	var m Marker
	b, err := os.ReadFile(markerPath(target))
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// List reports every file in the cache directory, ours and not. A caller that
// wants to know what can be reclaimed asks Prune with dryRun.
func List(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || strings.HasSuffix(name, MarkerSuffix) || strings.HasSuffix(name, ".lock") || strings.Contains(name, ".partial-") || strings.Contains(name, ".trash-") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := Entry{Path: filepath.Join(dir, name), File: name, Size: info.Size()}
		if m, err := readMarker(e.Path); err == nil {
			e.Ours, e.Repo, e.FetchedAt, e.LastUsedAt = true, m.Repo, m.FetchedAt, m.LastUsedAt
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// PruneRequest is what the control plane asks for.
type PruneRequest struct {
	Dir string
	// Keep names the files that must survive whatever else is true: every file
	// a plan, a job, an in-flight prefetch or a staged model version on this
	// node references. The control plane computes it; this package trusts it.
	Keep []string
	// MinAge is how long a file must have gone unused before it may go, so a
	// rollback within the window still finds its weights warm.
	MinAge time.Duration
	// TargetFreeGb stops the prune as soon as the volume has this much free.
	// 0 = remove every prunable file.
	TargetFreeGb int
	DryRun       bool
}

// PruneResult is what a prune did, or would do.
type PruneResult struct {
	Removed    []Entry  `json:"removed"`
	FreedBytes int64    `json:"freedBytes"`
	Kept       int      `json:"kept"`
	Skipped    []string `json:"skipped,omitempty"` // files not ours, named so the operator sees why nothing happened
	DryRun     bool     `json:"dryRun"`
}

// Prune removes cached files that are ours, unreferenced and old enough,
// least-recently-used first. It takes each file's own download lock, so a pull
// in flight is never pruned underneath.
func Prune(req PruneRequest) (PruneResult, error) {
	res := PruneResult{Removed: []Entry{}, DryRun: req.DryRun}
	entries, err := List(req.Dir)
	if err != nil {
		return res, err
	}
	keep := map[string]bool{}
	for _, k := range req.Keep {
		keep[filepath.Base(k)] = true
	}
	now := time.Now().UTC()
	candidates := []Entry{}
	for _, e := range entries {
		switch {
		case keep[e.File]:
			res.Kept++
		case !e.Ours:
			// Not ours: never touched, whatever the disk pressure. The operator
			// sees it named rather than silently ignored.
			res.Skipped = append(res.Skipped, e.File)
		case req.MinAge > 0 && !e.LastUsedAt.IsZero() && now.Sub(e.LastUsedAt) < req.MinAge:
			res.Kept++
		default:
			candidates = append(candidates, e)
		}
	}
	// Least recently used first: the file most likely to be wanted again stays.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].LastUsedAt.Before(candidates[j].LastUsedAt) })
	for _, e := range candidates {
		if req.TargetFreeGb > 0 {
			if free, err := freeGb(req.Dir); err == nil && free >= req.TargetFreeGb {
				break // enough room: stop rather than empty the cache
			}
		}
		if req.DryRun {
			res.Removed = append(res.Removed, e)
			res.FreedBytes += e.Size
			continue
		}
		if err := removeLocked(e.Path); err != nil {
			return res, fmt.Errorf("prune %s: %w", e.File, err)
		}
		res.Removed = append(res.Removed, e)
		res.FreedBytes += e.Size
	}
	return res, nil
}

// removeLocked takes the file's download lock, re-checks that it is still ours,
// renames it out of the way and unlinks it.
func removeLocked(target string) error {
	lock := target + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("a pull holds this file's lock: not pruned")
		}
		return err
	}
	_, _ = fmt.Fprintf(f, "prune %d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = f.Close()
	defer func() { _ = os.Remove(lock) }()

	if _, err := readMarker(target); err != nil {
		return fmt.Errorf("marker gone: refusing to remove a file this cache did not write")
	}
	trash := target + ".trash-" + time.Now().UTC().Format("20060102T150405")
	if err := os.Rename(target, trash); err != nil {
		return err
	}
	_ = os.Remove(markerPath(target))
	return os.Remove(trash)
}

// SweepPartials removes the leftovers of crashed pulls: temp files and locks
// older than age with nothing writing them.
func SweepPartials(dir string, age time.Duration) (int, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	cut := time.Now().Add(-age)
	for _, de := range des {
		name := de.Name()
		if !strings.Contains(name, ".partial-") && !strings.HasSuffix(name, ".lock") && !strings.Contains(name, ".trash-") {
			continue
		}
		info, err := de.Info()
		if err != nil || info.ModTime().After(cut) {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n, nil
}

// writeMarkerRaw replaces a marker wholesale (used by tests and by Touch).
func writeMarkerRaw(target string, m Marker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(target), b, 0o644)
}
