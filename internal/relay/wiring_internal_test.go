package relay

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// storeWithoutAppender is a run.Store that does not implement run.SummaryAppender, so a test can
// prove the relay handler refuses one rather than accepting it and rewriting a whole report per
// continuation batch. The embedded interface is nil; nothing calls it.
type storeWithoutAppender struct {
	// Store supplies the method set without supplying an implementation.
	run.Store
}

// wantPanic runs fn and reports the recovered value, or nil when fn returned normally. It is how the
// constructor tests tell a refusal from a silent acceptance.
func wantPanic(t *testing.T, fn func()) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// TestNewHTTPTransportRefusesIncompleteWiring pins that a worker transport built without a base URL
// or without a token panics at construction. Both are wiring errors, and a transport that accepted
// them would dial nowhere or present an empty bearer token, which the relay answers with 401 on
// every call for the life of the worker. Failing at startup is what turns that into a fixable error.
func TestNewHTTPTransportRefusesIncompleteWiring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		BaseURL   string
		Token     string
		WantPanic bool
	}{{ // Test 0: No base URL at all.
		BaseURL: "", Token: "tok", WantPanic: true,
	}, { // Test 1: No token at all.
		BaseURL: "http://relay.invalid", Token: "", WantPanic: true,
	}, { // Test 2: Neither, which is an entirely unconfigured worker.
		BaseURL: "", Token: "", WantPanic: true,
	}, { // Test 3: Both present, which is the wiring a worker actually has.
		BaseURL: "http://relay.invalid", Token: "tok", WantPanic: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := wantPanic(t, func() { NewHTTPTransport(test.BaseURL, test.Token, nil) })
			if test.WantPanic && got == nil {
				t.Errorf("NewHTTPTransport(%q, %q) did not panic, so a worker starts with wiring "+
					"that can never reach the control node", test.BaseURL, test.Token)
			}
			if !test.WantPanic && got != nil {
				t.Errorf("NewHTTPTransport() panicked with %v on complete wiring", got)
			}
		})
	}
}

// TestNewHTTPTransportDefaultsAndTrimsTheBaseURL pins the two things the constructor does to its
// inputs: a nil client becomes http.DefaultClient rather than a nil dereference on the first call,
// and trailing slashes come off the base URL. Without the trim, a base URL written with a trailing
// slash builds "//relay/v1/claim", which is a different path and answers 404 on every call.
func TestNewHTTPTransportDefaultsAndTrimsTheBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		BaseURL     string
		WantBaseURL string
	}{{ // Test 0: An ordinary base URL is left alone.
		BaseURL: "http://relay.invalid", WantBaseURL: "http://relay.invalid",
	}, { // Test 1: One trailing slash comes off.
		BaseURL: "http://relay.invalid/", WantBaseURL: "http://relay.invalid",
	}, { // Test 2: Several trailing slashes all come off.
		BaseURL: "http://relay.invalid///", WantBaseURL: "http://relay.invalid",
	}, { // Test 3: A path prefix survives, minus its trailing slash.
		BaseURL: "http://relay.invalid/base/", WantBaseURL: "http://relay.invalid/base",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tr, ok := NewHTTPTransport(test.BaseURL, "tok", nil).(*httpTransport)
			if !ok {
				t.Fatal("NewHTTPTransport() did not return an *httpTransport")
			}
			if diff := cmp.Diff(test.WantBaseURL, tr.baseURL); diff != "" {
				t.Errorf("base URL mismatch (-want +got):\n%s", diff)
			}
			if tr.client != http.DefaultClient {
				t.Error("a nil client did not become http.DefaultClient, so the first call " +
					"dereferences nil")
			}
			if tr.batches == nil || tr.leases == nil {
				t.Error("the batch or lease map was left nil, so the first append or claim panics")
			}
		})
	}
}

// TestNewHandlerRefusesIncompleteWiring pins the control node's own refusals. A relay handler with no
// store, with a store that cannot append a report batch, or with no worker pool is not a handler that
// can be made safe at request time: with no pools every presented token resolves to nothing and the
// mesh is dead, and with a store that cannot append, a report split across batches rewrites the whole
// accumulated set on each one. Failing at construction is the only place these are still fixable.
func TestNewHandlerRefusesIncompleteWiring(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Build     func()
		Name      string
		WantPanic bool
	}{{ // Test 0: No store at all.
		Name: "nil store", WantPanic: true,
		Build: func() { NewHandler(nil, SinglePool("tok"), nil, nil, nil) },
	}, { // Test 1: A store that is not a run.SummaryAppender.
		Name: "store without appender", WantPanic: true,
		Build: func() { NewHandler(storeWithoutAppender{}, SinglePool("tok"), nil, nil, nil) },
	}, { // Test 2: No pools at all, which would leave every token unresolvable.
		Name: "nil pools", WantPanic: true,
		Build: func() { NewHandler(run.NewMemStore(), nil, nil, nil, nil) },
	}, { // Test 3: A Pools value declaring no pool, same outcome by a different route.
		Name: "empty pools", WantPanic: true,
		Build: func() { NewHandler(run.NewMemStore(), &Pools{}, nil, nil, nil) },
	}, { // Test 4: Complete wiring with a nil logger, which is allowed and becomes a no-op.
		Name: "nil logger allowed", WantPanic: false,
		Build: func() { NewHandler(run.NewMemStore(), SinglePool("tok"), nil, nil, nil) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := wantPanic(t, test.Build)
			if test.WantPanic && got == nil {
				t.Errorf("NewHandler() accepted %s, so the control node serves the mesh with "+
					"wiring that cannot work", test.Name)
			}
			if !test.WantPanic && got != nil {
				t.Errorf("NewHandler() panicked with %v on complete wiring", got)
			}
		})
	}
}

// TestNewPolicyClientRefusesNilTransport pins that a worker policy client cannot be built without a
// transport. The plan-content gate runs where the run executes, so a policy client that could not
// ask anything would fail every terraform run closed at the first gate rather than at startup.
func TestNewPolicyClientRefusesNilTransport(t *testing.T) {
	t.Parallel()
	if got := wantPanic(t, func() { NewPolicyClient(nil) }); got == nil {
		t.Error("NewPolicyClient(nil) did not panic, so a worker starts with a policy client that " +
			"cannot read a single rule")
	}
	if got := wantPanic(t, func() { NewPolicyClient(Loopback(run.NewMemStore())) }); got != nil {
		t.Errorf("NewPolicyClient() panicked with %v on a real transport", got)
	}
}

// TestNewClientRefusesNilTransport pins that a relay run store cannot be built without a transport,
// the same refusal NewPolicyClient makes. Without it a worker wired that way panics with a nil
// dereference inside the dispatcher's claim loop instead of at startup, where the wiring error is
// still readable.
func TestNewClientRefusesNilTransport(t *testing.T) {
	t.Parallel()
	if got := wantPanic(t, func() { NewClient(nil) }); got == nil {
		t.Error("NewClient(nil) did not panic")
	}
}

// TestNormalizeOwner pins the bound and the alphabet a worker's asserted lease name is held to.
//
// The name is not the product's own text and it reaches two places that cannot take arbitrary input:
// it is hashed into an audit chain link, and it is the identity a report is matched on. Unbounded, a
// single claim writes a huge actor into every bundle exported afterwards. Unconstrained, the name can
// carry the separators the actor string is built from, so a worker asserts a second identity inside
// its own name that no token ever proved.
func TestNormalizeOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{{ // Test 0: An empty name stays empty rather than becoming something.
		In: "", WantResult: "",
	}, { // Test 1: The ordinary shape a worker asserts survives untouched.
		In: "worker-a.1_2", WantResult: "worker-a.1_2",
	}, { // Test 2: The separators the audit actor is built from cannot be smuggled in.
		In:         "w1 pool:production worker:release-admin",
		WantResult: "w1_pool_production_worker_release-admin",
	}, { // Test 3: A newline cannot break the actor across lines in the record.
		In: "w1\nadmin", WantResult: "w1_admin",
	}, { // Test 4: A name exactly at the bound is kept whole.
		In: strings.Repeat("a", maxOwnerLen), WantResult: strings.Repeat("a", maxOwnerLen),
	}, { // Test 5: One byte past the bound is cut to the bound.
		In: strings.Repeat("a", maxOwnerLen+1), WantResult: strings.Repeat("a", maxOwnerLen),
	}, { // Test 6: A far longer name is still cut to the bound, not merely shortened.
		In: strings.Repeat("b", 200_000), WantResult: strings.Repeat("b", maxOwnerLen),
	}, { // Test 7: Letters outside ASCII are replaced rather than passed through.
		In: "wörker", WantResult: "w_rker",
	}, { // Test 8: A percent and a quote, which a reader scanning the field would misparse.
		In: `w%22"a`, WantResult: "w_22_a",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := normalizeOwner(test.In)
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("normalizeOwner() mismatch (-want +got):\n%s", diff)
			}
			if len(got) > maxOwnerLen {
				t.Errorf("normalizeOwner() returned %d bytes, past the bound of %d", len(got),
					maxOwnerLen)
			}
		})
	}
}

// TestNormalizeOwnerNeverEmitsBrokenText pins that cutting a multi-byte name at a byte bound cannot
// put invalid UTF-8 into the audit chain. The cut lands mid-rune by construction here, and what comes
// out has to still be text a reader and a JSON encoder can both handle, or the entry that carries it
// is unreadable from then on.
func TestNormalizeOwnerNeverEmitsBrokenText(t *testing.T) {
	t.Parallel()
	// Three-byte runes do not divide the 128-byte bound evenly, so the cut lands inside a rune.
	got := normalizeOwner(strings.Repeat("日", 50))
	if !utf8.ValidString(got) {
		t.Fatalf("normalizeOwner() returned invalid UTF-8 %q, which poisons every export carrying it",
			got)
	}
	for _, r := range got {
		if r != '_' {
			t.Fatalf("normalizeOwner() kept rune %q from a name of non-ASCII runes", r)
		}
	}
	if got == "" {
		t.Error("normalizeOwner() emptied a non-empty name, so the asserted identity vanished")
	}
}

// TestAppendWarning pins that adding a note to a run's warning never loses what was already there and
// never says the same thing twice. The warning travels in the run's receipt, so a note repeated on
// every report would grow without bound in the evidence a reader has to trust.
func TestAppendWarning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Existing   string
		Note       string
		WantResult string
	}{{ // Test 0: Nothing there yet, so the note stands alone without a separator.
		Existing: "", Note: "clock skew", WantResult: "clock skew",
	}, { // Test 1: An unrelated warning keeps its text and gains the note.
		Existing: "no recap", Note: "clock skew", WantResult: "no recap; clock skew",
	}, { // Test 2: The same note twice is not repeated.
		Existing: "clock skew", Note: "clock skew", WantResult: "clock skew",
	}, { // Test 3: The note already inside a longer warning is not repeated either.
		Existing: "no recap; clock skew", Note: "clock skew", WantResult: "no recap; clock skew",
	}, { // Test 4: An empty note added to nothing stays empty.
		Existing: "", Note: "", WantResult: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, appendWarning(test.Existing, test.Note)); diff != "" {
				t.Errorf("appendWarning() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClampTime pins the boundary behavior of the window a worker's reported timing is held inside.
// The values reach the run's outcome entry and its receipt, so a time exactly on a bound must not be
// reported as moved: a spurious "the executor reported a time outside the window" warning on every
// run would train a reader to ignore the one time it is true.
func TestClampTime(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	floor := base
	ceiling := base.Add(time.Hour)
	tests := []struct {
		In        time.Time
		WantTime  time.Time
		WantMoved bool
	}{{ // Test 0: A time inside the window is untouched.
		In: base.Add(30 * time.Minute), WantTime: base.Add(30 * time.Minute), WantMoved: false,
	}, { // Test 1: Exactly on the floor is inside, not moved.
		In: floor, WantTime: floor, WantMoved: false,
	}, { // Test 2: Exactly on the ceiling is inside, not moved.
		In: ceiling, WantTime: ceiling, WantMoved: false,
	}, { // Test 3: One nanosecond before the floor is pulled up to it.
		In: floor.Add(-time.Nanosecond), WantTime: floor, WantMoved: true,
	}, { // Test 4: One nanosecond past the ceiling is pulled down to it.
		In: ceiling.Add(time.Nanosecond), WantTime: ceiling, WantMoved: true,
	}, { // Test 5: A wildly skewed clock is held to the ceiling, not refused.
		In: base.AddDate(50, 0, 0), WantTime: ceiling, WantMoved: true,
	}, { // Test 6: A zero time, which is what an unset value decodes to, is pulled to the floor.
		In: time.Time{}, WantTime: floor, WantMoved: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, moved := clampTime(test.In, floor, ceiling)
			if !got.Equal(test.WantTime) {
				t.Errorf("clampTime() = %v, want %v", got, test.WantTime)
			}
			if moved != test.WantMoved {
				t.Errorf("clampTime() moved = %v, want %v", moved, test.WantMoved)
			}
		})
	}
}

// TestRunPathEscapesTheID pins that a run id is escaped into exactly one path segment. The id reaches
// the URL a worker builds, so an unescaped separator in it would address a different path than the
// run it names, and a "../" would climb out of the relay's own route space entirely.
func TestRunPathEscapesTheID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ID         string
		Suffix     string
		WantResult string
	}{{ // Test 0: The ordinary generated shape needs no escaping.
		ID: "run_a1b2", Suffix: "/log", WantResult: "/relay/v1/runs/run_a1b2/log",
	}, { // Test 1: A separator cannot open a second path segment.
		ID: "a/b", Suffix: "", WantResult: "/relay/v1/runs/a%2Fb",
	}, { // Test 2: A traversal cannot climb out of the relay's routes.
		ID: "../../admin", Suffix: "/save", WantResult: "/relay/v1/runs/..%2F..%2Fadmin/save",
	}, { // Test 3: A space is percent encoded, never left raw or turned into a plus.
		ID: "a b", Suffix: "", WantResult: "/relay/v1/runs/a%20b",
	}, { // Test 4: A query separator cannot forge the continuation marker.
		ID: "a?part=continue", Suffix: "/host-summary",
		WantResult: "/relay/v1/runs/a%3Fpart=continue/host-summary",
	}, { // Test 5: Non-ASCII is encoded as its UTF-8 bytes.
		ID: "日", Suffix: "", WantResult: "/relay/v1/runs/%E6%97%A5",
	}, { // Test 6: An empty id still produces a path, which the relay answers rather than matches.
		ID: "", Suffix: "/save", WantResult: "/relay/v1/runs//save",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, runPath(test.ID, test.Suffix)); diff != "" {
				t.Errorf("runPath() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPartQuery pins the one marker that decides whether a report batch adds to what a run has stored
// or replaces it. Getting it backwards makes a run wide enough to need two calls store only its final
// partial batch, in the record its committed outcome and its receipt are both built from.
func TestPartQuery(t *testing.T) {
	t.Parallel()
	if diff := cmp.Diff("", partQuery(false)); diff != "" {
		t.Errorf("partQuery(false) mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("?part=continue", partQuery(true)); diff != "" {
		t.Errorf("partQuery(true) mismatch (-want +got):\n%s", diff)
	}
}

// TestContinuesReport pins that only the exact continuation marker is read as one. Anything else has
// to mean a whole report, because reading a stray query as a continuation would let a first batch
// upsert onto a stale set instead of clearing it.
func TestContinuesReport(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Query      string
		WantResult bool
	}{{ // Test 0: No query at all is a whole report.
		Query: "", WantResult: false,
	}, { // Test 1: The exact marker is a continuation.
		Query: "?part=continue", WantResult: true,
	}, { // Test 2: A different value is not.
		Query: "?part=start", WantResult: false,
	}, { // Test 3: An empty value is not.
		Query: "?part=", WantResult: false,
	}, { // Test 4: A different key is not.
		Query: "?continue=1", WantResult: false,
	}, { // Test 5: The marker alongside other keys is still a continuation.
		Query: "?other=1&part=continue", WantResult: true,
	}, { // Test 6: Case matters, so a near miss is not read as the marker.
		Query: "?part=Continue", WantResult: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodPost, "http://relay.invalid/x"+test.Query, nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if got := continuesReport(req); got != test.WantResult {
				t.Errorf("continuesReport(%q) = %v, want %v", test.Query, got, test.WantResult)
			}
		})
	}
}

// TestPoolAllows pins the queue boundary itself. A queue routes work to the segment that can reach
// it, so the queues a token may claim are the blast radius of that token. The case that matters most
// is the empty request: asking for nothing is asking for the default queue, so a confined pool that
// does not serve the default queue must not receive it by omission.
func TestPoolAllows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		WantQueue  string
		Declared   []string
		Want       []string
		WantResult bool
	}{{ // Test 0: A pool with no queues declared is unconfined and serves anything.
		Name: "unconfined serves a named queue", Declared: nil, Want: []string{"prod"},
		WantResult: true,
	}, { // Test 1: An unconfined pool also serves the default queue asked for by omission.
		Name: "unconfined serves the default", Declared: nil, Want: nil, WantResult: true,
	}, { // Test 2: A confined pool does not get the default queue by asking for nothing.
		Name: "confined refuses the default by omission", Declared: []string{"dmz"}, Want: nil,
		WantResult: false, WantQueue: "",
	}, { // Test 3: Nor by naming the default queue explicitly.
		Name: "confined refuses the default named", Declared: []string{"dmz"}, Want: []string{""},
		WantResult: false, WantQueue: "",
	}, { // Test 4: A pool that declares the default queue does receive it by omission.
		Name: "declaring the default grants it", Declared: []string{"", "dmz"}, Want: nil,
		WantResult: true,
	}, { // Test 5: Its own queue is served, so confinement does not break the worker.
		Name: "own queue served", Declared: []string{"dmz"}, Want: []string{"dmz"}, WantResult: true,
	}, { // Test 6: Naming its own queue alongside another does not smuggle the other through.
		Name: "own queue plus another refused", Declared: []string{"dmz"},
		Want: []string{"dmz", "prod"}, WantResult: false, WantQueue: "prod",
	}, { // Test 7: The first queue it may not have is the one named in the refusal.
		Name: "first refused queue named", Declared: []string{"dmz"},
		Want: []string{"prod", "staging"}, WantResult: false, WantQueue: "prod",
	}, { // Test 8: Queue names are compared exactly, so case is not a way in.
		Name: "case is not a way in", Declared: []string{"prod"}, Want: []string{"PROD"},
		WantResult: false, WantQueue: "PROD",
	}, { // Test 9: Nor is a name that merely contains a served one.
		Name: "substring is not a way in", Declared: []string{"prod"}, Want: []string{"prod2"},
		WantResult: false, WantQueue: "prod2",
	}, { // Test 10: A non-ASCII queue name matches only itself.
		Name: "non-ascii matches itself", Declared: []string{"produção"}, Want: []string{"produção"},
		WantResult: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			p := &Pool{Name: "p", Queues: test.Declared}
			gotQueue, gotOK := p.allows(test.Want)
			if gotOK != test.WantResult {
				t.Fatalf("allows(%v) with queues %v = %v, want %v", test.Want, test.Declared, gotOK,
					test.WantResult)
			}
			if !gotOK {
				if diff := cmp.Diff(test.WantQueue, gotQueue); diff != "" {
					t.Errorf("refused queue mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestPoolsResolve pins which presented token resolves to which pool, and that nothing resolves when
// nothing should. Resolving the wrong pool hands a token the queues of another, which is the whole
// boundary; resolving something for an empty or absent token opens the mesh to an anonymous caller.
func TestPoolsResolve(t *testing.T) {
	t.Parallel()
	first, second := "tok-first", "tok-second"
	pools := &Pools{pools: []Pool{
		{Name: "dmz", TokenSHA256: HashToken(first), Queues: []string{"dmz"}},
		{Name: "prod", TokenSHA256: HashToken(second), Queues: []string{"prod"}},
	}}
	tests := []struct {
		Pools     *Pools
		Presented string
		WantName  string
	}{{ // Test 0: The first pool's token resolves to it.
		Pools: pools, Presented: first, WantName: "dmz",
	}, { // Test 1: The second pool's token resolves to it, so the loop does not stop at the first.
		Pools: pools, Presented: second, WantName: "prod",
	}, { // Test 2: An unknown token resolves to nothing.
		Pools: pools, Presented: "tok-unknown", WantName: "",
	}, { // Test 3: An empty token resolves to nothing rather than to the first pool.
		Pools: pools, Presented: "", WantName: "",
	}, { // Test 4: A token that is the stored digest itself is not the token.
		Pools: pools, Presented: HashToken(first), WantName: "",
	}, { // Test 5: A nil Pools resolves to nothing rather than panicking.
		Pools: nil, Presented: first, WantName: "",
	}, { // Test 6: A Pools declaring no pool resolves to nothing.
		Pools: &Pools{}, Presented: first, WantName: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := test.Pools.resolve(test.Presented)
			if test.WantName == "" {
				if got != nil {
					t.Fatalf("resolve() = pool %q, want no pool", got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("resolve() = nil, want pool %q", test.WantName)
			}
			if diff := cmp.Diff(test.WantName, got.Name); diff != "" {
				t.Errorf("resolved pool mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHashTokenIsLowercaseHexSHA256 pins the digest form a pool file stores, because the file is
// compared byte for byte against it. A digest in another case or another encoding would resolve no
// token at all, which reads as a dead mesh rather than as a bad file.
func TestHashTokenIsLowercaseHexSHA256(t *testing.T) {
	t.Parallel()
	// The published SHA-256 of the empty string, so the function is checked against the algorithm
	// rather than against itself.
	const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if diff := cmp.Diff(emptyDigest, HashToken("")); diff != "" {
		t.Errorf("HashToken(\"\") mismatch (-want +got):\n%s", diff)
	}
	for testNum, token := range []string{"tok", "Tok", strings.Repeat("x", 10_000), "日本語"} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := HashToken(token)
			if len(got) != 64 {
				t.Errorf("HashToken() returned %d characters, want 64", len(got))
			}
			if got != strings.ToLower(got) {
				t.Errorf("HashToken() returned %q, which is not lowercase hex", got)
			}
			if _, err := hex.DecodeString(got); err != nil {
				t.Errorf("HashToken() returned %q, which is not hex: %v", got, err)
			}
		})
	}
	if HashToken("tok") == HashToken("Tok") {
		t.Error("HashToken() folded case, so two distinct tokens resolve to one pool")
	}
}

// TestPoolContextCarriesTheProvenPool pins that the pool a token resolved to travels with the
// request and that a request which never went through the authenticating middleware carries none.
// Every endpoint reads the pool from the context to decide whether it may touch a run, so a context
// that quietly produced a pool nobody proved would hand out the queues of another segment.
func TestPoolContextCarriesTheProvenPool(t *testing.T) {
	t.Parallel()
	if got := poolFrom(context.Background()); got != nil {
		t.Errorf("poolFrom(background) = pool %q, want none", got.Name)
	}
	p := &Pool{Name: "dmz", Queues: []string{"dmz"}}
	if got := poolFrom(withPool(context.Background(), p)); got != p {
		t.Errorf("poolFrom() = %v, want the pool that was put in", got)
	}
	// A nil pool put in reads back as nil, which is what the unconfined path expects.
	if got := poolFrom(withPool(context.Background(), nil)); got != nil {
		t.Errorf("poolFrom() = pool %q after a nil pool was carried, want none", got.Name)
	}
}

// TestLoadPoolsBoundaries pins every refusal the pool file makes and the two normalizations it
// performs. The file fails rather than degrading, because a file that quietly meant "no worker may
// connect" looks like a network problem, and one that quietly meant "every worker may claim
// everything" silently undoes the confinement the file exists to express.
//
//nolint:funlen // Test function.
func TestLoadPoolsBoundaries(t *testing.T) {
	t.Parallel()
	digest := HashToken("tok")
	other := HashToken("tok-other")
	tests := []struct {
		Name    string
		Doc     string
		WantErr bool
	}{{ // Test 0: A pool file that declares one complete pool is accepted.
		Name: "one complete pool", WantErr: false,
		Doc: "workers:\n  - name: a\n    token_sha256: " + digest + "\n",
	}, { // Test 1: An entirely empty file declares no workers and is refused.
		Name: "empty file", Doc: "", WantErr: true,
	}, { // Test 2: A file with the key but no entries is refused.
		Name: "no workers", Doc: "workers: []\n", WantErr: true,
	}, { // Test 3: A file that is not YAML at all is refused rather than read as empty.
		Name: "not yaml", Doc: "workers: [\n  - name\n", WantErr: true,
	}, { // Test 4: A file whose top level is a list, not the documented wrapper, is refused.
		Name: "wrong shape", Doc: "- name: a\n", WantErr: true,
	}, { // Test 5: A pool with no name cannot be named in a refusal, so it is refused.
		Name: "no name", Doc: "workers:\n  - token_sha256: " + digest + "\n", WantErr: true,
	}, { // Test 6: A pool with no digest has no token to resolve.
		Name: "no token", Doc: "workers:\n  - name: a\n", WantErr: true,
	}, { // Test 7: A plaintext token in the digest field is refused, never hashed for the operator.
		Name: "plaintext token", Doc: "workers:\n  - name: a\n    token_sha256: hunter2\n",
		WantErr: true,
	}, { // Test 8: A digest one character short is refused.
		Name: "digest too short", Doc: "workers:\n  - name: a\n    token_sha256: " +
			digest[:63] + "\n", WantErr: true,
	}, { // Test 9: A digest one character long is refused.
		Name: "digest too long", Doc: "workers:\n  - name: a\n    token_sha256: " + digest + "0\n",
		WantErr: true,
	}, { // Test 10: A 64-character value that is not hex is refused.
		Name: "not hex", Doc: "workers:\n  - name: a\n    token_sha256: " +
			strings.Repeat("z", 64) + "\n", WantErr: true,
	}, { // Test 11: Two pools sharing a token cannot be told apart, so the file is refused.
		Name: "shared token", Doc: "workers:\n  - name: a\n    token_sha256: " + digest +
			"\n  - name: b\n    token_sha256: " + digest + "\n", WantErr: true,
	}, { // Test 12: The duplicate check sees through case, since digests are lowercased first.
		Name: "shared token differing in case", Doc: "workers:\n  - name: a\n    token_sha256: " +
			digest + "\n  - name: b\n    token_sha256: " + strings.ToUpper(digest) + "\n",
		WantErr: true,
	}, { // Test 13: Two pools with distinct tokens are fine.
		Name: "two distinct pools", Doc: "workers:\n  - name: a\n    token_sha256: " + digest +
			"\n  - name: b\n    token_sha256: " + other + "\n", WantErr: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "workers.yaml")
			if err := os.WriteFile(path, []byte(test.Doc), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			got, err := LoadPools(path)
			if test.WantErr {
				if err == nil {
					t.Fatalf("LoadPools() accepted %s, so an install starts believing it is "+
						"confined when it is not", test.Name)
				}
				if got != nil {
					t.Error("LoadPools() returned pools alongside an error, so a caller that " +
						"ignores the error runs with a half-read file")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadPools() error = %v on %s", err, test.Name)
			}
			if got == nil || len(got.pools) == 0 {
				t.Fatal("LoadPools() returned no pools without an error")
			}
		})
	}
}

// TestLoadPoolsRefusesAMissingFile pins that a pool file that is not there stops the server. Reading
// a missing file as "no pools" would start an install whose confinement file was renamed, moved, or
// never deployed, and it would look like a working server until a worker connected.
func TestLoadPoolsRefusesAMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := LoadPools(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("LoadPools() accepted a missing file")
	}
	// A directory is not a file either, and reading one has to fail rather than parse as empty.
	if _, err := LoadPools(t.TempDir()); err == nil {
		t.Error("LoadPools() accepted a directory in place of a pool file")
	}
}

// TestLoadPoolsNormalizesTheStoredDigest pins that a digest written in upper case or with stray
// whitespace still resolves the token it is the hash of. The comparison is byte for byte against a
// lowercase hex digest, so without the normalization an operator who pasted a digest from a tool that
// upper-cases it would have a pool no token can ever resolve, and the mesh would simply be dead.
func TestLoadPoolsNormalizesTheStoredDigest(t *testing.T) {
	t.Parallel()
	const token = "tok-upper"
	path := filepath.Join(t.TempDir(), "workers.yaml")
	doc := "workers:\n  - name: a\n    token_sha256: \"  " + strings.ToUpper(HashToken(token)) + "  \"\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	pools, err := LoadPools(path)
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}
	got := pools.resolve(token)
	if got == nil {
		t.Fatal("a digest stored in upper case resolved no token, so the pool is unreachable")
	}
	if diff := cmp.Diff("a", got.Name); diff != "" {
		t.Errorf("resolved pool mismatch (-want +got):\n%s", diff)
	}
}

// TestDecodeCappedBoundaries pins the streaming array decoder at every edge that decides whether a
// worker can force unbounded work on the control node. The cap has to apply during the decode, an
// array that is not one has to be refused, and a body exactly at the cap has to be accepted, since
// refusing it would drop a legitimate report a batch size was chosen to fit.
//
//nolint:funlen // Test function.
func TestDecodeCappedBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Body      string
		Max       int
		WantCount int
		WantErr   bool
		WantOver  bool
	}{{ // Test 0: An empty array decodes to no elements and no error.
		Name: "empty array", Body: "[]", Max: 5, WantCount: 0,
	}, { // Test 1: One element under the cap.
		Name: "one element", Body: `[{"host":"web01"}]`, Max: 5, WantCount: 1,
	}, { // Test 2: Exactly the cap is accepted, since the batch size is chosen to fit under it.
		Name: "exactly the cap", Body: `[{},{},{}]`, Max: 3, WantCount: 3,
	}, { // Test 3: One past the cap is refused with the sentinel that maps to 413.
		Name: "one past the cap", Body: `[{},{},{},{}]`, Max: 3, WantErr: true, WantOver: true,
	}, { // Test 4: A cap of zero refuses the first element rather than accepting one.
		Name: "zero cap with an element", Body: `[{}]`, Max: 0, WantErr: true, WantOver: true,
	}, { // Test 5: A cap of zero still accepts an empty array, which carries no work.
		Name: "zero cap empty array", Body: "[]", Max: 0, WantCount: 0,
	}, { // Test 6: An object is not an array and is refused before anything is read.
		Name: "object not array", Body: `{"host":"web01"}`, Max: 5, WantErr: true,
	}, { // Test 7: JSON null is not an array either.
		Name: "null", Body: "null", Max: 5, WantErr: true,
	}, { // Test 8: A bare string is not an array.
		Name: "string", Body: `"web01"`, Max: 5, WantErr: true,
	}, { // Test 9: An entirely empty body is refused rather than read as an empty array.
		Name: "empty body", Body: "", Max: 5, WantErr: true,
	}, { // Test 10: An array that is never closed is refused.
		Name: "unterminated array", Body: `[{"host":"web01"}`, Max: 5, WantErr: true,
	}, { // Test 11: An element of the wrong JSON type is refused.
		Name: "wrong element type", Body: `[1,2]`, Max: 5, WantErr: true,
	}, { // Test 12: Whitespace and newlines around the array are fine.
		Name: "whitespace", Body: "\n  [ {\"host\":\"web01\"} ]\n ", Max: 5, WantCount: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := decodeCapped[run.HostSummary](strings.NewReader(test.Body), test.Max)
			if test.WantErr {
				if err == nil {
					t.Fatalf("decodeCapped(%q) accepted a body it must refuse", test.Body)
				}
				if test.WantOver && !errors.Is(err, errTooManyElements) {
					t.Errorf("decodeCapped() error = %v, want errTooManyElements so the caller "+
						"answers 413 and the worker learns to report in smaller batches", err)
				}
				if !test.WantOver && errors.Is(err, errTooManyElements) {
					t.Errorf("decodeCapped() reported an over-cap body for %q", test.Body)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeCapped(%q) error = %v", test.Body, err)
			}
			if diff := cmp.Diff(test.WantCount, len(got)); diff != "" {
				t.Errorf("element count mismatch (-want +got):\n%s", diff)
			}
			if got == nil {
				t.Error("decodeCapped() returned a nil slice, so a caller cannot tell an empty " +
					"report from an absent one")
			}
		})
	}
}

// TestDecodeCappedRoundTripsWhatWentIn pins that the elements come back as they went in, with their
// own JSON tags and no reordering. The decoded values are what a run's committed outcome and its
// receipt are built from, so a field silently lost here is evidence that is quietly wrong.
func TestDecodeCappedRoundTripsWhatWentIn(t *testing.T) {
	t.Parallel()
	body := `[{"host":"web01","ok":3,"changed":1,"failures":0,"worst":"changed"},` +
		`{"host":"дб01","ok":0,"changed":0,"failures":2,"worst":"failed"}]`
	got, err := decodeCapped[run.HostSummary](strings.NewReader(body), maxRelayElements)
	if err != nil {
		t.Fatalf("decodeCapped() error = %v", err)
	}
	want := []run.HostSummary{
		{Host: "web01", OK: 3, Changed: 1, Worst: "changed"},
		{Host: "дб01", Failures: 2, Worst: "failed"},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("decoded summaries mismatch (-want +got):\n%s", diff)
	}
}

// TestDecodeCappedHoldsEventsToTheSameCap pins that the cap applies to the event stream too, not only
// to summaries. Events are the widest report a run makes, so they are the body a worker would use to
// force an unbounded decode on the control node.
func TestDecodeCappedHoldsEventsToTheSameCap(t *testing.T) {
	t.Parallel()
	over := "[" + strings.TrimSuffix(strings.Repeat(`{"type":"runner_ok"},`,
		maxRelayElements+1), ",") + "]"
	if _, err := decodeCapped[event.Event](strings.NewReader(over), maxRelayElements); !errors.Is(err,
		errTooManyElements) {
		t.Errorf("decodeCapped() error = %v on an over-cap event body, want errTooManyElements", err)
	}
	at := "[" + strings.TrimSuffix(strings.Repeat(`{"type":"runner_ok"},`, maxRelayElements),
		",") + "]"
	got, err := decodeCapped[event.Event](strings.NewReader(at), maxRelayElements)
	if err != nil {
		t.Fatalf("decodeCapped() refused a body exactly at the cap: %v", err)
	}
	if len(got) != maxRelayElements {
		t.Errorf("decoded %d events, want %d", len(got), maxRelayElements)
	}
}
