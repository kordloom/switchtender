package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// sealPattern is the stored form of every seal.
var sealPattern = regexp.MustCompile(`^bk1:[0-9a-f]{64}$`)

// testFullRecord returns a record with every field set to a value other than its zero, so changing
// any one of them is a change the seal must notice.
func testFullRecord() Record {
	return Record{
		Kind: KindStep, RunID: "run_c", RunKind: run.KindPipeline, ParentID: "run_p", Index: 2,
		Attempt: 1, SpecBinding: "sha256:aa", ParentBinding: "sha256:bb", ApprovalRequested: true,
		HeldByPolicy: "prod changes", RequireDistinctApprover: true,
		PolicySet: &PolicySet{Digest: "pd", Count: 2, Rules: []string{"a", "b"}},
		Roots: []Root{
			{EntryID: "aud_1", ContentDigest: "cd1", Path: "/runs/run_p/decision/approved"},
			{EntryID: "aud_2", ContentDigest: "cd2", Path: "/runs/run_p/steps/run_c/release"},
		},
		Status: string(run.StatusSucceeded), OutputsDigest: "sha256:cc",
		Facts: testFullFacts(), ApprovedSpecDigest: "sha256:dd", ApprovedSpecBinding: "sha256:ee",
	}
}

// testFullFacts returns facts with every field set to a value other than its zero.
func testFullFacts() Facts {
	destroys := 2
	return Facts{
		Actor: "dev", ActorType: "token", ActorUserID: "usr_1", Account: "deploy-bot",
		Initiator: Initiator{InitiatedBy: "dev", CredentialID: "cred_1", BoundTo: "agent",
			ProvisionedBy: "admin", ProvisionedByType: "session"},
		Queue: "edge", Labels: []Label{{Key: "env", Value: "prod"}, {Key: "team", Value: "net"}},
		Reversibility: &Reversibility{Class: "reversible", Reasons: []string{"undo"}},
		ProposedFrom:  "run_plan", TemplateID: "tpl_1", Source: "schedule", SourceID: "sch_1",
		Intent: "rotate certs", StepName: "deploy", RequireReason: "always", CreatedAt: 1760000000,
		PlanDestroys: &destroys, DryRunScans: `[{"tool":"ansible"}]`, OrgID: "org_1",
		GitRef: "refs/pull/7/head",
	}
}

// TestVerifyRefusesEveryChangedField changes each field of a record, and each field of the structs
// it holds, one at a time, and requires the seal over the original to refuse every one. A guard
// fails the test when Record, Root, or PolicySet gains a field this table does not change.
func TestVerifyRefusesEveryChangedField(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	tests := []struct {
		Mutate func(*Record)
		Field  string
	}{{ // Test 0: Kind.
		Field: "Kind", Mutate: func(r *Record) { r.Kind = KindShard },
	}, { // Test 1: RunID.
		Field: "RunID", Mutate: func(r *Record) { r.RunID = "run_d" },
	}, { // Test 2: RunKind.
		Field: "RunKind", Mutate: func(r *Record) { r.RunKind = "" },
	}, { // Test 3: ParentID.
		Field: "ParentID", Mutate: func(r *Record) { r.ParentID = "run_q" },
	}, { // Test 4: Index.
		Field: "Index", Mutate: func(r *Record) { r.Index = 3 },
	}, { // Test 5: Attempt.
		Field: "Attempt", Mutate: func(r *Record) { r.Attempt = 0 },
	}, { // Test 6: SpecBinding.
		Field: "SpecBinding", Mutate: func(r *Record) { r.SpecBinding = "sha256:ab" },
	}, { // Test 7: ParentBinding.
		Field: "ParentBinding", Mutate: func(r *Record) { r.ParentBinding = "" },
	}, { // Test 8: ApprovalRequested.
		Field: "ApprovalRequested", Mutate: func(r *Record) { r.ApprovalRequested = false },
	}, { // Test 9: HeldByPolicy.
		Field: "HeldByPolicy", Mutate: func(r *Record) { r.HeldByPolicy = "" },
	}, { // Test 10: RequireDistinctApprover.
		Field:  "RequireDistinctApprover",
		Mutate: func(r *Record) { r.RequireDistinctApprover = false },
	}, { // Test 11: PolicySet removed.
		Field: "PolicySet", Mutate: func(r *Record) { r.PolicySet = nil },
	}, { // Test 12: PolicySet digest.
		Field: "PolicySet.Digest", Mutate: func(r *Record) { r.PolicySet.Digest = "pe" },
	}, { // Test 13: PolicySet count.
		Field: "PolicySet.Count", Mutate: func(r *Record) { r.PolicySet.Count = 1 },
	}, { // Test 14: PolicySet rules.
		Field: "PolicySet.Rules", Mutate: func(r *Record) { r.PolicySet.Rules = []string{"a"} },
	}, { // Test 15: A root dropped.
		Field: "Roots", Mutate: func(r *Record) { r.Roots = r.Roots[:1] },
	}, { // Test 16: Roots reordered.
		Field: "Roots", Mutate: func(r *Record) { slices.Reverse(r.Roots) },
	}, { // Test 17: A root's entry id.
		Field: "Roots.EntryID", Mutate: func(r *Record) { r.Roots[0].EntryID = "aud_9" },
	}, { // Test 18: A root digest that differs.
		Field: "Roots.ContentDigest", Mutate: func(r *Record) { r.Roots[1].ContentDigest = "cd3" },
	}, { // Test 19: A root's path.
		Field: "Roots.Path", Mutate: func(r *Record) {
			r.Roots[0].Path = "/runs/run_p/decision/rejected"
		},
	}, { // Test 20: Status.
		Field: "Status", Mutate: func(r *Record) { r.Status = string(run.StatusFailed) },
	}, { // Test 21: OutputsDigest.
		Field: "OutputsDigest", Mutate: func(r *Record) { r.OutputsDigest = "sha256:cd" },
	}, { // Test 22: Facts cleared.
		Field: "Facts", Mutate: func(r *Record) { r.Facts = Facts{} },
	}, { // Test 23: ApprovedSpecDigest.
		Field: "ApprovedSpecDigest", Mutate: func(r *Record) { r.ApprovedSpecDigest = "" },
	}, { // Test 24: ApprovedSpecBinding.
		Field: "ApprovedSpecBinding", Mutate: func(r *Record) { r.ApprovedSpecBinding = "sha256:ef" },
	}, { // Test 25: Actor.
		Field: "Facts.Actor", Mutate: func(r *Record) { r.Facts.Actor = "ops" },
	}, { // Test 26: ActorType.
		Field: "Facts.ActorType", Mutate: func(r *Record) { r.Facts.ActorType = "agent" },
	}, { // Test 27: ActorUserID.
		Field: "Facts.ActorUserID", Mutate: func(r *Record) { r.Facts.ActorUserID = "usr_2" },
	}, { // Test 28: Account.
		Field: "Facts.Account", Mutate: func(r *Record) { r.Facts.Account = "" },
	}, { // Test 29: Initiator cleared.
		Field: "Facts.Initiator", Mutate: func(r *Record) { r.Facts.Initiator = Initiator{} },
	}, { // Test 30: Initiator initiated by.
		Field:  "Facts.Initiator.InitiatedBy",
		Mutate: func(r *Record) { r.Facts.Initiator.InitiatedBy = "ops" },
	}, { // Test 31: Initiator credential.
		Field:  "Facts.Initiator.CredentialID",
		Mutate: func(r *Record) { r.Facts.Initiator.CredentialID = "cred_2" },
	}, { // Test 32: Initiator bound to.
		Field: "Facts.Initiator.BoundTo", Mutate: func(r *Record) { r.Facts.Initiator.BoundTo = "" },
	}, { // Test 33: Initiator provisioned by.
		Field:  "Facts.Initiator.ProvisionedBy",
		Mutate: func(r *Record) { r.Facts.Initiator.ProvisionedBy = "dev" },
	}, { // Test 34: Initiator provisioned by type.
		Field:  "Facts.Initiator.ProvisionedByType",
		Mutate: func(r *Record) { r.Facts.Initiator.ProvisionedByType = "token" },
	}, { // Test 35: Queue.
		Field: "Facts.Queue", Mutate: func(r *Record) { r.Facts.Queue = "" },
	}, { // Test 36: A label dropped.
		Field: "Facts.Labels", Mutate: func(r *Record) { r.Facts.Labels = r.Facts.Labels[:1] },
	}, { // Test 37: A label's key.
		Field: "Facts.Labels.Key", Mutate: func(r *Record) { r.Facts.Labels[0].Key = "stage" },
	}, { // Test 38: A label's value.
		Field: "Facts.Labels.Value", Mutate: func(r *Record) { r.Facts.Labels[1].Value = "db" },
	}, { // Test 39: Reversibility removed.
		Field: "Facts.Reversibility", Mutate: func(r *Record) { r.Facts.Reversibility = nil },
	}, { // Test 40: Reversibility class.
		Field:  "Facts.Reversibility.Class",
		Mutate: func(r *Record) { r.Facts.Reversibility.Class = "irreversible" },
	}, { // Test 41: Reversibility reasons.
		Field:  "Facts.Reversibility.Reasons",
		Mutate: func(r *Record) { r.Facts.Reversibility.Reasons = nil },
	}, { // Test 42: ProposedFrom.
		Field: "Facts.ProposedFrom", Mutate: func(r *Record) { r.Facts.ProposedFrom = "" },
	}, { // Test 43: TemplateID.
		Field: "Facts.TemplateID", Mutate: func(r *Record) { r.Facts.TemplateID = "tpl_2" },
	}, { // Test 44: Source.
		Field: "Facts.Source", Mutate: func(r *Record) { r.Facts.Source = "api" },
	}, { // Test 45: SourceID.
		Field: "Facts.SourceID", Mutate: func(r *Record) { r.Facts.SourceID = "" },
	}, { // Test 46: Intent.
		Field: "Facts.Intent", Mutate: func(r *Record) { r.Facts.Intent = "nothing" },
	}, { // Test 47: StepName.
		Field: "Facts.StepName", Mutate: func(r *Record) { r.Facts.StepName = "verify" },
	}, { // Test 48: RequireReason.
		Field: "Facts.RequireReason", Mutate: func(r *Record) { r.Facts.RequireReason = "" },
	}, { // Test 49: CreatedAt a second earlier.
		Field: "Facts.CreatedAt", Mutate: func(r *Record) { r.Facts.CreatedAt-- },
	}, { // Test 50: PlanDestroys removed.
		Field: "Facts.PlanDestroys", Mutate: func(r *Record) { r.Facts.PlanDestroys = nil },
	}, { // Test 51: PlanDestroys lowered.
		Field: "Facts.PlanDestroys", Mutate: func(r *Record) {
			zero := 0
			r.Facts.PlanDestroys = &zero
		},
	}, { // Test 52: DryRunScans.
		Field: "Facts.DryRunScans", Mutate: func(r *Record) { r.Facts.DryRunScans = "" },
	}, { // Test 53: OrgID.
		Field: "Facts.OrgID", Mutate: func(r *Record) { r.Facts.OrgID = "org_2" },
	}, { // Test 54: GitRef.
		Field: "Facts.GitRef", Mutate: func(r *Record) { r.Facts.GitRef = "refs/heads/main" },
	}}
	covered := map[string]bool{}
	for _, test := range tests {
		covered[test.Field] = true
	}
	prefixes := map[reflect.Type]string{
		reflect.TypeFor[Record](): "", reflect.TypeFor[Root](): "Roots.",
		reflect.TypeFor[PolicySet](): "PolicySet.", reflect.TypeFor[Facts](): "Facts.",
		reflect.TypeFor[Initiator](): "Facts.Initiator.", reflect.TypeFor[Label](): "Facts.Labels.",
		reflect.TypeFor[Reversibility](): "Facts.Reversibility.",
	}
	for typ, prefix := range prefixes {
		for i := range typ.NumField() {
			name := prefix + typ.Field(i).Name
			if !covered[name] {
				t.Errorf("field %s is changed by no row of this table", name)
			}
		}
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			stored := k.Seal(testFullRecord())
			if !k.Verify(testFullRecord(), stored) {
				t.Fatal("the unchanged record does not verify")
			}
			changed := testFullRecord()
			test.Mutate(&changed)
			if k.Verify(changed, stored) {
				t.Errorf("a record with %s changed verifies", test.Field)
			}
		})
	}
}

// TestVerifyRefusesAWrongKeyOrForm pins that a seal verifies only under the key that made it and
// only in its exact stored form.
func TestVerifyRefusesAWrongKeyOrForm(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	other := testKey(t, 2)
	rec := testFullRecord()
	good := k.Seal(rec)
	hexPart := strings.TrimPrefix(good, SealPrefix)
	tests := []struct {
		Key    *Key
		Stored string
		Want   bool
	}{{ // Test 0: The key and form that made it.
		Key: k, Stored: good, Want: true,
	}, { // Test 1: A wrong key.
		Key: other, Stored: good, Want: false,
	}, { // Test 2: A seal made under a wrong key.
		Key: k, Stored: other.Seal(rec), Want: false,
	}, { // Test 3: A wrong prefix.
		Key: k, Stored: "bk2:" + hexPart, Want: false,
	}, { // Test 4: No prefix.
		Key: k, Stored: hexPart, Want: false,
	}, { // Test 5: Upper-case hex.
		Key: k, Stored: SealPrefix + strings.ToUpper(hexPart), Want: false,
	}, { // Test 6: Truncated.
		Key: k, Stored: good[:len(good)-1], Want: false,
	}, { // Test 7: Extended.
		Key: k, Stored: good + "0", Want: false,
	}, { // Test 8: Empty.
		Key: k, Stored: "", Want: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Key.Verify(rec, test.Stored); got != test.Want {
				t.Errorf("Verify = %t, want %t", got, test.Want)
			}
		})
	}
}

// TestVerifyClaimRefusesAnyChange pins that a claim seal verifies only for the exact claim bytes,
// on the run, and under the key that made it, and that a claim seal and a row seal never stand for
// each other.
func TestVerifyClaimRefusesAnyChange(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	claim := []byte(`{"id":"dec_1","verdict":"approved","entry":{"actor":"approver"}}`)
	good := k.SealClaim("run_a", claim)
	if !sealPattern.MatchString(good) {
		t.Fatalf("claim seal %q is not in the stored form", good)
	}
	tests := []struct {
		Key    *Key
		RunID  string
		Claim  []byte
		Stored string
		Want   bool
	}{{ // Test 0: The claim, run, and key that made it.
		Key: k, RunID: "run_a", Claim: claim, Stored: good, Want: true,
	}, { // Test 1: The same claim stored on another run.
		Key: k, RunID: "run_b", Claim: claim, Stored: good,
	}, { // Test 2: The approver rewritten.
		Key: k, RunID: "run_a", Stored: good,
		Claim: []byte(`{"id":"dec_1","verdict":"approved","entry":{"actor":"mallory"}}`),
	}, { // Test 3: The same claim with a byte added.
		Key: k, RunID: "run_a", Claim: append(append([]byte(nil), claim...), ' '), Stored: good,
	}, { // Test 4: A wrong key.
		Key: testKey(t, 2), RunID: "run_a", Claim: claim, Stored: good,
	}, { // Test 5: A row seal offered as the claim seal.
		Key: k, RunID: "run_a", Claim: claim,
		Stored: k.Seal(Record{Kind: KindClaim, RunID: "run_a"}),
	}, { // Test 6: No seal.
		Key: k, RunID: "run_a", Claim: claim, Stored: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Key.VerifyClaim(test.RunID, test.Claim, test.Stored); got != test.Want {
				t.Errorf("VerifyClaim = %t, want %t", got, test.Want)
			}
		})
	}
	if k.Verify(Record{Kind: KindClaim, RunID: "run_a"}, good) {
		t.Error("a claim seal verified as a row seal")
	}
}

// TestSealIsDeterministic pins the stored form and that one record under one key always seals the
// same, and that an empty slice seals the same as a nil one, since a store round trip does not keep
// the difference.
func TestSealIsDeterministic(t *testing.T) {
	t.Parallel()
	k := testKey(t, 4)
	tests := []struct {
		A, B      Record
		WantEqual bool
	}{{ // Test 0: The same record twice.
		A: testFullRecord(), B: testFullRecord(), WantEqual: true,
	}, { // Test 1: Nil and empty roots.
		A: Record{Kind: KindSubmit, Roots: nil}, B: Record{Kind: KindSubmit, Roots: []Root{}},
		WantEqual: true,
	}, { // Test 2: Nil and empty rule names.
		A: Record{PolicySet: &PolicySet{Digest: "d"}},
		B: Record{PolicySet: &PolicySet{Digest: "d", Rules: []string{}}}, WantEqual: true,
	}, { // Test 3: No rule set and an empty one differ.
		A: Record{}, B: Record{PolicySet: &PolicySet{}}, WantEqual: false,
	}, { // Test 4: Two kinds over otherwise equal records differ.
		A: Record{Kind: KindSubmit}, B: Record{Kind: KindRun}, WantEqual: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, b := k.Seal(test.A), k.Seal(test.B)
			for _, s := range []string{a, b} {
				if !sealPattern.MatchString(s) {
					t.Errorf("seal %q is not in the stored form", s)
				}
			}
			if got := a == b; got != test.WantEqual {
				t.Errorf("seals equal = %t, want %t", got, test.WantEqual)
			}
		})
	}
}

// TestRecordBuilders pins the record each builder produces for every kind of row, from the row's
// own fields, its parent's binding, and the roots it is given.
func TestRecordBuilders(t *testing.T) {
	t.Parallel()
	pid := "run_p"
	two, zero := 2, 0
	parent := &run.Run{
		ID: pid, Kind: run.KindPipeline, Playbook: "site.yml", HeldByPolicy: "prod changes",
		RequireDistinctApprover: true, DecisionID: "aud_d",
		PolicySet: &run.PolicySet{Digest: "pd", Count: 1, Rules: []string{"prod changes"}},
	}
	step := &run.Run{
		ID: "run_s", ParentID: &pid, StepIndex: &two, Attempt: 1, Playbook: "deploy.yml",
		Status: run.StatusSucceeded, Outputs: map[string]any{"version": "1.2"},
	}
	shard := &run.Run{ID: "run_h", Kind: "", ParentID: &pid, ShardIndex: &zero, Limit: "web1"}
	node := &run.Run{ID: "run_g", Kind: run.KindApproval, ParentID: &pid, StepIndex: &two,
		HeldByPolicy: `approval step "gate"`, RequireDistinctApprover: true}
	root := Root{EntryID: "aud_d", ContentDigest: "cd", Path: "/runs/run_p/decision/approved"}
	bind := func(r *run.Run) string {
		b, err := outcome.SpecBinding(r)
		if err != nil {
			t.Fatalf("bind %s: %v", r.ID, err)
		}
		return b
	}
	outputs, err := OutputsDigest(step.Outputs)
	if err != nil {
		t.Fatalf("outputs digest: %v", err)
	}
	parentGov := Record{
		RunID: pid, RunKind: run.KindPipeline, Index: noIndex, SpecBinding: bind(parent),
		HeldByPolicy: "prod changes", RequireDistinctApprover: true,
		PolicySet: &PolicySet{Digest: "pd", Count: 1, Rules: []string{"prod changes"}},
	}
	tests := []struct {
		Build      func() (Record, error)
		WantRecord Record
	}{{ // Test 0: Submit.
		Build: func() (Record, error) { return SubmitRecord(parent) },
		WantRecord: func() Record {
			r := parentGov
			r.Kind = KindSubmit
			return r
		}(),
	}, { // Test 1: Run.
		Build: func() (Record, error) { return RunRecord(parent, root) },
		WantRecord: func() Record {
			r := parentGov
			r.Kind, r.Roots = KindRun, []Root{root}
			return r
		}(),
	}, { // Test 2: Gate.
		Build: func() (Record, error) { return GateRecord(parent, node, "sha256:state", root) },
		WantRecord: Record{
			Kind: KindGate, RunID: "run_g", RunKind: run.KindApproval, ParentID: pid, Index: 2,
			SpecBinding: "sha256:state", ParentBinding: bind(parent),
			HeldByPolicy: `approval step "gate"`, RequireDistinctApprover: true,
			Roots: []Root{root},
		},
	}, { // Test 3: Shard.
		Build: func() (Record, error) { return ShardRecord(parent, shard) },
		WantRecord: Record{
			Kind: KindShard, RunID: "run_h", ParentID: pid, Index: 0, SpecBinding: bind(shard),
			ParentBinding: bind(parent),
		},
	}, { // Test 4: Step.
		Build: func() (Record, error) { return StepRecord(parent, step, []Root{root}) },
		WantRecord: Record{
			Kind: KindStep, RunID: "run_s", ParentID: pid, Index: 2, Attempt: 1,
			SpecBinding: bind(step), ParentBinding: bind(parent), Roots: []Root{root},
		},
	}, { // Test 5: Result.
		Build: func() (Record, error) { return ResultRecord(step) },
		WantRecord: Record{
			Kind: KindResult, RunID: "run_s", ParentID: pid, Index: 2, Attempt: 1,
			Status: string(run.StatusSucceeded), OutputsDigest: outputs,
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := test.Build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if diff := cmp.Diff(test.WantRecord, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("record mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBuildersRefuseAForeignChild pins that a child is sealed only against the parent it names.
func TestBuildersRefuseAForeignChild(t *testing.T) {
	t.Parallel()
	other := "run_other"
	one := 1
	parent := &run.Run{ID: "run_p", Kind: run.KindPipeline}
	foreign := &run.Run{ID: "run_c", ParentID: &other, StepIndex: &one}
	orphan := &run.Run{ID: "run_o", StepIndex: &one}
	tests := []struct {
		Build func() (Record, error)
		Want  error
	}{{ // Test 0: A gate of another workflow.
		Build: func() (Record, error) { return GateRecord(parent, foreign, "s", Root{}) },
		Want:  ErrParent,
	}, { // Test 1: A shard of another split.
		Build: func() (Record, error) { return ShardRecord(parent, foreign) }, Want: ErrParent,
	}, { // Test 2: A step of another workflow.
		Build: func() (Record, error) { return StepRecord(parent, foreign, nil) }, Want: ErrParent,
	}, { // Test 3: A row with no parent.
		Build: func() (Record, error) { return StepRecord(parent, orphan, nil) }, Want: ErrParent,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := test.Build(); !errors.Is(err, test.Want) {
				t.Errorf("error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestStepRecordKeepsItsOwnRoots pins that a caller reusing its roots slice cannot change a record
// already built from it.
func TestStepRecordKeepsItsOwnRoots(t *testing.T) {
	t.Parallel()
	pid := "run_p"
	one := 1
	parent := &run.Run{ID: pid, Kind: run.KindPipeline}
	child := &run.Run{ID: "run_c", ParentID: &pid, StepIndex: &one}
	roots := []Root{{EntryID: "aud_1"}}
	rec, err := StepRecord(parent, child, roots)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	roots[0].EntryID = "aud_2"
	if diff := cmp.Diff([]Root{{EntryID: "aud_1"}}, rec.Roots); diff != "" {
		t.Errorf("roots mismatch (-want +got):\n%s", diff)
	}
}

// storeRoundTrip returns a copy of r whose extra vars went through the JSON round trip a SQL store
// applies when it writes a row and reads it back.
func storeRoundTrip(t *testing.T, r *run.Run) *run.Run {
	t.Helper()
	raw, err := json.Marshal(r.ExtraVars)
	if err != nil {
		t.Fatalf("encode extra vars: %v", err)
	}
	var vars map[string]any
	if err := json.Unmarshal(raw, &vars); err != nil {
		t.Fatalf("decode extra vars: %v", err)
	}
	stored := r.Clone()
	stored.ExtraVars = vars
	return stored
}

// sealedZone is a value a caller can put in extra vars that a store hands back as a map, its keys
// in another order than the struct declares them.
type sealedZone struct {
	// Zone names the zone.
	Zone string
	// Host names the host.
	Host string
}

// TestASealSurvivesAStoreRoundTrip pins that a row sealed in memory verifies once a store has
// handed its extra vars back through JSON, which turns an integer past 2^53 into the nearest float
// and a struct into a map, and that a change which survives the round trip still moves the seal. A
// child's record binds its parent by the same rule. The run sealed keeps the vars it was given.
func TestASealSurvivesAStoreRoundTrip(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	pid, zero := "run_p", 0
	shard := &run.Run{ID: "run_h", ParentID: &pid, ShardIndex: &zero, Limit: "web1"}
	submit := func(r *run.Run) (Record, error) { return SubmitRecord(r) }
	sharded := func(r *run.Run) (Record, error) { return ShardRecord(r, shard) }
	past := int64(1)<<53 + 1
	tests := []struct {
		// Seal builds the record over the run carrying the vars.
		Seal func(r *run.Run) (Record, error)
		// Vars are the run's extra vars in memory.
		Vars map[string]any
		// Edit is a writer's change to the row the store hands back, nil for none.
		Edit func(stored *run.Run)
		// WantVerified is whether the seal made in memory verifies over the stored row.
		WantVerified bool
	}{{ // Test 0: An integer past 2^53 on a submitted run.
		Seal: submit, Vars: map[string]any{"build": past}, WantVerified: true,
	}, { // Test 1: An integer past 2^53 on a split, bound by its shard.
		Seal: sharded, Vars: map[string]any{"build": past}, WantVerified: true,
	}, { // Test 2: A struct, which a store hands back as a map.
		Seal: submit, Vars: map[string]any{"zone": sealedZone{Zone: "b", Host: "web1"}},
		WantVerified: true,
	}, { // Test 3: A changed value that survives the round trip.
		Seal: submit, Vars: map[string]any{"build": past, "target": "web1"},
		Edit:         func(s *run.Run) { s.ExtraVars["target"] = "everywhere" },
		WantVerified: false,
	}, { // Test 4: The integer changed to one that survives the round trip as a different float.
		Seal: submit, Vars: map[string]any{"build": past},
		Edit:         func(s *run.Run) { s.ExtraVars["build"] = float64(past + 3) },
		WantVerified: false,
	}, { // Test 5: A split's vars changed under its shard.
		Seal: sharded, Vars: map[string]any{"build": past, "target": "web1"},
		Edit:         func(s *run.Run) { s.ExtraVars["target"] = "everywhere" },
		WantVerified: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{ID: pid, Kind: run.KindSplit, Playbook: "site.yml",
				ExtraVars: maps.Clone(test.Vars)}
			rec, err := test.Seal(r)
			if err != nil {
				t.Fatalf("seal in memory: %v", err)
			}
			sealed := k.Seal(rec)
			stored := storeRoundTrip(t, r)
			if test.Edit != nil {
				test.Edit(stored)
			}
			got, err := test.Seal(stored)
			if err != nil {
				t.Fatalf("rebuild from the store: %v", err)
			}
			if verified := k.Verify(got, sealed); verified != test.WantVerified {
				t.Errorf("Verify = %t, want %t", verified, test.WantVerified)
			}
			if diff := cmp.Diff(test.Vars, r.ExtraVars); diff != "" {
				t.Errorf("the sealed run's vars changed (-want +got):\n%s", diff)
			}
		})
	}
}

// TestOutputsDigest pins that outputs digest the same in memory and after a store round trip, and
// that different outputs digest differently.
func TestOutputsDigest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		A, B      map[string]any
		WantEqual bool
	}{{ // Test 0: Nil and empty.
		A: nil, B: map[string]any{}, WantEqual: true,
	}, { // Test 1: An integer and the float a store reads it back as.
		A: map[string]any{"n": 1}, B: map[string]any{"n": float64(1)}, WantEqual: true,
	}, { // Test 2: Key order does not matter.
		A: map[string]any{"a": 1, "b": map[string]any{"x": "y", "c": true}},
		B: map[string]any{"b": map[string]any{"c": true, "x": "y"}, "a": 1}, WantEqual: true,
	}, { // Test 3: A changed value.
		A: map[string]any{"version": "1.2"}, B: map[string]any{"version": "1.3"}, WantEqual: false,
	}, { // Test 4: An added key.
		A: map[string]any{"a": 1}, B: map[string]any{"a": 1, "b": nil}, WantEqual: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := OutputsDigest(test.A)
			if err != nil {
				t.Fatalf("digest a: %v", err)
			}
			b, err := OutputsDigest(test.B)
			if err != nil {
				t.Fatalf("digest b: %v", err)
			}
			if got := a == b; got != test.WantEqual {
				t.Errorf("digests equal = %t, want %t (%s, %s)", got, test.WantEqual, a, b)
			}
		})
	}
	if _, err := OutputsDigest(map[string]any{"f": func() {}}); err == nil {
		t.Error("outputs that cannot be encoded digest without an error")
	}
	bad := &run.Run{ID: "run_x", Outputs: map[string]any{"c": make(chan int)}}
	if _, err := ResultRecord(bad); err == nil {
		t.Error("a result record over outputs that cannot be encoded builds without an error")
	}
}

// TestRootOf pins that a root names an entry by its id, content digest, and path.
func TestRootOf(t *testing.T) {
	t.Parallel()
	e := &audit.Entry{ID: "aud_1", ContentDigest: "cd", Path: "/p", Method: audit.MethodDecision,
		Actor: "approver"}
	want := Root{EntryID: "aud_1", ContentDigest: "cd", Path: "/p"}
	if diff := cmp.Diff(want, RootOf(e)); diff != "" {
		t.Errorf("root mismatch (-want +got):\n%s", diff)
	}
}
