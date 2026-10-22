package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// commandLog is a runner that records each command it executes and succeeds.
type commandLog struct {
	// mu guards ran.
	mu sync.Mutex
	// ran holds the commands executed, in order.
	ran []string
}

// Run records the command and succeeds.
func (c *commandLog) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = append(c.ran, spec.Command)
	return roundhouse.Result{}, nil
}

// commands returns the commands executed so far.
func (c *commandLog) commands() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ran)
}

// runOf reads the run id from the install db.
func runOf(t *testing.T, db, id string) *run.Run {
	t.Helper()
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	r, err := bundle.Runs().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return r
}

// TestFailUnsealedEndsTheLockout pins the way out of the lockout a writer can leave on an install
// upgraded with unsealed work. Evidence that keyed operation began, a RELEASE entry, another key's
// adoption record, or a seal string on a row, makes adopt refuse, while the start gate finds no
// record and sends the operator to adopt, so restore was the only remedy offered. Both refusals now
// name --fail-unsealed. It records the adoption trusting no row and fails every unsealed unfinished
// run, after which the gate lets a keyed start through and that server runs only new work: a run
// the writer planted a seal on is refused when it is decided.
//
//nolint:funlen // Test function.
func TestFailUnsealedEndsTheLockout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the evidence the writer left.
		Name string
		// Tamper leaves it.
		Tamper func(t *testing.T, fx *adoptFixture)
		// WantHeld is the status the held run ends in.
		WantHeld run.Status
		// WantGate is what the gate's refusal must say besides naming --fail-unsealed.
		WantGate string
	}{{ // Test 0: A RELEASE entry on the chain.
		Name: "release on the chain",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			if err := bundle.Audits().Append(ctx, &audit.Entry{ID: audit.NewID(), Actor: "dispatcher",
				Method: audit.MethodRelease, Path: audit.ReleasePath("run_wf", "run_step")}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
		},
		WantHeld: run.StatusFailed,
		WantGate: "being asked to adopt is itself evidence of tampering",
	}, { // Test 1: An adoption record made under another key.
		Name: "another key's record",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			other, err := binding.DeriveKey([]byte(strings.Repeat("x", 32)))
			if err != nil {
				t.Fatalf("DeriveKey() error = %v", err)
			}
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			if err := bundle.Audits().Append(ctx, &audit.Entry{ID: other.AdoptionID(),
				Actor: "cli:someone", ActorType: actorTypeCLI, Method: audit.MethodCLI,
				Path: audit.AdoptedPath}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}
		},
		WantHeld: run.StatusFailed,
		WantGate: "being asked to adopt is itself evidence of tampering",
	}, { // Test 2: A seal string written on the held run.
		Name: "planted seal",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE runs SET spec_seal = 'bk1:00' WHERE id = '"+fx.Held+"'")
		},
		WantHeld: run.StatusPendingApproval,
		WantGate: "the record was removed from the chain",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fx := newAdoptFixture(t)
			test.Tamper(t, fx)
			bundle, err := openBundle(fx.DB)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			err = adoptionGate(ctx, bundle.Runs(), bundle.Audits(), fx.Key, nil, fx.DB)
			if err == nil || !strings.Contains(err.Error(), "adopt --fail-unsealed") ||
				!strings.Contains(err.Error(), test.WantGate) {
				t.Fatalf("adoptionGate() before = %v, want a refusal naming --fail-unsealed and "+
					"saying %q", err, test.WantGate)
			}
			err = adopt(ctx, adoptRequest{DB: fx.DB, Yes: true, Out: io.Discard, Prompt: io.Discard,
				In: strings.NewReader("")})
			if !errors.Is(err, binding.ErrDrift) || !strings.Contains(err.Error(), "adopt --fail-unsealed") {
				t.Fatalf("adopt() = %v, want a refusal naming --fail-unsealed", err)
			}
			var out strings.Builder
			if err := adopt(ctx, adoptRequest{DB: fx.DB, Yes: true, FailUnsealed: true, Out: &out,
				Prompt: io.Discard, In: strings.NewReader("")}); err != nil {
				t.Fatalf("adopt --fail-unsealed error = %v\noutput:\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "trusting no row") {
				t.Errorf("output does not say the adoption trusts no row:\n%s", out.String())
			}
			if err := adoptionGate(ctx, bundle.Runs(), bundle.Audits(), fx.Key, nil, fx.DB); err != nil {
				t.Fatalf("adoptionGate() after = %v, want the start allowed", err)
			}
			for id, want := range map[string]run.Status{fx.Held: test.WantHeld,
				fx.Queued: run.StatusFailed} {
				r := runOf(t, fx.DB, id)
				if r.Status != want {
					t.Errorf("run %s is %s, want %s", id, r.Status, want)
				}
				if want == run.StatusFailed && r.Error != dispatch.NotAdoptedText {
					t.Errorf("run %s failed with %q, want %q", id, r.Error, dispatch.NotAdoptedText)
				}
				if want == run.StatusFailed && dispatch.CarriesSeal(r) {
					t.Errorf("run %s was failed carrying a seal", id)
				}
				owed, err := bundle.Runs().OutcomeOwed(ctx, id)
				if err != nil || (want.Terminal() && owed) {
					t.Errorf("OutcomeOwed(%s) = %t, %v, want the outcome committed", id, owed, err)
				}
			}

			runner := &commandLog{}
			keyed := dispatch.New(bundle.Runs(), runner, nil, dispatch.WithNoJanitor(),
				dispatch.WithAudits(bundle.Audits()), dispatch.WithBindingKey(fx.Key),
				dispatch.WithClaimInterval(5*time.Millisecond))
			defer keyed.Close()
			if _, err := keyed.Approve(ctx, fx.Held, outcome.Decider{Name: "approver",
				Type: "human"}); err == nil {
				t.Error("Approve() of the held run succeeded, want it refused")
			}
			fresh, err := keyed.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand("fresh"), run.WithActor("requester"))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			deadline := time.Now().Add(time.Minute)
			for !runOf(t, fx.DB, fresh.ID).Status.Terminal() {
				if time.Now().After(deadline) {
					t.Fatal("the fresh run never finished")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if diff := cmp.Diff([]string{"fresh"}, runner.commands()); diff != "" {
				t.Errorf("executed commands mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestFailUnsealedRefuses pins what --fail-unsealed still refuses, writing nothing: an install
// whose chain already records its adoption, a chain that does not verify, and a run with nobody to
// ask and no --yes. A dry run shows the runs and writes nothing.
//
//nolint:funlen // Test function.
func TestFailUnsealedRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the case.
		Name string
		// Tamper leaves the install in its state, nil for none.
		Tamper func(t *testing.T, fx *adoptFixture)
		// Request is how adopt is run, without its database.
		Request adoptRequest
		// WantErr is what the error must say, empty for none.
		WantErr string
		// WantShown is what the output must say, empty for nothing.
		WantShown string
	}{{ // Test 0: An install already adopted.
		Name: "adopted before",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			if err := adopt(ctx, adoptRequest{DB: fx.DB, Yes: true, FailUnsealed: true,
				Out: io.Discard, Prompt: io.Discard, In: strings.NewReader("")}); err != nil {
				t.Fatalf("first adopt --fail-unsealed error = %v", err)
			}
		},
		Request: adoptRequest{Yes: true, FailUnsealed: true},
		WantErr: "adoption was already recorded on this install",
	}, { // Test 1: A chain rewritten in place.
		Name: "chain rewritten",
		Tamper: func(t *testing.T, fx *adoptFixture) {
			editChain(t, fx.DB, "UPDATE audit_entries SET actor = 'forged' WHERE seq = 1")
		},
		Request: adoptRequest{Yes: true, FailUnsealed: true},
		WantErr: "the audit chain does not verify at entry 1",
	}, { // Test 2: Nobody to ask and no --yes.
		Name:      "no terminal",
		Request:   adoptRequest{FailUnsealed: true, In: strings.NewReader("y\n")},
		WantErr:   "standard input is not a terminal",
		WantShown: "carry no seal and are failed",
	}, { // Test 3: A dry run.
		Name:      "dry run",
		Request:   adoptRequest{FailUnsealed: true, DryRun: true},
		WantShown: "Dry run: nothing was written",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			fx := newAdoptFixture(t)
			if test.Tamper != nil {
				test.Tamper(t, fx)
			}
			before := entryCount(t, fx.DB)
			held, queued := runOf(t, fx.DB, fx.Held), runOf(t, fx.DB, fx.Queued)
			req := test.Request
			var out strings.Builder
			req.DB, req.Out, req.Prompt = fx.DB, &out, io.Discard
			if req.In == nil {
				req.In = strings.NewReader("")
			}
			err := adopt(ctx, req)
			if test.WantErr == "" && err != nil {
				t.Fatalf("adopt() error = %v, want none", err)
			}
			if test.WantErr != "" && (err == nil || !strings.Contains(err.Error(), test.WantErr)) {
				t.Fatalf("adopt() error = %v, want it to say %q", err, test.WantErr)
			}
			if !strings.Contains(out.String(), test.WantShown) {
				t.Errorf("output does not say %q:\n%s", test.WantShown, out.String())
			}
			if after := entryCount(t, fx.DB); after != before {
				t.Errorf("adopt appended %d chain entries, want none", after-before)
			}
			for _, was := range []*run.Run{held, queued} {
				if now := runOf(t, fx.DB, was.ID); now.Status != was.Status || now.Error != was.Error {
					t.Errorf("run %s moved from %s to %s (%s)", was.ID, was.Status, now.Status,
						now.Error)
				}
			}
		})
	}
}

// TestAdoptShowsTheChainBeforeAsking pins what adopt prints before it asks, so the person running
// it can tell a second adoption from the first: the chain's entry count, its newest anchor or that
// it has none, the install it is bound to or that adoption binds it, and that being asked to adopt
// an install adopted before is evidence of tampering. The table names who decided each row and the
// decision entry, and counts the claims whose decision the chain does not hold.
//
//nolint:funlen // Test function.
func TestAdoptShowsTheChainBeforeAsking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the install.
		Name string
		// Anchored records a git anchor over the chain's newest entry.
		Anchored bool
		// Empty adopts an install with no runs and no entries.
		Empty bool
		// FailUnsealed runs adopt with --fail-unsealed.
		FailUnsealed bool
	}{{ // Test 0: A bound chain with an anchor.
		Name: "anchored", Anchored: true,
	}, { // Test 1: A bound chain with no anchor.
		Name: "no anchors",
	}, { // Test 2: An install whose chain no entry binds.
		Name: "unbound", Empty: true,
	}, { // Test 3: The same display before --fail-unsealed asks.
		Name: "fail unsealed", Anchored: true, FailUnsealed: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var fx *adoptFixture
			if test.Empty {
				fx = &adoptFixture{DB: tempDB(t)}
				if _, err := installIdentity(fx.DB); err != nil {
					t.Fatalf("installIdentity() error = %v", err)
				}
			} else {
				fx = newAdoptFixture(t)
			}
			id, err := installIdentity(fx.DB)
			if err != nil {
				t.Fatalf("installIdentity() error = %v", err)
			}
			count := entryCount(t, fx.DB)
			want := []string{fmt.Sprintf("The audit chain holds %d entries.", count),
				"Adoption runs once per install"}
			switch {
			case test.Empty:
				want = append(want, "It has no anchors.",
					"bound to no install yet, and this adoption binds it to install "+id.InstallID)
			default:
				want = append(want, "It is bound to install "+id.InstallID)
			}
			if test.Anchored {
				bundle, err := openBundle(fx.DB)
				if err != nil {
					t.Fatalf("openBundle() error = %v", err)
				}
				chain, err := bundle.Audits().Chain(ctx)
				if err != nil {
					t.Fatalf("Chain() error = %v", err)
				}
				head := chain[len(chain)-1]
				if err := bundle.Audits().(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
					ID: "anc_head", Type: audit.AnchorGit, Shape: audit.AnchorShapeLinear,
					Seq: head.Seq, Link: head.Hash, At: head.At, Ref: "refs/notes/anchor"}); err != nil {
					t.Fatalf("SaveAnchor() error = %v", err)
				}
				_ = bundle.Close()
				want = append(want, fmt.Sprintf("Its newest anchor is a %s anchor at %d, "+
					"refs/notes/anchor.", audit.AnchorGit, head.Seq))
			} else if !test.Empty {
				want = append(want, "It has no anchors.")
			}
			var out strings.Builder
			if err := adopt(ctx, adoptRequest{DB: fx.DB, DryRun: true, FailUnsealed: test.FailUnsealed,
				Out: &out, Prompt: io.Discard, In: strings.NewReader("")}); err != nil {
				t.Fatalf("adopt() error = %v\noutput:\n%s", err, out.String())
			}
			for _, s := range want {
				if !strings.Contains(out.String(), s) {
					t.Errorf("output does not say %q:\n%s", s, out.String())
				}
			}
		})
	}
}

// TestShowAdoptionNamesTheDecisions pins the decision columns and the unrecorded claim count.
func TestShowAdoptionNamesTheDecisions(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	showAdoption(&out, &dispatch.AdoptionWork{Rows: []dispatch.AdoptedRow{
		{ID: "run_held", Seal: "submit", Actor: "requester", SpecDigest: "sha256:aa"},
		{ID: "run_claimed", Seal: "claim", Actor: "requester", SpecDigest: "sha256:bb",
			DecidedBy: "approver", DecisionEntry: "aud_claim", Unrecorded: true},
	}})
	for _, s := range []string{"DECIDED BY", "DECISION ENTRY", "approver", "aud_claim",
		"1 decision claims carry a decision the audit chain does not hold yet"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output does not show %q:\n%s", s, out.String())
		}
	}
}
