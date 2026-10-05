package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// TestARunsGradeReadsItsProject covers the approver's view of a project run. The playbook names a
// role and the role holds the work, so the run detail has to read the role from the project's
// checkout, the way the gate does, or an approver is shown a grade the rule never used.
//
// Each case builds its own checkout once it is running, rather than sharing one made before the
// cases waited their turn: on a busy machine a parallel case can wait minutes for a slot, and a
// checkout that sat in the temporary directory that long was the one thing the case depended on
// that it did not hold.
func TestARunsGradeReadsItsProject(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tests := []struct {
		Name       string
		Checkouts  bool
		WantClass  string
		WantReason string
	}{{ // Test 0: With the checkouts, the role is read.
		Name:       "with checkouts",
		Checkouts:  true,
		WantClass:  run.Irreversible,
		WantReason: "a task in roles/purge/tasks/main.yml removes a file",
	}, { // Test 1: Without them the grade says what it rests on rather than guessing.
		Name:       "without checkouts",
		WantClass:  run.ReversibleCostly,
		WantReason: "the playbook's own contents were not examined",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			syncer, projects := gradeProject(t)
			opts := []Option{WithProjects(projects)}
			if test.Checkouts {
				opts = append(opts, WithProjectFiles(syncer))
			}
			store := run.NewMemStore()
			if err := store.Save(context.Background(), &run.Run{ID: "run_1", Tool: run.ToolAnsible,
				Playbook: "site.yml", ProjectID: "proj_1", Status: run.StatusPendingApproval}); err != nil {
				t.Fatalf("Save(run) error = %v", err)
			}
			handler := New(store, &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_1", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("get run = %d, body %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Reversibility run.Reversibility `json:"reversibility"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			said := strings.Join(got.Reversibility.Reasons, "\n")
			if got.Reversibility.Class != test.WantClass || !strings.Contains(said, test.WantReason) {
				t.Errorf("reversibility = %q, want %q saying %q:\n%s", got.Reversibility.Class,
					test.WantClass, test.WantReason, said)
			}
		})
	}
}

// gradeProject builds project proj_1 with a committed checkout whose playbook names a role that
// removes a file, and returns the syncer over the checkout and the project store.
func gradeProject(t *testing.T) (*project.Syncer, project.Store) {
	t.Helper()
	cache := t.TempDir()
	checkout := filepath.Join(cache, "proj_1")
	for rel, body := range map[string]string{
		"site.yml": "- hosts: all\n  roles:\n    - purge\n",
		"roles/purge/tasks/main.yml": "- ansible.builtin.file:\n" +
			"    path: /srv/archive\n    state: absent\n",
	} {
		full := filepath.Join(checkout, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"}, {"add", "."}, {"commit", "-m", "first"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = checkout
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	syncer, err := project.NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	if err := projects.Save(context.Background(), &project.Project{
		ID: "proj_1", Name: "infra", RepoURL: "https://example.com/repo.git",
	}); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	return syncer, projects
}

// TestASplitRunIsGradedOnWhatItsShardsDid covers a finished split. A parent stores no host rows of
// its own, since the work ran in its shards, so the grade never saw the outcome: a split of a
// playbook that changed nothing graded costly while the same playbook run unsplit graded
// reversible.
func TestASplitRunIsGradedOnWhatItsShardsDid(t *testing.T) {
	t.Parallel()
	quiet := []run.HostSummary{{Host: "web01", OK: 2}, {Host: "web02", OK: 2}}
	tests := []struct {
		Name       string
		Shards     [][]run.HostSummary
		WantClass  string
		WantReason string
	}{{ // Test 0: No shard changed a host, so there is nothing to undo, as for the unsplit run.
		Name:       "no change anywhere",
		Shards:     [][]run.HostSummary{quiet, {{Host: "db01", OK: 1}}},
		WantClass:  run.Reversible,
		WantReason: "no host reporting a change",
	}, { // Test 1: One changed host in one shard is a change to the run.
		Name:       "one shard changed a host",
		Shards:     [][]run.HostSummary{quiet, {{Host: "db01", OK: 1, Changed: 1}}},
		WantClass:  run.ReversibleCostly,
		WantReason: "changes state",
	}, { // Test 2: A shard that reported no hosts leaves the outcome unknown, which is not clean.
		Name:       "a shard reported nothing",
		Shards:     [][]run.HostSummary{quiet, nil},
		WantClass:  run.ReversibleCostly,
		WantReason: "changes state",
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			if err := store.Save(ctx, &run.Run{ID: "run_split", Kind: run.KindSplit, Tool: run.ToolAnsible,
				Playbook: "site.yml", Status: run.StatusSucceeded}); err != nil {
				t.Fatalf("Save(parent) error = %v", err)
			}
			for i, hosts := range test.Shards {
				parent, index := "run_split", i
				shard := &run.Run{ID: fmt.Sprintf("run_shard_%d", i), ParentID: &parent, ShardIndex: &index,
					Tool: run.ToolAnsible, Playbook: "site.yml", Status: run.StatusRunning}
				if err := store.Save(ctx, shard); err != nil {
					t.Fatalf("Save(shard) error = %v", err)
				}
				// Host rows land while a run is live, since a finished run's summary is fenced. The
				// store cleans the rows it is given in place and the subtests share quiet, so each
				// save gets its own copy.
				rows := append([]run.HostSummary(nil), hosts...)
				if err := store.SaveHostSummary(ctx, shard.ID, rows); err != nil {
					t.Fatalf("SaveHostSummary() error = %v", err)
				}
				shard.Status = run.StatusSucceeded
				if err := store.Save(ctx, shard); err != nil {
					t.Fatalf("Save(finished shard) error = %v", err)
				}
			}
			handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_split", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("get run = %d, body %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Reversibility run.Reversibility `json:"reversibility"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			said := strings.Join(got.Reversibility.Reasons, "\n")
			if got.Reversibility.Class != test.WantClass || !strings.Contains(said, test.WantReason) {
				t.Errorf("reversibility = %q, want %q saying %q:\n%s", got.Reversibility.Class,
					test.WantClass, test.WantReason, said)
			}
		})
	}
}
