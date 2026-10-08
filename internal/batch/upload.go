package batch

// PutResults writes a finished run's result files to URLs that carry their own
// authorization — an object store's presigned PUT URLs, in practice — with one
// plain HTTP PUT each. The runner still knows no store: whoever runs it mints
// the URLs and decides what the objects are.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Written says which result files were written; a file with nothing in it is
// not written, as OpenAI leaves output_file_id or error_file_id null.
type Written struct {
	Output bool `json:"output"`
	Errors bool `json:"errors"`
}

// PutResults PUTs output.jsonl to outputURL and errors.jsonl to errorURL ("" =
// that file is not written anywhere). No Authorization header is sent: the
// URL is the credential, and a store refuses a request that carries two. The
// body streams from disk with its length stated, which a presigned PUT
// requires (no chunked encoding). A run that is resumed writes again and
// replaces what an earlier attempt wrote.
func PutResults(ctx context.Context, client *http.Client, outputURL, errorURL string, res Result) (Written, error) {
	var w Written
	if client == nil {
		client = http.DefaultClient
	}
	var err error
	if w.Output, err = putFile(ctx, client, outputURL, res.OutputPath); err != nil {
		return w, err
	}
	if w.Errors, err = putFile(ctx, client, errorURL, res.ErrorPath); err != nil {
		return w, err
	}
	return w, nil
}

func putFile(ctx context.Context, client *http.Client, dst, path string) (bool, error) {
	if dst == "" {
		return false, nil
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) || (err == nil && st.Size() == 0) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, dst, f)
	if err != nil {
		return false, fmt.Errorf("batch: write %s: %w", filepath.Base(path), err)
	}
	req.ContentLength = st.Size() // an *os.File body would otherwise go chunked
	req.Header.Set("Content-Type", "application/jsonl")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("batch: write %s: %w", filepath.Base(path), redact(err, dst))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return false, fmt.Errorf("batch: write %s: %s: %s", filepath.Base(path), resp.Status, strings.TrimSpace(string(b)))
	}
	return true, nil
}

// redact keeps a URL's query — a presigned URL's signature — out of an error
// that is printed to a log.
func redact(err error, raw string) error {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), raw, raw[:i]+"?…"))
	}
	return err
}
