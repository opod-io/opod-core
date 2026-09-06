package scheduler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/models"
)

// ensureLocalGGUF resolves the local filesystem path of a sharded model's
// GGUF, downloading from HuggingFace when needed. Returns the absolute path
// to the file on the leader's disk; caller hands that to
// ensureGGUFOnAllWorkers for downstream distribution.
//
// Three input shapes:
//
//	source.type: file         → return source.path verbatim, just check it exists
//	source.type: huggingface  → download to <modelsDir>/<source.file>, return
//	                            that path. Skip download if already present.
//	other                     → error (Bedrock / Vertex / Ollama don't apply
//	                            to sharded entries)
//
// The download is a single streaming GET to huggingface.co/<repo>/resolve/main/<file>
// with a generous timeout — GGUFs can be 10s of GB. Resume support is left
// for a follow-up; for now an interrupted download leaves the .partial file
// behind and the next call starts over.
func (o *Orchestrator) ensureLocalGGUF(ctx context.Context, entry models.Entry) (string, error) {
	switch entry.Source.Type {
	case "file":
		if entry.Source.Path == "" {
			return "", fmt.Errorf("catalog %s: source.type=file requires source.path", entry.ID)
		}
		if _, err := os.Stat(entry.Source.Path); err != nil {
			return "", fmt.Errorf("catalog %s: source.path %q not present and source.type=file (place the GGUF manually or change source.type to huggingface for auto-download)", entry.ID, entry.Source.Path)
		}
		return entry.Source.Path, nil

	case "huggingface":
		if entry.Source.Repo == "" || entry.Source.File == "" {
			return "", fmt.Errorf("catalog %s: source.type=huggingface requires both source.repo and source.file for auto-download", entry.ID)
		}
		// source.file is joined into ModelsDir and interpolated into the
		// download URL, so a path-y value could escape either. Mirror the
		// basename-only rule the worker upload endpoint enforces.
		if f := entry.Source.File; f == "." || strings.Contains(f, "..") ||
			strings.ContainsAny(f, "/\\") || filepath.Base(f) != f {
			return "", fmt.Errorf("catalog %s: source.file %q must be a bare filename (no path separators or ..)", entry.ID, f)
		}
		if o.ModelsDir == "" {
			return "", fmt.Errorf("catalog %s wants HF auto-download but Orchestrator.ModelsDir is unset; configure storage.models_dir or pre-place at source.path with type=file", entry.ID)
		}
		target := filepath.Join(o.ModelsDir, entry.Source.File)
		// Already present? Skip the multi-GB pull — but only when the local
		// size matches what HF says it should be, so a previously truncated
		// download isn't trusted forever.
		if st, err := os.Stat(target); err == nil && st.Size() > 0 {
			if want, ok := o.hfExpectedSize(ctx, hfURL(entry)); ok && want != st.Size() {
				o.Log.Warn("local gguf size differs from huggingface — re-downloading",
					"id", entry.ID, "path", target, "local_bytes", st.Size(), "remote_bytes", want)
			} else {
				o.Log.Info("gguf already on leader — skipping HF download",
					"id", entry.ID, "path", target, "size", st.Size())
				return target, nil
			}
		}
		if err := o.downloadFromHF(ctx, entry, target); err != nil {
			return "", err
		}
		return target, nil

	default:
		return "", fmt.Errorf("catalog %s: source.type=%q can't be auto-resolved for sharding (need file or huggingface)", entry.ID, entry.Source.Type)
	}
}

// hfURL builds the resolve URL for an entry's GGUF. source.file is
// path-escaped — it's validated as a bare filename by ensureLocalGGUF, but
// escape anyway so odd characters can't alter the URL path.
func hfURL(entry models.Entry) string {
	return fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s",
		entry.Source.Repo, url.PathEscape(entry.Source.File))
}

// hfExpectedSize asks HF (HEAD) how big the file should be, so the
// "already downloaded" fast path can detect truncated local copies.
// Returns ok=false when HEAD fails or reports no usable length; the
// caller keeps the fast path in that case.
func (o *Orchestrator) hfExpectedSize(ctx context.Context, fileURL string) (int64, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, fileURL, nil)
	if err != nil {
		return 0, false
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		o.Log.Debug("HEAD for expected gguf size failed — trusting local copy", "url", fileURL, "err", err)
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		o.Log.Debug("HEAD for expected gguf size unusable — trusting local copy",
			"url", fileURL, "status", resp.Status, "content_length", resp.ContentLength)
		return 0, false
	}
	return resp.ContentLength, true
}

// downloadFromHF streams a single GGUF from HuggingFace to target, writing
// to a unique .partial-* sibling (so concurrent downloads don't collide) and
// renaming on success so an interrupted download doesn't leave a half-file
// the next caller might mistake for complete. The byte count is verified
// against the server-declared Content-Length before the rename.
func (o *Orchestrator) downloadFromHF(ctx context.Context, entry models.Entry, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
	}
	url := hfURL(entry)

	// Use a per-download HTTP client with a much longer timeout than
	// o.HTTP (which is 60s, fine for control-plane calls but not for a
	// 40 GB tarball). 6h cap is enough for ~12 Mbps which is a realistic
	// floor for "the operator forgot to plug in to gigabit."
	client := &http.Client{Timeout: 6 * time.Hour}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "opod/"+entry.ID)

	o.Log.Info("downloading gguf from huggingface",
		"id", entry.ID, "repo", entry.Source.Repo, "file", entry.Source.File, "url", url)
	t0 := time.Now()

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s → %s", url, resp.Status)
	}
	expected := resp.ContentLength // -1 when the server doesn't declare a length

	// Unique temp name (os.CreateTemp) rather than a fixed "<target>.partial":
	// two concurrent downloads of the same entry would otherwise open and
	// interleave writes into the same file and corrupt it.
	f, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".partial-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", target, err)
	}
	tmp := f.Name()
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: %w", url, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, closeErr)
	}
	// Integrity: a complete-but-truncated body (HTTP 200, fewer bytes than
	// declared) would otherwise be renamed into place and fanned out to every
	// worker. Verify the byte count against the server-declared length.
	if expected > 0 && n != expected {
		_ = os.Remove(tmp)
		return fmt.Errorf("download %s: got %d bytes, expected %d (truncated)", url, n, expected)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, target, err)
	}
	o.Log.Info("gguf downloaded",
		"id", entry.ID, "path", target, "bytes", n,
		"duration_s", time.Since(t0).Seconds())
	return nil
}
