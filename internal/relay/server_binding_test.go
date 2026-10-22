package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// bndWorker is the lease name every worker in these tests claims under.
const bndWorker = "worker-1"

// bndStoreError is what a failing chain read answers. It carries the word a run's error must
// never show for a read that failed, since on a run it would read as tampering.
const bndStoreError = "dial tcp 10.0.0.5:5432: connect: connection refused"

// bndNode is a control node serving one worker pool, with the stores and the key behind it.
type bndNode struct {
	// Store is the run store behind the node, read and written directly by a test.
	Store run.Store
	// Audits is the chain behind the node, written directly by a test.
	Audits audit.Store
	// Key seals the rows a test builds, nil on an unkeyed node.
	Key *binding.Key
	// Binder is the verifier the node holds leased runs to, nil on an unkeyed node.
	Binder *binding.Verifier
	// URL is the node's base URL.
	URL string
	// mu guards announced.
	mu sync.Mutex
	// announced lists the runs the node announced, as "id status".
	announced []string
}

// bndFailingEntries is a chain whose reads by id always fail, the way a database that went away
// mid-check answers.
type bndFailingEntries struct {
	// Store serves every other call.
	audit.Store
}

// Entry fails with bndStoreError.
func (bndFailingEntries) Entry(context.Context, string) (*audit.Entry, error) {
	return nil, errors.New(bndStoreError)
}

// bndCancelAfterClaim is a run store in which a person cancels each run right after it is leased,
// the window between a claim and the control node's refusal of the run.
type bndCancelAfterClaim struct {
	// Store serves every call.
	run.Store
	// SummaryAppender serves the continuation writes, which no test here makes.
	run.SummaryAppender
}

// Claim leases a run and then records a cancel request on it.
func (s bndCancelAfterClaim) Claim(ctx context.Context, owner string, queues []string) (*run.Run,
	error) {
	leased, err := s.Store.Claim(ctx, owner, queues)
	if err != nil {
		return nil, err
	}
	if err := s.RequestCancel(ctx, leased.ID); err != nil {
		return nil, err
	}
	return leased, nil
}

// bndConfig shapes the control node newBndNode stands up.
type bndConfig struct {
	// Unkeyed stands the node up with no binding key.
	Unkeyed bool
	// FailEntries makes every chain read by id fail, for the verifier and for the node alike.
	FailEntries bool
	// CancelAfterClaim records a cancel request on each run right after the node leases it.
	CancelAfterClaim bool
}

// newBndNode stands up a control node shaped by cfg.
func newBndNode(t *testing.T, cfg bndConfig, opts ...HandlerOption) *bndNode {
	t.Helper()
	n := &bndNode{Store: run.NewMemStore(), Audits: audit.NewMemStore()}
	chain := n.Audits
	if cfg.FailEntries {
		chain = bndFailingEntries{Store: n.Audits}
	}
	served := n.Store
	if cfg.CancelAfterClaim {
		appender, ok := n.Store.(run.SummaryAppender)
		if !ok {
			t.Fatal("the memory store does not append summaries")
		}
		served = bndCancelAfterClaim{Store: n.Store, SummaryAppender: appender}
	}
	if !cfg.Unkeyed {
		key, err := binding.DeriveKey(bytes.Repeat([]byte{7}, 32))
		if err != nil {
			t.Fatalf("DeriveKey() error = %v", err)
		}
		n.Key = key
		n.Binder = dispatch.NewBindingVerifier(n.Store, chain, key)
	}
	announcer := AnnouncerFunc(func(r *run.Run) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.announced = append(n.announced, r.ID+" "+string(r.Status))
	})
	opts = append([]HandlerOption{WithBinder(n.Binder), WithAnnouncer(announcer)}, opts...)
	ts := httptest.NewServer(NewHandler(served, SinglePool("tok"), zap.NewNop(), nil, chain,
		opts...))
	t.Cleanup(ts.Close)
	n.URL = ts.URL
	return n
}

// announcements returns what the node announced so far.
func (n *bndNode) announcements() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.announced...)
}

// seal seals r the way the process seals a row it is about to create, with the record rec builds.
func (n *bndNode) seal(t *testing.T, r *run.Run, rec func() (binding.Record, error)) {
	t.Helper()
	r.Sanitize()
	got, err := rec()
	if err != nil {
		t.Fatalf("build the record of %s: %v", r.ID, err)
	}
	r.SpecSeal = n.Key.Seal(got)
}

// save stores r.
func (n *bndNode) save(t *testing.T, r *run.Run) {
	t.Helper()
	if err := n.Store.Save(context.Background(), r); err != nil {
		t.Fatalf("Save(%s) error = %v", r.ID, err)
	}
}

// rewrite applies a database writer's edit to the stored run id.
func (n *bndNode) rewrite(t *testing.T, id string, edit func(r *run.Run)) {
	t.Helper()
	r, err := n.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	edit(r)
	n.save(t, r)
}

// entry appends a chain entry with method and path and returns it as the chain holds it.
func (n *bndNode) entry(t *testing.T, method, path string) *audit.Entry {
	t.Helper()
	ctx := context.Background()
	e := &audit.Entry{
		ID: audit.NewID(), Actor: "user:approver", Method: method, Path: path,
		ContentDigest: "sha256:" + strings.Repeat("ab", 32),
	}
	if err := n.Audits.Append(ctx, e); err != nil {
		t.Fatalf("Append(%s) error = %v", path, err)
	}
	got, err := n.Audits.Entry(ctx, e.ID)
	if err != nil {
		t.Fatalf("Entry(%s) error = %v", e.ID, err)
	}
	return got
}

// submitted stores a sealed top-level run as Submit builds one.
func (n *bndNode) submitted(t *testing.T, id string) *run.Run {
	t.Helper()
	r := &run.Run{
		ID: id, Playbook: "site.yml", Inventory: "web01\n", Status: run.StatusPending,
		CreatedAt: time.Now(), Actor: "casey", RequireDistinctApprover: true,
	}
	n.seal(t, r, func() (binding.Record, error) { return binding.SubmitRecord(r) })
	n.save(t, r)
	return r
}

// approved stores a sealed top-level run an approver released, stamped with both digests, whose
// seal names root. A nil root records a DECISION entry for the run and names it.
func (n *bndNode) approved(t *testing.T, id string, root *binding.Root) *run.Run {
	t.Helper()
	if root == nil {
		e := n.entry(t, audit.MethodDecision, "/runs/"+id+"/decision/"+outcome.StepApproved)
		got := binding.RootOf(e)
		root = &got
	}
	r := &run.Run{
		ID: id, Playbook: "deploy.yml", Inventory: "db01\n", Status: run.StatusPendingApproval,
		CreatedAt: time.Now(), Actor: "casey", ApprovalRequested: true,
	}
	r.Sanitize()
	stampSpec(t, r)
	n.seal(t, r, func() (binding.Record, error) { return binding.RunRecord(r, *root) })
	n.save(t, r)
	return n.decide(t, id, root.EntryID, run.StatusPending, "")
}

// decide records decisionID as the decision that released the held run id into to, owner taking
// its lease when it goes to running, through the only writes that set a run's decision, and
// returns the run as stored.
func (n *bndNode) decide(t *testing.T, id, decisionID string, to run.Status, owner string) *run.Run {
	t.Helper()
	ctx := context.Background()
	if ok, err := n.Store.ClaimDecision(ctx, id, decisionID, "approve"); err != nil || !ok {
		t.Fatalf("ClaimDecision(%s) = %t, %v, want the claim", id, ok, err)
	}
	settle := run.DecisionSettle{Status: to, Owner: owner}
	ok, err := n.Store.SettleDecision(ctx, id, decisionID, settle)
	if err != nil || !ok {
		t.Fatalf("SettleDecision(%s) = %t, %v, want it settled", id, ok, err)
	}
	r, err := n.Store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return r
}

// stampSpec stamps r with the digests of its spec as it now stands, as a decision does.
func stampSpec(t *testing.T, r *run.Run) {
	t.Helper()
	digest, err := outcome.SpecDigest(r)
	if err != nil {
		t.Fatalf("SpecDigest(%s) error = %v", r.ID, err)
	}
	b, err := outcome.SpecBinding(r)
	if err != nil {
		t.Fatalf("SpecBinding(%s) error = %v", r.ID, err)
	}
	r.ApprovedSpecDigest, r.ApprovedSpecBinding = digest, b
}

// parentRun stores a sealed running split or workflow parent of kind, approved when decided.
func (n *bndNode) parentRun(t *testing.T, id, kind string, decided bool) *run.Run {
	t.Helper()
	p := &run.Run{
		ID: id, Kind: kind, Playbook: "site.yml", Inventory: "web01\nweb02\n",
		Status: run.StatusRunning, ClaimedBy: "control-node", CreatedAt: time.Now(),
	}
	if !decided {
		n.seal(t, p, func() (binding.Record, error) { return binding.SubmitRecord(p) })
		n.save(t, p)
		return p
	}
	e := n.entry(t, audit.MethodDecision, "/runs/"+id+"/decision/"+outcome.StepApproved)
	p.Status, p.ClaimedBy = run.StatusPendingApproval, ""
	n.seal(t, p, func() (binding.Record, error) { return binding.RunRecord(p, binding.RootOf(e)) })
	n.save(t, p)
	return n.decide(t, id, e.ID, run.StatusRunning, "control-node")
}

// shard stores a sealed pending shard of parent.
func (n *bndNode) shard(t *testing.T, parent *run.Run, id string) *run.Run {
	t.Helper()
	index := 0
	c := &run.Run{
		ID: id, ParentID: &parent.ID, ShardIndex: &index, Playbook: parent.Playbook,
		Inventory: "web01\n", Status: run.StatusPending, CreatedAt: time.Now(),
	}
	n.seal(t, c, func() (binding.Record, error) { return binding.ShardRecord(parent, c) })
	n.save(t, c)
	return c
}

// step stores a sealed pending first step of the workflow parent. A decided parent releases it,
// so its launch is recorded in a RELEASE entry under the fixed id and its seal names both roots.
func (n *bndNode) step(t *testing.T, parent *run.Run, id string) *run.Run {
	t.Helper()
	index := 0
	c := &run.Run{
		ID: id, ParentID: &parent.ID, StepIndex: &index, StepName: "deploy", Playbook: "deploy.yml",
		Inventory: "web01\n", Status: run.StatusPending, CreatedAt: time.Now(),
	}
	var roots []binding.Root
	if parent.DecisionID != "" {
		decision, err := n.Audits.Entry(context.Background(), parent.DecisionID)
		if err != nil {
			t.Fatalf("Entry(%s) error = %v", parent.DecisionID, err)
		}
		release := &audit.Entry{
			ID:            audit.ReleaseID([]string{parent.DecisionID}, parent.ID, index, c.Attempt),
			Actor:         "system:dispatch",
			Method:        audit.MethodRelease,
			Path:          audit.ReleasePath(parent.ID, id),
			ContentDigest: "sha256:" + strings.Repeat("cd", 32),
		}
		if err := n.Audits.Append(context.Background(), release); err != nil {
			t.Fatalf("Append(release) error = %v", err)
		}
		held, err := n.Audits.Entry(context.Background(), release.ID)
		if err != nil {
			t.Fatalf("Entry(release) error = %v", err)
		}
		roots = []binding.Root{binding.RootOf(decision), binding.RootOf(held)}
	}
	n.seal(t, c, func() (binding.Record, error) { return binding.StepRecord(parent, c, roots) })
	n.save(t, c)
	return c
}

// claim asks the node for work as bndWorker would, returning the answer's status, headers, and
// body.
func (n *bndNode) claim(t *testing.T) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		n.URL+"/relay/v1/claim", strings.NewReader(`{"owner":"`+bndWorker+`","queues":[""]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("claim error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read claim answer: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// chainPaths returns the path of every entry on the node's chain.
func (n *bndNode) chainPaths(t *testing.T) []string {
	t.Helper()
	chain, err := n.Audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	paths := make([]string, 0, len(chain))
	for _, e := range chain {
		paths = append(paths, e.Path)
	}
	return paths
}

// TestClaimRefusesADriftedRun pins invariant 8's first half: the control node holds every run it
// leases to the seal this process made over it, before a worker is handed the run. A worker holds
// no key, and the binding header it checks is one a principal who can write the database rewrites
// along with the spec, so the control node's check is the one such a writer cannot pass.
//
// A refused run never leaves this side of the boundary. It is ended failed with the reason under
// the claim's owner, its outcome committed and the run announced when it is a top-level run, no
// claim is recorded, and the worker is answered as though nothing were pending. An honest run is
// handed over as before, with the binding header it always carried and no seal on the wire.
func TestClaimRefusesADriftedRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// Build stores the rows and makes the writer's move, returning the id the claim leases.
		Build func(t *testing.T, n *bndNode) string
		// FailEntries makes every chain read by id fail.
		FailEntries bool
		// WantError is the reason the run records, empty for a run handed to the worker.
		WantError string
	}{{ // Test 0: A submitted run nobody touched is handed over.
		Name: "honest submitted",
		Build: func(t *testing.T, n *bndNode) string {
			return n.submitted(t, "run_honest").ID
		},
	}, { // Test 1: A submitted run whose spec was rewritten before claim is refused.
		Name: "spec rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			n.submitted(t, "run_spec")
			n.rewrite(t, "run_spec", func(r *run.Run) { r.Playbook = "wipe.yml" })
			return "run_spec"
		},
		WantError: "refused: run run_spec: its spec or its approval fields changed after it " +
			"was submitted",
	}, { // Test 2: A submitted run whose approval field was cleared before claim is refused.
		Name: "governance rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			n.submitted(t, "run_gov")
			n.rewrite(t, "run_gov", func(r *run.Run) { r.RequireDistinctApprover = false })
			return "run_gov"
		},
		WantError: "refused: run run_gov: its spec or its approval fields changed after it " +
			"was submitted",
	}, { // Test 3: A run the writer inserted with no seal is refused.
		Name: "inserted unsealed",
		Build: func(t *testing.T, n *bndNode) string {
			n.save(t, &run.Run{ID: "run_bare", Playbook: "site.yml", Inventory: "web01\n",
				Status: run.StatusPending, CreatedAt: time.Now()})
			return "run_bare"
		},
		WantError: "refused: run run_bare: it carries no seal, so this process did not build " +
			"or release it",
	}, { // Test 4: A run the writer inserted under a seal copied from another run is refused.
		Name: "seal copied",
		Build: func(t *testing.T, n *bndNode) string {
			donor := n.submitted(t, "run_donor")
			n.rewrite(t, "run_donor", func(r *run.Run) { r.Status = run.StatusSucceeded })
			n.save(t, &run.Run{ID: "run_copy", Playbook: donor.Playbook, Inventory: donor.Inventory,
				Status: run.StatusPending, CreatedAt: time.Now(), Actor: donor.Actor,
				RequireDistinctApprover: true, SpecSeal: donor.SpecSeal})
			return "run_copy"
		},
		WantError: "refused: run run_copy: its spec or its approval fields changed after it " +
			"was submitted",
	}, { // Test 5: An approved run nobody touched is handed over with its binding header.
		Name: "honest approved",
		Build: func(t *testing.T, n *bndNode) string {
			return n.approved(t, "run_ok", nil).ID
		},
	}, { // Test 6: An approved run whose spec and both stamps were rewritten is refused.
		Name: "spec and stamps rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			n.approved(t, "run_stamps", nil)
			n.rewrite(t, "run_stamps", func(r *run.Run) {
				r.Inventory = "db01\ndb02\ndb03\n"
				stampSpec(t, r)
			})
			return "run_stamps"
		},
		WantError: "refused: run run_stamps: its spec, its approval fields, or its decision " +
			"changed after it was approved",
	}, { // Test 7: An approved run whose DECISION entry is not on the chain is refused.
		Name: "decision gone",
		Build: func(t *testing.T, n *bndNode) string {
			root := binding.Root{EntryID: "aud_gone", ContentDigest: "sha256:" +
				strings.Repeat("ef", 32), Path: "/runs/run_gone/decision/" + outcome.StepApproved}
			return n.approved(t, "run_gone", &root).ID
		},
		WantError: "refused: run run_gone: chain entry aud_gone it derives from is not on the chain",
	}, { // Test 8: A shard rewritten after its split was built is refused.
		Name: "shard rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			p := n.parentRun(t, "run_split", run.KindSplit, false)
			n.shard(t, p, "run_shard")
			n.rewrite(t, "run_shard", func(r *run.Run) { r.Inventory = "db01\n" })
			return "run_shard"
		},
		WantError: "refused: run run_shard: it or its split changed after the split was built",
	}, { // Test 9: A shard whose split was rewritten is refused on the split's seal.
		Name: "split rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			p := n.parentRun(t, "run_split", run.KindSplit, false)
			n.shard(t, p, "run_shard")
			n.rewrite(t, "run_split", func(r *run.Run) { r.Playbook = "wipe.yml" })
			return "run_shard"
		},
		WantError: "refused: run run_shard: parent run run_split: its spec or its approval " +
			"fields changed after it was submitted",
	}, { // Test 10: A step of an approved workflow, released on the chain, is handed over.
		Name: "honest released step",
		Build: func(t *testing.T, n *bndNode) string {
			p := n.parentRun(t, "run_flow", run.KindPipeline, true)
			return n.step(t, p, "run_step").ID
		},
	}, { // Test 11: A step of an approved workflow rewritten after it was built is refused.
		Name: "released step rewritten",
		Build: func(t *testing.T, n *bndNode) string {
			p := n.parentRun(t, "run_flow", run.KindPipeline, true)
			n.step(t, p, "run_step")
			n.rewrite(t, "run_step", func(r *run.Run) { r.Playbook = "wipe.yml" })
			return "run_step"
		},
		WantError: "refused: run run_step: it, its workflow, or the entries that released it " +
			"changed after it was built",
	}, { // Test 12: A chain that cannot be read is not tampering, and its error is not recorded.
		Name: "chain read fails",
		Build: func(t *testing.T, n *bndNode) string {
			return n.approved(t, "run_unread", nil).ID
		},
		FailEntries: true,
		WantError:   "could not verify: run run_unread: read chain entry ",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			n := newBndNode(t, bndConfig{FailEntries: test.FailEntries})
			id := test.Build(t, n)
			want, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", id, err)
			}
			status, header, body := n.claim(t)
			got, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%s) after claim error = %v", id, err)
			}
			claimed := slices.Contains(n.chainPaths(t), "/relay/claim/"+id)
			if test.WantError == "" {
				if status != http.StatusOK {
					t.Fatalf("claim answered %d %s, want the run handed over", status, body)
				}
				if !bytes.Contains(body, []byte(`"id":"`+id+`"`)) {
					t.Errorf("claim answered %s, want run %s", body, id)
				}
				if header.Get(leaseHeader) == "" {
					t.Error("a handed-over run carried no lease capability")
				}
				if diff := cmp.Diff(want.ApprovedSpecBinding, header.Get(bindingHeader)); diff != "" {
					t.Errorf("binding header mismatch (-want +got):\n%s", diff)
				}
				if bytes.Contains(body, []byte(want.SpecSeal)) {
					t.Error("the claim answer carried the run's seal, which never crosses the wire")
				}
				if got.Status != run.StatusPending || got.ClaimedBy != bndWorker {
					t.Errorf("handed-over run is %s held by %q, want pending held by %s",
						got.Status, got.ClaimedBy, bndWorker)
				}
				if !claimed {
					t.Error("the claim of a handed-over run was not recorded")
				}
				if diff := cmp.Diff([]string(nil), n.announcements(),
					cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("announcements mismatch (-want +got):\n%s", diff)
				}
				return
			}
			if status != http.StatusNoContent {
				t.Fatalf("claim answered %d %s, want 204 for a refused run", status, body)
			}
			if header.Get(leaseHeader) != "" || header.Get(bindingHeader) != "" {
				t.Error("a refused run's claim answer carried a lease or a binding")
			}
			if got.Status != run.StatusFailed {
				t.Errorf("refused run is %s, want failed", got.Status)
			}
			if !strings.HasPrefix(got.Error, test.WantError) {
				t.Errorf("refused run's error = %q, want it to start %q", got.Error, test.WantError)
			}
			if test.FailEntries && (strings.Contains(got.Error, "refused") ||
				strings.Contains(got.Error, "changed after")) {
				t.Errorf("a failed chain read recorded %q, which reads as tampering", got.Error)
			}
			if got.ClaimedBy != bndWorker || got.ClaimSecret != "" {
				t.Errorf("refused run ended held by %q with a live capability %t, want it ended "+
					"under %s's claim", got.ClaimedBy, got.ClaimSecret != "", bndWorker)
			}
			if claimed {
				t.Error("a refused run was recorded as claimed, but it never left the control node")
			}
			wantAnnounced := []string{id + " failed"}
			if got.ParentID != nil {
				wantAnnounced = nil
			}
			if diff := cmp.Diff(wantAnnounced, n.announcements(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("announcements mismatch (-want +got):\n%s", diff)
			}
			committed := slices.Contains(n.chainPaths(t), "/runs/"+id+"/outcome/failed")
			if committed != (got.ParentID == nil) {
				t.Errorf("outcome committed = %t, want %t: a top-level run's is committed and a "+
					"child's rolls into its parent", committed, got.ParentID == nil)
			}
			if got.ParentID != nil {
				parent, err := n.Store.Get(ctx, *got.ParentID)
				if err != nil {
					t.Fatalf("Get(parent) error = %v", err)
				}
				if parent.Status != run.StatusRunning {
					t.Errorf("parent is %s after its child was refused, want it left to its "+
						"coordinator", parent.Status)
				}
			}
		})
	}
}

// TestARefusalLostToACancelIsLeftToTheSweep pins the fence on a refusal. The control node ends a
// refused run through the claim it just made, so a cancel that lands between the lease and the
// refusal wins: the run is not ended failed over the person who canceled it, nothing is committed
// or announced, and the sweep settles the run as it settles any claim nobody started.
func TestARefusalLostToACancelIsLeftToTheSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newBndNode(t, bndConfig{CancelAfterClaim: true})
	n.submitted(t, "run_cancel")
	n.rewrite(t, "run_cancel", func(r *run.Run) { r.Playbook = "wipe.yml" })
	status, _, body := n.claim(t)
	if status != http.StatusNoContent {
		t.Fatalf("claim answered %d %s, want 204 for a refused run", status, body)
	}
	got, err := n.Store.Get(ctx, "run_cancel")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusPending || got.Error != "" || !got.CancelRequested {
		t.Errorf("run is %s with %q, cancel requested %t, want it pending and left to the sweep",
			got.Status, got.Error, got.CancelRequested)
	}
	for _, path := range n.chainPaths(t) {
		if strings.HasPrefix(path, "/runs/run_cancel/outcome/") || path == "/relay/claim/run_cancel" {
			t.Errorf("chain holds %s for a run that was neither handed over nor ended here", path)
		}
	}
	if diff := cmp.Diff([]string(nil), n.announcements(), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("announcements mismatch (-want +got):\n%s", diff)
	}
}

// TestFinishedRunCarriesAResultSeal pins invariant 8's second half: a worker holds no key, so the
// control node seals the result a shard or step reports, over the outputs the store holds, in the
// terminal write itself. A walk reading the step back holds its status and outputs to that seal,
// so a writer who rewrites them afterwards is refused. Only a succeeded child is sealed, and an
// unkeyed node seals nothing.
func TestFinishedRunCarriesAResultSeal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// Unkeyed stands the node up with no binding key.
		Unkeyed bool
		// Build stores the row a worker claims and returns its id.
		Build func(t *testing.T, n *bndNode) string
		// Status is how the worker reports the run ended.
		Status run.Status
		// WantSealed is whether the finished row carries a result seal.
		WantSealed bool
	}{{ // Test 0: A shard that succeeded is sealed, outputs a store must clean included.
		Name: "succeeded shard",
		Build: func(t *testing.T, n *bndNode) string {
			return n.shard(t, n.parentRun(t, "run_split", run.KindSplit, false), "run_shard").ID
		},
		Status: run.StatusSucceeded, WantSealed: true,
	}, { // Test 1: A released workflow step that succeeded is sealed.
		Name: "succeeded step",
		Build: func(t *testing.T, n *bndNode) string {
			return n.step(t, n.parentRun(t, "run_flow", run.KindPipeline, true), "run_step").ID
		},
		Status: run.StatusSucceeded, WantSealed: true,
	}, { // Test 2: A shard that failed feeds nothing forward and is not sealed.
		Name: "failed shard",
		Build: func(t *testing.T, n *bndNode) string {
			return n.shard(t, n.parentRun(t, "run_split", run.KindSplit, false), "run_shard").ID
		},
		Status: run.StatusFailed,
	}, { // Test 3: A top-level run feeds no walk and is not sealed.
		Name: "succeeded top-level run",
		Build: func(t *testing.T, n *bndNode) string {
			return n.submitted(t, "run_top").ID
		},
		Status: run.StatusSucceeded,
	}, { // Test 4: A node with no key seals nothing.
		Name:    "unkeyed node",
		Unkeyed: true,
		Build: func(t *testing.T, n *bndNode) string {
			index := 0
			p := &run.Run{ID: "run_split", Kind: run.KindSplit, Playbook: "site.yml",
				Status: run.StatusRunning, ClaimedBy: "control-node", CreatedAt: time.Now()}
			n.save(t, p)
			n.save(t, &run.Run{ID: "run_shard", ParentID: &p.ID, ShardIndex: &index,
				Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now()})
			return "run_shard"
		},
		Status: run.StatusSucceeded,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			n := newBndNode(t, bndConfig{Unkeyed: test.Unkeyed})
			id := test.Build(t, n)
			c := NewClient(NewHTTPTransport(n.URL, "tok", nil))
			leased, err := c.Claim(ctx, bndWorker, []string{""})
			if err != nil {
				t.Fatalf("Claim() error = %v", err)
			}
			if leased.ID != id {
				t.Fatalf("Claim() leased %s, want %s", leased.ID, id)
			}
			code := 0
			leased.Status, leased.ExitCode = test.Status, &code
			// A NUL is legal in a JSON string and refused by a text column, so the store keeps a
			// replacement in its place. The seal has to cover what the store keeps.
			leased.Outputs = map[string]any{"version": "1.4.2", "note": "built\x00ok",
				"hosts": []any{"web01"}}
			if err := c.Save(ctx, leased); err != nil {
				t.Fatalf("Save(%s) error = %v", test.Status, err)
			}
			got, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", id, err)
			}
			if got.Status != test.Status {
				t.Fatalf("finished run is %s, want %s", got.Status, test.Status)
			}
			if sealed := got.ResultSeal != ""; sealed != test.WantSealed {
				t.Fatalf("finished run sealed = %t, want %t", sealed, test.WantSealed)
			}
			if !test.WantSealed {
				return
			}
			if err := n.Binder.Result(ctx, got); err != nil {
				t.Fatalf("Result() of the run as the control node finished it = %v, want nil", err)
			}
			n.rewrite(t, id, func(r *run.Run) { r.Outputs["version"] = "6.6.6" })
			forged, err := n.Store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get(%s) error = %v", id, err)
			}
			err = n.Binder.Result(ctx, forged)
			if !errors.Is(err, binding.ErrDrift) {
				t.Fatalf("Result() of rewritten outputs = %v, want %v", err, binding.ErrDrift)
			}
			want := "refused: run " + id + ": its status or its outputs changed after it finished"
			if diff := cmp.Diff(want, dispatch.RefusalText(err)); diff != "" {
				t.Errorf("refusal mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestARelayProposalIsSealed pins the plan gate's proposal on a relay worker. The control node
// builds the apply a worker's plan proposes, so it seals the proposal as a submission is sealed,
// after the rules set its approval fields, and the apply it queues is one the next claim hands
// over. A proposal whose approval fields a writer clears is refused at claim, and a node with no
// key seals nothing.
func TestARelayProposalIsSealed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		// Unkeyed stands the node up with no binding key.
		Unkeyed bool
		// Destroys is what the plan reports it would destroy, over the limit holds the apply.
		Destroys int
		// WantStatus is the proposal's status.
		WantStatus run.Status
	}{{ // Test 0: An apply within the limit is sealed and queued.
		Name: "queued", Destroys: 0, WantStatus: run.StatusPending,
	}, { // Test 1: An apply over the limit is sealed and held.
		Name: "held", Destroys: 3, WantStatus: run.StatusPendingApproval,
	}, { // Test 2: A node with no key proposes an unsealed apply.
		Name: "unkeyed", Unkeyed: true, Destroys: 0, WantStatus: run.StatusPending,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			policies := policy.NewMemStore()
			if err := policies.Save(ctx, &policy.Policy{
				ID: "pol_1", Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1,
			}); err != nil {
				t.Fatalf("Save policy: %v", err)
			}
			n := newBndNode(t, bndConfig{Unkeyed: test.Unkeyed}, WithPlanSealer(testPlanSealer{}),
				withPolicies(policies))
			plan := &run.Run{
				ID: "run_plan", Status: run.StatusPending, CreatedAt: time.Now(),
				Tool: run.ToolTerraform, Command: "infra/prod", Actor: "casey", OrgID: "org_1",
				CommitSHA: "abc123", ProjectID: "prj_1",
			}
			if !test.Unkeyed {
				n.seal(t, plan, func() (binding.Record, error) { return binding.SubmitRecord(plan) })
			}
			n.save(t, plan)
			c := NewClient(NewHTTPTransport(n.URL, "tok", nil))
			if _, err := c.Claim(ctx, bndWorker, []string{""}); err != nil {
				t.Fatalf("Claim(plan) error = %v", err)
			}
			proposal, err := c.ProposeApply(ctx, plan.ID, test.Destroys, true, testPlanFile)
			if err != nil {
				t.Fatalf("ProposeApply() error = %v", err)
			}
			stored, err := n.Store.Get(ctx, proposal.ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			if stored.Status != test.WantStatus {
				t.Fatalf("proposal is %s, want %s", stored.Status, test.WantStatus)
			}
			if test.Unkeyed {
				if stored.SpecSeal != "" {
					t.Error("a node with no key sealed the proposal")
				}
				return
			}
			if err := n.Binder.Executable(ctx, stored); err != nil {
				t.Fatalf("Executable(proposal) = %v, want nil", err)
			}
			if test.WantStatus == run.StatusPending {
				leased, err := c.Claim(ctx, bndWorker, []string{""})
				if err != nil {
					t.Fatalf("Claim(proposal) error = %v, want the apply handed over", err)
				}
				if leased.ID != proposal.ID {
					t.Errorf("Claim() leased %s, want the proposal %s", leased.ID, proposal.ID)
				}
				return
			}
			n.rewrite(t, proposal.ID, func(r *run.Run) {
				r.Status, r.HeldByPolicy, r.RequireDistinctApprover = run.StatusPending, "", false
			})
			if _, err := c.Claim(ctx, bndWorker, []string{""}); !errors.Is(err, run.ErrNonePending) {
				t.Fatalf("Claim(released by the writer) error = %v, want %v", err,
					run.ErrNonePending)
			}
			got, err := n.Store.Get(ctx, proposal.ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			want := "refused: run " + proposal.ID + ": its spec or its approval fields changed " +
				"after it was submitted"
			if got.Status != run.StatusFailed || got.Error != want {
				t.Errorf("proposal released by the writer is %s with %q, want failed with %q",
					got.Status, got.Error, want)
			}
		})
	}
}

// withPolicies sets the approval policies a node reads, the store NewHandler takes as an argument.
func withPolicies(p policy.Store) HandlerOption {
	return func(s *relayServer) { s.policies = p }
}
