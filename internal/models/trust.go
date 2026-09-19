package models

// Signed catalogs: a minisign signature beside a catalog file
// (<name>.yaml + <name>.yaml.minisig), checked against one public key the
// operator configures (OPOD_CATALOG_PUBKEY through the config contract).
//
// The rule, in full:
//
//   - no key configured            → nothing is checked; signature files are ignored;
//   - a key, and a signature file   → it must verify. A signature that is
//     present and wrong is ALWAYS a refusal — never "warn and load": it means
//     the file changed after it was signed, or was signed by someone else;
//   - a key, and no signature file  → loaded, unless signatures are required
//     (OPOD_CATALOG_REQUIRE_SIGNED=1), in which case it is refused;
//   - the embedded catalog is never subject to any of this: it ships inside
//     the binary, and whoever trusts the binary trusts it. A file on disk that
//     is byte-for-byte an embedded entry (what `opod catalog export` writes) is
//     that entry.
//
// Without the require switch a signature is an integrity check on the files
// that carry one, not a boundary: whoever can write the directory can also
// delete the signature. The switch is what makes it a boundary.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedisct1/go-minisign"
	"github.com/opod-io/opod-sdk/catalog"
)

// SignatureSuffix is appended to a catalog file's full name: minisign's own
// convention, so `minisign -S -m my-model.yaml` writes the file we read.
const SignatureSuffix = ".minisig"

// ErrCatalogSignature wraps every refusal, so a caller can tell "this catalog
// file is not trusted" from "this catalog file does not parse".
var ErrCatalogSignature = errors.New("catalog signature")

// CatalogTrust is the configured verification policy. The nil value checks
// nothing, which is what an operator who configured no key gets.
type CatalogTrust struct {
	key           minisign.PublicKey
	requireSigned bool
}

// NewCatalogTrust builds the policy. pubkey is a minisign public key — the
// base64 line itself — or the path of a file holding one (a minisign.pub with
// its comment line, or the bare line). Empty pubkey and requireSigned=false is
// "no policy" (nil, nil); requiring signatures with no key to check them
// against is a configuration error, not a policy.
func NewCatalogTrust(pubkey string, requireSigned bool) (*CatalogTrust, error) {
	pubkey = strings.TrimSpace(pubkey)
	if pubkey == "" {
		if requireSigned {
			return nil, fmt.Errorf("%w: signatures are required (OPOD_CATALOG_REQUIRE_SIGNED) but no public key is configured (OPOD_CATALOG_PUBKEY)", ErrCatalogSignature)
		}
		return nil, nil
	}
	key, err := minisign.NewPublicKey(pubkey)
	if err != nil {
		key, err = publicKeyFromFile(pubkey)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: OPOD_CATALOG_PUBKEY is neither a minisign public key nor a file holding one: %v", ErrCatalogSignature, err)
	}
	return &CatalogTrust{key: key, requireSigned: requireSigned}, nil
}

func publicKeyFromFile(path string) (minisign.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return minisign.PublicKey{}, err
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if i := strings.LastIndex(text, "\n"); i >= 0 { // minisign.pub: a comment line, then the key
		text = strings.TrimSpace(text[i+1:])
	}
	return minisign.NewPublicKey(text)
}

// KeyID is the configured key's id as minisign prints it, for messages.
func (t *CatalogTrust) KeyID() string {
	if t == nil {
		return ""
	}
	id := t.key.KeyId
	for i, j := 0, len(id)-1; i < j; i, j = i+1, j-1 { // minisign prints it little-endian
		id[i], id[j] = id[j], id[i]
	}
	return fmt.Sprintf("%X", id[:])
}

// Verify applies the policy to one catalog file's bytes. sig is the content of
// its signature file, nil when there is none. It reports whether a signature
// was checked and held.
func (t *CatalogTrust) Verify(name string, data, sig []byte) (signed bool, err error) {
	if t == nil {
		return false, nil
	}
	if sig == nil {
		if t.requireSigned {
			return false, fmt.Errorf("%w: %s has no signature (%s%s beside it) and signatures are required — sign it with the key %s, or unset OPOD_CATALOG_REQUIRE_SIGNED",
				ErrCatalogSignature, name, filepath.Base(name), SignatureSuffix, t.KeyID())
		}
		return false, nil
	}
	decoded, err := minisign.DecodeSignature(string(sig))
	if err != nil {
		return false, fmt.Errorf("%w: %s%s is not a minisign signature: %v", ErrCatalogSignature, name, SignatureSuffix, err)
	}
	if ok, err := t.key.Verify(data, decoded); err != nil || !ok {
		return false, fmt.Errorf("%w: %s does not verify against the configured key %s (%v) — the file changed after it was signed, or another key signed it; it is not loaded",
			ErrCatalogSignature, name, t.KeyID(), err)
	}
	return true, nil
}

// VerifyFile is Verify for a file on disk: the signature is <path>.minisig.
func (t *CatalogTrust) VerifyFile(path string, data []byte) (signed bool, err error) {
	if t == nil {
		return false, nil
	}
	sig, err := os.ReadFile(path + SignatureSuffix)
	switch {
	case errors.Is(err, os.ErrNotExist):
		sig = nil
	case err != nil:
		return false, fmt.Errorf("%w: read %s%s: %v", ErrCatalogSignature, path, SignatureSuffix, err)
	case sig == nil:
		sig = []byte{} // an empty signature file is present, and wrong
	}
	return t.Verify(path, data, sig)
}

// isEmbeddedEntry reports whether a file on disk is, byte for byte, the entry
// of that name inside the binary.
func isEmbeddedEntry(name string, data []byte) bool {
	embedded, err := catalog.Read(name)
	return err == nil && bytes.Equal(embedded, data)
}
