package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedisct1/go-minisign"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
)

// testMinisignKey is a throwaway keypair generated in the test; no key
// material lives in the repository.
func testMinisignKey(t *testing.T) (minisign.PrivateKey, string) {
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
	return sk, base64.StdEncoding.EncodeToString(raw)
}

// `opod model add --from <file>` checks <file>.minisig under the operator's
// policy before anything is planned, saved or pulled.
func TestModelAddFromVerifiesTheSignature(t *testing.T) {
	sk, pub := testMinisignKey(t)
	_, otherPub := testMinisignKey(t)
	entry := []byte("id: my-model\nsource:\n  type: ollama\n  ollama_name: my-model\n")
	encoded, err := sk.Sign(entry, minisign.SignOptions{Hashed: true, TrustedComment: "test-only signature"})
	if err != nil {
		t.Fatal(err)
	}
	sig := encoded.Encode()

	write := func(withSig []byte) string {
		path := filepath.Join(t.TempDir(), "my-model.yaml")
		if err := os.WriteFile(path, entry, 0o644); err != nil {
			t.Fatal(err)
		}
		if withSig != nil {
			if err := os.WriteFile(path+models.SignatureSuffix, withSig, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}

	for _, c := range []struct {
		name     string
		env      config.Env
		sig      []byte
		refused  bool
		verified bool
		carried  bool // the signature is returned to be saved beside the copy
	}{
		{name: "signed, the right key", env: config.Env{CatalogPubKey: pub, CatalogMustSign: true}, sig: sig, verified: true, carried: true},
		{name: "signed, another key configured", env: config.Env{CatalogPubKey: otherPub}, sig: sig, refused: true},
		{name: "unsigned, key configured, not required", env: config.Env{CatalogPubKey: pub}},
		{name: "unsigned, signatures required", env: config.Env{CatalogPubKey: pub, CatalogMustSign: true}, refused: true},
		{name: "signatures required with no key", env: config.Env{CatalogMustSign: true}, refused: true},
		{name: "no key: unsigned", env: config.Env{}},
		{name: "no key: the signature is not checked but travels with the file", env: config.Env{}, sig: sig, carried: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, verifiedBy, err := verifyCatalogFile(c.env, write(c.sig), entry)
			if c.refused {
				if !errors.Is(err, models.ErrCatalogSignature) {
					t.Fatalf("want a signature refusal, got sig=%q err=%v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (verifiedBy != "") != c.verified || (got != nil) != c.carried {
				t.Fatalf("verifiedBy=%q carried=%v; want verified=%v carried=%v", verifiedBy, got != nil, c.verified, c.carried)
			}
		})
	}

	// A file edited after signing is refused, whatever else is configured.
	path := write(sig)
	edited := append(append([]byte{}, entry...), []byte("# edited\n")...)
	if _, _, err := verifyCatalogFile(config.Env{CatalogPubKey: pub}, path, edited); !errors.Is(err, models.ErrCatalogSignature) {
		t.Fatalf("an edited file with a stale signature must be refused, got %v", err)
	}
}
