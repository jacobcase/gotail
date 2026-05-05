package forward_test

import (
	"bytes"
	"testing"
	"unsafe"

	"github.com/jacobcase/gotail/v2/forward"
)

func TestIdentityDecoder_AliasesInput(t *testing.T) {
	in := []byte("hello")
	out, err := forward.IdentityDecoder(in)
	if err != nil {
		t.Fatalf("IdentityDecoder: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("got %q, want %q", out, in)
	}
	// IdentityDecoder must not copy — the documented contract is that the
	// returned slice aliases the caller's buffer. Verify by comparing the
	// underlying data pointer.
	if unsafe.SliceData(out) != unsafe.SliceData(in) {
		t.Fatalf("IdentityDecoder allocated a copy; expected alias of input")
	}
}
