package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// bundleSecret is the secret inside the file an imported type writes and nothing references. It
// must reach no warning, no record, and no log line.
const bundleSecret = "bundle-s3cret-77c1d0"

// TestUnreferencedFileWarnsOnTheRun proves the run-time half of an imported type that writes a file
// no injector references: the file is still written, and the run carries a warning naming the
// credential, the type, and the file, with a structured log line saying the same. Neither carries a
// word of what the file holds. A type whose every file is referenced warns about nothing.
func TestUnreferencedFileWarnsOnTheRun(t *testing.T) {
	t.Parallel()
	bundle := "-----BEGIN BUNDLE-----\n" + bundleSecret + "\n-----END BUNDLE-----\n"
	tests := []struct {
		Type        *credential.CredentialType
		Values      map[string]string
		WantFiles   int
		WantWarning string
	}{{ // Test 0: An imported type writes its single file and hands the path to nothing.
		Type: &credential.CredentialType{
			ID: "ctype_legacy", Name: "Legacy Bundle", Origin: credential.OriginAWX,
			Fields: []credential.Field{
				{Name: "host"}, {Name: "bundle", Secret: true, Multiline: true},
			},
			FileInjectors: map[string]string{"template": "{{ bundle }}"},
			EnvInjectors:  map[string]string{"BUNDLE_HOST": "{{ host }}"},
		},
		Values:    map[string]string{"host": "edge.example.com", "bundle": bundle},
		WantFiles: 1,
		WantWarning: `credential "legacy" of type "Legacy Bundle" wrote file template, which no ` +
			`env or extra-var injector of the type references, so nothing told the tool where it ` +
			`is. Hand the path over with {{ tower.filename }} on the type, or delete the file ` +
			`injector`,
	}, { // Test 1: Two named files, the key handed to nothing.
		Type: &credential.CredentialType{
			ID: "ctype_legacy", Name: "Legacy Bundle", Origin: credential.OriginAWX,
			Fields: []credential.Field{{Name: "bundle", Secret: true, Multiline: true}},
			FileInjectors: map[string]string{
				"template.ca": "{{ bundle }}", "template.key": "{{ bundle }}",
			},
			EnvInjectors: map[string]string{"BUNDLE_CA": "{{ tower.filename.ca }}"},
		},
		Values:      map[string]string{"bundle": bundle},
		WantFiles:   2,
		WantWarning: `wrote file key, which no env or extra-var injector of the type references`,
	}, { // Test 2: An imported type whose file is referenced warns about nothing.
		Type: &credential.CredentialType{
			ID: "ctype_legacy", Name: "Legacy Bundle", Origin: credential.OriginAWX,
			Fields:        []credential.Field{{Name: "bundle", Secret: true, Multiline: true}},
			FileInjectors: map[string]string{"template": "{{ bundle }}"},
			EnvInjectors:  map[string]string{"BUNDLE_FILE": "{{ tower.filename }}"},
		},
		Values:    map[string]string{"bundle": bundle},
		WantFiles: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fx := newFileFixture(t)
			if err := test.Type.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if err := fx.types.Save(ctx, test.Type); err != nil {
				t.Fatal(err)
			}
			values, err := json.Marshal(test.Values)
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := fx.sealer.Seal(string(values))
			if err != nil {
				t.Fatal(err)
			}
			if err := fx.creds.Save(ctx, &credential.Credential{
				ID: "cred_legacy", Name: "legacy", TypeID: test.Type.ID, Secret: sealed,
			}); err != nil {
				t.Fatal(err)
			}
			var (
				mu      sync.Mutex
				written []string
			)
			runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
				_ io.Writer) (roundhouse.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				for _, f := range spec.CredentialFiles {
					body, err := os.ReadFile(f)
					if err == nil && strings.Contains(string(body), bundleSecret) {
						written = append(written, f)
					}
				}
				return roundhouse.Result{ExitCode: 0}, nil
			})
			core, logs := observer.New(zapcore.DebugLevel)
			store := run.NewMemStore()
			d := New(store, runner, zap.New(core), fx.options()...)
			defer d.Close()
			r, err := d.Submit(ctx, "site.yml", "hosts.ini",
				run.WithCredentialIDs([]string{"cred_legacy"}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			got := waitTerminal(t, store, r.ID)
			if got.Status != run.StatusSucceeded {
				t.Fatalf("status = %q (%s), want succeeded", got.Status, got.Error)
			}
			mu.Lock()
			if len(written) != test.WantFiles {
				t.Errorf("the run wrote %d files holding the value, want %d", len(written),
					test.WantFiles)
			}
			mu.Unlock()

			record, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(record), bundleSecret) ||
				strings.Contains(string(record), "BEGIN BUNDLE") {
				t.Errorf("the file's contents reached the run record: %s", record)
			}
			var warned []observer.LoggedEntry
			for _, e := range logs.All() {
				line, _ := json.Marshal(e.ContextMap())
				if strings.Contains(e.Message, bundleSecret) || strings.Contains(string(line), bundleSecret) {
					t.Errorf("the file's contents reached the server log: %s %s", e.Message, line)
				}
				if strings.Contains(e.Message, "no injector references") {
					warned = append(warned, e)
				}
			}
			// A runner that records no events earns the run an unrelated warning of its own, so only
			// this one is looked for.
			if test.WantWarning == "" {
				if strings.Contains(got.Warning, "injector of the type references") || len(warned) != 0 {
					t.Errorf("a type with every file referenced warned: %q, %d log lines",
						got.Warning, len(warned))
				}
				return
			}
			if !strings.Contains(got.Warning, test.WantWarning) {
				t.Errorf("run warning = %q, want it to say %q", got.Warning, test.WantWarning)
			}
			if len(warned) != 1 || warned[0].Level != zapcore.WarnLevel {
				t.Fatalf("logged %d warnings about the file, want one at warn level", len(warned))
			}
			fields := warned[0].ContextMap()
			for key, want := range map[string]any{
				"run_id": r.ID, "credential_id": "cred_legacy", "credential_type": "Legacy Bundle",
				"credential_type_id": "ctype_legacy", "type_origin": credential.OriginAWX,
			} {
				if fields[key] != want {
					t.Errorf("log field %s = %v, want %v", key, fields[key], want)
				}
			}
			if file, _ := fields["file"].(string); !strings.Contains(test.WantWarning, "file "+file+",") {
				t.Errorf("log field file = %q, which the warning %q does not name", file,
					test.WantWarning)
			}
		})
	}
}

// TestADeliveredUnreferencedFileWarnsOnTheRun proves the same run-time warning on a relay worker,
// which holds no type store: the warning reads the type the control node delivered beside the
// credential, so an imported type that writes a file nothing references is named on the run there
// too, and the warning carries none of what the file holds.
func TestADeliveredUnreferencedFileWarnsOnTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	bundle := "-----BEGIN BUNDLE-----\n" + bundleSecret + "\n-----END BUNDLE-----\n"
	typ := &credential.CredentialType{
		ID: "ctype_legacy", Name: "Legacy Bundle", Origin: credential.OriginAWX,
		Fields:        []credential.Field{{Name: "bundle", Secret: true, Multiline: true}},
		FileInjectors: map[string]string{"template": "{{ bundle }}"},
	}
	if err := typ.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	values, err := json.Marshal(map[string]string{"bundle": bundle})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	held := newHeldDeliveries()
	p := &handoff.Payload{RunID: "run_relay", CredentialIDs: []string{"cred_legacy"},
		Credentials: []*handoff.Credential{handoff.NewCredential(&credential.Credential{
			ID: "cred_legacy", Name: "legacy", TypeID: typ.ID}, string(values))}}
	p.AddType(typ)
	held.payloads["run_relay"] = p
	d := New(run.NewMemStore(), roundhouse.RunnerFunc(nil), zap.NewNop(), WithNoJanitor(),
		WithSecretDelivery(held), WithRunFilesRoot(t.TempDir()))
	t.Cleanup(d.Close)
	r := &run.Run{ID: "run_relay", Tool: run.ToolBash, CredentialIDs: []string{"cred_legacy"}}
	spec := &roundhouse.Spec{}
	cleanup, _, err := d.materializeFrom(ctx, d.secretsFor(r), r, spec)
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("materializeFrom() error = %v", err)
	}
	if len(spec.CredentialFiles) != 1 {
		t.Errorf("CredentialFiles = %v, want the one file the type writes", spec.CredentialFiles)
	}
	want := `credential "legacy" of type "Legacy Bundle" wrote file template, which no env or ` +
		`extra-var injector of the type references`
	if !strings.Contains(r.Warning, want) {
		t.Errorf("run warning = %q, want it to say %q", r.Warning, want)
	}
	if strings.Contains(r.Warning, bundleSecret) {
		t.Error("the warning carries what the file holds")
	}
}
