//go:build unix

package roundhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// untemplatedEnv names the ansible-core release a CI job installed, so a job that found another
// release fails instead of proving the wrong one.
const untemplatedEnv = "SWITCHTENDER_ANSIBLE_CORE"

// TestUntemplatedVarsFileMarksOnlySecretAnswers pins the vars file's form: a named value is written
// as {"__ansible_unsafe": value} and every other value as it was, and the spec's own map is untouched.
func TestUntemplatedVarsFileMarksOnlySecretAnswers(t *testing.T) {
	t.Parallel()
	payload := "{{ lookup('pipe','touch M') }}"
	vars := map[string]any{"pw": payload, "plain": "{{ 1 + 1 }}", "count": 3}
	spec := Spec{Tool: run.ToolAnsible, ExtraVars: vars, UntemplatedVars: []string{"pw", "absent"},
		RunDir: t.TempDir()}
	cleanup, err := materializeExtraVars(&spec)
	if err != nil {
		t.Fatalf("materializeExtraVars() error = %v", err)
	}
	t.Cleanup(cleanup)
	if len(spec.ExtraVarsFiles) != 1 {
		t.Fatalf("ExtraVarsFiles = %q, want one file", spec.ExtraVarsFiles)
	}
	data, err := os.ReadFile(spec.ExtraVarsFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("vars file is not JSON: %v\n%s", err, data)
	}
	want := map[string]any{"pw": map[string]any{"__ansible_unsafe": payload}, "plain": "{{ 1 + 1 }}",
		"count": float64(3)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("vars file mismatch (-want +got):\n%s", diff)
	}
	if vars["pw"] != payload {
		t.Errorf("the spec's own map was written: pw = %v", vars["pw"])
	}
}

// TestUntemplatedVarsReachOtherToolsPlain pins that only Ansible's vars file carries the mark: a
// Terraform run gets the secret answer as the plain string it was typed as.
func TestUntemplatedVarsReachOtherToolsPlain(t *testing.T) {
	t.Parallel()
	payload := "{{ lookup('pipe','touch M') }}"
	spec := Spec{Tool: run.ToolTerraform, Command: t.TempDir(), Image: "x",
		ExtraVars: map[string]any{"pw": payload}, UntemplatedVars: []string{"pw"}}
	plan, cleanup, err := toolContainerPlan(spec)
	t.Cleanup(cleanup)
	if err != nil {
		t.Fatalf("toolContainerPlan() error = %v", err)
	}
	if !slices.Contains(plan.extraEnv, "TF_VAR_pw="+payload) {
		t.Errorf("extraEnv = %q, want TF_VAR_pw holding the answer as typed", plan.extraEnv)
	}
	for _, kv := range plan.extraEnv {
		if strings.Contains(kv, unsafeVarKey) {
			t.Errorf("extraEnv entry %q carries the Ansible mark", kv)
		}
	}
}

// ansibleCoreRelease matches the release in ansible-playbook --version.
var ansibleCoreRelease = regexp.MustCompile(`core ([0-9]+\.[0-9]+\.[0-9]+)`)

// requireRealAnsible returns ansible-playbook's path, standing the test down when it is not
// installed and failing instead under the full suite, and fails when a CI job names a release and
// found another.
func requireRealAnsible(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and ansible-playbook is not on PATH")
		}
		t.Skip("ansible-playbook is not on PATH; this proof needs real Ansible")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("ansible-playbook --version: %v", err)
	}
	m := ansibleCoreRelease.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("could not read the ansible-core release from %q", out)
	}
	if want := os.Getenv(untemplatedEnv); want != "" && want != m[1] {
		t.Fatalf("found ansible-core %s, want the %s this job installed", m[1], want)
	}
	t.Logf("ansible-core %s at %s", m[1], bin)
	return bin
}

// untemplatedPlaybook prints each named variable between markers, so the output shows exactly what
// the play received.
func untemplatedPlaybook(names ...string) string {
	var b strings.Builder
	b.WriteString("- hosts: localhost\n  connection: local\n  gather_facts: false\n  tasks:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "    - debug:\n        msg: \"%s=<{{ %s }}>\"\n", n, n)
	}
	return b.String()
}

// TestUntemplatedSecretAnswerIsNotRenderedByAnsible runs real Ansible under check mode with secret
// answers holding template code. Each must print exactly as typed, the lookup must not run, and a
// plain variable beside them must still render. CI runs it on every pinned ansible-core release. It
// also probes the fallback form, a YAML vars file of !unsafe strings, and reports which forms each
// release honors.
func TestUntemplatedSecretAnswerIsNotRenderedByAnsible(t *testing.T) {
	requireRealAnsible(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "M")
	secrets := map[string]string{
		"pw":     "{{ lookup('pipe','touch " + marker + "') }}",
		"braces": "p{{x}}w",
		"block":  "{%abc",
		"note":   "a{#b",
	}
	names := []string{"pw", "braces", "block", "note"}
	playbook := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(playbook, []byte(untemplatedPlaybook(append(names, "plain")...)), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{"plain": "c{{ 1 + 1 }}d"}
	for k, v := range secrets {
		vars[k] = v
	}
	out, err := runUntemplatedPlay(t, Spec{Tool: run.ToolAnsible, Playbook: playbook, Inventory: "localhost,",
		DryRun: true, ExtraVars: vars, UntemplatedVars: names, RunDir: dir,
		Env: []string{"ANSIBLE_NOCOLOR=1", "HOME=" + dir, "ANSIBLE_LOCAL_TEMP=" + filepath.Join(dir, "tmp")}})
	if _, serr := os.Stat(marker); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the marker file exists, so Ansible ran the lookup in a secret answer\n%s", out)
	}
	if err != nil {
		t.Fatalf("Run() error = %v\n%s", err, out)
	}
	for _, n := range names {
		if want := n + "=<" + secrets[n] + ">"; !strings.Contains(out, want) {
			t.Errorf("output does not hold %q byte for byte\n%s", want, out)
		}
	}
	if !strings.Contains(out, "plain=<c2d>") {
		t.Errorf("the plain variable did not render to c2d\n%s", out)
	}
	t.Logf("JSON __ansible_unsafe form: held")
	t.Logf("YAML !unsafe form: %s", probeYAMLUnsafe(t))
}

// runUntemplatedPlay runs spec on the host runner and returns its output.
func runUntemplatedPlay(t *testing.T, spec Spec) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var out strings.Builder
	res, err := NewAnsibleRunner().Run(ctx, spec, &out)
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d", res.ExitCode)
	}
	return out.String(), err
}

// probeYAMLUnsafe runs the same check with the answer in a YAML vars file as an !unsafe string, the
// fallback form, and reports whether the release honored it. It reports rather than fails, since the
// JSON form is the one built.
func probeYAMLUnsafe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "M")
	answer := "{{ lookup('pipe','touch " + marker + "') }}"
	playbook := filepath.Join(dir, "site.yml")
	if err := os.WriteFile(playbook, []byte(untemplatedPlaybook("pw")), 0o600); err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	varsFile := filepath.Join(dir, "vars.yml")
	if err := os.WriteFile(varsFile, []byte("pw: !unsafe "+string(quoted)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runUntemplatedPlay(t, Spec{Tool: run.ToolAnsible, Playbook: playbook, Inventory: "localhost,",
		DryRun: true, ExtraVarsFiles: []string{varsFile},
		Env: []string{"ANSIBLE_NOCOLOR=1", "HOME=" + dir, "ANSIBLE_LOCAL_TEMP=" + filepath.Join(dir, "tmp")}})
	_, statErr := os.Stat(marker)
	switch {
	case err != nil:
		return "the play failed: " + err.Error()
	case !errors.Is(statErr, fs.ErrNotExist):
		return "NOT held, the lookup ran"
	case !strings.Contains(out, "pw=<"+answer+">"):
		return "NOT held, the answer did not print as typed"
	default:
		return "held"
	}
}
