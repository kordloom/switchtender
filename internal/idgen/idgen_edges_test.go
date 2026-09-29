package idgen_test

import (
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/idgen"
)

// TestNewShape pins the exact shape of a minted identifier at every size a caller uses, including
// the empty and the very large. Every entity in the product is named by one of these, and ids reach
// URLs, audit chain paths, and receipts, so a length or an alphabet that drifts changes what a
// stored path means and what an operator can paste back.
func TestNewShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says which edge this size sits on.
		Name string
		// Prefix is the caller's namespace marker.
		Prefix string
		// Bytes is how many random bytes the id carries.
		Bytes int
	}{{ // Test 0: The prefix is optional.
		Name: "no prefix", Prefix: "", Bytes: 6,
	}, { // Test 1: The common product size.
		Name: "credential", Prefix: "cred_", Bytes: 6,
	}, { // Test 2: The widest product size.
		Name: "run", Prefix: "run_", Bytes: 8,
	}, { // Test 3: The smallest nonempty draw.
		Name: "one byte", Prefix: "x_", Bytes: 1,
	}, { // Test 4: A key-sized draw.
		Name: "thirty two bytes", Prefix: "k_", Bytes: 32,
	}, { // Test 5: A non-ASCII prefix passes through unchanged.
		Name: "unicode prefix", Prefix: "运行_", Bytes: 6,
	}, { // Test 6: A very long prefix.
		Name: "long prefix", Prefix: strings.Repeat("p", 4096), Bytes: 6,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := idgen.New(test.Prefix, test.Bytes)
			if !strings.HasPrefix(got, test.Prefix) {
				t.Fatalf("New(%q, %d) = %q, want it to start with the prefix",
					test.Prefix, test.Bytes, got)
			}
			suffix := strings.TrimPrefix(got, test.Prefix)
			if wantLen := 2 * test.Bytes; len(suffix) != wantLen {
				t.Errorf("%s: random part is %d characters, want %d for %d hex encoded bytes",
					test.Name, len(suffix), wantLen, test.Bytes)
			}
			// Hex, lowercase, and decodable. An id that is not is an id a URL, a shell, or a chain
			// path can change without anyone noticing.
			raw, err := hex.DecodeString(suffix)
			if err != nil {
				t.Fatalf("%s: random part %q is not hex: %v", test.Name, suffix, err)
			}
			if len(raw) != test.Bytes {
				t.Errorf("%s: decoded to %d bytes, want %d", test.Name, len(raw), test.Bytes)
			}
			if suffix != strings.ToLower(suffix) {
				t.Errorf("%s: random part %q is not lowercase hex", test.Name, suffix)
			}
		})
	}
}

// TestNewIsUniqueAcrossManyMints pins that ids do not repeat in a volume one install reaches.
// Every store keys rows by these, and a repeat means one entity silently overwrites another: a
// credential, a run, or an audit anchor. Six bytes is the smallest draw any caller uses, so it is
// the one to hold under load.
func TestNewIsUniqueAcrossManyMints(t *testing.T) {
	t.Parallel()
	const mints = 50_000
	seen := make(map[string]struct{}, mints)
	for i := range mints {
		id := idgen.New("cred_", 6)
		if _, dup := seen[id]; dup {
			t.Fatalf("New minted %q twice within %d ids", id, i+1)
		}
		seen[id] = struct{}{}
	}
}

// TestNewIsUniqueUnderConcurrency pins that concurrent callers get distinct ids. Ids are minted
// from every request handler at once, so a shared buffer or a reused slice would show up here as a
// duplicate and nowhere else. Run under the race detector this also proves New keeps no shared
// state.
func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()
	const workers, each = 64, 500
	out := make(chan string, workers*each)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				out <- idgen.New("run_", 8)
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := make(map[string]struct{}, workers*each)
	for id := range out {
		if _, dup := seen[id]; dup {
			t.Fatalf("concurrent callers were handed the same id %q", id)
		}
		seen[id] = struct{}{}
	}
	if diff := cmp.Diff(workers*each, len(seen)); diff != "" {
		t.Errorf("minted id count mismatch (-want +got):\n%s", diff)
	}
}

// TestNewFillsEveryByteFromTheRandomSource pins that the whole buffer is drawn from the random
// source rather than left at its zero value. An id whose bytes are partly zero looks well formed
// and is guessable, which for a credential id or a token label is the difference between an opaque
// handle and one an outsider can enumerate.
func TestNewFillsEveryByteFromTheRandomSource(t *testing.T) {
	t.Parallel()
	const size, samples = 8, 256
	varied := make([]bool, size)
	first := make([]byte, size)
	for i := range samples {
		raw, err := hex.DecodeString(strings.TrimPrefix(idgen.New("run_", size), "run_"))
		if err != nil {
			t.Fatalf("sample %d is not hex: %v", i, err)
		}
		if i == 0 {
			copy(first, raw)
			continue
		}
		for pos := range size {
			if raw[pos] != first[pos] {
				varied[pos] = true
			}
		}
	}
	for pos, ok := range varied {
		if !ok {
			t.Errorf("byte %d never changed across %d ids, so it is not being drawn from the "+
				"random source", pos, samples)
		}
	}
}

// TestNewWithZeroBytesReturnsOnlyThePrefix pins the boundary at the bottom of the range. Zero bytes
// is not refused, and the result carries no randomness at all, so two calls return the same string.
// Nothing in the product asks for zero, and this exists so that if something ever does, the failure
// is a test that changed rather than a store quietly keyed on a constant.
func TestNewWithZeroBytesReturnsOnlyThePrefix(t *testing.T) {
	t.Parallel()
	got := idgen.New("cred_", 0)
	if diff := cmp.Diff("cred_", got); diff != "" {
		t.Errorf("New(prefix, 0) mismatch (-want +got):\n%s", diff)
	}
	if again := idgen.New("cred_", 0); again != got {
		t.Errorf("New(prefix, 0) returned %q then %q, want the same constant both times", got, again)
	}
}

// TestNewPanicsOnANegativeCount pins that a negative size is a hard, immediate failure rather than
// a silently empty id. A caller that computed a size wrongly must find out at the call, not by
// discovering later that a whole class of objects shares one identifier.
func TestNewPanicsOnANegativeCount(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("New(prefix, -1) returned instead of panicking, so a bad size mints a " +
				"prefix-only id")
		}
	}()
	_ = idgen.New("cred_", -1)
}
