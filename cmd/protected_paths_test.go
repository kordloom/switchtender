//go:build unix

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// protectedImage is a digest-pinned image reference, so a refusal is the mount guard's and not the
// digest rule's.
const protectedImage = "quay.io/ansible/creator-ee@sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000000"

// TestServeProtectedPathsRefuseEveryFileServeCreates starts serve's wiring in an empty directory, the
// way the quick start does with the default --db, lists every file it created there, and asserts the
// runner serve builds refuses each one as a mount. A file serve writes that the guard does not know
// is a file an image run can read.
func TestServeProtectedPathsRefuseEveryFileServeCreates(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv(identityDirEnv, "")
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	setString(t, &serveDB, defaultDBPath)
	setBool(t, &serveAllowContainerEE, true)
	setBool(t, &serveRequireImageDigest, true)
	setString(t, &containerRuntime, "docker")
	launched := fakeRuntime(t, "docker")

	bundle, err := openBundle(serveDB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	if _, err := loadProducerIdentity(context.Background(), bundle.Audits(), serveDB); err != nil {
		t.Fatalf("loadProducerIdentity() error = %v", err)
	}
	if _, err := writeInitialToken(serveDB, "tok"); err != nil {
		t.Fatalf("writeInitialToken() error = %v", err)
	}
	infra := filepath.Join(dir, "infra")
	if err := os.MkdirAll(infra, 0o700); err != nil {
		t.Fatal(err)
	}
	playbook := filepath.Join(infra, "site.yml")
	if err := os.WriteFile(playbook, []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := newServeRunner(nil, t.TempDir())
	files := 0
	for _, e := range created {
		if e.IsDir() {
			continue
		}
		files++
		path := filepath.Join(dir, e.Name())
		spec := roundhouse.Spec{Tool: run.ToolAnsible, Playbook: playbook, Inventory: path,
			Image: protectedImage}
		if _, err := runner.Run(context.Background(), spec, os.Stderr); !errors.Is(err, roundhouse.ErrForbiddenMount) {
			t.Errorf("inventory %s: Run() error = %v, want ErrForbiddenMount", e.Name(), err)
		}
	}
	if files < 2 {
		t.Fatalf("serve's wiring created %d files in %s, want the database and the token at least", files, dir)
	}
	tf := roundhouse.Spec{Tool: run.ToolTerraform, Command: dir, Image: protectedImage}
	if _, err := runner.Run(context.Background(), tf, os.Stderr); !errors.Is(err, roundhouse.ErrForbiddenMount) {
		t.Errorf("Terraform in the database's directory: Run() error = %v, want ErrForbiddenMount", err)
	}
	if argv, ok := launched(); ok {
		t.Errorf("a container launched with argv %v, want none", argv)
	}
}

// TestWorkerProtectedPathsRefuseItsRoots pins that the runner a worker builds refuses its run-files
// root, its project cache, and its managed Ansible runtime as mounts, for a relay worker and for one
// that opens the database, and that only the second protects the database's files.
func TestWorkerProtectedPathsRefuseItsRoots(t *testing.T) {
	tests := []struct {
		Server     string
		WantDBFile error
	}{{ // Test 0: A relay worker opens no database.
		Server: "https://control.example", WantDBFile: nil,
	}, { // Test 1: A worker that opens the database protects its files as serve does.
		Server: "", WantDBFile: roundhouse.ErrForbiddenMount,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(identityDirEnv, "")
			t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
			cache := t.TempDir()
			t.Setenv("HOME", cache)
			t.Setenv("XDG_CACHE_HOME", cache)
			setString(t, &workerDB, filepath.Join(dir, "switchtender.db"))
			setString(t, &workerServer, test.Server)
			setBool(t, &workerAllowContainerEE, true)
			setBool(t, &workerRequireImageDigest, true)
			setString(t, &containerRuntime, "docker")
			launched := fakeRuntime(t, "docker")
			if err := os.WriteFile(workerDB, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			runFiles := t.TempDir()
			runner := newWorkerRunner(runFiles)
			ansibleDir, err := ansibleRoot(ansibleRuntimeDirFlag, workerDB)
			if err != nil {
				t.Fatal(err)
			}
			for _, root := range []string{runFiles, projectCacheDir(), ansibleDir} {
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				spec := roundhouse.Spec{Tool: run.ToolTerraform, Command: root, Image: protectedImage}
				if _, err := runner.Run(context.Background(), spec, os.Stderr); !errors.Is(err, roundhouse.ErrForbiddenMount) {
					t.Errorf("Terraform in %s: Run() error = %v, want ErrForbiddenMount", root, err)
				}
			}
			infra := filepath.Join(dir, "infra")
			if err := os.MkdirAll(infra, 0o700); err != nil {
				t.Fatal(err)
			}
			spec := roundhouse.Spec{Tool: run.ToolAnsible, Playbook: filepath.Join(infra, "site.yml"),
				Inventory: workerDB, Image: protectedImage}
			_, err = runner.Run(context.Background(), spec, os.Stderr)
			if test.WantDBFile == nil && errors.Is(err, roundhouse.ErrForbiddenMount) {
				t.Errorf("database file as inventory: Run() error = %v, want no mount refusal", err)
			}
			if test.WantDBFile != nil && !errors.Is(err, test.WantDBFile) {
				t.Errorf("database file as inventory: Run() error = %v, want %v", err, test.WantDBFile)
			}
			if argv, ok := launched(); ok && test.WantDBFile != nil {
				t.Errorf("a container launched with argv %v, want none", argv)
			}
		})
	}
}
