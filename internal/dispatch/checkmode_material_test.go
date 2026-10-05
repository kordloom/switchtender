package dispatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// materialSeen is what an executed run was handed, read while it ran.
type materialSeen struct {
	// DryRun is whether the run executed in check mode.
	DryRun bool
	// Answer is the secret survey answer the run's variables carried.
	Answer any
	// Token reports whether a federated token file was readable.
	Token bool
	// Facts reports whether a fact cache directory existed.
	Facts bool
}

// materialRunner lists inventories the way ListingRunner does and records what each run was handed.
type materialRunner struct {
	*inventorytest.ListingRunner
	// mu guards seen.
	mu sync.Mutex
	// seen holds one observation per run.
	seen []materialSeen
}

// Run records the run's material and succeeds.
func (m *materialRunner) Run(ctx context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	s := materialSeen{DryRun: spec.DryRun, Answer: spec.ExtraVars["db_password"]}
	for _, e := range spec.Env {
		if p, ok := strings.CutPrefix(e, federation.TokenFileEnvVar+"="); ok {
			b, err := os.ReadFile(p)
			s.Token = err == nil && len(b) > 0
		}
	}
	if spec.FactCacheDir != "" {
		_, err := os.Stat(spec.FactCacheDir)
		s.Facts = err == nil
	}
	m.mu.Lock()
	m.seen = append(m.seen, s)
	m.mu.Unlock()
	return m.ListingRunner.Run(ctx, spec, out)
}

// TestAnApprovedForcingDryRunKeepsItsRunMaterial pins that a dry run held because its playbook
// forces real tasks under check mode, once approved, executes with everything the other features
// give a run: its composed inventory's resolved hosts, its sealed survey answer opened, its
// federated token, and its fact cache, still in check mode.
func TestAnApprovedForcingDryRunKeepsItsRunMaterial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"site.yml": forcedTaskPlaybook})
	issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
		fedSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(ctx, genericCred()); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	const answer = "db-answer-for-the-tool"
	sealed, err := fedSealer.Seal(answer)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	runner := &materialRunner{ListingRunner: &inventorytest.ListingRunner{}}
	invs := composeInventories(t)
	store := run.NewMemStore()
	d := New(store, runner, zap.NewNop(), WithInventories(invs), WithPolicies(excludeDryRuns(t)),
		WithNoJanitor(), WithCredentials(creds, fedSealer), WithFederation(issuer),
		WithFactCache(factcache.NewMemStore()), WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_smart", Name: "web everywhere",
		Kind: inventory.KindSmart, OrgID: "org_x", HostFilter: "groups__name=web"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	held, err := d.Submit(ctx, filepath.Join(dir, "site.yml"), "", run.WithTool(run.ToolAnsible),
		run.WithDryRun(true), run.WithInventory("inv_smart"),
		run.WithCredentialIDs([]string{"cred_oidc"}), run.WithFactCache(true, 0),
		run.WithSealedVars(map[string]string{"db_password": sealed}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval || held.ChangeFree() {
		t.Fatalf("status = %q findings %v, want a held forcing dry run", held.Status,
			held.DryRunFindings())
	}
	if _, err := d.Approve(ctx, held.ID, decider("approver", "session")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	final := waitTerminal(t, store, held.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q (%s), want succeeded", final.Status, final.Error)
	}
	runner.mu.Lock()
	seen := append([]materialSeen(nil), runner.seen...)
	runner.mu.Unlock()
	want := []materialSeen{{DryRun: true, Answer: answer, Token: true, Facts: true}}
	if diff := cmp.Diff(want, seen); diff != "" {
		t.Errorf("executed material mismatch (-want +got):\n%s", diff)
	}
	if final.InventoryResolution == nil ||
		!cmp.Equal([]string{"web1", "web2"}, final.InventoryResolution.Hosts) {
		t.Errorf("resolution = %+v, want the smart inventory's web hosts", final.InventoryResolution)
	}
}
