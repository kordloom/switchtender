package dispatch

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestMaskerRedact covers redacting secret values from a chunk: passthrough with no secrets, simple
// replacement, skipping a too-short value, masking the longest match first, and masking a line of a
// multi-line secret.
func TestMaskerRedact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Secrets []string
		In      string
		Want    string
	}{{ // Test 0: No secrets passes the chunk through unchanged.
		Name: "no secrets", Secrets: nil, In: "token=abc123", Want: "token=abc123",
	}, { // Test 1: A known secret is replaced by the mask token.
		Name: "simple", Secrets: []string{"supersecret"}, In: "value is supersecret here",
		Want: "value is *** here",
	}, { // Test 2: A value shorter than the minimum is left alone.
		Name: "too short", Secrets: []string{"ab"}, In: "ab cd", Want: "ab cd",
	}, { // Test 3: The longest secret is masked first, so a superstring is fully redacted.
		Name: "longest first", Secrets: []string{"secret", "secretvalue"}, In: "secretvalue",
		Want: "***",
	}, { // Test 4: A line of a multi-line secret is redacted on its own.
		Name: "multiline line", Secrets: []string{"header\nsecretline"}, In: "x secretline y",
		Want: "x *** y",
	}, { // Test 5: Several secrets are all redacted.
		Name: "multiple", Secrets: []string{"aaaa", "bbbb"}, In: "aaaa then bbbb",
		Want: "*** then ***",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			m := &masker{}
			m.set(test.Secrets)
			got := string(m.redact([]byte(test.In)))
			if diff := cmp.Diff(test.Want, got); diff != "" {
				t.Errorf("redact mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMaskerRedactEvent confirms an event's free-text fields and string set_stats outputs are masked
// at every nesting depth while non-string outputs are untouched.
func TestMaskerRedactEvent(t *testing.T) {
	t.Parallel()
	m := &masker{}
	m.set([]string{"topsecret"})
	e := event.Event{
		Message: "topsecret",
		Stdout:  "printed topsecret",
		Stderr:  "warn topsecret",
		Diff:    "-topsecret",
		Outputs: map[string]any{
			"leaked": "topsecret",
			"count":  5,
			"data":   map[string]any{"db_password": "topsecret", "port": 22},
			"list":   []any{"topsecret", 7, map[string]any{"inner": "topsecret"}},
		},
	}
	m.redactEvent(&e)

	want := event.Event{
		Message: "***", Stdout: "printed ***", Stderr: "warn ***", Diff: "-***",
		Outputs: map[string]any{
			"leaked": "***",
			"count":  5,
			"data":   map[string]any{"db_password": "***", "port": 22},
			"list":   []any{"***", 7, map[string]any{"inner": "***"}},
		},
	}
	if diff := cmp.Diff(want, e); diff != "" {
		t.Errorf("redactEvent mismatch (-want +got):\n%s", diff)
	}
}

// TestRunMasksSecretInLog drives a full run: a tool echoes a credential's secret value, and the
// stored log has the value redacted rather than leaked.
func TestRunMasksSecretInLog(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("API_TOKEN=supersecretvalue")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "tok", Kind: credential.KindEnv,
		Source: credential.SourceLocal, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			_, _ = io.WriteString(out, "leaking supersecretvalue into output\n")
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)

	store := run.NewMemStore()
	d := New(store, runner, nil, WithCredentials(creds, sealer))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithCredentialIDs([]string{"cred_1"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitTerminal(t, store, created.ID)
	if got.Status != run.StatusSucceeded {
		t.Fatalf("run status = %q, want succeeded", got.Status)
	}
	body, err := store.Log(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	logStr := string(body)
	if strings.Contains(logStr, "supersecretvalue") {
		t.Errorf("secret leaked into stored log: %q", logStr)
	}
	if !strings.Contains(logStr, maskToken) {
		t.Errorf("expected mask token in log, got %q", logStr)
	}
}

// TestRunMasksSecretInError drives a failing run: the runner's error embeds a credential's secret
// value, and the stored run error has the value redacted rather than leaked.
func TestRunMasksSecretInError(t *testing.T) {
	t.Parallel()
	sealer := credential.NewSealer("pass", "salt")
	sealed, err := sealer.Seal("API_TOKEN=supersecretvalue")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	creds := credential.NewMemStore()
	if err := creds.Save(context.Background(), &credential.Credential{
		ID: "cred_1", Name: "tok", Kind: credential.KindEnv,
		Source: credential.SourceLocal, Secret: sealed,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{}, errors.New("curl -H 'Authorization: supersecretvalue': exit 1")
		},
	)

	store := run.NewMemStore()
	d := New(store, runner, nil, WithCredentials(creds, sealer))
	defer d.Close()

	created, err := d.Submit(context.Background(), "play.yml", "inv",
		run.WithCredentialIDs([]string{"cred_1"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	got := waitTerminal(t, store, created.ID)
	if got.Status != run.StatusFailed {
		t.Fatalf("run status = %q, want failed", got.Status)
	}
	if strings.Contains(got.Error, "supersecretvalue") {
		t.Errorf("secret leaked into run error: %q", got.Error)
	}
	if !strings.Contains(got.Error, maskToken) {
		t.Errorf("expected mask token in run error, got %q", got.Error)
	}
}
