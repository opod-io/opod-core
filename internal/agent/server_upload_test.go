package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestFileUpload_RoundTrip exercises the upload happy path: stream bytes to
// /v1/process/upload, get 200, then HEAD /v1/process/file and get 200 back.
func TestFileUpload_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	const token = "sk-test"
	srv := &Server{Token: token, ModelsDir: dir}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/process/file", srv.auth(srv.fileCheck))
	mux.HandleFunc("/v1/process/upload", srv.auth(srv.fileUpload))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := bytes.Repeat([]byte("opod-test "), 1024) // 12 KB
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])

	// 1) Pre-upload check should 404
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodHead,
		ts.URL+"/v1/process/file?name=hello.gguf&sha256="+hexSum, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("pre-check: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("pre-check status = %d, want 404", resp.StatusCode)
	}

	// 2) Upload
	up, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/v1/process/upload?name=hello.gguf&sha256="+hexSum, bytes.NewReader(body))
	up.Header.Set("Authorization", "Bearer "+token)
	up.ContentLength = int64(len(body))
	upResp, err := http.DefaultClient.Do(up)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer upResp.Body.Close()
	if upResp.StatusCode != 200 {
		b, _ := io.ReadAll(upResp.Body)
		t.Fatalf("upload status = %d body=%q", upResp.StatusCode, string(b))
	}

	// 3) File now exists at expected path with right contents
	got, err := os.ReadFile(filepath.Join(dir, "hello.gguf"))
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("uploaded content mismatch")
	}

	// 4) Post-upload check should 200
	r2, _ := http.NewRequestWithContext(context.Background(), http.MethodHead,
		ts.URL+"/v1/process/file?name=hello.gguf&sha256="+hexSum, nil)
	r2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(r2)
	if err != nil {
		t.Fatalf("post-check: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("post-check status = %d, want 200", resp2.StatusCode)
	}
	if got := resp2.Header.Get("X-File-Path"); got != filepath.Join(dir, "hello.gguf") {
		t.Errorf("X-File-Path = %q, want %q", got, filepath.Join(dir, "hello.gguf"))
	}
}

// TestFileUpload_ShaMismatch sends a body whose sha doesn't match the
// declared one; the worker must 422 and not leave a partial file behind.
func TestFileUpload_ShaMismatch(t *testing.T) {
	dir := t.TempDir()
	const token = "sk-test"
	srv := &Server{Token: token, ModelsDir: dir}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/process/upload", srv.auth(srv.fileUpload))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := []byte("real content")
	wrongSum := "deadbeef" + // padding to look like sha256 hex
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	up, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/v1/process/upload?name=bad.gguf&sha256="+wrongSum,
		bytes.NewReader(body))
	up.Header.Set("Authorization", "Bearer "+token)
	up.ContentLength = int64(len(body))
	resp, err := http.DefaultClient.Do(up)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 422; body=%q", resp.StatusCode, string(b))
	}
	// No partial file should remain
	for _, name := range []string{"bad.gguf", "bad.gguf.partial"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("file %s exists after sha mismatch — should be removed", name)
		}
	}
}

// TestFileUpload_RejectPathEscape ensures `name` can't contain path
// separators or .. components.
func TestFileUpload_RejectPathEscape(t *testing.T) {
	dir := t.TempDir()
	const token = "sk-test"
	srv := &Server{Token: token, ModelsDir: dir}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/process/upload", srv.auth(srv.fileUpload))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, bad := range []string{"../escape.gguf", "sub/dir.gguf", "..", "."} {
		up, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
			ts.URL+"/v1/process/upload?name="+bad+"&sha256=abc", nil)
		up.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(up)
		if err != nil {
			t.Fatalf("upload %q: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("name=%q got status %d, want 400", bad, resp.StatusCode)
		}
	}
}
