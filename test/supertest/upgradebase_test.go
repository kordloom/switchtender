package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestTheUpgradeStartsFromAnOlderRelease is the guard on the one thing the upgrade and rollback
// phases exist to do, which is cross a version boundary.
//
// The previous release was resolved as "the newest tag", which is the version being built for the
// whole window between a tag being cut and the chart being bumped for the next one. In that window
// the upgrade phase installed a version, upgraded it to itself, and reported an upgrade; the
// rollback phase then rolled it back to itself and reported reversibility. Nothing failed, because
// nothing was asked.
func TestTheUpgradeStartsFromAnOlderRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Building string
		WantTag  string
		Tags     []string
	}{{ // Test 0: The defect. The newest tag names the version this tree builds.
		Name:     "newest tag is the version being built",
		Tags:     []string{"v1.96.0", "v1.95.1", "v1.95.0"},
		Building: "1.96.0", WantTag: "v1.95.1",
	}, { // Test 1: The ordinary window, after a chart bump and before the tag.
		Name:     "newest tag is already older",
		Tags:     []string{"v1.95.1", "v1.95.0", "v1.94.0"},
		Building: "1.96.0", WantTag: "v1.95.1",
	}, { // Test 2: A tag ahead of this tree, which is not something to upgrade from.
		Name:     "a tag newer than this tree exists",
		Tags:     []string{"v2.0.0", "v1.96.0", "v1.95.1"},
		Building: "1.96.0", WantTag: "v1.95.1",
	}, { // Test 3: Nothing older exists, which must be said rather than guessed at.
		Name:     "no older release",
		Tags:     []string{"v1.96.0"},
		Building: "1.96.0", WantTag: "",
	}, { // Test 4: Patch numbers order as numbers, not as text.
		Name:     "double digit patch",
		Tags:     []string{"v1.95.10", "v1.95.9", "v1.95.1"},
		Building: "1.96.0", WantTag: "v1.95.10",
	}, { // Test 5: A minor bump, where every patch of the previous minor is a candidate.
		Name:     "minor bump picks the newest patch below it",
		Tags:     []string{"v1.96.0", "v1.95.2", "v1.95.1"},
		Building: "1.96.0", WantTag: "v1.95.2",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := previousTag(test.Tags, test.Building)
			if diff := cmp.Diff(test.WantTag, got); diff != "" {
				t.Errorf("previous release tag (-want +got):\n%s", diff)
			}
			if got != "" && got == "v"+test.Building {
				t.Errorf("the upgrade would start from %s, which is the version being built, so "+
					"it would cross no version boundary at all", got)
			}
		})
	}
}

// TestVersionsOrderAsNumbers pins the comparison the tag choice rests on.
func TestVersionsOrderAsNumbers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		A        string
		B        string
		WantLess bool
	}{{ // Test 0: A patch below its successor.
		A: "1.95.1", B: "1.95.2", WantLess: true,
	}, { // Test 1: Text ordering would put 9 after 10, so this is the case that catches it.
		A: "1.95.9", B: "1.95.10", WantLess: true,
	}, { // Test 2: A minor bump outranks any patch below it.
		A: "1.95.99", B: "1.96.0", WantLess: true,
	}, { // Test 3: Equal is not less, which is what keeps a version off its own upgrade path.
		A: "1.96.0", B: "1.96.0", WantLess: false,
	}, { // Test 4: The other direction.
		A: "1.96.0", B: "1.95.1", WantLess: false,
	}, { // Test 5: A short version against a long one, where the missing field counts as zero.
		A: "1.96", B: "1.96.1", WantLess: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantLess, lessVersion(test.A, test.B)); diff != "" {
				t.Errorf("%s < %s (-want +got):\n%s", test.A, test.B, diff)
			}
		})
	}
}

// TestTheChartSaysWhichVersionThisTreeBuilds covers the read that anchors the whole choice.
func TestTheChartSaysWhichVersionThisTreeBuilds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Chart   string
		WantVer string
		WantErr bool
	}{{ // Test 0: The chart as it is written.
		Name:    "quoted appVersion",
		Chart:   "apiVersion: v2\nname: switchtender\nversion: 0.8.3\nappVersion: \"1.96.0\"\n",
		WantVer: "1.96.0",
	}, { // Test 1: Unquoted, which YAML also permits.
		Name:    "unquoted appVersion",
		Chart:   "name: switchtender\nappVersion: 1.96.0\n",
		WantVer: "1.96.0",
	}, { // Test 2: A chart naming no version must say so rather than return an empty one.
		Name:    "no appVersion",
		Chart:   "apiVersion: v2\nname: switchtender\nversion: 0.8.3\n",
		WantErr: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			chartDir := filepath.Join(dir, "deploy/helm/switchtender")
			if err := os.MkdirAll(chartDir, 0o755); err != nil {
				t.Fatalf("temp chart directory: %v", err)
			}
			chartFile := filepath.Join(chartDir, "Chart.yaml")
			if err := os.WriteFile(chartFile, []byte(test.Chart), 0o644); err != nil {
				t.Fatalf("write temp chart: %v", err)
			}
			got, err := chartAppVersion(dir)
			if test.WantErr {
				if err == nil {
					t.Errorf("want an error, got version %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("chartAppVersion: %v", err)
			}
			if diff := cmp.Diff(test.WantVer, got); diff != "" {
				t.Errorf("appVersion (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTheRealChartIsReadable holds the parser to the chart actually in the repository, so a change
// to how the chart is written cannot quietly strand the upgrade phase.
func TestTheRealChartIsReadable(t *testing.T) {
	t.Parallel()
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	repo := filepath.Join(root, "..", "..")
	got, err := chartAppVersion(repo)
	if err != nil {
		t.Fatalf("read the repository's own chart: %v", err)
	}
	if got == "" {
		t.Error("the repository's chart names no appVersion")
	}
}
