package scheduler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// ensureGGUFOnAllWorkers makes sure localPath is present on every worker
// in workers (matched by sha256). For each missing host, streams the file
// over the worker's /v1/process/upload endpoint. Skips upload when the
// worker reports the file is already there.
//
// Returns the absolute path of the GGUF on each worker, keyed by node ID —
// the worker's models_dir generally differs from the leader's, so a process
// launched remotely (e.g. the coordinator llama-server) must be pointed at
// the worker's own path, not the leader-local one.
//
// Returns the first failure if any host can't be brought into sync; the
// caller (CreateSharded) treats this as fatal and aborts before launching
// rpc-server.
func (o *Orchestrator) ensureGGUFOnAllWorkers(ctx context.Context, workers []store.Node, localPath string) (map[string]string, error) {
	sum, err := sha256File(localPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", localPath, err)
	}
	name := filepath.Base(localPath)
	paths := make(map[string]string, len(workers))
	for _, w := range workers {
		remotePath, err := o.ensureGGUFOnNode(ctx, w, localPath, name, sum)
		if err != nil {
			return nil, fmt.Errorf("node %s (%s): %w", w.ID, w.Hostname, err)
		}
		paths[w.ID] = remotePath
	}
	return paths, nil
}

// ensureGGUFOnNode returns the absolute path of the GGUF on the worker
// (reported by the worker via the X-File-Path header / upload "path" field).
func (o *Orchestrator) ensureGGUFOnNode(ctx context.Context, node store.Node, localPath, name, sum string) (string, error) {
	// Cheap probe first.
	checkURL := workerURL(node.Address) + "/v1/process/file?" + url.Values{
		"name":   {name},
		"sha256": {sum},
	}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, checkURL, nil)
	req.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	auth.SignRequest(req, node.ID, node.WorkerToken)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("file check: %w", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		o.Log.Info("gguf already on worker — skipping upload",
			"node", node.ID, "name", name, "sha256", sum[:12])
		if p := resp.Header.Get("X-File-Path"); p != "" {
			return p, nil
		}
		// Older worker that doesn't report its path — fall back to the
		// leader-local path (pre-existing behavior).
		o.Log.Warn("worker file check did not report a path — assuming leader-local layout",
			"node", node.ID, "name", name)
		return localPath, nil
	case http.StatusNotFound:
		// fall through to upload
	case http.StatusServiceUnavailable:
		return "", fmt.Errorf("worker has no models_dir configured (set storage.models_dir on the worker)")
	default:
		return "", fmt.Errorf("file check returned %s", resp.Status)
	}

	// Upload. Stream the file body to avoid loading the whole GGUF into RAM
	// just to hand it to net/http — Request.Body is an io.Reader.
	f, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		// An unstat-able file can't be given a Content-Length, and the
		// upload would be ambiguous anyway — bail out.
		return "", fmt.Errorf("stat %s: %w", localPath, err)
	}

	uploadURL := workerURL(node.Address) + "/v1/process/upload?" + url.Values{
		"name":   {name},
		"sha256": {sum},
	}.Encode()
	upReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, f)
	upReq.Header.Set("Authorization", "Bearer "+node.WorkerToken)
	// Sign like every other worker call (file check, process start/stop) so
	// a worker running OPOD_REJECT_BEARER=1 accepts the upload. SignRequest
	// signs method/path/ts only, so it doesn't consume the streamed body.
	auth.SignRequest(upReq, node.ID, node.WorkerToken)
	upReq.Header.Set("Content-Type", "application/octet-stream")
	upReq.ContentLength = stat.Size()

	o.Log.Info("uploading gguf to worker",
		"node", node.ID, "name", name, "bytes", stat.Size(), "sha256", sum[:12])

	// o.HTTP has a 60s timeout meant for small control calls (file-check,
	// process start/stop). Streaming a multi-GB GGUF over the overlay can't
	// finish in 60s, so it 502'd mid-upload. Use a dedicated client with a
	// generous timeout (cancellation still flows through the request ctx),
	// mirroring the 6h client the HF download uses.
	uploadClient := &http.Client{Timeout: 6 * time.Hour}
	upResp, err := uploadClient.Do(upReq)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(upResp.Body, 1024))
		return "", fmt.Errorf("upload returned %s: %s", upResp.Status, bytes.TrimSpace(body))
	}
	var upBody struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(upResp.Body).Decode(&upBody); err != nil || upBody.Path == "" {
		// Older worker that doesn't report its path — fall back to the
		// leader-local path (pre-existing behavior).
		o.Log.Warn("worker upload response did not report a path — assuming leader-local layout",
			"node", node.ID, "name", name)
		o.Log.Info("gguf uploaded", "node", node.ID, "name", name)
		return localPath, nil
	}
	o.Log.Info("gguf uploaded", "node", node.ID, "name", name, "path", upBody.Path)
	return upBody.Path, nil
}

// sha256File mirrors agent.sha256File — duplicated here to avoid an awkward
// scheduler→agent dependency just for one helper.
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
