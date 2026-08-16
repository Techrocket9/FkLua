package spectest

import (
	"testing"

	"github.com/eliben/watgo"
)

// compileTestModule builds binary wasm from WAT so tests can construct corpora
// without checking .wasm blobs into the repo.
func compileTestModule(t *testing.T, wat string) []byte {
	t.Helper()
	b, err := watgo.CompileWATToWASM([]byte(wat))
	if err != nil {
		t.Fatalf("compile wat: %v", err)
	}
	return b
}
