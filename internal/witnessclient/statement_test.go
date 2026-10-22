package witnessclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// TestFetchStatement pins what FetchStatement takes from a witness: the statement of what it holds
// for the enrolled install, verified, signed by a key the enrollment lists, and about this
// enrollment's producer key and label. A statement with its signature altered, one a key the
// enrollment does not list signed, and one about another producer key or label are each refused as
// the enrollment not accepting it. A witness that holds nothing yet answers with a signed statement
// of size 0, held to the same checks, and an unsigned 404 that says the same is rejected. A witness
// that holds no record under the enrollment's key, one that answers 502, and one that answers
// another status are each told apart, since adopt answers each differently.
//
//nolint:funlen // Test function.
func TestFetchStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newTestWitness(t)
	in := newTestInstall(t)
	enr := in.enroll(t, w)
	in.entries(t, 3)
	b := in.beat(t)
	if err := in.client(t, time.Minute, nil).Beat(ctx, b); err != nil {
		t.Fatalf("Beat() error = %v", err)
	}
	held := w.statement(t, enr.KeyID)
	var root string
	atts, err := in.attestations(t).Attestations(ctx, 0)
	if err != nil {
		t.Fatalf("Attestations() error = %v", err)
	}
	for _, a := range atts {
		if a.Kind == audit.AttestationHead && a.Size == b.Seq {
			root = a.Root
		}
	}
	// A second install enrolled with the same witness and never pushed.
	idle := newTestInstall(t)
	idleEnr := idle.enroll(t, w)
	empty := w.statement(t, idleEnr.KeyID)
	// The enrollment of a producer key the witness never enrolled.
	stranger, err := identity.LoadFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	strangerEnr := *enr
	strangerEnr.KeyID, strangerEnr.PublicKey = stranger.KeyID(), stranger.PublicKeyBase64()
	foreign, err := identity.LoadWitnessFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessFile() error = %v", err)
	}
	resigned := func(t *testing.T, edit func(st *witness.Statement), by identity.Identity) []byte {
		t.Helper()
		return resignStatement(t, held, edit, by)
	}
	tests := []struct {
		// Name labels the witness's answer.
		Name string
		// Enrollment is the enrollment the statement is fetched under, the pushed install's when nil.
		Enrollment *Enrollment
		// Answer builds what a stub witness answers with, its status and body. Nil asks the real
		// witness.
		Answer func(t *testing.T) (int, []byte)
		// Want are the errors the fetch must be, none for a statement.
		Want []error
		// WantHead is the seq, link, size, and root of a statement the fetch returns.
		WantHead [4]string
	}{{ // Test 0: The real witness answers with the head it countersigned.
		Name:     "honest",
		WantHead: [4]string{fmt.Sprint(b.Seq), b.Hash, fmt.Sprint(b.Seq), root},
	}, { // Test 1: The real statement with its signature altered.
		Name: "signature altered",
		Answer: func(t *testing.T) (int, []byte) {
			st := *held
			st.Sig = "A" + st.Sig[1:]
			if st.Sig == held.Sig {
				st.Sig = "B" + st.Sig[1:]
			}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			return http.StatusOK, raw
		},
		Want: []error{ErrWitnessKey},
	}, { // Test 2: A key the enrollment does not list signed the statement.
		Name: "unlisted signer",
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resigned(t, func(*witness.Statement) {}, foreign)
		},
		Want: []error{ErrWitnessKey},
	}, { // Test 3: The witness's key signed a statement about another producer key.
		Name: "producer key swapped",
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resigned(t, func(st *witness.Statement) {
				st.KeyID, st.PublicKey = stranger.KeyID(), stranger.PublicKeyBase64()
			}, w.id)
		},
		Want: []error{ErrEnrollment},
	}, { // Test 4: The witness's key signed a statement about another label.
		Name: "label swapped",
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resigned(t, func(st *witness.Statement) {
				st.InstallID = stranger.InstallID
			}, w.id)
		},
		Want: []error{ErrEnrollment},
	}, { // Test 5: The real witness holds nothing for an install that never pushed.
		Name:       "nothing witnessed",
		Enrollment: idleEnr,
		Want:       []error{ErrNothingWitnessed},
	}, { // Test 6: The real witness holds no record under the enrollment's key.
		Name:       "not enrolled",
		Enrollment: &strangerEnr,
		Want:       []error{ErrRejected, witness.ErrNotEnrolled},
	}, { // Test 7: A tunnel in front of a stopped witness answers 502.
		Name: "bad gateway",
		Answer: func(*testing.T) (int, []byte) {
			return http.StatusBadGateway, []byte(`{"error":"bad gateway"}`)
		},
		Want: []error{ErrUnreachable},
	}, { // Test 8: A witness that answers neither the statement nor a 404 rejects the request.
		Name: "rate limited",
		Answer: func(*testing.T) (int, []byte) {
			return http.StatusTooManyRequests, []byte(`{"error":"slow down"}`)
		},
		Want: []error{ErrRejected},
	}, { // Test 9: An unsigned 404 saying nothing was witnessed is not the witness's word.
		Name:       "unsigned nothing witnessed",
		Enrollment: idleEnr,
		Answer: func(*testing.T) (int, []byte) {
			return http.StatusNotFound, []byte(`{"error":"nothing witnessed yet"}`)
		},
		Want: []error{ErrRejected},
	}, { // Test 10: The signed statement of nothing with its signature altered.
		Name:       "nothing witnessed, signature altered",
		Enrollment: idleEnr,
		Answer: func(t *testing.T) (int, []byte) {
			st := *empty
			st.Sig = "A" + st.Sig[1:]
			if st.Sig == empty.Sig {
				st.Sig = "B" + st.Sig[1:]
			}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			return http.StatusOK, raw
		},
		Want: []error{ErrWitnessKey},
	}, { // Test 11: A statement of nothing signed by a key the enrollment does not list.
		Name:       "nothing witnessed, unlisted signer",
		Enrollment: idleEnr,
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resignStatement(t, empty, func(*witness.Statement) {}, foreign)
		},
		Want: []error{ErrWitnessKey},
	}, { // Test 12: The witness's key signed a statement of nothing about another label.
		Name:       "nothing witnessed, label swapped",
		Enrollment: idleEnr,
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resignStatement(t, empty, func(st *witness.Statement) {
				st.InstallID = stranger.InstallID
			}, w.id)
		},
		Want: []error{ErrEnrollment},
	}, { // Test 13: A signed statement of size 0 that names a head is out of form.
		Name:       "size 0 naming a head",
		Enrollment: idleEnr,
		Answer: func(t *testing.T) (int, []byte) {
			return http.StatusOK, resignStatement(t, empty, func(st *witness.Statement) {
				st.LastSeq, st.LastHead = held.LastSeq, held.LastHead
			}, w.id)
		},
		Want: []error{ErrWitnessKey},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			at := *enr
			if test.Enrollment != nil {
				at = *test.Enrollment
			}
			if test.Answer != nil {
				code, body := test.Answer(t)
				stub := httptest.NewServer(http.HandlerFunc(
					func(rw http.ResponseWriter, _ *http.Request) {
						rw.Header().Set("Content-Type", "application/json")
						rw.WriteHeader(code)
						_, _ = rw.Write(body)
					}))
				t.Cleanup(stub.Close)
				at.Witness = stub.URL
			}
			st, err := FetchStatement(ctx, testHTTPClient(), &at)
			for _, want := range test.Want {
				if !errors.Is(err, want) {
					t.Errorf("FetchStatement() error = %v, want it to be %v", err, want)
				}
			}
			if len(test.Want) > 0 {
				if st != nil {
					t.Errorf("FetchStatement() = %+v with error %v, want no statement", st, err)
				}
				for _, other := range []error{ErrWitnessKey, ErrEnrollment, ErrNothingWitnessed,
					ErrUnreachable, ErrRejected} {
					if errors.Is(err, other) && !slices.Contains(test.Want, other) {
						t.Errorf("FetchStatement() error = %v, which is also %v", err, other)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchStatement() error = %v, want none", err)
			}
			got := [4]string{fmt.Sprint(st.LastSeq), st.LastHead, fmt.Sprint(st.Size), st.Root}
			if diff := cmp.Diff(test.WantHead, got); diff != "" {
				t.Errorf("statement head mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// resignStatement returns base as edit leaves it, signed by by, as a witness answer's body.
func resignStatement(t *testing.T, base *witness.Statement, edit func(st *witness.Statement),
	by identity.Identity) []byte {
	t.Helper()
	st := *base
	edit(&st)
	if err := witness.SignStatement(&st, by); err != nil {
		t.Fatalf("SignStatement() error = %v", err)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return raw
}
