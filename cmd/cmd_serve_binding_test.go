package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/run"
)

// gateKey derives a binding key from a seed of first repeated, the way every server of one
// install derives the same key from the same identity.
func gateKey(t *testing.T, first byte) *binding.Key {
	t.Helper()
	key, err := binding.DeriveKey(bytes.Repeat([]byte{first}, 32))
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	return key
}

// gateInstall is one install's runs and chain, in memory, as an upgrade finds them.
type gateInstall struct {
	// Runs is the runs table.
	Runs run.Store
	// Audits is the chain.
	Audits audit.Store
	// Unsealed is the id of a run an earlier release left with no seal, empty when there is none.
	Unsealed string
}

// gateChainDown is a chain that reads but refuses every append, the way a database that went away
// between the gate's reads and its write answers.
type gateChainDown struct {
	// Store serves every read.
	audit.Store
}

// Append fails as the database does.
func (gateChainDown) Append(context.Context, *audit.Entry) error {
	return errors.New("the chain went away")
}

// newGateInstall returns an install with no runs.
func newGateInstall() *gateInstall {
	return &gateInstall{Runs: run.NewMemStore(), Audits: audit.NewMemStore()}
}

// leaveUnsealed stores a queued run with no seal, as a release with no binding key leaves one.
func (in *gateInstall) leaveUnsealed(t *testing.T) {
	t.Helper()
	r := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy", Status: run.StatusPending,
		CreatedAt: time.Now(), Actor: "requester"}
	if err := in.Runs.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	in.Unsealed = r.ID
}

// record appends an adoption record under the id key fixes, writing no seal.
func (in *gateInstall) record(t *testing.T, key *binding.Key) {
	t.Helper()
	if err := in.Audits.Append(context.Background(), &audit.Entry{ID: key.AdoptionID(),
		Actor: "cli:operator", ActorType: actorTypeCLI, Method: audit.MethodCLI,
		Path: audit.AdoptedPath}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

// TestServeRefusesToStartKeyedWithoutAnAdoptionRecord pins the start gate. A keyed server whose
// chain holds no adoption record under its key refuses to start over work an earlier release left
// unsealed, naming the run and the command that adopts it, rather than failing that work on its
// first claim. It starts once the record is there, when there is nothing adoption would seal, and
// always when it holds no key.
//
//nolint:funlen // Test function.
func TestServeRefusesToStartKeyedWithoutAnAdoptionRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		// Name labels the install.
		Name string
		// Keyed starts the server with a binding key.
		Keyed bool
		// Setup leaves the install as the start finds it, given the server's key.
		Setup func(t *testing.T, in *gateInstall, key *binding.Key)
		// WantSays are what a refusal must say, nil for the refusal that sends the operator to adopt.
		WantSays []string
		// WantRefused reports whether the start is refused.
		WantRefused bool
	}{{ // Test 0: Unsealed work and no record refuses the start.
		Name: "no record", Keyed: true,
		Setup:       func(t *testing.T, in *gateInstall, _ *binding.Key) { in.leaveUnsealed(t) },
		WantRefused: true,
	}, { // Test 1: The record present lets the start go ahead, the work refused one by one later.
		Name: "record present", Keyed: true,
		Setup: func(t *testing.T, in *gateInstall, key *binding.Key) {
			in.leaveUnsealed(t)
			in.record(t, key)
		},
	}, { // Test 2: An adopted install starts.
		Name: "adopted", Keyed: true,
		Setup: func(t *testing.T, in *gateInstall, key *binding.Key) {
			in.leaveUnsealed(t)
			if _, err := dispatch.Adopt(ctx, dispatch.AdoptionDeps{Runs: in.Runs,
				Audits: in.Audits, Key: key}); err != nil {
				t.Fatalf("Adopt() error = %v", err)
			}
		},
	}, { // Test 3: An install with nothing adoption would seal starts.
		Name: "nothing to adopt", Keyed: true,
		Setup: func(*testing.T, *gateInstall, *binding.Key) {},
	}, { // Test 4: A run adoption would leave unsealed is no reason to hold the start.
		Name: "only rows adoption leaves", Keyed: true,
		Setup: func(t *testing.T, in *gateInstall, _ *binding.Key) {
			r := &run.Run{ID: run.NewID(), Tool: run.ToolBash, Command: "deploy",
				Status: run.StatusPending, CreatedAt: time.Now(), ParentID: new("run_gone"),
				StepIndex: new(0)}
			if err := in.Runs.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
	}, { // Test 5: A server with no key is never gated.
		Name:  "unkeyed",
		Setup: func(t *testing.T, in *gateInstall, _ *binding.Key) { in.leaveUnsealed(t) },
	}, { // Test 6: A record made under another key is not this install's.
		Name: "another key's record", Keyed: true,
		Setup: func(t *testing.T, in *gateInstall, _ *binding.Key) {
			in.leaveUnsealed(t)
			in.record(t, gateKey(t, 9))
		},
		WantRefused: true,
	}, { // Test 7: Sealed work beside no record means the record was removed, and adopt would trust it.
		Name: "record removed", Keyed: true,
		Setup: func(t *testing.T, in *gateInstall, _ *binding.Key) {
			r := &run.Run{ID: "run_sealed", Tool: run.ToolBash, Command: "deploy",
				Status: run.StatusPending, CreatedAt: time.Now(), SpecSeal: binding.SealPrefix + "00"}
			if err := in.Runs.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		},
		WantRefused: true,
		WantSays: []string{"no adoption record", "run_sealed", "the record was removed",
			"Restore the database", "Nothing was changed"},
	}, { // Test 8: A first start whose empty adoption the chain could not record does not go ahead.
		Name: "record not written", Keyed: true,
		Setup: func(_ *testing.T, in *gateInstall, _ *binding.Key) {
			in.Audits = gateChainDown{Store: in.Audits}
		},
		WantRefused: true,
		WantSays:    []string{"approval binding", "record the adoption", "the chain went away"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			in := newGateInstall()
			key := gateKey(t, 7)
			test.Setup(t, in, key)
			if !test.Keyed {
				key = nil
			}
			err := adoptionGate(ctx, in.Runs, in.Audits, key, nil, "switchtender.db")
			if got := err != nil; got != test.WantRefused {
				t.Fatalf("adoptionGate() error = %v, want refused %t", err, test.WantRefused)
			}
			if !test.WantRefused {
				// A keyed start that goes ahead leaves a record behind, so a row a writer later
				// strips of its seal is refused, never offered to adopt.
				if key != nil {
					recorded, err := dispatch.Recorded(ctx, dispatch.AdoptionDeps{Runs: in.Runs,
						Audits: in.Audits, Key: key})
					if err != nil || !recorded {
						t.Errorf("Recorded() after a keyed start = %t, %v, want an adoption record",
							recorded, err)
					}
				}
				return
			}
			says := test.WantSays
			if says == nil {
				says = []string{"no adoption record", in.Unsealed, "switchtender adopt",
					"Nothing was changed"}
			}
			for _, want := range says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// TestServeRefusesToStartOverUnadoptedWork pins the gate where serve calls it: a server started on
// a database whose earlier release left a run unsealed, with an identity to derive its key from,
// refuses before it serves anything.
func TestServeRefusesToStartOverUnadoptedWork(t *testing.T) {
	// Not parallel: runServe reads package-level flag variables and the environment.
	quietServeFlags(t)
	fx := newAdoptFixture(t)
	setString(t, &serveDB, fx.DB)
	// A start the gate failed to refuse binds a port nothing else holds, and ends with the context.
	setString(t, &serveAddr, "127.0.0.1:0")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := testCommand()
	cmd.SetContext(ctx)
	err := runServe(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no adoption record") {
		t.Fatalf("runServe() error = %v, want the start refused for the work adoption has not "+
			"recorded", err)
	}
	for _, id := range []string{fx.Held, fx.Queued} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("refusal %q does not name run %s", err, id)
		}
	}
}
