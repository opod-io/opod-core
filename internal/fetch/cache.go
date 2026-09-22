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
// A pinned revision lives one level down, in `<repo-slug>@<rev>/` (RevisionDir),
// and the same two facts hold there. A GGUF revision directory holds files that
// are listed and pruned one by one, like the top of the cache. A SNAPSHOT
// directory (snapshot.go: it has a manifest) is one unit — an engine loads the
// directory, so half of it is worth nothing: it is listed once with its total
// size, and a prune takes every file's lock and removes all of it, or none.
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
	"path"
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

// Entry is one cached file — or one snapshot directory — as `opod cache ls`
// reports it.
type Entry struct {
	Path string `json:"path"`
	// File is the name relative to the cache directory: `a.gguf` at the top,
	// `<repo-slug>@<rev>/a.gguf` for a pinned file, `<repo-slug>@<rev>` for a
	// snapshot.
	File       string    `json:"file"`
	Repo       string    `json:"repo,omitempty"`
	Size       int64     `json:"size"` // a snapshot: the sum of what the directory holds
	Ours       bool      `json:"ours"` // has our marker: the only files a prune may touch
	FetchedAt  time.Time `json:"fetchedAt,omitempty"`
	LastUsedAt time.Time `json:"lastUsedAt,omitempty"`
	// Revision is set for everything under a revision directory.
	Revision string `json:"revision,omitempty"`
	// Snapshot marks a whole snapshot directory, listed and pruned as one
	// unit; Files is how many files it holds. It is ours only when the manifest
	// reads and every file in it carries our marker — one foreign file and the
	// directory is left alone, whole.
	Snapshot bool `json:"snapshot,omitempty"`
	Files    int  `json:"files,omitempty"`
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

// bookkeeping says a name is this package's own working file (a marker, a
// lock, a temp or trash sibling), never a cache entry.
func bookkeeping(name string) bool {
	return strings.HasSuffix(name, MarkerSuffix) || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, ".partial-") || strings.Contains(name, ".trash-")
}

// revisionOf reads the revision out of a `<repo-slug>@<rev>` directory name.
// Any other directory in the cache is not ours to look into.
func revisionOf(name string) (string, bool) {
	i := strings.LastIndex(name, "@")
	if i <= 0 || i == len(name)-1 || ValidRevision(name[i+1:]) != nil {
		return "", false
	}
	return name[i+1:], true
}

// List reports every file in the cache directory and in its revision
// directories, ours and not; a snapshot directory is one entry. A caller that
// wants to know what can be reclaimed asks Prune with dryRun.
func List(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, de := range des {
		if !de.IsDir() {
			if e, ok := fileEntry(dir, "", "", de); ok {
				out = append(out, e)
			}
			continue
		}
		if rev, ok := revisionOf(de.Name()); ok {
			out = append(out, listRevisionDir(dir, de.Name(), rev)...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// fileEntry describes one cached file; sub is its revision directory ("" at
// the top of the cache).
func fileEntry(dir, sub, rev string, de os.DirEntry) (Entry, bool) {
	if de.IsDir() || bookkeeping(de.Name()) {
		return Entry{}, false
	}
	info, err := de.Info()
	if err != nil {
		return Entry{}, false
	}
	e := Entry{Path: filepath.Join(dir, sub, de.Name()), File: path.Join(sub, de.Name()), Size: info.Size(), Revision: rev}
	if m, err := readMarker(e.Path); err == nil {
		e.Ours, e.Repo, e.FetchedAt, e.LastUsedAt = true, m.Repo, m.FetchedAt, m.LastUsedAt
	}
	return e, true
}

// listRevisionDir lists one `<repo-slug>@<rev>` directory: a single entry when
// it is a snapshot, its files one by one when it is not.
func listRevisionDir(dir, name, rev string) []Entry {
	des, err := os.ReadDir(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, name, SnapshotManifestName)); err == nil {
		return []Entry{snapshotEntry(dir, name, rev, des)}
	}
	var out []Entry
	for _, de := range des {
		if e, ok := fileEntry(dir, name, rev, de); ok {
			out = append(out, e)
		}
	}
	return out
}

// snapshotEntry sums a snapshot directory into one entry. Last used is the
// most recent use of any file in it: an engine that loads the directory
// touches them all (SnapshotPath).
func snapshotEntry(dir, name, rev string, des []os.DirEntry) Entry {
	sdir := filepath.Join(dir, name)
	e := Entry{Path: sdir, File: name, Revision: rev, Snapshot: true, Ours: true}
	var man SnapshotManifest
	if b, err := os.ReadFile(filepath.Join(sdir, SnapshotManifestName)); err != nil || json.Unmarshal(b, &man) != nil {
		e.Ours = false // a manifest we cannot read is not proof that we wrote the rest
	}
	e.Repo, e.FetchedAt = man.Repo, man.CompletedAt
	for _, de := range des {
		n := de.Name()
		if de.IsDir() {
			e.Ours = false // a snapshot is flat: a subdirectory is someone else's
			continue
		}
		if bookkeeping(n) {
			continue
		}
		if info, err := de.Info(); err == nil {
			e.Size += info.Size()
		}
		if n == SnapshotManifestName {
			continue
		}
		e.Files++
		m, err := readMarker(filepath.Join(sdir, n))
		if err != nil {
			e.Ours = false
			continue
		}
		if m.LastUsedAt.After(e.LastUsedAt) {
			e.LastUsedAt = m.LastUsedAt
		}
	}
	return e
}

// PruneRequest is what the control plane asks for.
type PruneRequest struct {
	Dir string
	// Keep names the files that must survive whatever else is true: every file
	// a plan, a job, an in-flight prefetch or a staged model version on this
	// node references. The control plane computes it; this package trusts it.
	// A bare file name keeps that name wherever it is cached, at the top or
	// under any revision; a path keeps exactly that entry. A snapshot is kept
	// by its directory (`<repo-slug>@<rev>`, bare or as a path) or by the path
	// of any file inside it — never by a bare file name, since every snapshot
	// holds a config.json.
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
	// TotalBytes / FreeBytes are the volume holding the cache, as the prune
	// measured it where it ran. A manager reads them instead of inferring the
	// numbers from a node probe that may not have run — on a node whose disk is
	// full, the probe is the first thing the kubelet stops admitting, so the one
	// node whose free space matters reported none (2026-09-21). 0 = statfs
	// failed, and VolumeErr says so.
	TotalBytes int64  `json:"totalBytes,omitempty"`
	FreeBytes  int64  `json:"freeBytes,omitempty"`
	VolumeErr  string `json:"volumeErr,omitempty"`
}

// keepSet answers "does the keep list name this entry?" (PruneRequest.Keep).
type keepSet struct {
	base map[string]bool // the last path element of every keep
	rel  []string        // every keep as a slash path relative to the cache directory
}

func newKeepSet(dir string, keep []string) keepSet {
	ks := keepSet{base: map[string]bool{}}
	for _, k := range keep {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		ks.base[filepath.Base(k)] = true
		if filepath.IsAbs(k) {
			r, err := filepath.Rel(dir, k)
			if err != nil {
				continue
			}
			k = r
		}
		ks.rel = append(ks.rel, filepath.ToSlash(filepath.Clean(k)))
	}
	return ks
}

func (ks keepSet) has(e Entry) bool {
	if !e.Snapshot {
		if ks.base[path.Base(e.File)] {
			return true
		}
	} else if ks.base[e.File] {
		return true
	}
	for _, r := range ks.rel {
		if r == e.File || (e.Snapshot && strings.HasPrefix(r, e.File+"/")) {
			return true
		}
	}
	return false
}

// Prune removes cached files that are ours, unreferenced and old enough,
// least-recently-used first. It takes each file's own download lock, so a pull
// in flight is never pruned underneath. A snapshot directory goes whole, under
// every one of its files' locks, or stays whole.
func Prune(req PruneRequest) (PruneResult, error) {
	res := PruneResult{Removed: []Entry{}, DryRun: req.DryRun}
	if total, free, err := volumeBytes(req.Dir); err == nil {
		res.TotalBytes, res.FreeBytes = int64(total), int64(free)
	} else {
		res.VolumeErr = err.Error()
	}
	entries, err := List(req.Dir)
	if err != nil {
		return res, err
	}
	keep := newKeepSet(req.Dir, req.Keep)
	now := time.Now().UTC()
	candidates := []Entry{}
	for _, e := range entries {
		switch {
		case keep.has(e):
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
		remove := removeLocked
		if e.Snapshot {
			remove = removeSnapshotLocked
		}
		if err := remove(e.Path); err != nil {
			return res, fmt.Errorf("prune %s: %w", e.File, err)
		}
		if e.Revision != "" {
			// The revision directory goes once it is empty; os.Remove refuses
			// one that still holds anything, ours or not.
			rdir := e.Path
			if !e.Snapshot {
				rdir = filepath.Dir(e.Path)
			}
			_ = os.Remove(rdir)
		}
		res.Removed = append(res.Removed, e)
		res.FreedBytes += e.Size
	}
	return res, nil
}

// lockForPrune takes a file's download lock, or says a pull holds it.
func lockForPrune(target string) (release func(), err error) {
	lock := target + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("a pull holds this file's lock: not pruned")
		}
		return nil, err
	}
	_, _ = fmt.Fprintf(f, "prune %d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = f.Close()
	return func() { _ = os.Remove(lock) }, nil
}

// trash renames a file out of the way and unlinks it, marker included.
func trash(target string) error {
	gone := target + ".trash-" + time.Now().UTC().Format("20060102T150405")
	if err := os.Rename(target, gone); err != nil {
		return err
	}
	_ = os.Remove(markerPath(target))
	return os.Remove(gone)
}

// removeLocked takes the file's download lock, re-checks that it is still ours,
// renames it out of the way and unlinks it.
func removeLocked(target string) error {
	release, err := lockForPrune(target)
	if err != nil {
		return err
	}
	defer release()
	if _, err := readMarker(target); err != nil {
		return fmt.Errorf("marker gone: refusing to remove a file this cache did not write")
	}
	return trash(target)
}

// removeSnapshotLocked removes a snapshot directory as one unit. It takes the
// download lock of EVERY file first — one held by a pull and nothing is
// touched — and re-checks under the locks that each file is still ours. The
// manifest goes first: from that instant SnapshotPath answers "no", so an
// engine starting mid-prune falls back to its own pull instead of loading a
// directory that is losing files.
func removeSnapshotLocked(sdir string) error {
	des, err := os.ReadDir(sdir)
	if err != nil {
		return err
	}
	var files []string
	for _, de := range des {
		if de.IsDir() {
			return fmt.Errorf("holds a directory this cache did not write: not pruned")
		}
		if n := de.Name(); n != SnapshotManifestName && !bookkeeping(n) {
			files = append(files, filepath.Join(sdir, n))
		}
	}
	var held []func()
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for _, f := range files {
		release, err := lockForPrune(f)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		held = append(held, release)
	}
	for _, f := range files {
		if _, err := readMarker(f); err != nil {
			return fmt.Errorf("%s: marker gone: refusing to remove a snapshot this cache did not wholly write", filepath.Base(f))
		}
	}
	if err := os.Remove(filepath.Join(sdir, SnapshotManifestName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, f := range files {
		if err := trash(f); err != nil {
			return err
		}
	}
	return nil // the directory itself goes in Prune, once these locks are released
}

// SweepPartials removes the leftovers of crashed pulls: temp files and locks
// older than age with nothing writing them, at the top of the cache and in
// every revision directory.
func SweepPartials(dir string, age time.Duration) (int, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := sweepDir(dir, des, time.Now().Add(-age))
	for _, de := range des {
		if _, ok := revisionOf(de.Name()); !ok || !de.IsDir() {
			continue
		}
		sub := filepath.Join(dir, de.Name())
		if sdes, err := os.ReadDir(sub); err == nil {
			n += sweepDir(sub, sdes, time.Now().Add(-age))
		}
	}
	return n, nil
}

func sweepDir(dir string, des []os.DirEntry, cut time.Time) int {
	n := 0
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || (!strings.Contains(name, ".partial-") && !strings.HasSuffix(name, ".lock") && !strings.Contains(name, ".trash-")) {
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
	return n
}

// writeMarkerRaw replaces a marker wholesale (used by tests and by Touch).
func writeMarkerRaw(target string, m Marker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(target), b, 0o644)
}
