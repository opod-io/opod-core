package fetch

// One fetch for every caller — the worker's llama-server launch, the
// leader's sharding pull, `opod fetch` (the control plane's prefetch Job).
//
// The design-partner cell (2026-09-14) corrupted a shared node cache when
// three workers pulled one file into the same path at once: each writer
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
		Touch(target) // least-recently-used pruning needs to know it was wanted (ADR-046)
		return target, nil
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
		return target, nil // another caller finished it while we waited
	}
	defer release()
	if complete(target, want, known) {
		Touch(target)
		return target, nil
	}
	if err := download(ctx, opt, fileURL, target, want); err != nil {
		return target, err
	}
	if err := verifyDigest(target, opt.SHA256); err != nil {
		// The file is wrong, so it must not survive to be "found complete" by
		// the next call. Removing it costs a re-download; keeping it would
		// serve the wrong weights forever.
		_ = os.Remove(target)
		return "", fmt.Errorf("fetch: %w", err)
	}
	// The marker is what makes this file prunable later: a file without one is
	// never deleted by cache management, whatever the disk pressure (ADR-046).
	if err := WriteMarker(target, repo, file, want); err != nil {
		opt.Log.Warn("cache marker not written: this file will never be pruned", "file", file, "err", err)
	}
	return target, nil
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
// place once the byte count matches the declared size.
func download(ctx context.Context, opt Options, fileURL, target string, want int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "opod-fetch")
	if opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Token)
	}
	opt.Log.Info("fetching file", "url", fileURL, "to", target)
	t0 := time.Now()
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", fileURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s → %s", fileURL, resp.Status)
	}
	expected := resp.ContentLength
	if expected <= 0 {
		expected = want
	}
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".partial-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", target, err)
	}
	tmp := f.Name()
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: %w", fileURL, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	if expected > 0 && n != expected {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: got %d bytes, expected %d (truncated)", fileURL, n, expected)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, target, err)
	}
	opt.Log.Info("file fetched", "path", target, "bytes", n, "duration_s", time.Since(t0).Seconds())
	return nil
}

// verifyDigest checks a downloaded file against the digest the caller
// declared. No digest = nothing to check (the Hub's size check still applies).
func verifyDigest(target, want string) error {
	want = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(want, "sha256:")))
	if want == "" {
		return nil
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s hashes to sha256:%s, not the sha256:%s this version records", filepath.Base(target), got, want)
	}
	return nil
}
