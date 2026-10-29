package fetch

import (
	"context"
	"testing"

	"github.com/opod-io/opod-sdk/adminapi"
)

// T32.23: the fetch reports whether the repository publishes a signature, and
// never reports one it has not verified as a pass.
func TestSignatureOfSaysWhatTheRepositoryPublishes(t *testing.T) {
	ctx := context.Background()
	weights := map[string]string{"model.safetensors": "tensors", "config.json": "{}"}

	plain := newSnapshotHub(t, "org/plain", "abc", weights, "model.safetensors")
	if got := SignatureOf(ctx, "org/plain", Options{Endpoint: plain.URL, Revision: "abc"}); got.State != adminapi.SignatureUnsigned {
		t.Fatalf("a repository with no %s: %+v, want unsigned", adminapi.SignatureFile, got)
	}

	signedFiles := map[string]string{"model.safetensors": "tensors", "config.json": "{}", adminapi.SignatureFile: `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`}
	signed := newSnapshotHub(t, "org/signed", "abc", signedFiles, "model.safetensors")
	got := SignatureOf(ctx, "org/signed", Options{Endpoint: signed.URL, Revision: "abc"})
	if got.State != adminapi.SignatureUnverified || got.File != adminapi.SignatureFile {
		t.Fatalf("a repository that publishes %s: %+v, want unverified — this release verifies nothing, so it must not say more", adminapi.SignatureFile, got)
	}

	plain.Close()
	if got := SignatureOf(ctx, "org/plain", Options{Endpoint: plain.URL, Revision: "abc"}); got.State != adminapi.SignatureUnknown || got.Detail == "" {
		t.Fatalf("a Hub that cannot be asked: %+v, want unknown with the reason", got)
	}
}
