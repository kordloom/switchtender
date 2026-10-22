package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlutil"
	"github.com/kordloom/switchtender/internal/witnessclient"
	"github.com/kordloom/switchtender/spanbeat"
	"github.com/kordloom/switchtender/witness"
)

// errAdoptTestDown is what a chain that cannot take the adoption record returns.
var errAdoptTestDown = errors.New("database is locked")

// adoptFixture is an install upgraded in place: a SQLite database and its producer identity, on
// which a release with no binding key left a held run and a queued run, neither sealed. The
// queued run is in the default queue, and nothing is left running to claim it.
type adoptFixture struct {
	// DB is the database path.
	DB string
	// Key is the binding key the install's identity derives.
	Key *binding.Key
	// Held is the id of the run waiting for a decision.
	Held string
	// Queued is the id of the run waiting in a queue nobody serves.
	Queued string
}

// newAdoptFixture builds the install. The earlier release is a dispatcher with no binding key,
// over a chain bound to the install the way its server bound it.
func newAdoptFixture(t *testing.T) *adoptFixture {
	t.Helper()
	ctx := context.Background()
	fx := &adoptFixture{DB: tempDB(t)}
	id, err := installIdentity(fx.DB)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}
	if fx.Key, err = bindingKeyOf(&id); err != nil {
		t.Fatalf("bindingKeyOf() error = %v", err)
	}
	bundle, err := openBundle(fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	if binder, ok := bundle.Audits().(audit.InstallBinder); ok {
		binder.BindInstall(id.InstallID)
	}
	idle := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
		roundhouse.Result, error) {
		return roundhouse.Result{}, errors.New("nothing runs in this test")
	})
	old := dispatch.New(bundle.Runs(), idle, nil, dispatch.WithNoJanitor(),
		dispatch.WithAudits(bundle.Audits()))
	held, err := old.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithActor("requester"), run.WithRequireApproval(true))
	old.Close()
	if err != nil {
		t.Fatalf("Submit() held error = %v", err)
	}
	// The queued run is stored once the earlier release has stopped, so nothing executes it.
	queued := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "report",
		Status: run.StatusPending, CreatedAt: time.Now(), Actor: "requester"}
	if err := bundle.Runs().Save(ctx, queued); err != nil {
		t.Fatalf("Save() queued error = %v", err)
	}
	if held.SpecSeal != "" || queued.SpecSeal != "" {
		t.Fatal("the release with no binding key sealed a run")
	}
	// The earlier release's server recorded each request on the chain as it took it.
	for _, r := range []*run.Run{held, queued} {
		if err := bundle.Audits().Append(ctx, &audit.Entry{ID: audit.NewID(), Actor: r.Actor,
			ActorType: "token", Method: "POST", Path: "/v1/runs"}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	fx.Held, fx.Queued = held.ID, queued.ID
	return fx
}

// adoptState is what an install holds after adopt ran on it.
type adoptState struct {
	// Recorded reports whether the chain holds the adoption record the install's key names.
	Recorded bool
	// Sealed are the ids of the fixture's two runs that carry a spec seal, held first.
	Sealed []string
	// Entries counts the chain's entries.
	Entries int
}

// state reads the install as adopt left it, and requires every sealed run to pass the check a
// keyed server holds it to.
func (fx *adoptFixture) state(t *testing.T) adoptState {
	t.Helper()
	ctx := context.Background()
	bundle, err := openBundle(fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	var got adoptState
	record, err := bundle.Audits().Entry(ctx, fx.Key.AdoptionID())
	switch {
	case err == nil:
		got.Recorded = record.Path == audit.AdoptedPath && record.Method == audit.MethodCLI &&
			strings.HasPrefix(record.Actor, "cli:")
	case !errors.Is(err, audit.ErrEntryNotFound):
		t.Fatalf("Entry() error = %v", err)
	}
	chain, err := bundle.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	got.Entries = len(chain)
	v := dispatch.NewBindingVerifier(bundle.Runs(), bundle.Audits(), fx.Key)
	for _, id := range []string{fx.Held, fx.Queued} {
		if id == "" {
			continue
		}
		r, err := bundle.Runs().Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if r.SpecSeal == "" {
			continue
		}
		got.Sealed = append(got.Sealed, id)
		check := v.Executable
		if r.Status == run.StatusPendingApproval {
			check = v.Held
		}
		if err := check(ctx, r); err != nil {
			t.Errorf("adopted run %s does not pass a keyed server's check: %v", id, err)
		}
	}
	return got
}

// adoptRecordFails opens an install whose chain refuses the adoption record, the way a process
// that dies between computing the seals and recording them leaves it.
func adoptRecordFails(db string) (storeBundle, error) {
	bundle, err := openExisting(db)
	if err != nil {
		return nil, err
	}
	return recordFailsBundle{storeBundle: bundle}, nil
}

// recordFailsBundle is a store bundle whose chain refuses the adoption record.
type recordFailsBundle struct {
	// storeBundle is the install's stores.
	storeBundle
}

// Audits returns the chain, refusing the adoption record.
func (b recordFailsBundle) Audits() audit.Store {
	return recordFailsChain{Store: b.storeBundle.Audits()}
}

// recordFailsChain is a chain that refuses to append the adoption record.
type recordFailsChain struct {
	// Store is the chain.
	audit.Store
}

// Append refuses the adoption record and appends anything else.
func (c recordFailsChain) Append(ctx context.Context, e *audit.Entry) error {
	if e.Path == audit.AdoptedPath {
		return errAdoptTestDown
	}
	return c.Store.Append(ctx, e)
}

// TestAdoptCommandRecordsAndSeals pins the adopt command on an install a release with no binding
// key left work on. It records the adoption and seals each row so a keyed server accepts it, when
// told to with --yes or answered yes at a terminal. It writes nothing on a dry run, on any other
// answer, or with nobody to ask. A chain that refuses the record leaves no row sealed, since the
// record is what makes adoption happen once.
//
//nolint:funlen // Test function.
func TestAdoptCommandRecordsAndSeals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the run of the command.
		Name string
		// Empty adopts an install with no runs at all.
		Empty bool
		// Request is how adopt is run, without its database.
		Request adoptRequest
		// WantErr is what the error must say, empty for none.
		WantErr string
		// WantState is what the install holds afterward.
		WantState adoptState
		// WantShown is what the output must name, the two runs' ids standing for themselves.
		WantShown []string
	}{{ // Test 0: --yes seals both runs and records the adoption.
		Name:      "yes",
		Request:   adoptRequest{Yes: true},
		WantState: adoptState{Recorded: true, Sealed: []string{"held", "queued"}},
		WantShown: []string{"held", "queued", "Recorded the adoption", "Sealed 2 rows"},
	}, { // Test 1: A dry run shows both runs and writes nothing.
		Name:      "dry run",
		Request:   adoptRequest{DryRun: true},
		WantShown: []string{"held", "queued", "Dry run: nothing was written"},
	}, { // Test 2: A yes at a terminal seals both runs.
		Name:      "answered yes",
		Request:   adoptRequest{Terminal: true, In: strings.NewReader("y\n")},
		WantState: adoptState{Recorded: true, Sealed: []string{"held", "queued"}},
		WantShown: []string{"held", "queued", "Sealed 2 rows"},
	}, { // Test 3: Any other answer cancels, having written nothing.
		Name:      "answered no",
		Request:   adoptRequest{Terminal: true, In: strings.NewReader("n\n")},
		WantErr:   "adoption canceled, nothing was changed",
		WantShown: []string{"held", "queued"},
	}, { // Test 4: With nobody at a terminal to ask and no --yes, nothing is written.
		Name:      "no terminal",
		Request:   adoptRequest{In: strings.NewReader("y\n")},
		WantErr:   "standard input is not a terminal",
		WantShown: []string{"held", "queued"},
	}, { // Test 5: A chain that refuses the record leaves every row unsealed.
		Name:    "record refused",
		Request: adoptRequest{Yes: true, Open: adoptRecordFails},
		WantErr: errAdoptTestDown.Error(),
	}, { // Test 6: An install with no runs still records that adoption ran.
		Name: "nothing to seal", Empty: true,
		Request:   adoptRequest{Yes: true},
		WantState: adoptState{Recorded: true},
		WantShown: []string{"Adoption seals no rows", "Sealed 0 rows",
			"this adoption binds it to install", audit.IdentityFile},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fx := newAdoptFixture(t)
			if test.Empty {
				fx = &adoptFixture{DB: tempDB(t)}
				id, err := installIdentity(fx.DB)
				if err != nil {
					t.Fatalf("installIdentity() error = %v", err)
				}
				if fx.Key, err = bindingKeyOf(&id); err != nil {
					t.Fatalf("bindingKeyOf() error = %v", err)
				}
			}
			before := fx.state(t).Entries
			req := test.Request
			var out, prompt strings.Builder
			req.DB, req.Out, req.Prompt = fx.DB, &out, &prompt
			if req.In == nil {
				req.In = strings.NewReader("")
			}
			err := adopt(context.Background(), req)
			if test.WantErr == "" && err != nil {
				t.Fatalf("adopt() error = %v, want none\noutput:\n%s", err, out.String())
			}
			if test.WantErr != "" && (err == nil || !strings.Contains(err.Error(), test.WantErr)) {
				t.Fatalf("adopt() error = %v, want it to say %q", err, test.WantErr)
			}
			got := fx.state(t)
			names := map[string]string{fx.Held: "held", fx.Queued: "queued"}
			for i, id := range got.Sealed {
				got.Sealed[i] = names[id]
			}
			want := test.WantState
			want.Entries = before
			if want.Recorded {
				want.Entries = before + 1
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("install mismatch (-want +got):\n%s\noutput:\n%s", diff, out.String())
			}
			shown := out.String()
			if !test.Empty {
				shown = strings.NewReplacer(fx.Held, "held", fx.Queued, "queued").Replace(shown)
			}
			for _, s := range test.WantShown {
				if !strings.Contains(shown, s) {
					t.Errorf("output does not show %q:\n%s", s, shown)
				}
			}
		})
	}
}

// TestAdoptRefusesAnInstallThatRanKeyed pins the refusals that keep adopt from being a second
// trusted moment. A chain that does not verify cannot show that adoption never ran, a seal on any
// run shows keyed operation began, finished runs included, and so does a RELEASE entry or an
// adoption record on the chain. Each refusal writes nothing, and the earlier release's runs stay
// unsealed.
//
//nolint:funlen // Test function.
func TestAdoptRefusesAnInstallThatRanKeyed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	appendEntry := func(t *testing.T, fx *adoptFixture, e *audit.Entry) {
		t.Helper()
		bundle, err := openBundle(fx.DB)
		if err != nil {
			t.Fatalf("openBundle() error = %v", err)
		}
		defer func() { _ = bundle.Close() }()
		if err := bundle.Audits().Append(ctx, e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	tests := []struct {
		// Name labels the install's state.
		Name string
		// Tamper leaves the install in that state.
		Tamper func(t *testing.T, fx *adoptFixture)
		// WantText is what the refusal must say.
		WantText string
		// WantSealed are the fixture runs the tamper itself sealed, held first.
		WantSealed []string
	}{{ // Test 0: An entry rewritten in place breaks the chain.
		Name: "chain rewritten",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE audit_entries SET actor = 'forged' WHERE seq = 1")
		},
		WantText: "the audit chain does not verify at entry 1",
	}, { // Test 1: A chain that no longer reaches an anchor lost entries off its end.
		Name: "chain cut short",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			chain, err := bundle.Audits().Chain(ctx)
			if err != nil || len(chain) == 0 {
				t.Fatalf("Chain() = %d entries, error %v", len(chain), err)
			}
			anchors, ok := bundle.Audits().(audit.AnchorStore)
			if !ok {
				t.Fatal("the SQLite chain keeps no anchors")
			}
			head := chain[len(chain)-1]
			if err := anchors.SaveAnchor(ctx, &audit.Anchor{ID: "anc_cut", Type: audit.AnchorGit,
				Shape: audit.AnchorShapeLinear, Seq: head.Seq + 3, Link: head.Hash, At: head.At,
				Ref: "refs/notes/anchor"}); err != nil {
				t.Fatalf("SaveAnchor() error = %v", err)
			}
		},
		WantText: "no longer satisfies an anchor",
	}, { // Test 2: A finished run carries a result seal.
		Name: "finished run sealed",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE runs SET status = 'succeeded', result_seal = 'bk1:00' "+
				"WHERE id = '"+fx.Queued+"'")
		},
		WantText: "1 runs already carry a seal",
	}, { // Test 3: A held run carries a spec seal.
		Name: "held run sealed",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE runs SET spec_seal = 'bk1:00' WHERE id = '"+fx.Held+"'")
		},
		WantText:   "1 runs already carry a seal",
		WantSealed: []string{"held"},
	}, { // Test 4: A held run carries a decision claim a keyed process sealed.
		Name: "claim sealed",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE runs SET decision_id = 'aud_claimed', decision_claim = "+
				`'{"id":"aud_claimed","verdict":"rejected","status":"failed","claim_seal":"bk1:00"}' `+
				"WHERE id = '"+fx.Held+"'")
		},
		WantText: "1 runs already carry a seal",
	}, { // Test 5: The chain holds a RELEASE entry.
		Name: "release on the chain",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			appendEntry(t, fx, &audit.Entry{ID: audit.NewID(), Actor: "dispatcher",
				Method: audit.MethodRelease, Path: audit.ReleasePath("run_wf", "run_step")})
		},
		WantText: "the audit chain holds a RELEASE entry",
	}, { // Test 6: The chain holds an adoption record made under another key.
		Name: "another key's record",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			other, err := binding.DeriveKey([]byte(strings.Repeat("x", 32)))
			if err != nil {
				t.Fatalf("DeriveKey() error = %v", err)
			}
			appendEntry(t, fx, &audit.Entry{ID: other.AdoptionID(), Actor: "cli:someone",
				ActorType: actorTypeCLI, Method: audit.MethodCLI, Path: audit.AdoptedPath})
		},
		WantText: "an adoption record made under another binding key",
	}, { // Test 7: The chain already holds this install's adoption record.
		Name: "adopted before",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			if err := adopt(ctx, adoptRequest{DB: fx.DB, Yes: true, Out: io.Discard,
				Prompt: io.Discard, In: strings.NewReader("")}); err != nil {
				t.Fatalf("first adopt() error = %v", err)
			}
		},
		WantText:   "adoption was already recorded on this install",
		WantSealed: []string{"held", "queued"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fx := newAdoptFixture(t)
			test.Tamper(t, fx)
			before := entryCount(t, fx.DB)
			var out strings.Builder
			err := adopt(ctx, adoptRequest{DB: fx.DB, Yes: true, Out: &out, Prompt: io.Discard,
				In: strings.NewReader("")})
			if !errors.Is(err, binding.ErrDrift) || !strings.Contains(err.Error(), test.WantText) {
				t.Fatalf("adopt() error = %v, want a refusal saying %q\noutput:\n%s", err,
					test.WantText, out.String())
			}
			if !strings.Contains(err.Error(), "Nothing was changed") {
				t.Errorf("refusal %q does not say that nothing was changed", err)
			}
			if after := entryCount(t, fx.DB); after != before {
				t.Errorf("the refused adopt appended %d chain entries", after-before)
			}
			if got := sealedOf(t, fx); !cmp.Equal(test.WantSealed, got, cmpopts.EquateEmpty()) {
				t.Errorf("sealed runs = %v, want %v", got, test.WantSealed)
			}
		})
	}
}

// entryCount counts the chain entries of db.
func entryCount(t *testing.T, db string) int {
	t.Helper()
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	chain, err := bundle.Audits().Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	return len(chain)
}

// sealedOf names which of the fixture's two runs carry a spec seal, held first, by role.
func sealedOf(t *testing.T, fx *adoptFixture) []string {
	t.Helper()
	bundle, err := openBundle(fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	var sealed []string
	for _, role := range []struct{ id, name string }{{fx.Held, "held"}, {fx.Queued, "queued"}} {
		r, err := bundle.Runs().Get(context.Background(), role.id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", role.id, err)
		}
		if r.SpecSeal != "" {
			sealed = append(sealed, role.name)
		}
	}
	return sealed
}

// answerAfter is a confirmation answer read only after act has run, standing for a writer who
// acts while the person running adopt reads the rows.
type answerAfter struct {
	// once runs act on the first read.
	once sync.Once
	// act is what the writer does.
	act func()
	// answer is what the person types.
	answer io.Reader
}

// Read runs act once, then reads the answer.
func (a *answerAfter) Read(p []byte) (int, error) {
	a.once.Do(a.act)
	return a.answer.Read(p)
}

// TestAdoptSealsWhatWasConfirmed pins that adopt seals the rows it showed, as they read when it
// showed them. A writer who rewrites a shown run and plants a new one while the person reads the
// list gets neither through: the rewritten run carries a seal over what it was shown as, which a
// keyed server refuses, and the planted run carries no seal at all.
func TestAdoptSealsWhatWasConfirmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptFixture(t)
	var planted string
	in := &answerAfter{answer: strings.NewReader("y\n"), act: func() {
		editChain(t, fx.DB, "UPDATE runs SET command = 'deploy --everywhere' WHERE id = '"+
			fx.Held+"'")
		bundle, err := openBundle(fx.DB)
		if err != nil {
			t.Errorf("openBundle() error = %v", err)
			return
		}
		defer func() { _ = bundle.Close() }()
		r := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "exfiltrate",
			Status: run.StatusPendingApproval, CreatedAt: time.Now(), Actor: "writer"}
		if err := bundle.Runs().Save(ctx, r); err != nil {
			t.Errorf("Save() error = %v", err)
		}
		planted = r.ID
	}}
	var out strings.Builder
	if err := adopt(ctx, adoptRequest{DB: fx.DB, Terminal: true, In: in, Out: &out,
		Prompt: io.Discard}); err != nil {
		t.Fatalf("adopt() error = %v\noutput:\n%s", err, out.String())
	}
	bundle, err := openBundle(fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	v := dispatch.NewBindingVerifier(bundle.Runs(), bundle.Audits(), fx.Key)
	get := func(id string) *run.Run {
		r, err := bundle.Runs().Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		return r
	}
	if err := v.Held(ctx, get(fx.Held)); !errors.Is(err, binding.ErrDrift) ||
		!strings.Contains(err.Error(), "changed after it was submitted") {
		t.Errorf("the run rewritten after it was shown: check = %v, want it refused as changed", err)
	}
	if r := get(planted); r.SpecSeal != "" {
		t.Errorf("the run planted after the list was shown carries seal %q, want none", r.SpecSeal)
	}
	if err := v.Executable(ctx, get(fx.Queued)); err != nil {
		t.Errorf("the untouched run: check = %v, want nil", err)
	}
}

// relinkWithout removes the chain entry dropID from db's audit chain and re-links every later
// entry so the chain verifies from genesis again, then removes every anchor at or past the dropped
// entry's position. It uses nothing but the public hash function and SQL, which is what a database
// writer has.
func relinkWithout(t *testing.T, db, dropID string) {
	t.Helper()
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer func() { _ = raw.Close() }()
	rows, err := raw.Query("SELECT id, at, actor, actor_type, on_behalf_of, method, path, " +
		"content_digest, seq, prev_hash, hash, install_id FROM audit_entries ORDER BY seq ASC")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	var entries []*audit.Entry
	for rows.Next() {
		var e audit.Entry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.ActorType, &e.OnBehalfOf, &e.Method, &e.Path,
			&e.ContentDigest, &e.Seq, &e.PrevHash, &e.Hash, &e.InstallID); err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		if e.At, err = sqlutil.ParseTime(at); err != nil {
			t.Fatalf("ParseTime() error = %v", err)
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	_ = rows.Close()
	k := slices.IndexFunc(entries, func(e *audit.Entry) bool { return e.ID == dropID })
	if k < 0 {
		t.Fatalf("entry %s is not on the chain", dropID)
	}
	dropped := entries[k]
	if _, err := raw.Exec("DELETE FROM audit_entries WHERE id=?", dropID); err != nil {
		t.Fatalf("delete error = %v", err)
	}
	var prev *audit.Entry
	if k > 0 {
		prev = entries[k-1]
	}
	for _, e := range entries[k+1:] {
		if prev == nil {
			e.Seq, e.PrevHash = 1, ""
		} else {
			e.Seq, e.PrevHash = prev.Seq+1, prev.Hash
		}
		e.Hash = audit.EntryHash(e)
		if _, err := raw.Exec("UPDATE audit_entries SET seq=?, prev_hash=?, hash=? WHERE id=?",
			e.Seq, e.PrevHash, e.Hash, e.ID); err != nil {
			t.Fatalf("relink error = %v", err)
		}
		prev = e
	}
	if _, err := raw.Exec("DELETE FROM audit_anchors WHERE seq >= ?", dropped.Seq); err != nil {
		t.Fatalf("delete anchors error = %v", err)
	}
}

// adoptWitnessSetup is an adoptFixture beside a serve test witness, which a test enrolls it with,
// pushes beats to, or stands a stub in front of.
type adoptWitnessSetup struct {
	// fx is the install.
	fx *adoptFixture
	// w is the witness the install enrolls with.
	w *serveTestWitness
	// id is the install's producer identity.
	id identity.Identity
	// path is the enrollment file beside the install's license file.
	path string
	// url is the witness URL the enrollment names, a stub's once one is put in front.
	url string
	// head is the statement the witness answered after the last push, nil before one.
	head *witness.Statement
	// empty is the witness's signed statement of nothing, as it answered right after enrollment.
	empty []byte
}

// newAdoptWitnessSetup returns the install and a witness, not enrolled yet.
func newAdoptWitnessSetup(t *testing.T) *adoptWitnessSetup {
	t.Helper()
	fx := newAdoptFixture(t)
	id, err := installIdentity(fx.DB)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}
	return &adoptWitnessSetup{fx: fx, w: newServeTestWitness(t), id: id,
		path: witnessclient.PathFor(license.PathFor(fx.DB))}
}

// enroll enrolls the install with the witness and writes the enrollment beside its license file.
func (s *adoptWitnessSetup) enroll(t *testing.T) {
	t.Helper()
	enr, err := witnessclient.Enroll(context.Background(), nil, s.w.url, s.id, nil, time.Now())
	if err != nil {
		t.Fatalf("Enroll() error = %v", err)
	}
	if err := enr.Save(s.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	s.url = enr.Witness
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		enr.Witness+"/v1/installs/"+enr.KeyID+"/attestation", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET statement error = %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if s.empty, err = io.ReadAll(res.Body); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("GET statement after enrollment = %d %s, %v, want the signed statement of "+
			"nothing", res.StatusCode, s.empty, err)
	}
}

// push appends a span beat, pushes it to the witness so the store holds its countersignatures,
// and keeps the statement the witness answers with afterward.
func (s *adoptWitnessSetup) push(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	bundle, err := openBundle(s.fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	audits := bundle.Audits()
	if binder, ok := audits.(audit.InstallBinder); ok {
		binder.BindInstall(s.id.InstallID)
	}
	atts, ok := audits.(audit.AttestationStore)
	if !ok {
		t.Fatal("the SQLite chain keeps no countersignatures")
	}
	var b spanbeat.AppendedBeat
	for range 1000 {
		b, err = (auditBeatStore{store: audits}).AppendSpanBeatNow(ctx, 60)
		if !errors.Is(err, audit.ErrClockBehind) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatalf("AppendSpanBeatNow() error = %v", err)
	}
	c, err := witnessclient.New(s.path, s.id, audits, atts, time.Minute, nil)
	if err != nil {
		t.Fatalf("witnessclient.New() error = %v", err)
	}
	if err := c.Beat(ctx, b); err != nil {
		t.Fatalf("Beat() error = %v", err)
	}
	enr, err := witnessclient.Load(s.path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if s.head, err = witnessclient.FetchStatement(ctx, nil, enr); err != nil {
		t.Fatalf("FetchStatement() error = %v", err)
	}
}

// entries appends n ordinary entries to the chain.
func (s *adoptWitnessSetup) entries(t *testing.T, n int) {
	t.Helper()
	bundle, err := openBundle(s.fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	if binder, ok := bundle.Audits().(audit.InstallBinder); ok {
		binder.BindInstall(s.id.InstallID)
	}
	for range n {
		if err := bundle.Audits().Append(context.Background(), &audit.Entry{ID: audit.NewID(),
			Actor: "requester", ActorType: "token", Method: "POST", Path: "/v1/runs"}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
}

// stub puts a server answering every request with code and body in front of the install, by
// pointing its enrollment at it.
func (s *adoptWitnessSetup) stub(t *testing.T, code int, body []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(code)
		_, _ = rw.Write(body)
	}))
	t.Cleanup(srv.Close)
	enr, err := witnessclient.Load(s.path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	enr.Witness = srv.URL
	if err := enr.Save(s.path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	s.url = srv.URL
}

// replay puts a stub in front of the install that answers with the witness's last statement as
// edit leaves it.
func (s *adoptWitnessSetup) replay(t *testing.T, edit func(t *testing.T, st *witness.Statement)) {
	t.Helper()
	if s.head == nil {
		t.Fatal("nothing was pushed, so there is no statement to replay")
	}
	st := *s.head
	edit(t, &st)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	s.stub(t, http.StatusOK, raw)
}

// adoptOnce adopts the install, as its upgrade did.
func (s *adoptWitnessSetup) adoptOnce(t *testing.T) {
	t.Helper()
	if err := adopt(context.Background(), adoptRequest{DB: s.fx.DB, Yes: true, Out: io.Discard,
		Prompt: io.Discard, In: strings.NewReader("")}); err != nil {
		t.Fatalf("first adopt() error = %v", err)
	}
}

// relink is the writer of F2: it removes the adoption record, re-links the chain after it, clears
// every seal, and rewrites the held run, with nothing but SQL.
func (s *adoptWitnessSetup) relink(t *testing.T) {
	t.Helper()
	relinkWithout(t, s.fx.DB, s.fx.Key.AdoptionID())
	editChain(t, s.fx.DB, "UPDATE runs SET spec_seal='', result_seal=''")
	editChain(t, s.fx.DB, "UPDATE runs SET command='deploy --everywhere' WHERE id='"+s.fx.Held+"'")
}

// TestAdoptHoldsTheChainToTheWitnessHead pins that adopt on an install enrolled with a witness
// holds the chain to the head the witness countersigned, which the database writer cannot reach.
// A chain re-linked without its adoption record, or cut back, is refused in the witness's name,
// with --fail-unsealed too. A chain longer than the head adopts. A witness that holds nothing yet
// bounds nothing, unless the store holds countersignatures, which is a tripwire. A witness that does
// not answer, and a statement the enrollment does not accept, are refused. An install that is not
// enrolled is not held to a witness, and says so. Every refusal writes nothing.
//
//nolint:funlen,maintidx // Test function.
func TestAdoptHoldsTheChainToTheWitnessHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the install's state.
		Name string
		// Arrange leaves the install in that state.
		Arrange func(t *testing.T, s *adoptWitnessSetup)
		// Flag is the --witness-enrollment value, given the setup, empty when nil.
		Flag func(t *testing.T, s *adoptWitnessSetup) string
		// FailUnsealed runs adopt with --fail-unsealed.
		FailUnsealed bool
		// Want is the error adopt must return, nil for an adoption.
		Want error
		// WantText is what the error must say, with {url}, {seq}, {head}, {size}, and {root} the
		// witness's.
		WantText []string
		// WantShown is what the output must say, with the same placeholders.
		WantShown []string
	}{{ // Test 0: An honest install whose witness holds its head adopts and names the head.
		Name: "honest",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
		},
		WantShown: []string{"The witness at {url} countersigned entry {seq} (link {head}, tree of " +
			"{size} entries, root {root})", "and the chain reaches it"},
	}, { // Test 1: A chain re-linked without its adoption record misses the witness's head.
		Name: "re-linked",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.adoptOnce(t)
			s.entries(t, 2)
			s.push(t)
			s.relink(t)
		},
		Want: binding.ErrDrift,
		WantText: []string{"does not reach the head the witness at {url} countersigned",
			"entry {seq} with link {head}", "a tree of {size} entries with root {root}",
			"Entries the witness saw are gone or rewritten", restoreAdvice},
	}, { // Test 2: Entries appended since the last beat leave the chain reaching the head.
		Name: "longer than the head",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			s.entries(t, 2)
		},
		WantShown: []string{"countersigned entry {seq}", "and the chain reaches it"},
	}, { // Test 3: A chain cut back below the witness's tree is refused naming the tree.
		Name: "cut back",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			editChain(t, s.fx.DB, "DELETE FROM audit_entries WHERE seq > "+
				"(SELECT MAX(seq) - 2 FROM audit_entries)")
		},
		Want:     binding.ErrDrift,
		WantText: []string{"a tree of {size} entries", "{url}", "are missing"},
	}, { // Test 4: A witness that holds nothing yet bounds nothing, says so, and then holds the
		// record once adopt pushes it.
		Name:      "nothing witnessed",
		Arrange:   func(t *testing.T, s *adoptWitnessSetup) { s.enroll(t) },
		WantShown: []string{"The witness at {url} has witnessed nothing for this install yet"},
	}, { // Test 5: Nothing witnessed while the store holds countersignatures trips the wire. The
		// witness's signed statement of nothing, kept from before the first push, is what a witness
		// restored from that time answers.
		Name: "nothing witnessed over countersignatures",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			s.stub(t, http.StatusOK, s.empty)
		},
		Want: binding.ErrDrift,
		WantText: []string{"the witness at {url} says it has witnessed nothing",
			"holds 2 countersignatures from a witness, the newest over entry {seq}"},
	}, { // Test 6: A witness that does not answer is refused, with no way past it.
		Name: "unreachable",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.stub(t, http.StatusBadGateway, []byte(`{"error":"bad gateway"}`))
		},
		Want:     witnessclient.ErrUnreachable,
		WantText: []string{"the witness at {url} did not answer", "Run adopt again once"},
	}, { // Test 7: A replayed statement with its signature altered is not accepted.
		Name: "bad signature",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			s.replay(t, func(_ *testing.T, st *witness.Statement) {
				st.Sig = "A" + st.Sig[1:]
				if st.Sig == s.head.Sig {
					st.Sig = "B" + st.Sig[1:]
				}
			})
		},
		Want:     witnessclient.ErrWitnessKey,
		WantText: []string{"the witness at {url} answered with a statement", "--refresh-keys"},
	}, { // Test 8: A statement a second witness identity signed is not accepted.
		Name: "foreign witness key",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			s.replay(t, func(t *testing.T, st *witness.Statement) {
				other, err := audit.LoadWitnessIdentity(t.TempDir())
				if err != nil {
					t.Fatalf("LoadWitnessIdentity() error = %v", err)
				}
				if err := witness.SignStatement(st, other); err != nil {
					t.Fatalf("SignStatement() error = %v", err)
				}
			})
		},
		Want:     witnessclient.ErrWitnessKey,
		WantText: []string{"the witness at {url} answered with a statement", "witness key"},
	}, { // Test 9: An install that is not enrolled adopts, held to no witness, and says so.
		Name: "not enrolled",
		WantShown: []string{"This install is not enrolled with a witness (no ",
			witnessclient.EnrollmentFile + "), so no witness bounds this adoption"},
	}, { // Test 10: A flag naming a missing file is an error, whatever lies beside the license.
		Name:    "flag names a missing file",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) { s.enroll(t) },
		Flag: func(t *testing.T, _ *adoptWitnessSetup) string {
			return filepath.Join(t.TempDir(), "missing.json")
		},
		Want:     witnessclient.ErrEnrollment,
		WantText: []string{"--witness-enrollment", "missing.json"},
	}, { // Test 11: --fail-unsealed on the re-linked chain is held to the head the same way.
		Name: "fail unsealed over a re-link",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.adoptOnce(t)
			s.entries(t, 2)
			s.push(t)
			s.relink(t)
		},
		FailUnsealed: true,
		Want:         binding.ErrDrift,
		WantText: []string{"does not reach the head the witness at {url} countersigned",
			"a tree of {size} entries with root {root}"},
	}, { // Test 12: An enrollment naming a plain-http witness off the loopback is refused unasked.
		Name: "plain http witness",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			enr, err := witnessclient.Load(s.path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			enr.Witness = "http://10.0.0.5:9440"
			if err := enr.Save(s.path); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			s.url = enr.Witness
		},
		Want:     witnessclient.ErrEnrollment,
		WantText: []string{"{url} is plain http", "https"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			s := newAdoptWitnessSetup(t)
			if test.Arrange != nil {
				test.Arrange(t, s)
			}
			req := adoptRequest{DB: s.fx.DB, Yes: true, FailUnsealed: test.FailUnsealed,
				Prompt: io.Discard, In: strings.NewReader("")}
			if test.Flag != nil {
				req.WitnessEnrollment = test.Flag(t, s)
			}
			fill := strings.NewReplacer("{url}", s.url)
			if s.head != nil {
				fill = strings.NewReplacer("{url}", s.url, "{seq}", fmt.Sprint(s.head.LastSeq),
					"{head}", s.head.LastHead, "{size}", fmt.Sprint(s.head.Size), "{root}",
					s.head.Root)
			}
			before, sealedBefore := s.fx.state(t), sealedOf(t, s.fx)
			var out strings.Builder
			req.Out = &out
			err := adopt(ctx, req)
			got := s.fx.state(t)
			if test.Want == nil {
				if err != nil {
					t.Fatalf("adopt() error = %v, want an adoption\noutput:\n%s", err, out.String())
				}
				// An enrolled install appends a span beat after the record and pushes it.
				appended := 1
				if s.url != "" {
					appended = 2
					if !strings.Contains(out.String(), "which covers the adoption record") {
						t.Errorf("output does not name the head that covers the record:\n%s",
							out.String())
					}
				}
				if !got.Recorded || got.Entries != before.Entries+appended {
					t.Errorf("adopt() left recorded %v with %d entries, want %d appended to %d",
						got.Recorded, got.Entries, appended, before.Entries)
				}
			} else {
				if !errors.Is(err, test.Want) {
					t.Fatalf("adopt() error = %v, want %v\noutput:\n%s", err, test.Want, out.String())
				}
				if !strings.Contains(err.Error(), "Nothing was changed") {
					t.Errorf("refusal %q does not say that nothing was changed", err)
				}
				if diff := cmp.Diff(before, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("the refused adopt changed the chain (-before +after):\n%s", diff)
				}
				if after := sealedOf(t, s.fx); !cmp.Equal(sealedBefore, after,
					cmpopts.EquateEmpty()) {
					t.Errorf("the refused adopt changed the sealed runs from %v to %v",
						sealedBefore, after)
				}
				for _, want := range test.WantText {
					if want = fill.Replace(want); !strings.Contains(err.Error(), want) {
						t.Errorf("refusal does not say %q:\n%v", want, err)
					}
				}
			}
			for _, want := range test.WantShown {
				if want = fill.Replace(want); !strings.Contains(out.String(), want) {
					t.Errorf("output does not show %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// recordSeq returns the chain position of the install's adoption record, zero when there is none.
func (s *adoptWitnessSetup) recordSeq(t *testing.T) int64 {
	t.Helper()
	bundle, err := openBundle(s.fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	e, err := bundle.Audits().Entry(context.Background(), s.fx.Key.AdoptionID())
	if errors.Is(err, audit.ErrEntryNotFound) {
		return 0
	}
	if err != nil {
		t.Fatalf("Entry() error = %v", err)
	}
	return e.Seq
}

// lastEntry returns the chain's newest entry.
func (s *adoptWitnessSetup) lastEntry(t *testing.T) *audit.Entry {
	t.Helper()
	bundle, err := openBundle(s.fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	chain, err := bundle.Audits().Chain(context.Background())
	if err != nil || len(chain) == 0 {
		t.Fatalf("Chain() = %d entries, %v, want some", len(chain), err)
	}
	return chain[len(chain)-1]
}

// witnessed returns the statement the witness holds for the install now, read from the witness
// itself, nil when it holds nothing.
func (s *adoptWitnessSetup) witnessed(t *testing.T) *witness.Statement {
	t.Helper()
	enr, err := witnessclient.Load(s.path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	enr.Witness = s.w.url
	st, err := witnessclient.FetchStatement(context.Background(), nil, enr)
	if errors.Is(err, witnessclient.ErrNothingWitnessed) {
		return nil
	}
	if err != nil {
		t.Fatalf("FetchStatement() error = %v", err)
	}
	return st
}

// TestAdoptPushesTheRecordToTheWitness pins that adopt on an enrolled install closes the window the
// adoption record sat in before: it appends a span beat right after the record and pushes it, so
// the witness's head covers the record before adopt returns, and a writer who then removes the
// record and re-links the chain is refused in the witness's name. --fail-unsealed does the same. A
// push that fails keeps the adoption, says the record is not yet witnessed until serve's first
// beat lands, and returns an error so a script sees a non-zero exit.
//
//nolint:funlen // Test function.
func TestAdoptPushesTheRecordToTheWitness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the run.
		Name string
		// FailUnsealed runs adopt with --fail-unsealed.
		FailUnsealed bool
		// Down makes the witness answer every push 502 while its statement stays readable.
		Down bool
		// Want is the error adopt must return, nil when the push lands.
		Want error
	}{{ // Test 0: Adopt pushes the record, and a re-link after it is refused.
		Name: "adopt",
	}, { // Test 1: --fail-unsealed pushes its record the same way.
		Name: "fail unsealed", FailUnsealed: true,
	}, { // Test 2: A push that fails keeps the adoption and exits non-zero.
		Name: "push fails", Down: true, Want: witnessclient.ErrUnreachable,
	}, { // Test 3: --fail-unsealed whose push fails does the same.
		Name: "fail unsealed, push fails", FailUnsealed: true, Down: true,
		Want: witnessclient.ErrUnreachable,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			s := newAdoptWitnessSetup(t)
			s.enroll(t)
			s.push(t)
			if test.Down {
				s.w.mode.Store(serveWitnessDown)
			}
			before := s.fx.state(t)
			var out strings.Builder
			err := adopt(ctx, adoptRequest{DB: s.fx.DB, Yes: true, FailUnsealed: test.FailUnsealed,
				Out: &out, Prompt: io.Discard, In: strings.NewReader("")})
			seq := s.recordSeq(t)
			if seq == 0 {
				t.Fatalf("adopt() = %v left no adoption record\noutput:\n%s", err, out.String())
			}
			// Adopt appends the record and the beat. --fail-unsealed also records each run it fails.
			if got := s.fx.state(t); !test.FailUnsealed && got.Entries != before.Entries+2 {
				t.Errorf("adopt() left %d entries, want the record and a beat appended to %d",
					got.Entries, before.Entries)
			}
			if last := s.lastEntry(t); last.Actor != audit.SpanActor || last.Seq <= seq {
				t.Errorf("the newest entry is %s %s at %d, want a span beat after the record at %d",
					last.Actor, last.Path, last.Seq, seq)
			}
			head := s.witnessed(t)
			if test.Want != nil {
				if !errors.Is(err, test.Want) {
					t.Fatalf("adopt() error = %v, want %v\noutput:\n%s", err, test.Want, out.String())
				}
				for _, want := range []string{"the adoption is recorded", "not witnessed",
					"serve's first beat lands", s.url} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the failed push does not say %q:\n%v", want, err)
					}
				}
				if strings.Contains(err.Error(), "Nothing was changed") {
					t.Errorf("the failed push says nothing was changed, and the adoption was "+
						"written:\n%v", err)
				}
				if head == nil || head.Size >= seq {
					t.Errorf("the witness holds %+v after a failed push, want the head from before "+
						"the record at %d", head, seq)
				}
				return
			}
			if err != nil {
				t.Fatalf("adopt() error = %v\noutput:\n%s", err, out.String())
			}
			if head == nil || head.Size <= seq {
				t.Fatalf("the witness holds %+v after adopt, want a tree past the record at %d",
					head, seq)
			}
			shown := fmt.Sprintf("The witness at %s countersigned entry %d (link %s, tree of %d "+
				"entries, root %s)", s.url, head.LastSeq, head.LastHead, head.Size, head.Root)
			if !strings.Contains(out.String(), shown) || !strings.Contains(out.String(),
				fmt.Sprintf("which covers the adoption record at entry %d", seq)) {
				t.Errorf("output does not name the head that covers the record:\n%s", out.String())
			}
			// The writer's move inside the old window: keyed service goes on, the record is removed
			// and the chain re-linked before the next push.
			s.entries(t, 2)
			s.relink(t)
			again, sealedBefore := s.fx.state(t), sealedOf(t, s.fx)
			out.Reset()
			err = adopt(ctx, adoptRequest{DB: s.fx.DB, Yes: true, Out: &out, Prompt: io.Discard,
				In: strings.NewReader("")})
			if !errors.Is(err, binding.ErrDrift) || !strings.Contains(err.Error(),
				"does not reach the head the witness at "+s.url) {
				t.Fatalf("adopt() after the re-link = %v, want the witness head refusal\noutput:\n%s",
					err, out.String())
			}
			if diff := cmp.Diff(again, s.fx.state(t), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the refused adopt changed the chain (-before +after):\n%s", diff)
			}
			if after := sealedOf(t, s.fx); !cmp.Equal(sealedBefore, after, cmpopts.EquateEmpty()) {
				t.Errorf("the refused adopt sealed %v, want %v", after, sealedBefore)
			}
		})
	}
}

// TestAnUnsignedNothingWitnessedIsRefused pins that an unsigned answer saying the witness holds
// nothing is not taken as the witness's word. The writer of F2 removes the record and re-links the
// chain, deletes the countersignatures the tripwire reads, and answers the statement request with
// the 404 a witness once sent for nothing witnessed, from a network position. Adopt and
// --fail-unsealed refuse, writing nothing, since a witness that holds nothing says so in a signed
// statement of size 0.
func TestAnUnsignedNothingWitnessedIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, failUnsealed := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail unsealed %t", failUnsealed), func(t *testing.T) {
			t.Parallel()
			s := newAdoptWitnessSetup(t)
			s.enroll(t)
			s.push(t)
			s.adoptOnce(t)
			s.entries(t, 1)
			s.relink(t)
			editChain(t, s.fx.DB, "DELETE FROM audit_attestations")
			s.stub(t, http.StatusNotFound, []byte(`{"error":"nothing witnessed yet"}`))
			before, sealedBefore := s.fx.state(t), sealedOf(t, s.fx)
			var out strings.Builder
			err := adopt(ctx, adoptRequest{DB: s.fx.DB, Yes: true, FailUnsealed: failUnsealed,
				Out: &out, Prompt: io.Discard, In: strings.NewReader("")})
			if !errors.Is(err, witnessclient.ErrRejected) || !strings.Contains(err.Error(),
				"Nothing was changed") {
				t.Fatalf("adopt() = %v, want the unsigned answer refused\noutput:\n%s", err,
					out.String())
			}
			if diff := cmp.Diff(before, s.fx.state(t), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("the refused adopt changed the chain (-before +after):\n%s", diff)
			}
			if after := sealedOf(t, s.fx); !cmp.Equal(sealedBefore, after, cmpopts.EquateEmpty()) {
				t.Errorf("the refused adopt sealed %v, want %v", after, sealedBefore)
			}
		})
	}
}

// plantedControl is a database value that rewrites a line of a terminal reading it: it erases the
// line, returns the cursor, and writes a forged sentence in its place.
const plantedControl = "x\x1b[2K\rforged"

// plantedC1 is a database value carrying a C1 control character, which some terminals read as an
// escape introducer, then a byte that is not UTF-8 at all.
const plantedC1 = "y\u009b2J\x9bgone"

// controlFree reports the first character of text that would act on a terminal: any control
// character but newline and tab, and the DEL and C1 range, or a byte that is not UTF-8. It returns
// the empty string for text that is safe to print.
func controlFree(text string) string {
	for rest := text; rest != ""; {
		r, size := utf8.DecodeRuneInString(rest)
		switch {
		case r == utf8.RuneError && size == 1:
			return "invalid UTF-8"
		case r < 0x20 && r != '\n' && r != '\t', r >= 0x7f && r <= 0x9f:
			return fmt.Sprintf("U+%04X", r)
		}
		rest = rest[size:]
	}
	return ""
}

// plantControls writes rows into the fixture's database carrying control characters the way a
// writer with database access can: a run whose id and requester carry them, a run to leave whose
// id does, and an anchor over the real head whose reference does.
func plantControls(t *testing.T, fx *adoptFixture) {
	t.Helper()
	ctx := context.Background()
	bundle, err := openBundle(fx.DB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	for _, r := range []*run.Run{
		{ID: "run_" + plantedControl, Tool: run.ToolBash, Command: "report",
			Status: run.StatusPending, CreatedAt: time.Now(), Actor: plantedControl},
		{ID: "run_" + plantedC1, Tool: run.ToolBash, Command: "report",
			Status: run.StatusPending, CreatedAt: time.Now(), Actor: plantedC1},
		{ID: "run_orphan_" + plantedControl, Tool: run.ToolBash, Command: "report",
			Status: run.StatusPending, CreatedAt: time.Now(), ParentID: new("run_gone"),
			StepIndex: new(0)},
	} {
		if err := bundle.Runs().Save(ctx, r); err != nil {
			t.Fatalf("Save(%q) error = %v", r.ID, err)
		}
	}
	chain, err := bundle.Audits().Chain(ctx)
	if err != nil || len(chain) == 0 {
		t.Fatalf("Chain() = %d entries, error %v", len(chain), err)
	}
	anchors, ok := bundle.Audits().(audit.AnchorStore)
	if !ok {
		t.Fatal("the SQLite chain keeps no anchors")
	}
	head := chain[len(chain)-1]
	if err := anchors.SaveAnchor(ctx, &audit.Anchor{ID: "anc_planted", Type: audit.AnchorGit,
		Shape: audit.AnchorShapeLinear, Seq: head.Seq, Link: head.Hash, At: head.At,
		Ref: "refs/notes/anchor" + plantedControl}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
}

// TestAdoptPrintsNoControlCharactersFromTheDatabase pins that a database value never reaches the
// operator's terminal raw. A writer plants an escape and a carriage return in a run's requester, in
// run ids, and in an anchor's reference, and adopt, adopt --fail-unsealed, and the start gate's
// refusal name them: each must print them escaped, in the plan, the confirmation, and every
// refusal, so no forged line or cursor movement reaches the person reading.
//
//nolint:funlen // Test function.
func TestAdoptPrintsNoControlCharactersFromTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the path.
		Name string
		// Plant leaves extra rows in the install beyond plantControls.
		Plant func(t *testing.T, fx *adoptFixture)
		// Request is how adopt is run, without its database.
		Request adoptRequest
		// WantErr reports whether adopt must refuse.
		WantErr bool
		// WantShown are the planted values whose escaped form the output or the error must hold.
		WantShown []string
	}{{ // Test 0: The confirmed plan prints the requester, the ids, and the anchor escaped.
		Name:    "confirmed",
		Request: adoptRequest{Yes: true},
		WantShown: []string{plantedControl, plantedC1, "refs/notes/anchor" + plantedControl,
			"run_orphan_" + plantedControl},
	}, { // Test 1: A dry run prints the same plan escaped.
		Name:      "dry run",
		Request:   adoptRequest{DryRun: true},
		WantShown: []string{plantedControl, plantedC1, "refs/notes/anchor" + plantedControl},
	}, { // Test 2: A plan the person declines was printed escaped before it asked.
		Name:      "declined",
		Request:   adoptRequest{Terminal: true, In: strings.NewReader("n\n")},
		WantErr:   true,
		WantShown: []string{plantedControl, plantedC1, "refs/notes/anchor" + plantedControl},
	}, { // Test 3: A run that already carries a seal refuses by naming its id escaped.
		Name: "sealed row refusal",
		Plant: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE runs SET spec_seal = 'bk1:00' WHERE actor = char(120, 27)"+
				" || '[2K' || char(13) || 'forged'")
		},
		Request:   adoptRequest{Yes: true},
		WantErr:   true,
		WantShown: []string{"run_" + plantedControl},
	}, { // Test 4: --fail-unsealed prints the requester and the ids escaped.
		Name:      "fail unsealed",
		Request:   adoptRequest{Yes: true, FailUnsealed: true},
		WantShown: []string{plantedControl, plantedC1, "refs/notes/anchor" + plantedControl},
	}, { // Test 5: An adoption record by a planted actor refuses with the actor escaped.
		Name: "earlier adoption",
		Plant: func(t *testing.T, fx *adoptFixture) {
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			if err := bundle.Audits().Append(ctx, &audit.Entry{ID: fx.Key.AdoptionID(),
				Actor: plantedControl, ActorType: actorTypeCLI, Method: audit.MethodCLI,
				Path: audit.AdoptedPath}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
		},
		Request:   adoptRequest{Yes: true},
		WantErr:   true,
		WantShown: []string{plantedControl},
	}, { // Test 6: A keyed entry with a planted id and path refuses with both escaped.
		Name: "keyed entry",
		Plant: func(t *testing.T, fx *adoptFixture) {
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			if err := bundle.Audits().Append(ctx, &audit.Entry{ID: "ent_" + plantedControl,
				Actor: "x", ActorType: actorTypeCLI, Method: audit.MethodRelease,
				Path: "/p/y\u009b2J"}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
		},
		Request:   adoptRequest{Yes: true},
		WantErr:   true,
		WantShown: []string{"ent_" + plantedControl, "/p/y\u009b2J"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fx := newAdoptFixture(t)
			plantControls(t, fx)
			if test.Plant != nil {
				test.Plant(t, fx)
			}
			req := test.Request
			var out, prompt strings.Builder
			req.DB, req.Out, req.Prompt = fx.DB, &out, &prompt
			if req.In == nil {
				req.In = strings.NewReader("")
			}
			err := adopt(ctx, req)
			if test.WantErr != (err != nil) {
				t.Fatalf("adopt() error = %v, want an error: %t\noutput:\n%q", err, test.WantErr,
					out.String())
			}
			shown := out.String() + prompt.String()
			if err != nil {
				shown += "\n" + err.Error()
			}
			if bad := controlFree(shown); bad != "" {
				t.Errorf("output carries %s from the database:\n%q", bad, shown)
			}
			for _, planted := range test.WantShown {
				if !strings.Contains(shown, strconv.Quote(planted)) &&
					!strings.Contains(shown, strconv.Quote(planted)[1:len(strconv.Quote(planted))-1]) {
					t.Errorf("output does not show %q escaped:\n%s", planted, shown)
				}
			}
		})
	}
	// Test 7: The start gate's refusal names the rows it would adopt escaped.
	t.Run("test 7 gate", func(t *testing.T) {
		t.Parallel()
		in := newGateInstall()
		for _, id := range []string{"run_" + plantedControl, "run_" + plantedC1} {
			r := &run.Run{ID: id, Tool: run.ToolBash, Command: "deploy", Status: run.StatusPending,
				CreatedAt: time.Now(), Actor: "requester"}
			if err := in.Runs.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		}
		err := adoptionGate(ctx, in.Runs, in.Audits, gateKey(t, 1), nil, "switchtender.db")
		if err == nil {
			t.Fatal("adoptionGate() = nil, want a refusal")
		}
		if bad := controlFree(err.Error()); bad != "" {
			t.Errorf("refusal carries %s from the database:\n%q", bad, err.Error())
		}
		for _, planted := range []string{"run_" + plantedControl, "run_" + plantedC1} {
			if !strings.Contains(err.Error(), strconv.Quote(planted)) {
				t.Errorf("refusal does not show %q escaped:\n%s", planted, err)
			}
		}
	})
}

// TestPrintableEscapesWhatActsOnATerminal pins the characters printable quotes: an escape, a
// carriage return, a C1 control, DEL, and a byte that is not UTF-8.
func TestPrintableEscapesWhatActsOnATerminal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{{ // Test 0: Plain text prints as it is.
		In: "refs/notes/anchor é", WantResult: "refs/notes/anchor é",
	}, { // Test 1: An escape sequence is quoted.
		In: "a\x1b[2Kb", WantResult: `"a\x1b[2Kb"`,
	}, { // Test 2: A carriage return is quoted.
		In: "a\rb", WantResult: `"a\rb"`,
	}, { // Test 3: A C1 control is quoted.
		In: "a\u009bb", WantResult: `"a\u009bb"`,
	}, { // Test 4: DEL is quoted.
		In: "a\x7fb", WantResult: `"a\x7fb"`,
	}, { // Test 5: A lone C1 byte, not UTF-8, is quoted.
		In: "a\x9bb", WantResult: `"a\x9bb"`,
	}, { // Test 6: A real replacement character prints as it is.
		In: "a\ufffdb", WantResult: "a\ufffdb",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := printable(test.In); got != test.WantResult {
				t.Errorf("printable(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}
