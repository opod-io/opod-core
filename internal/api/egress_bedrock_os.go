package api

import (
	"io"
	"os"
)

// Thin OS wrappers used by the Vertex ADC probe. Split out so the test
// file can substitute these globals via package-level vars instead of
// monkey-patching syscalls.

func osLookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

func osOpenFile(p string) (io.ReadCloser, error) {
	return os.Open(p) //nolint:gosec // probing well-known ADC path
}
