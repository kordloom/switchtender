package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestAReleaseMarkedOnlyBySnapshotIsStillFound pins the lookup for a public history that begins with
// snapshots of earlier releases. Those releases carry a snapshot/v tag rather than a v tag, so the
// upgrade phases must find the previous release and its chart through either.
func TestAReleaseMarkedOnlyBySnapshotIsStillFound(t *testing.T) {
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
	release := func(appVersion, tag string) {
		t.Helper()
		if err := os.MkdirAll(chart, 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		body := "appVersion: \"" + appVersion + "\"\n"
		if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		git("add", ".")
		git("commit", "-q", "-m", appVersion)
		if tag != "" {
			git("tag", tag)
		}
	}
	git("init", "-q")
	git("config", "user.email", "supertest@example.invalid")
	git("config", "user.name", "supertest")
	release("1.0.0", "snapshot/v1.0.0")
	release("1.1.0", "snapshot/v1.1.0")
	release("1.2.0", "")

	tags, err := releaseTags(repo)
	if err != nil {
		t.Fatalf("releaseTags() error = %v", err)
	}
	if got, want := strings.Join(tags, " "), "v1.1.0 v1.0.0"; got != want {
		t.Errorf("releaseTags() = %q, want %q, newest first with the snapshot prefix dropped", got, want)
	}
	prev, err := previousReleaseImage("", repo)
	if err != nil {
		t.Fatalf("previousReleaseImage() error = %v", err)
	}
	if prev != "ghcr.io/kordloom/switchtender:1.1.0" {
		t.Errorf("previousReleaseImage() = %q, want the newest snapshot below 1.2.0", prev)
	}
	h := &harness{repo: repo, work: t.TempDir()}
	dir, err := h.chartAt("1.1.0")
	if err != nil {
		t.Fatalf("chartAt() of a snapshot-only release error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "appVersion: \"1.1.0\"\n" {
		t.Errorf("chartAt(1.1.0) read %q, want the chart at snapshot/v1.1.0", got)
	}
}
