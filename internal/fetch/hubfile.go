package fetch

// A small file read from a repo — a model's config.json — for a caller that
// needs one fact before it starts anything, not the file on disk.
//
// The weights path (GGUF, Snapshot) is built for tens of GB: a lock, a temp
// sibling, a 6 h client. A question like "does this model tie its output layer
// to its embedding?" needs a few kilobytes and must never hold a create up for
// long, so this is one bounded GET and nothing is written. A complete snapshot
// already on disk is read instead, so a model the node has pulled costs no
// network call at all.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// smallFileTimeout bounds one HubSmallFile call when the caller's context has
// no shorter deadline: the answer is a few kilobytes, and a Hub that cannot
// give it in this long is not one to wait on.
const smallFileTimeout = 15 * time.Second

// smallFileMax is the most HubSmallFile reads. A config.json is a few KB; a
// file bigger than this is not the small file the caller meant.
const smallFileMax = 1 << 20

// HubSmallFile returns one small file of repo: from the complete snapshot of
// repo@opt.Revision under dir when there is one (dir "" = never look), else
// from the Hub with one GET. A refused credential comes back as a *HubError,
// like every other Hub call here.
func HubSmallFile(ctx context.Context, repo, file, dir string, opt Options) ([]byte, error) {
	if repo == "" {
		return nil, errors.New("fetch: repo required")
	}
	if err := ValidGGUFName(file); err != nil { // a bare file name, nothing path-y
		return nil, err
	}
	if err := ValidRevision(opt.Revision); err != nil {
		return nil, err
	}
	if sdir, ok := SnapshotPath(dir, repo, opt.Revision); ok {
		if b, err := readSmall(filepath.Join(sdir, file)); err == nil {
			return b, nil
		}
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: smallFileTimeout}
	}
	fileURL := HFFileURL(opt.Endpoint, repo, opt.Revision, file)
	rctx, cancel := context.WithTimeout(ctx, smallFileTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "opod-fetch")
	if opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Token)
	}
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", fileURL, err)
	}
	defer resp.Body.Close()
	if err := hubRefusal(resp, opt, repo, fileURL); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s → %s", fileURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, smallFileMax+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", fileURL, err)
	}
	if len(b) > smallFileMax {
		return nil, fmt.Errorf("%s is larger than %d bytes", fileURL, smallFileMax)
	}
	return b, nil
}

// readSmall reads a local file of at most smallFileMax bytes.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, smallFileMax+1))
	if err != nil {
		return nil, err
	}
	if len(b) > smallFileMax {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, smallFileMax)
	}
	return b, nil
}
