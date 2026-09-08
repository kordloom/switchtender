package roundhouse

import (
	"errors"
	"fmt"
	"testing"
)

// TestDigestPinRequiresASha256DigestNotJustAnAtSign checks that digest pinning is judged on the
// @sha256: segment rather than on the presence of an '@'.
//
// An '@' by itself buys nothing. A registry resolves "image@latest" and "image@md5:..." the same way
// it resolves a tag, so both stay mutable: the operator turns pinning on, the reference passes, and
// the image the run pulls can still be swapped underneath it. That is the whole failure the setting
// exists to prevent, and a check that only looks for '@' permits it while reporting that pinning is
// enforced. The suite covered a plain tag and a real sha256 digest, so a predicate loosened to any
// '@' passed every case.
func TestDigestPinRequiresASha256DigestNotJustAnAtSign(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want bool
	}{{ // Test 0: An at sign with no digest algorithm is still a mutable reference.
		In: "quay.io/x/y@latest", Want: false,
	}, { // Test 1: A non-sha256 algorithm is not the immutable digest form.
		In: "quay.io/x/y@md5:abc123", Want: false,
	}, { // Test 2: A bare at sign pins nothing.
		In: "quay.io/x/y@", Want: false,
	}, { // Test 3: sha256: without the at sign is a tag named sha256, not a digest.
		In: "quay.io/x/y:sha256:abc123", Want: false,
	}, { // Test 4: The real digest form still passes.
		In: "quay.io/x/y@sha256:abc123", Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := isDigestPinned(test.In); got != test.Want {
				t.Errorf("isDigestPinned(%q) = %v, want %v", test.In, got, test.Want)
			}
			// The predicate is only interesting through the gate that uses it, so the same
			// references are pushed through validateRunImage with pinning on.
			c := newContainerRunner("docker", "missing", true, nil, &pluginCache{},
				DefaultContainerLimits())
			err := c.validateRunImage(test.In)
			if test.Want && err != nil {
				t.Errorf("validateRunImage(%q) error = %v, want nil", test.In, err)
			}
			if !test.Want && !errors.Is(err, ErrUnpinnedImage) {
				t.Errorf("validateRunImage(%q) error = %v, want ErrUnpinnedImage", test.In, err)
			}
		})
	}
}
