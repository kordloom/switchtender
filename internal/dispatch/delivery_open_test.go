package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// relayDynamicKind is a dynamic secrets engine registered for the delivery tests, so opening a
// run's secrets on the control node mints something that has to be handed back.
const relayDynamicKind = "relay_delivery_dynamic"

// relayRevoked records which minted relay test secrets were revoked, by the config they were minted
// from.
//
//nolint:gochecknoglobals // Written only by the registered engine, read by the tests that use it.
var relayRevoked sync.Map

// init registers the relay test engine once, before any parallel test reads the engine registry.
func init() {
	secretsource.RegisterDynamic(relayDynamicKind,
		func(_ context.Context, config string) (string, *secretsource.Lease, error) {
			return "dynamic-" + config, secretsource.NewLease(relayDynamicKind,
				func(context.Context) error {
					relayRevoked.Store(config, true)
					return nil
				}), nil
		})
}

// openFixture is a control node's credential stores with one credential of each shape a run can
// carry, and a dispatcher wired over them.
type openFixture struct {
	// d is the control node's dispatcher.
	d *Dispatcher
	// sealer seals and opens the stored values.
	sealer *credential.Sealer
	// key is the passphrase-protected SSH key the ssh credential holds.
	key string
	// answer is the secret survey answer, sealed.
	answer string
}

// fixture values every delivery test looks for.
const (
	// openKeyPassphrase unlocks the fixture's SSH key.
	openKeyPassphrase = "open-fixture-passphrase-6d2e"
	// openEnvValue is the env credential's secret value.
	openEnvValue = "env-secret-value-48af"
	// openVaultPassword is the vault credential's password.
	openVaultPassword = "vault-password-value-a1b3"
	// openKubeToken is the token inside the typed credential's kubeconfig.
	openKubeToken = "typed-kube-token-77c0"
	// openRegistryPassword is the registry login's password.
	openRegistryPassword = "registry-password-5be9"
	// openAnswer is the secret survey answer.
	openAnswer = "survey-answer-value-c4d8"
)

// newOpenFixture stores every fixture credential, sealed, and returns a dispatcher over them.
func newOpenFixture(t *testing.T, extra ...Option) *openFixture {
	t.Helper()
	ctx := context.Background()
	sealer := secretVarsSealer()
	fx := &openFixture{sealer: sealer, key: encryptedSSHKey(t, openKeyPassphrase)}
	creds := credential.NewMemStore()
	types := credential.NewMemTypeStore()
	typ := &credential.CredentialType{
		ID: "ctype_kube", Name: "Kubeconfig",
		Fields:        []credential.Field{{Name: "kubeconfig", Secret: true, Multiline: true}},
		FileInjectors: map[string]string{"template": "{{ kubeconfig }}"},
		EnvInjectors:  map[string]string{"KUBECONFIG": "{{ tower.filename }}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if err := types.Save(ctx, typ); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	fields, err := json.Marshal(map[string]string{"kubeconfig": "apiVersion: v1\nusers:\n- user:\n" +
		"    token: " + openKubeToken + "\n"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	seal := func(plain string) string {
		t.Helper()
		sealed, err := sealer.Seal(plain)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		return sealed
	}
	for _, c := range []*credential.Credential{
		{ID: "cred_ssh", Name: "fleet key", Kind: credential.KindSSHKey,
			Secret:   seal(credential.BuildSSHKeySecret(fx.key, openKeyPassphrase)),
			Settings: map[string]string{"user": "deploy", "become_method": "sudo"}},
		{ID: "cred_env", Name: "cloud env", Kind: credential.KindEnv,
			Secret: seal("CLOUD_TOKEN=" + openEnvValue)},
		{ID: "cred_vault", Name: "vault pw", Kind: credential.KindVaultPassword, VaultID: "prod",
			Secret: seal(openVaultPassword)},
		{ID: "cred_kube", Name: "prod kube", TypeID: "ctype_kube", Secret: seal(string(fields))},
		{ID: "cred_reg", Name: "registry", Kind: credential.KindRegistry,
			Secret: seal("puller\n" + openRegistryPassword)},
		{ID: "cred_dyn", Name: "minted", Kind: credential.KindToken, Source: relayDynamicKind,
			Secret: seal("cfg-" + t.Name())},
		{ID: "cred_other", Name: "not this run's", Kind: credential.KindToken,
			Secret: seal("unrelated-secret-0e1f")},
		genericCred(),
		{ID: "cred_foreign", Name: "sealed elsewhere", Kind: credential.KindToken,
			Secret: func() string {
				s, err := credential.NewSealer("another", "install").Seal("x")
				if err != nil {
					t.Fatalf("Seal() error = %v", err)
				}
				return s
			}()},
	} {
		if err := creds.Save(ctx, c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	invs := inventory.NewMemStore()
	if err := invs.Save(ctx, &inventory.Inventory{ID: "inv_fleet", Name: "fleet",
		Content: "web1\n", CredentialIDs: []string{"cred_env"}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	fx.answer = seal(openAnswer)
	opts := append([]Option{WithNoJanitor(), WithCredentials(creds, sealer),
		WithCredentialTypes(types), WithInventories(invs), WithRunFilesRoot(t.TempDir())}, extra...)
	fx.d = New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), opts...)
	t.Cleanup(fx.d.Close)
	return fx
}

// fullRun is a run carrying every shape of secret: its own credentials, its inventory's, a registry
// login for its image, and a secret survey answer.
func (fx *openFixture) fullRun() *run.Run {
	return &run.Run{
		ID: "run_full", Tool: run.ToolAnsible, Playbook: "site.yml",
		CredentialIDs: []string{"cred_ssh", "cred_kube", "cred_vault"}, InventoryID: "inv_fleet",
		Image: "registry.example/ee:1", PullCredentialID: "cred_reg",
		SealedNames: []string{"db_password"}, SealedVars: map[string]string{"db_password": fx.answer},
	}
}

// TestOpeningARunsSecretsTakesExactlyWhatItsExecutorWould pins the control node's half: the set
// opened for a relay worker is the set an executor with database access would open for the same
// run, no more. Its own credentials and its inventory's, in the order they are applied, the login
// its image is pulled with, the type a typed credential needs, and its secret answers, with every
// value resolved and no sealed form or source left on any record. A credential the run does not
// name is not opened.
func TestOpeningARunsSecretsTakesExactlyWhatItsExecutorWould(t *testing.T) {
	t.Parallel()
	fx := newOpenFixture(t)
	r := fx.fullRun()
	if !fx.d.NeedsSecrets(context.Background(), r) {
		t.Fatal("NeedsSecrets() = false for a run carrying every kind of secret")
	}
	p, release, err := fx.d.OpenSecrets(context.Background(), r)
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	if release != nil {
		t.Errorf("a release was returned although nothing was minted")
	}
	got := struct {
		Order, Delivered, Types, Answers []string
		Values                           map[string]string
	}{Order: p.CredentialIDs, Delivered: p.DeliveredIDs(), Answers: p.AnswerNames(),
		Values: map[string]string{}}
	for _, typ := range p.Types {
		got.Types = append(got.Types, typ.ID)
	}
	for _, c := range p.Credentials {
		if c.Record.Secret != "" || c.Record.Source != "" {
			t.Errorf("credential %s carries its sealed form or its source", c.Record.ID)
		}
		got.Values[c.Record.ID] = c.Value
	}
	want := struct {
		Order, Delivered, Types, Answers []string
		Values                           map[string]string
	}{
		Order:     []string{"cred_ssh", "cred_kube", "cred_vault", "cred_env"},
		Delivered: []string{"cred_env", "cred_kube", "cred_reg", "cred_ssh", "cred_vault"},
		Types:     []string{"ctype_kube"}, Answers: []string{"db_password"},
		Values: map[string]string{
			"cred_ssh":   credential.BuildSSHKeySecret(fx.key, openKeyPassphrase),
			"cred_env":   "CLOUD_TOKEN=" + openEnvValue,
			"cred_vault": openVaultPassword,
			"cred_kube": `{"kubeconfig":"apiVersion: v1\nusers:\n- user:\n    token: ` + openKubeToken +
				`\n"}`,
			"cred_reg": "puller\n" + openRegistryPassword,
		},
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("opened secrets mismatch (-want +got):\n%s", diff)
	}
	if p.Answers["db_password"] != openAnswer {
		t.Errorf("the survey answer was not opened")
	}
}

// TestNeedsSecretsSeesEveryShapeOfSecret pins the question the relay asks first, so a run whose
// only secret is an inventory's credential, a pull login, or a survey answer is not treated as
// needing nothing.
func TestNeedsSecretsSeesEveryShapeOfSecret(t *testing.T) {
	t.Parallel()
	fx := newOpenFixture(t)
	tests := []struct {
		Run        *run.Run
		WantResult bool
	}{{ // Test 0: Nothing secret at all.
		Run: &run.Run{ID: "r0"}, WantResult: false,
	}, { // Test 1: Its own credential.
		Run: &run.Run{ID: "r1", CredentialIDs: []string{"cred_ssh"}}, WantResult: true,
	}, { // Test 2: Only its inventory's credential.
		Run: &run.Run{ID: "r2", InventoryID: "inv_fleet"}, WantResult: true,
	}, { // Test 3: Only a registry login for its image.
		Run: &run.Run{ID: "r3", Image: "ee:1", PullCredentialID: "cred_reg"}, WantResult: true,
	}, { // Test 4: A pull credential with no image is never used.
		Run: &run.Run{ID: "r4", PullCredentialID: "cred_reg"}, WantResult: false,
	}, { // Test 5: Only a secret answer, named.
		Run: &run.Run{ID: "r5", SealedNames: []string{"pw"}}, WantResult: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := fx.d.NeedsSecrets(context.Background(), test.Run); got != test.WantResult {
				t.Errorf("NeedsSecrets() = %v, want %v", got, test.WantResult)
			}
		})
	}
}

// TestOpeningHandsBackWhatItMintedWhenItCannotFinish pins that a dynamic secret minted for a
// delivery is revoked by the release once the run's claim ends, and at once when the opening fails
// partway, since nothing will ever be sent to use it.
func TestOpeningHandsBackWhatItMintedWhenItCannotFinish(t *testing.T) {
	t.Parallel()
	fx := newOpenFixture(t)
	cfg := "cfg-" + t.Name()
	p, release, err := fx.d.OpenSecrets(context.Background(),
		&run.Run{ID: "run_dyn", CredentialIDs: []string{"cred_dyn"}})
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	if got := p.Credential("cred_dyn").Value; got != "dynamic-"+cfg {
		t.Errorf("the dynamic source was not resolved on the control node: %q", got)
	}
	if _, revoked := relayRevoked.Load(cfg); revoked || release == nil {
		t.Fatalf("revoked before the claim ended, or no release returned (release nil: %v)",
			release == nil)
	}
	release()
	if _, revoked := relayRevoked.Load(cfg); !revoked {
		t.Errorf("the release did not revoke what the opening minted")
	}

	relayRevoked.Delete(cfg)
	p, release, err = fx.d.OpenSecrets(context.Background(),
		&run.Run{ID: "run_dyn_fail", CredentialIDs: []string{"cred_dyn", "cred_missing"}})
	if err == nil || p != nil || release != nil {
		t.Fatalf("OpenSecrets() = %v, %v, release %v, want a failure with nothing returned", p, err,
			release != nil)
	}
	if _, revoked := relayRevoked.Load(cfg); !revoked {
		t.Errorf("a failed opening left a minted secret unrevoked")
	}
}

// TestOpeningRefusesWhatItCannotOpen pins that the control node fails closed: any secret that does
// not open stops the whole delivery with the reason, as it stops an executor with database access.
func TestOpeningRefusesWhatItCannotOpen(t *testing.T) {
	t.Parallel()
	fx := newOpenFixture(t)
	bare := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor())
	t.Cleanup(bare.Close)
	// swapped is another answer sealed under the fixture's own key, the way a ciphertext copied in
	// from another run of the same install would be.
	swapped, err := fx.sealer.Seal("another-runs-answer")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	tests := []struct {
		Name string
		D    *Dispatcher
		Run  *run.Run
		Want error
	}{{ // Test 0: A control node with no key opens nothing.
		Name: "no key", D: bare, Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_ssh"}},
		Want: credential.ErrNoKey,
	}, { // Test 1: A credential that does not exist.
		Name: "missing", D: fx.d, Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_missing"}},
		Want: credential.ErrNotFound,
	}, { // Test 2: A credential sealed under another key.
		Name: "foreign", D: fx.d, Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_foreign"}},
		Want: credential.ErrUnreadable,
	}, { // Test 3: A secret answer sealed under another key.
		Name: "foreign answer", D: fx.d,
		Run: &run.Run{ID: "r", SealedNames: []string{"pw"}, SealedVars: map[string]string{
			"pw": func() string {
				s, _ := credential.NewSealer("another", "install").Seal("x")
				return s
			}()}},
		Want: ErrSecretAnswer,
	}, { // Test 4: A secret answer named but absent.
		Name: "absent answer", D: fx.d, Run: &run.Run{ID: "r", SealedNames: []string{"pw"}},
		Want: ErrSecretAnswer,
	}, { // Test 5: A federated credential with no issuer on the control node.
		Name: "no issuer", D: fx.d, Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_oidc"}},
		Want: federation.ErrNotConfigured,
	}, { // Test 6: A secret answer swapped for another sealed under the same key, after the run's
		// digest bound the original: it would open cleanly, so only the digest stops it.
		Name: "swapped answer", D: fx.d,
		Run: &run.Run{ID: "r", SealedNames: []string{"db_password"},
			SealedVars:    map[string]string{"db_password": swapped},
			SealedDigests: run.SealedDigestsOf(map[string]string{"db_password": fx.answer})},
		Want: ErrSecretAnswer,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			p, release, err := test.D.OpenSecrets(context.Background(), test.Run)
			if !errors.Is(err, test.Want) {
				t.Fatalf("OpenSecrets() error = %v, want %v", err, test.Want)
			}
			if err != nil && (p != nil || release != nil) {
				t.Errorf("a refused opening returned a payload or a release")
			}
		})
	}
}

// TestATokenIsMintedForTheModeTheWorkerWillExecute pins the federated half. The identity token is
// minted on the control node, with the run's claims, for the execution mode the claiming worker
// will actually run: a plan for a dry run and for an apply a plan-content rule plans first, an
// apply otherwise. A token whose claims say plan must never be the one an apply runs with.
func TestATokenIsMintedForTheModeTheWorkerWillExecute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gated := policy.NewMemStore()
	if err := gated.Save(ctx, &policy.Policy{ID: policy.NewID(), Name: "tf-destroy-guard",
		Tool: run.ToolTerraform, MaxDestroy: 0}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		Name        string
		Policies    policy.Store
		Run         *run.Run
		WantDryRun  bool
		WantRunType string
	}{{ // Test 0: An ordinary apply.
		Name: "apply", Run: &run.Run{ID: "run_tf", Tool: run.ToolTerraform, Command: "infra"},
		WantDryRun: false, WantRunType: federation.RunTypeApply,
	}, { // Test 1: A dry run.
		Name:       "dry run",
		Run:        &run.Run{ID: "run_tf", Tool: run.ToolTerraform, Command: "infra", DryRun: true},
		WantDryRun: true, WantRunType: federation.RunTypeDryRun,
	}, { // Test 2: An apply the plan gate plans first.
		Name: "plan gated", Policies: gated,
		Run:        &run.Run{ID: "run_tf", Tool: run.ToolTerraform, Command: "infra"},
		WantDryRun: true, WantRunType: federation.RunTypeDryRun,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			issuer, err := federation.NewIssuer("https://st.example.com", federation.NewMemKeyStore(),
				fedSealer)
			if err != nil {
				t.Fatalf("NewIssuer() error = %v", err)
			}
			creds := credential.NewMemStore()
			if err := creds.Save(ctx, genericCred()); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			opts := []Option{WithNoJanitor(), WithCredentials(creds, fedSealer), WithFederation(issuer)}
			if test.Policies != nil {
				opts = append(opts, WithPolicies(test.Policies))
			}
			d := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), opts...)
			t.Cleanup(d.Close)
			r := test.Run
			r.CredentialIDs = []string{"cred_oidc"}
			p, _, err := d.OpenSecrets(ctx, r)
			if err != nil {
				t.Fatalf("OpenSecrets() error = %v", err)
			}
			c := p.Credential("cred_oidc")
			if c == nil || c.Token == "" || c.Value != "" {
				t.Fatalf("the federated credential carries no token, or a stored value")
			}
			claims := tokenClaims(t, issuer, c.Token)
			got := []any{p.DryRun, claims.RunType, claims.RunID, claims.Audience}
			want := []any{test.WantDryRun, test.WantRunType, "run_tf", "https://vault.example.com"}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("minted token mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// heldDelivery is a SecretDelivery a test fills with one payload per run.
type heldDeliveries struct {
	// mu guards everything below.
	mu sync.Mutex
	// payloads are what Receive hands over, by run id.
	payloads map[string]*handoff.Payload
	// errs are what Receive fails with, by run id.
	errs map[string]error
	// received counts Receive calls, by run id.
	received map[string]int
	// discarded counts Discard calls, by run id.
	discarded map[string]int
	// opened keeps every payload Receive handed over, so a test can check it was wiped.
	opened map[string]*handoff.Payload
	// credentials keeps every credential handed over, so a test can check each was wiped.
	credentials map[string][]*handoff.Credential
}

// newHeldDeliveries returns an empty delivery source.
func newHeldDeliveries() *heldDeliveries {
	return &heldDeliveries{payloads: map[string]*handoff.Payload{}, errs: map[string]error{},
		received: map[string]int{}, discarded: map[string]int{}, opened: map[string]*handoff.Payload{},
		credentials: map[string][]*handoff.Credential{}}
}

// Receive hands over the run's payload once.
func (h *heldDeliveries) Receive(_ context.Context, r *run.Run) (*handoff.Payload, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.received[r.ID]++
	if err := h.errs[r.ID]; err != nil {
		return nil, err
	}
	p := h.payloads[r.ID]
	delete(h.payloads, r.ID)
	if p != nil {
		h.opened[r.ID] = p
		h.credentials[r.ID] = append([]*handoff.Credential(nil), p.Credentials...)
	}
	return p, nil
}

// Discard records the call and forgets the run's payload.
func (h *heldDeliveries) Discard(runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.discarded[runID]++
	delete(h.payloads, runID)
}

// counts reports how many times the run was received and discarded.
func (h *heldDeliveries) counts(runID string) (received, discarded int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.received[runID], h.discarded[runID]
}

// wiped reports whether every payload and credential handed over for the run was wiped, and whether
// one was handed over at all.
func (h *heldDeliveries) wiped(runID string) (handed, wiped bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.opened[runID]
	if !ok {
		return false, true
	}
	if !p.Empty() || p.Answers != nil {
		return true, false
	}
	for _, c := range h.credentials[runID] {
		if c.Value != "" || c.Token != "" {
			return true, false
		}
	}
	return true, true
}

// TestADeliveredCredentialLandsExactlyAsAStoredOne pins that a relay worker applies a delivered
// secret through the same code, to the same effect, as a worker that opens it from the database:
// the same environment, the same file contents, the same vault labels and key, and the same values
// in the mask. The only difference allowed is the run directory a path points into.
func TestADeliveredCredentialLandsExactlyAsAStoredOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newOpenFixture(t)
	r := fx.fullRun()
	r.Image = ""
	stored := roundhouse.Spec{}
	storedCleanup, storedMask, err := fx.d.materializeFrom(ctx, storeSource{d: fx.d}, r, &stored)
	if err != nil {
		t.Fatalf("materialize from the store: %v", err)
	}
	defer storedCleanup()

	p, _, err := fx.d.OpenSecrets(ctx, r)
	if err != nil {
		t.Fatalf("OpenSecrets() error = %v", err)
	}
	held := newHeldDeliveries()
	held.payloads[r.ID] = p
	worker := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor(),
		WithSecretDelivery(held), WithRunFilesRoot(t.TempDir()))
	t.Cleanup(worker.Close)
	src := worker.secretsFor(&run.Run{ID: r.ID, CredentialIDs: r.CredentialIDs,
		SealedNames: r.SealedNames})
	delivered := roundhouse.Spec{}
	deliveredCleanup, deliveredMask, err := worker.materializeFrom(ctx, src,
		&run.Run{ID: r.ID, CredentialIDs: r.CredentialIDs}, &delivered)
	if err != nil {
		t.Fatalf("materialize from the delivery: %v", err)
	}
	defer deliveredCleanup()
	answers, err := src.answers(ctx, &run.Run{ID: r.ID, SealedNames: r.SealedNames})
	if err != nil || answers["db_password"] != openAnswer {
		t.Errorf("answers() = %v, %v, want the delivered answer", answers, err)
	}

	if diff := cmp.Diff(landed(t, &stored), landed(t, &delivered)); diff != "" {
		t.Errorf("a delivered credential landed differently (-stored +delivered):\n%s", diff)
	}
	storedMask, deliveredMask = keyNeutral(storedMask), keyNeutral(deliveredMask)
	if diff := cmp.Diff(storedMask, deliveredMask); diff != "" {
		t.Errorf("the mask differs (-stored +delivered):\n%s", diff)
	}
	if !slices.Contains(deliveredMask, openKubeToken) && !slices.ContainsFunc(deliveredMask,
		func(s string) bool { return strings.Contains(s, openKubeToken) }) {
		t.Errorf("the typed credential's file content is not masked")
	}
}

// landed reduces a materialized spec to what a tool sees, with every run directory path replaced by
// the content of the file it names, so two runs in two directories compare equal when they hand the
// tool the same thing.
func landed(t *testing.T, spec *roundhouse.Spec) map[string]any {
	t.Helper()
	read := func(path string) string {
		if path == "" {
			return ""
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		return fmt.Sprintf("%v %s", info.Mode().Perm(), raw)
	}
	env := map[string]string{}
	for _, e := range spec.Env {
		k, v, _ := strings.Cut(e, "=")
		if filepath.IsAbs(v) {
			v = "file: " + read(v)
		}
		env[k] = v
	}
	var extraVars, files, vaults []string
	for _, f := range spec.ExtraVarsFiles {
		extraVars = append(extraVars, read(f))
	}
	for _, f := range spec.CredentialFiles {
		files = append(files, read(f))
	}
	for _, v := range spec.VaultPasswords {
		vaults = append(vaults, v.Label+" "+read(v.Path))
	}
	slices.Sort(extraVars)
	slices.Sort(files)
	slices.Sort(vaults)
	key := ""
	if spec.PrivateKeyPath != "" {
		raw, err := os.ReadFile(spec.PrivateKeyPath)
		if err != nil {
			t.Fatalf("read the private key: %v", err)
		}
		key = keyIdentity(string(raw))
	}
	return map[string]any{"env": env, "extra_vars": extraVars, "files": files, "vaults": vaults,
		"private_key": key}
}

// keyIdentity names an SSH private key by its public half. Unlocking a passphrase-protected key
// re-encodes it with fresh random check bytes, so two unlocks of one key differ byte for byte and
// are the same key.
func keyIdentity(text string) string {
	priv, err := ssh.ParseRawPrivateKey([]byte(text))
	if err != nil {
		return text
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return text
	}
	return "key " + ssh.FingerprintSHA256(signer.PublicKey())
}

// keyNeutral returns a mask list sorted, with each unlocked private key named by its identity.
func keyNeutral(mask []string) []string {
	out := make([]string, len(mask))
	for i, m := range mask {
		out[i] = keyIdentity(m)
	}
	slices.Sort(out)
	return out
}

// TestADeliveredSecretThatIsMissingOrMisplacedFailsTheRun pins the worker's refusals. A run never
// executes with a secret missing: a claim that delivered nothing for a run naming credentials, a
// delivery lacking one the run names or the type a typed credential needs, an answer that did not
// arrive, the control node's own refusal, a token minted for the other execution mode, and anything
// asked after the delivery was wiped all fail with the reason.
func TestADeliveredSecretThatIsMissingOrMisplacedFailsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := errors.New(`worker pool "dmz" has registered no delivery key`)
	full := func(runID string) *handoff.Payload {
		return &handoff.Payload{RunID: runID, CredentialIDs: []string{"cred_env"},
			Credentials: []*handoff.Credential{handoff.NewCredential(&credential.Credential{
				ID: "cred_env", Kind: credential.KindEnv}, "A=b-value")},
			Answers: map[string]string{"pw": "answer-value"}}
	}
	planToken := func(runID string) *handoff.Payload {
		c := handoff.NewCredential(genericCred(), "")
		c.Token = "minted.plan.token"
		return &handoff.Payload{RunID: runID, DryRun: true, CredentialIDs: []string{"cred_oidc"},
			Credentials: []*handoff.Credential{c}}
	}
	tests := []struct {
		Name    string
		Payload *handoff.Payload
		Err     error
		Run     *run.Run
		Ask     string
		Wipe    bool
		Want    error
	}{{ // Test 0: Nothing delivered for a run naming credentials.
		Name: "nothing delivered", Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_env"}},
		Ask: "credentials", Want: ErrNotDelivered,
	}, { // Test 1: Nothing delivered for a run naming nothing is not an error.
		Name: "nothing needed", Run: &run.Run{ID: "r"}, Ask: "credentials", Want: nil,
	}, { // Test 2: A delivery lacking a credential the run names.
		Name: "credential missing", Payload: full("r"),
		Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_env", "cred_ssh"}}, Ask: "credentials",
		Want: ErrNotDelivered,
	}, { // Test 3: A typed credential whose type did not arrive.
		Name: "type missing", Payload: &handoff.Payload{RunID: "r", CredentialIDs: []string{"cred_kube"},
			Credentials: []*handoff.Credential{handoff.NewCredential(&credential.Credential{
				ID: "cred_kube", TypeID: "ctype_kube"}, `{"kubeconfig":"x"}`)}},
		Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_kube"}}, Ask: "materialize",
		Want: ErrNotDelivered,
	}, { // Test 4: A secret answer named and not delivered at all.
		Name: "answers not delivered", Run: &run.Run{ID: "r", SealedNames: []string{"pw"}},
		Ask: "answers", Want: ErrSecretAnswer,
	}, { // Test 5: A delivery lacking one of the answers the run names.
		Name: "answer missing", Payload: full("r"),
		Run: &run.Run{ID: "r", SealedNames: []string{"pw", "other"}}, Ask: "answers",
		Want: ErrSecretAnswer,
	}, { // Test 6: The control node's refusal reaches the run.
		Name: "refused", Err: refusal, Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_env"}},
		Ask: "credentials", Want: refusal,
	}, { // Test 7: A token minted for a plan, asked for by an apply.
		Name: "plan token in an apply", Payload: planToken("r"),
		Run: &run.Run{ID: "r", Tool: run.ToolBash, CredentialIDs: []string{"cred_oidc"}},
		Ask: "materialize", Want: ErrNotDelivered,
	}, { // Test 8: The same token, asked for by the plan it was minted for, is applied.
		Name: "plan token in a plan", Payload: planToken("r"),
		Run: &run.Run{ID: "r", Tool: run.ToolBash, CredentialIDs: []string{"cred_oidc"}},
		Ask: "materialize plan", Want: nil,
	}, { // Test 9: Anything asked once the delivery was wiped.
		Name: "after wipe", Payload: full("r"), Wipe: true,
		Run: &run.Run{ID: "r", CredentialIDs: []string{"cred_env"}}, Ask: "credentials",
		Want: ErrNotDelivered,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			held := newHeldDeliveries()
			if test.Payload != nil {
				held.payloads["r"] = test.Payload
			}
			if test.Err != nil {
				held.errs["r"] = test.Err
			}
			d := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor(),
				WithSecretDelivery(held), WithRunFilesRoot(t.TempDir()))
			t.Cleanup(d.Close)
			src := d.secretsFor(test.Run)
			if test.Wipe {
				if _, err := src.credentialIDs(ctx, &run.Run{ID: "r"}); err != nil {
					t.Fatalf("load before wipe: %v", err)
				}
				src.wipe()
			}
			var err error
			switch test.Ask {
			case "credentials":
				_, err = src.credentialIDs(ctx, test.Run)
			case "answers":
				_, err = src.answers(ctx, test.Run)
			case "materialize", "materialize plan":
				spec := roundhouse.Spec{DryRun: test.Ask == "materialize plan"}
				var cleanup func()
				cleanup, _, err = d.materializeFrom(ctx, src, test.Run, &spec)
				cleanup()
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if received, _ := held.counts("r"); received > 1 {
				t.Errorf("the delivery was received %d times, want at most once", received)
			}
		})
	}
}
