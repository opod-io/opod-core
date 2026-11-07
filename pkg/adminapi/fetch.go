package adminapi

// What `opod fetch` reports once the weights are in the node cache
// (OPOD_FETCH_RESULT). The fetch runs as a process — a manager's prefetch Job —
// so its result is a file, not an API answer: one JSON line, written after the
// weights landed and never before.

// Signature states a fetch reports for the weights it made present.
//
// There is deliberately no "verified" state yet: the fetch looks for a
// model-transparency signature (OpenSSF model signing, `model.sig` at the top
// of the repository) and says whether one is published, but it does not
// verify it. A manager must never read `unverified` as a pass.
const (
	// SignatureUnsigned: the repository at that revision publishes no signature.
	SignatureUnsigned = "unsigned"
	// SignatureUnverified: a signature is published beside the weights and
	// nothing has verified it.
	SignatureUnverified = "unverified"
	// SignatureUnknown: the fetch could not ask the Hub what the repository
	// holds, so it cannot say whether a signature is published.
	SignatureUnknown = "unknown"
)

// SignatureFile is the file model signing writes beside a model by default.
const SignatureFile = "model.sig"

// FetchResult is the line `opod fetch` writes to $OPOD_FETCH_RESULT.
type FetchResult struct {
	// Path is the file, or the snapshot directory, the weights are in.
	Path      string         `json:"path"`
	Signature FetchSignature `json:"signature"`
}

// FetchSignature is what the fetch found out about the weights' signature.
type FetchSignature struct {
	State  string `json:"state"`            // SignatureUnsigned | SignatureUnverified | SignatureUnknown
	File   string `json:"file,omitempty"`   // the signature file it found
	Detail string `json:"detail,omitempty"` // one sentence: why this state
}
