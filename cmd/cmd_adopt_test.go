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
	"strings"
	"sync"
	"testing"
	"time"

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
	}, { // Test 4: A witness that holds nothing yet bounds nothing, and says so.
		Name:      "nothing witnessed",
		Arrange:   func(t *testing.T, s *adoptWitnessSetup) { s.enroll(t) },
		WantShown: []string{"The witness at {url} has witnessed nothing for this install yet"},
	}, { // Test 5: Nothing witnessed while the store holds countersignatures trips the wire.
		Name: "nothing witnessed over countersignatures",
		Arrange: func(t *testing.T, s *adoptWitnessSetup) {
			s.enroll(t)
			s.push(t)
			s.stub(t, http.StatusNotFound, []byte(`{"error":"nothing witnessed yet"}`))
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
				if !got.Recorded || got.Entries != before.Entries+1 {
					t.Errorf("adopt() left recorded %v with %d entries, want the record appended "+
						"to %d", got.Recorded, got.Entries, before.Entries)
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
