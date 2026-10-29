package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/opod-io/opod-sdk/adminapi"
)

// T32.23: the result a manager reads from the pod's status is one JSON line of
// adminapi.FetchResult, written where OPOD_FETCH_RESULT points.
func TestWriteFetchResultIsOneJSONLine(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "termination-log")
	sig := adminapi.FetchSignature{State: adminapi.SignatureUnsigned, Detail: "org/m@main publishes no model.sig"}
	writeFetchResult(dst, "/data/models/m.gguf", sig, slog.Default())
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got adminapi.FetchResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result %q is not one JSON document: %v", raw, err)
	}
	if got.Path != "/data/models/m.gguf" || got.Signature != sig {
		t.Fatalf("result %+v, want the path and the signature check", got)
	}
}
