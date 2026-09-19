package models

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedisct1/go-minisign"
	"github.com/opod-io/opod-sdk/catalog"
)

// testSigner is a throwaway minisign keypair, generated per test. No key
// material lives in the repository.
type testSigner struct {
	sk     minisign.PrivateKey
	PubKey string // the base64 line an operator would configure
}

func newTestSigner(t *testing.T) testSigner {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sk := minisign.PrivateKey{SignatureAlgorithm: [2]byte{'E', 'd'}}
	if _, err := rand.Read(sk.KeyId[:]); err != nil {
		t.Fatal(err)
	}
	copy(sk.SecretKey[:], priv)
	pk := sk.PublicKey()
	raw := append(append(append([]byte{}, pk.SignatureAlgorithm[:]...), pk.KeyId[:]...), pk.PublicKey[:]...)
	return testSigner{sk: sk, PubKey: base64.StdEncoding.EncodeToString(raw)}
}

func (s testSigner) sign(t *testing.T, data []byte, hashed bool) []byte {
	t.Helper()
	sig, err := s.sk.Sign(data, minisign.SignOptions{Hashed: hashed, TrustedComment: "test-only signature"})
	if err != nil {
		t.Fatal(err)
	}
	return sig.Encode()
}

func mustTrust(t *testing.T, pubkey string, require bool) *CatalogTrust {
	t.Helper()
	trust, err := NewCatalogTrust(pubkey, require)
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

const signedEntry = "id: signed-model\ndisplay_name: Signed\nsource:\n  type: ollama\n  ollama_name: signed\n"

func writeEntry(t *testing.T, dir, name, body string, sig []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if sig != nil {
		if err := os.WriteFile(filepath.Join(dir, name+SignatureSuffix), sig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The policy, one row each, through the directory loader.
func TestCatalogSignaturePolicy(t *testing.T) {
	signer, stranger := newTestSigner(t), newTestSigner(t)
	good := signer.sign(t, []byte(signedEntry), true)
	legacy := signer.sign(t, []byte(signedEntry), false)
	foreign := stranger.sign(t, []byte(signedEntry), true)
	stale := signer.sign(t, []byte(signedEntry+"# edited later\n"), true)

	cases := []struct {
		name    string
		pubkey  string
		require bool
		sig     []byte
		refused string // a fragment of the refusal; "" = loads
	}{
		{name: "no key: an unsigned file loads"},
		{name: "no key: a signature file is ignored, even a wrong one", sig: foreign},
		{name: "key, valid signature (prehashed)", pubkey: signer.PubKey, sig: good},
		{name: "key, valid signature (legacy)", pubkey: signer.PubKey, sig: legacy},
		{name: "key, valid signature, signatures required", pubkey: signer.PubKey, require: true, sig: good},
		{name: "key, unsigned: still allowed by default", pubkey: signer.PubKey},
		{name: "key, unsigned, signatures required: refused", pubkey: signer.PubKey, require: true, refused: "has no signature"},
		{name: "key, signed by another key: refused", pubkey: signer.PubKey, sig: foreign, refused: "does not verify"},
		{name: "key, file changed after signing: refused", pubkey: signer.PubKey, sig: stale, refused: "does not verify"},
		{name: "key, a signature file that is not one: refused", pubkey: signer.PubKey, sig: []byte("not a signature\n"), refused: "not a minisign signature"},
		{name: "key, an empty signature file: refused", pubkey: signer.PubKey, sig: []byte{}, refused: "not a minisign signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeEntry(t, dir, "signed-model.yaml", signedEntry, c.sig)
			entries, err := LoadCatalogTrusted(mustTrust(t, c.pubkey, c.require), dir)
			if c.refused == "" {
				if err != nil || FindByID(entries, "signed-model") == nil {
					t.Fatalf("want the entry loaded, got %v / %v", entries, err)
				}
				return
			}
			if !errors.Is(err, ErrCatalogSignature) || !strings.Contains(err.Error(), c.refused) {
				t.Fatalf("want a signature refusal naming %q, got %v", c.refused, err)
			}
			if !strings.Contains(err.Error(), "signed-model.yaml") {
				t.Errorf("the refusal must name the file: %v", err)
			}
			if entries != nil {
				t.Errorf("a refused catalog returned entries: %v", entries)
			}
		})
	}
}

// A present-but-invalid signature is a refusal whether or not signatures are
// required: "unsigned is allowed" never means "badly signed is allowed".
func TestInvalidSignatureIsRefusedEvenWhenUnsignedIsAllowed(t *testing.T) {
	signer, stranger := newTestSigner(t), newTestSigner(t)
	dir := t.TempDir()
	writeEntry(t, dir, "unsigned.yaml", strings.ReplaceAll(signedEntry, "signed-model", "unsigned"), nil)
	writeEntry(t, dir, "signed-model.yaml", signedEntry, stranger.sign(t, []byte(signedEntry), true))
	if _, err := LoadCatalogTrusted(mustTrust(t, signer.PubKey, false), dir); !errors.Is(err, ErrCatalogSignature) {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// The embedded catalog ships inside the binary and is never subject to the
// policy — neither as the embedded set, nor as the copy `opod catalog export`
// writes to a directory. An exported file that was EDITED is a directory file
// again.
func TestEmbeddedCatalogIsExemptFromSignatures(t *testing.T) {
	signer := newTestSigner(t)
	trust := mustTrust(t, signer.PubKey, true)
	home := t.TempDir()
	t.Setenv("HOME", home)

	bundled, err := BundledCatalog()
	if err != nil || len(bundled) == 0 {
		t.Fatalf("bundled catalog: %d entries, %v", len(bundled), err)
	}
	exported := t.TempDir()
	if err := ExportBundled(exported); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCatalogTrusted(trust, "", exported)
	if err != nil {
		t.Fatalf("signatures required, only embedded entries and their exported copies present: %v", err)
	}
	if len(got) != len(bundled) {
		t.Fatalf("loaded %d entries, the binary embeds %d", len(got), len(bundled))
	}

	name := catalog.Names()[0]
	data, err := catalog.Read(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exported, name), append(data, []byte("\n# edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCatalogTrusted(trust, "", exported); !errors.Is(err, ErrCatalogSignature) || !strings.Contains(err.Error(), name) {
		t.Fatalf("an edited export is an unsigned directory file; got %v", err)
	}
}

// The user catalog (~/.opod/catalog) is where user-added entries live, and it
// is under the policy like every other directory.
func TestUserCatalogIsVerifiedAtLoad(t *testing.T) {
	signer, stranger := newTestSigner(t), newTestSigner(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".opod", "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEntry(t, dir, "signed-model.yaml", signedEntry, signer.sign(t, []byte(signedEntry), true))
	entries, err := LoadCatalogTrusted(mustTrust(t, signer.PubKey, true), "")
	if err != nil || FindByID(entries, "signed-model") == nil {
		t.Fatalf("a correctly signed user entry must load: %v", err)
	}
	if _, err := LoadCatalogTrusted(mustTrust(t, stranger.PubKey, false), ""); !errors.Is(err, ErrCatalogSignature) {
		t.Fatalf("the same entry under another key must be refused, got %v", err)
	}
}

// The key is the base64 line, or a file holding it — with or without
// minisign's comment line.
func TestCatalogTrustKeyForms(t *testing.T) {
	signer := newTestSigner(t)
	dir := t.TempDir()
	bare, pub := filepath.Join(dir, "bare.pub"), filepath.Join(dir, "minisign.pub")
	if err := os.WriteFile(bare, []byte(signer.PubKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, []byte("untrusted comment: minisign public key (test only)\r\n"+signer.PubKey+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sig := signer.sign(t, []byte(signedEntry), true)
	for _, form := range []string{signer.PubKey, "  " + signer.PubKey + "\n", bare, pub} {
		trust := mustTrust(t, form, true)
		if signed, err := trust.Verify("x.yaml", []byte(signedEntry), sig); err != nil || !signed {
			t.Errorf("key given as %q: signed=%v err=%v", form, signed, err)
		}
		if len(trust.KeyID()) != 16 {
			t.Errorf("key id %q", trust.KeyID())
		}
	}

	if trust, err := NewCatalogTrust("", false); trust != nil || err != nil {
		t.Errorf("no key, nothing required = no policy; got %v, %v", trust, err)
	}
	for _, bad := range []struct {
		pubkey  string
		require bool
	}{
		{"", true},           // required, with nothing to check against
		{"not-a-key", false}, // neither a key nor a file
		{filepath.Join(dir, "missing.pub"), false}, // a path that is not there
	} {
		if _, err := NewCatalogTrust(bad.pubkey, bad.require); !errors.Is(err, ErrCatalogSignature) {
			t.Errorf("NewCatalogTrust(%q, %v): want a configuration error, got %v", bad.pubkey, bad.require, err)
		}
	}
	// The nil policy checks nothing and never panics.
	var none *CatalogTrust
	if signed, err := none.VerifyFile(filepath.Join(dir, "nothing.yaml"), nil); signed || err != nil {
		t.Errorf("nil policy: %v, %v", signed, err)
	}
}

// A saved user entry keeps its signature beside it, and a signature left over
// from an entry that is gone does not condemn the next one.
func TestPersistUserCatalogEntryKeepsTheSignature(t *testing.T) {
	signer := newTestSigner(t)
	dir := t.TempDir()
	sig := signer.sign(t, []byte(signedEntry), true)
	dest, err := PersistUserCatalogEntry(dir, "signed-model", []byte(signedEntry), sig)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest + SignatureSuffix); err != nil || string(got) != string(sig) {
		t.Fatalf("signature beside the saved entry: %v", err)
	}
	if _, err := LoadCatalogTrusted(mustTrust(t, signer.PubKey, true), dir); err != nil {
		t.Fatalf("the saved copy must verify like the original: %v", err)
	}

	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := PersistUserCatalogEntry(dir, "signed-model", []byte(signedEntry+"# unsigned now\n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + SignatureSuffix); !os.IsNotExist(err) {
		t.Fatalf("the stale signature is still there: %v", err)
	}
	if _, err := LoadCatalogTrusted(mustTrust(t, signer.PubKey, false), dir); err != nil {
		t.Fatalf("an unsigned entry under a key, signatures not required: %v", err)
	}
}
