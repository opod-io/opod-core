package fetch

// One fetch for every caller — the worker's llama-server launch, the
// leader's sharding pull, `opod fetch` (the control plane's prefetch Job).
//
// A shared node cache was corrupted (2026-09-14) when three workers pulled
// one file into the same path at once: each writer
// opened the target and their writes interleaved. Here a download is
// (1) exclusive per target — a lock file taken with O_EXCL; a second caller
// waits for the first to finish instead of pulling beside it — (2) written to
// a unique temp sibling and renamed into place only after the byte count
// matches the server's Content-Length, so a reader never sees a partial file
// under the final name, and (3) skipped when the file is already there with
// the size the server declares.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultHFEndpoint is the Hub; Options.Endpoint (HF_ENDPOINT through the
// config contract) names a mirror or, in tests, a local server.
const DefaultHFEndpoint = "https://huggingface.co"

// HFFileURL is the resolve URL of one file in a repo at the given Hub endpoint
// ("" = the public Hub). An empty revision means "main", which is a MOVING
// target: the same URL serves different bytes after the repo owner pushes. A
// pinned revision (a commit sha, a tag or a branch) is what makes a model
// version reproducible — R15.16 asks for one on every managed fetch.
func HFFileURL(endpoint, repo, revision, file string) string {
	if endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/"); endpoint == "" {
		endpoint = DefaultHFEndpoint
	}
	if revision = strings.TrimSpace(revision); revision == "" {
		revision = "main"
	}
	return fmt.Sprintf("%s/%s/resolve/%s/%s", endpoint, repo, url.PathEscape(revision), url.PathEscape(file))
}

// ValidRevision refuses anything that could leave the cache directory or alter
// the URL path. A Hub revision is a commit sha, a tag or a branch name.
func ValidRevision(rev string) error {
	if rev == "" {
		return nil // "main"
	}
	if strings.Contains(rev, "..") || strings.ContainsAny(rev, "/\\ \t") || len(rev) > 128 {
		return fmt.Errorf("revision %q must be a commit sha, tag or branch with no path separators", rev)
	}
	return nil
}

// RevisionDir is where a pinned revision's files live: <dir>/<repo-slug>@<rev>.
// Two revisions of one repo therefore never collide, and neither collides with
// the unpinned file at the top of the cache — which is the whole point: a plan
// that pins v1 and one that pins v2 must be able to run on the same node at
// the same time (ADR-045 §5).
func RevisionDir(dir, repo, revision string) string {
	if strings.TrimSpace(revision) == "" {
		return dir
	}
	return filepath.Join(dir, repoSlug(repo)+"@"+revision)
}

// repoSlug flattens "org/name" into one path segment.
func repoSlug(repo string) string {
	out := make([]rune, 0, len(repo))
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return strings.Trim(string(out), "-")
}

// ValidGGUFName refuses anything that could leave the models directory or
// alter the URL path: a bare filename only.
func ValidGGUFName(file string) error {
	if file == "" || file == "." || strings.Contains(file, "..") || strings.ContainsAny(file, "/\\") || filepath.Base(file) != file {
		return fmt.Errorf("file %q must be a bare filename (no path separators or ..)", file)
	}
	return nil
}

// Options tune a fetch; the zero value is right for production.
type Options struct {
	HTTP     *http.Client  // default: 6 h timeout (a 40 GB file at a slow uplink)
	Log      *slog.Logger  // default: slog.Default()
	LockWait time.Duration // how long to wait for another caller's pull (default 6 h)
	Token    string        // Hugging Face token for gated repos ("" = anonymous)
	Endpoint string        // Hub base URL ("" = DefaultHFEndpoint); HF_ENDPOINT through the config contract
	// Revision pins the Hub revision (commit sha, tag or branch). Empty means
	// "main", which moves. A pinned revision is cached under its own directory
	// so two versions of one repo coexist on a node (R15.16).
	Revision string
	// SHA256 is the file's expected digest, when the caller knows it. A
	// mismatch is an error and the bad file is removed: a model version that
	// does not hash to what the record says is not that version, and serving it
	// would make the whole version story a lie.
	SHA256 string
}

func (opt Options) withDefaults() Options {
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 6 * time.Hour}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.LockWait == 0 {
		opt.LockWait = 6 * time.Hour
	}
	return opt
}

// GGUF makes <dir>/<file> present and returns its path. Present with the
// declared size = nothing to do; present with another size = pulled again;
// another caller pulling = wait for it.
func GGUF(ctx context.Context, repo, file, dir string, opt Options) (string, error) {
	if repo == "" || file == "" {
		return "", errors.New("fetch: repo and file required")
	}
	if err := ValidGGUFName(file); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	if dir == "" {
		return "", errors.New("fetch: models directory required")
	}
	opt = opt.withDefaults()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fetch: mkdir %s: %w", dir, err)
	}
	if err := ValidRevision(opt.Revision); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	if d := RevisionDir(dir, repo, opt.Revision); d != dir {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("fetch: mkdir %s: %w", d, err)
		}
		dir = d
	}
	return one(ctx, repo, file, dir, 0, opt)
}

// one makes a single file of repo present in dir — the step every mode shares,
// so a GGUF and each file of a snapshot get the same lock, the same atomic
// write, the same digest check and the same marker. dir is final (the revision
// directory already applied); size is the file's declared size when the caller
// already knows it (a snapshot listing), 0 to ask the server.
func one(ctx context.Context, repo, file, dir string, size int64, opt Options) (string, error) {
	target := filepath.Join(dir, file)
	fileURL := HFFileURL(opt.Endpoint, repo, opt.Revision, file)
	want, known := size, size > 0
	if !known {
		want, known = expectedSize(ctx, opt, fileURL)
	}
	if complete(target, want, known) {
		return verifiedCached(target, opt)
	}
	// Exclusive per target. The lock names the holder so a stale one (a
	// crashed puller) is readable; it is cleared when the holder finishes or
	// the waiter's patience runs out with no progress.
	lock := target + ".lock"
	release, err := takeLock(ctx, lock, target, want, known, opt)
	if err != nil {
		return "", err
	}
	if release == nil {
		return verifiedCached(target, opt) // another caller finished it while we waited
	}
	defer release()
	if complete(target, want, known) {
		return verifiedCached(target, opt)
	}
	if err := download(ctx, opt, repo, fileURL, target, want); err != nil {
		if errors.Is(err, errDigest) || RefusalReason(err) != "" {
			return "", fmt.Errorf("fetch: %w", err)
		}
		return target, err
	}
	// The marker is what makes this file prunable later: a file without one is
	// never deleted by cache management, whatever the disk pressure (ADR-046).
	// It also records the digest the download was verified against, so a later
	// pinned fetch of the same file is answered without hashing it again.
	if err := writeMarker(target, Marker{Repo: repo, File: file, Size: want, SHA256: normalizeDigest(opt.SHA256),
		FetchedAt: time.Now().UTC(), LastUsedAt: time.Now().UTC()}); err != nil {
		opt.Log.Warn("cache marker not written: this file will never be pruned", "file", file, "err", err)
	}
	return target, nil
}

// verifiedCached answers a fetch with a file that is already complete on disk. With a
// digest pinned it is the digest CHECK for that file: a complete file used to
// be trusted by size alone, so the digest was only ever compared on the
// download that first wrote the file — a plan that pinned the wrong sha256
// for a file the node already held rolled out and served (design-partner
// cell, 2026-09-29, PLAN T6.10). The hash is computed once per file and
// digest and recorded in the marker; the next pinned fetch reads the marker.
func verifiedCached(target string, opt Options) (string, error) {
	Touch(target) // least-recently-used pruning needs to know it was wanted (ADR-046)
	want := normalizeDigest(opt.SHA256)
	if want == "" {
		return target, nil
	}
	if m, err := readMarker(target); err == nil && m.SHA256 == want {
		return target, nil
	}
	got, err := sha256File(target)
	if err != nil {
		return "", fmt.Errorf("fetch: hash %s: %w", filepath.Base(target), err)
	}
	if got != want {
		return "", fmt.Errorf("fetch: %w: the cached %s hashes to sha256:%s, not the sha256:%s this version records",
			errDigest, filepath.Base(target), got, want)
	}
	if m, err := readMarker(target); err == nil {
		m.SHA256 = got
		_ = writeMarker(target, m)
	}
	return target, nil
}

// sha256File hashes a file on disk.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// complete: the file exists and, when the server declared a size, matches it.
func complete(target string, want int64, known bool) bool {
	st, err := os.Stat(target)
	if err != nil || st.Size() <= 0 {
		return false
	}
	return !known || st.Size() == want
}

// expectedSize asks the server (HEAD) how big the file is; ok=false when it
// cannot say, in which case a present file is trusted.
func expectedSize(ctx context.Context, opt Options, fileURL string) (int64, bool) {
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodHead, fileURL, nil)
	if err != nil {
		return 0, false
	}
	if opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Token)
	}
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return 0, false
	}
	return resp.ContentLength, true
}

// takeLock creates the lock file exclusively. When another caller holds it,
// wait (polling) until the target is complete — then return a nil release —
// or the lock disappears, then take it. A lock older than LockWait with the
// target still absent is treated as stale and replaced.
func takeLock(ctx context.Context, lock, target string, want int64, known bool, opt Options) (func(), error) {
	deadline := time.Now().Add(opt.LockWait)
	for {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			fmt.Fprintf(f, "pid %d at %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("fetch: lock %s: %w", lock, err)
		}
		// Someone else is pulling. Wait for their result.
		opt.Log.Info("another process is fetching the same file — waiting", "file", target)
		waited := false
		for {
			if complete(target, want, known) {
				return nil, nil
			}
			if _, err := os.Stat(lock); errors.Is(err, os.ErrNotExist) {
				break // the holder finished (or gave up) without a complete file: take the lock ourselves
			}
			if st, err := os.Stat(lock); err == nil && time.Since(st.ModTime()) > opt.LockWait {
				opt.Log.Warn("stale fetch lock — replacing it", "lock", lock, "age", time.Since(st.ModTime()).String())
				_ = os.Remove(lock)
				break
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("fetch: waited %s for another process to finish %s", opt.LockWait, target)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(pollInterval(waited)):
				waited = true
			}
		}
	}
}

func pollInterval(waited bool) time.Duration {
	if !waited {
		return 200 * time.Millisecond
	}
	return 2 * time.Second
}

// download streams the file to a unique temp sibling and renames it into
// place once the byte count matches the declared size and, when the caller
// declared a digest, the bytes hash to it.
//
// The hash is taken from the stream as it is written (io.MultiWriter, the same
// shape agent.fileUpload uses) rather than by re-reading the finished file: on
// a 40 GB safetensors the second read was ~80 s of pure I/O per verified pull,
// on the path a cold node takes to serve. SHA-256 itself is not the cost —
// Go's is per-arch assembly at ~2 GB/s — the second pass over the disk was.
func download(ctx context.Context, opt Options, repo, fileURL, target string, want int64) error {
	wantSum := normalizeDigest(opt.SHA256)
	tmp := partialPath(target)

	// Resume only when the caller declared a digest, and that is not a
	// limitation worth removing. Resumed bytes are bytes nobody re-read off
	// the network: with a digest a bad partial is CAUGHT, and without one the
	// only check is the length, which a corrupt tail passes. The case that
	// matters has a digest — a snapshot takes the sha256 the Hub records for
	// every LFS file (snapshot.go), and LFS is what a 40 GB weight file is.
	var from int64
	h := sha256.New()
	if wantSum != "" {
		if n, err := seedFromPartial(tmp, h); err != nil {
			opt.Log.Info("partial unusable, starting over", "path", tmp, "err", err)
		} else if n > 0 && (want <= 0 || n < want) {
			from = n
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "opod-fetch")
	if opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Token)
	}
	if from > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-")
	}
	opt.Log.Info("fetching file", "url", fileURL, "to", target, "resume_from", from)
	t0 := time.Now()
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", fileURL, err)
	}
	defer resp.Body.Close()
	if err := hubRefusal(resp, opt, repo, fileURL); err != nil {
		return err
	}

	// What the server did with the Range decides where we write. A server
	// that ignores it answers 200 with the WHOLE file, and one that thinks we
	// already have everything answers 416 — both mean start over, and neither
	// is a failure.
	expected := want
	switch {
	case resp.StatusCode == http.StatusPartialContent && from > 0:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != from {
			return fmt.Errorf("GET %s: asked to resume at %d, server answered Content-Range %q",
				fileURL, from, resp.Header.Get("Content-Range"))
		}
		if total > 0 {
			expected = total // on a 206 Content-Length is what REMAINS, not the file
		}
		opt.Log.Info("resuming download", "path", target, "from", from, "of", expected)
	case resp.StatusCode == http.StatusOK:
		if from > 0 {
			opt.Log.Info("server ignored Range, starting over", "url", fileURL)
			from, h = 0, sha256.New()
		}
		if resp.ContentLength > 0 {
			expected = resp.ContentLength
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && from > 0:
		// The partial is at least as long as the file: it is not a resume
		// point, it is junk. Drop it and let the next attempt fetch whole.
		_ = os.Remove(tmp)
		return fmt.Errorf("GET %s → %s (the partial file was longer than the file; it has been removed)", fileURL, resp.Status)
	default:
		return fmt.Errorf("GET %s → %s", fileURL, resp.Status)
	}

	f, err := openPartial(tmp, from)
	if err != nil {
		return fmt.Errorf("open partial for %s: %w", target, err)
	}
	var sink io.Writer = f
	if wantSum != "" {
		sink = io.MultiWriter(f, h)
	}
	n, copyErr := io.Copy(sink, resp.Body)
	closeErr := f.Close()
	total := from + n
	if copyErr != nil {
		// The partial stays ONLY when something could resume from it, which
		// means a digest was declared — the next attempt reads these bytes
		// back and asks for the rest, and the digest is what proves they were
		// good. With no digest nothing will ever resume from this file, so
		// keeping it is litter the sweeper has to clear, and the old
		// behaviour (leave nothing behind) is the right one.
		if wantSum == "" {
			_ = os.Remove(tmp)
			return fmt.Errorf("download %s: %w", fileURL, copyErr)
		}
		return fmt.Errorf("download %s: %w (kept %d bytes at %s to resume from)", fileURL, copyErr, total, filepath.Base(tmp))
	}
	if closeErr != nil {
		if wantSum == "" {
			_ = os.Remove(tmp)
		}
		return fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	if expected > 0 && total != expected {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: got %d bytes, expected %d (truncated)", fileURL, total, expected)
	}
	if wantSum != "" {
		if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
			// The file is wrong, so it must never reach its final name to be
			// "found complete" by the next call. Removing it costs a
			// re-download; keeping it would serve the wrong weights forever —
			// and after a resume it would also make every later attempt
			// resume from bytes we know are bad.
			_ = os.Remove(tmp)
			return fmt.Errorf("%w: %s hashes to sha256:%s, not the sha256:%s this version records",
				errDigest, filepath.Base(target), got, wantSum)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, target, err)
	}
	opt.Log.Info("file fetched", "path", target, "bytes", total, "resumed", from, "duration_s", time.Since(t0).Seconds())
	return nil
}

// partialPath is the ONE name a target's half-downloaded bytes live under. It
// is deterministic, which is what makes a resume possible: a unique temp name
// per attempt means the next attempt cannot find the last one's work. Two
// callers cannot collide on it because a fetch is exclusive per target (the
// .lock above), and the name still contains ".partial-" so the cache's
// bookkeeping filter and SweepPartials keep treating it as scratch.
func partialPath(target string) string { return target + ".partial-0" }

// seedFromPartial hashes the bytes already on disk into h and returns how many
// there were, so a resumed download keeps its single pass over the NETWORK.
// Reading the partial back costs one local pass — seconds on a disk that does
// GB/s — against minutes or hours of re-downloading, which is the trade this
// row exists to make. Nothing on disk is not an error: 0 bytes, hash untouched.
func seedFromPartial(tmp string, h io.Writer) (int64, error) {
	f, err := os.Open(tmp)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// openPartial opens the scratch file to append at from, or truncates it when
// the download starts at zero.
func openPartial(tmp string, from int64) (*os.File, error) {
	flags := os.O_CREATE | os.O_WRONLY
	if from > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(tmp, flags, 0o644)
}

// parseContentRange reads "bytes <start>-<end>/<total>". total is 0 when the
// server sent "*", which is legal and only means we keep the size we had.
func parseContentRange(v string) (start, total int64, ok bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	v = strings.TrimSpace(v)
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0, 0, false
	}
	span, size := v[:slash], v[slash+1:]
	dash := strings.Index(span, "-")
	if dash < 0 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(span[:dash]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if size != "*" {
		if t, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64); err == nil {
			total = t
		}
	}
	return start, total, true
}

// errDigest marks a download whose bytes did not hash to the digest the caller
// declared. The size check still applies when no digest was.
var errDigest = errors.New("digest mismatch")

// normalizeDigest strips the "sha256:" prefix and case so a digest compares
// as hex. Empty = nothing to check.
func normalizeDigest(want string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(want, "sha256:")))
}
