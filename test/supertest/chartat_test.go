package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestChartAtReadsTheChartAsItWasTagged pins that the previous release is installed with the chart
// it shipped with. Installing the old image with this tree's chart broke the paid-shape upgrade the
// moment the chart handed the database over in a way the old binary could not read.
func TestChartAtReadsTheChartAsItWasTagged(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	chart := filepath.Join(repo, "deploy", "helm", "switchtender")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(chart, 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "supertest@example.invalid")
	git("config", "user.name", "supertest")
	write("appVersion: 1.0.0\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	git("tag", "v1.0.0")
	write("appVersion: 1.1.0\n")
	git("commit", "-q", "-am", "two")

	h := &harness{repo: repo, work: t.TempDir()}
	dir, err := h.chartAt("1.0.0")
	if err != nil {
		t.Fatalf("chartAt() error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "appVersion: 1.0.0\n" {
		t.Errorf("chartAt(1.0.0) read %q, want the chart as tagged, not the working tree", got)
	}
	if _, err := h.chartAt("9.9.9"); err == nil {
		t.Error("chartAt() of a tag that does not exist = nil error, want a failure")
	}
}
