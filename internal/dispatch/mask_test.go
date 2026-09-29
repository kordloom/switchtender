package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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
	}, { // Test 6: A short secret is still masked in text shorter than the longest secret.
		Name:    "mixed lengths",
		Secrets: []string{"a-very-long-secret-value-that-dwarfs-the-others", "shortsec"},
		In:      "x shortsec y", Want: "x *** y",
	}, { // Test 7: Text shorter than every secret is returned untouched.
		Name: "shorter than any secret", Secrets: []string{"longsecret"}, In: "hi", Want: "hi",
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
		Play:    "deploy topsecret",
		Task:    "set topsecret var",
		Host:    "topsecret-host",
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
		Play: "deploy ***", Task: "set *** var", Host: "***-host",
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

// TestMaskerRedactEventMixedLengths checks a short secret is still masked in a short field when a
// much longer secret is also registered, the case the shortest-secret and per-secret length skips
// could wrongly bypass.
func TestMaskerRedactEventMixedLengths(t *testing.T) {
	t.Parallel()
	m := &masker{}
	m.set([]string{"a-very-long-secret-value-that-dwarfs-the-others", "shortsec"})
	e := event.Event{Host: "shortsec-01", Message: "a-very-long-secret-value-that-dwarfs-the-others"}
	m.redactEvent(&e)
	if diff := cmp.Diff(event.Event{Host: "***-01", Message: "***"}, e); diff != "" {
		t.Errorf("redactEvent mismatch (-want +got):\n%s", diff)
	}
}

// TestMaskerConcurrentSetAndRedact drives the masker the way a run does, with the log sink and the
// event tailer reading while credentials resolve and replace the secret set. It exists to be run
// under the race detector.
func TestMaskerConcurrentSetAndRedact(t *testing.T) {
	t.Parallel()
	m := &masker{}
	m.set([]string{"initial-secret-value"})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				e := benchEvent()
				m.redactEvent(&e)
				_ = m.redactString("value is initial-secret-value here")
				_ = m.redact([]byte("value is initial-secret-value here"))
				_ = m.longest()
				if i%50 == 0 {
					m.set(benchSecrets(4))
				}
			}
		}()
	}
	wg.Wait()
}

// benchSecrets returns n distinct secret values long enough to be masked, for benchmarking a run
// whose credentials resolved to many values.
func benchSecrets(n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, fmt.Sprintf("secret-value-%06d-padding-that-is-longer-than-the-stack-buffer", i))
	}
	return out
}

// benchEvent returns a runner event whose free-text fields and set_stats outputs are all populated,
// the shape redactEvent walks on every event of a run.
func benchEvent() event.Event {
	return event.Event{
		Type:    event.TypeRunnerOK,
		Play:    "deploy the application",
		Task:    "template the service unit file",
		Host:    "web-07.prod.example.com",
		Message: "the unit file was rendered and reloaded",
		Stdout:  strings.Repeat("output line from the module\n", 8),
		Stderr:  "",
		Diff:    strings.Repeat("+added configuration line\n", 4),
		Outputs: map[string]any{
			"release": "2026.07.29",
			"nested":  map[string]any{"host": "db-01", "port": 5432},
		},
	}
}

// BenchmarkMaskerRedactEvent measures the per-event cost of masking with a large secret set, the
// path every event of a run takes.
func BenchmarkMaskerRedactEvent(b *testing.B) {
	m := &masker{}
	m.set(benchSecrets(500))
	base := benchEvent()
	b.ReportAllocs()
	for b.Loop() {
		e := base
		e.Outputs = map[string]any{
			"release": "2026.07.29",
			"nested":  map[string]any{"host": "db-01", "port": 5432},
		}
		m.redactEvent(&e)
	}
}

// BenchmarkMaskerRedactString measures masking a single short field, the case a run pays for on
// every event field that cannot contain a secret.
func BenchmarkMaskerRedactString(b *testing.B) {
	m := &masker{}
	m.set(benchSecrets(500))
	b.ReportAllocs()
	for b.Loop() {
		_ = m.redactString("web-07")
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

// TestStreamMaskerSplitSecret covers the case that made chunk-at-a-time redaction unsafe: a secret
// arriving across two writes. Neither half contains the whole value, so redacting each chunk on its
// own let the plaintext through. Every split point of the secret is exercised, plus the case where
// it arrives one byte at a time.
func TestStreamMaskerSplitSecret(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-swordfish"
	tests := []struct {
		Name   string
		Chunks []string
	}{{ // Test 0: The whole stream in one write, the case that already worked.
		Name: "single chunk", Chunks: []string{"before " + secret + " after"},
	}, { // Test 1: Split inside the secret, the leak this closes.
		Name: "split mid secret", Chunks: []string{"before hunter2-", "swordfish after"},
	}, { // Test 2: Split one byte in.
		Name: "split after first byte", Chunks: []string{"before h", "unter2-swordfish after"},
	}, { // Test 3: Split one byte from the end.
		Name: "split before last byte", Chunks: []string{"before hunter2-swordfis", "h after"},
	}, { // Test 4: Three chunks, both boundaries inside the secret.
		Name: "three way split", Chunks: []string{"before hunt", "er2-sword", "fish after"},
	}, { // Test 5: The stream delivered one byte per write.
		Name: "byte at a time", Chunks: nil,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			chunks := test.Chunks
			if chunks == nil {
				for _, b := range []byte("before " + secret + " after") {
					chunks = append(chunks, string(b))
				}
			}
			m := &masker{}
			m.set([]string{secret})
			sm := &streamMasker{mask: m}
			var got strings.Builder
			for _, c := range chunks {
				got.Write(sm.next([]byte(c)))
			}
			got.Write(sm.flush())

			if strings.Contains(got.String(), secret) {
				t.Errorf("secret survived the stream: %q", got.String())
			}
			if want := "before " + maskToken + " after"; got.String() != want {
				t.Errorf("stream = %q, want %q", got.String(), want)
			}
		})
	}
}

// TestStreamMaskerPreservesStream checks the masker neither drops nor reorders output when nothing
// needs redacting, since it withholds part of every chunk to do its work.
func TestStreamMaskerPreservesStream(t *testing.T) {
	t.Parallel()
	m := &masker{}
	m.set([]string{"a-secret-value"})
	sm := &streamMasker{mask: m}
	const text = "line one\nline two\nline three\n"
	var got strings.Builder
	for i := 0; i < len(text); i += 5 {
		end := min(i+5, len(text))
		got.Write(sm.next([]byte(text[i:end])))
	}
	got.Write(sm.flush())
	if diff := cmp.Diff(text, got.String()); diff != "" {
		t.Errorf("stream mismatch (-want +got):\n%s", diff)
	}
}

// TestStreamMaskerNoSecrets checks a stream passes straight through when no secrets are configured,
// so a run with no credentials pays no buffering cost and its output is not delayed.
func TestStreamMaskerNoSecrets(t *testing.T) {
	t.Parallel()
	sm := &streamMasker{mask: &masker{}}
	out := sm.next([]byte("plain output"))
	if string(out) != "plain output" {
		t.Errorf("next() = %q, want the chunk unchanged", out)
	}
	if extra := sm.flush(); len(extra) != 0 {
		t.Errorf("flush() = %q, want nothing withheld", extra)
	}
}
