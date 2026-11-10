package wallcheck

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestListRoundTrip(t *testing.T) {
	t.Parallel()
	l := NewList()
	fps := []Fingerprint{
		{Hash: hash(KindShingle, []byte("b")), Kind: KindShingle},
		{Hash: hash(KindExact, []byte("a")), Kind: KindExact},
		{Hash: hash(KindFile, []byte("c")), Kind: KindFile},
		{Hash: hash(KindExact, []byte("a")), Kind: KindExact},
	}
	for _, fp := range fps {
		l.Add(fp)
	}
	var buf bytes.Buffer
	if _, err := l.WriteTo(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadList(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if diff := cmp.Diff(3, got.Len()); diff != "" {
		t.Errorf("len mismatch (-want +got):\n%s", diff)
	}
	for _, fp := range fps {
		if !got.Has(fp) {
			t.Errorf("round trip lost %s %s", fp.Hash, fp.Kind)
		}
	}
	if got.Has(Fingerprint{Hash: fps[1].Hash, Kind: KindAlpha}) {
		t.Error("Has matched a hash under the wrong kind")
	}
}

func TestReadList(t *testing.T) {
	t.Parallel()
	good := hash(KindExact, []byte("x"))
	tests := []struct {
		WantLen int
		Want    error
		In      string
	}{{ // Test 0: Comments and blank lines are skipped.
		In: "# header\n\n" + good + " exact\n", WantLen: 1,
	}, { // Test 1: An empty input is an empty list.
		In: "", WantLen: 0,
	}, { // Test 2: A short hash is refused.
		In: "abc exact\n", Want: ErrList,
	}, { // Test 3: Uppercase hex is refused.
		In: strings.ToUpper(good) + " exact\n", Want: ErrList,
	}, { // Test 4: An unknown kind is refused.
		In: good + " fuzzy\n", Want: ErrList,
	}, { // Test 5: A third field is refused.
		In: good + " exact extra\n", Want: ErrList,
	}, { // Test 6: A missing kind is refused.
		In: good + "\n", Want: ErrList,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			l, err := ReadList(strings.NewReader(test.In))
			if !errors.Is(err, test.Want) {
				t.Fatalf("got error %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.WantLen, l.Len()); diff != "" {
				t.Errorf("len mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
