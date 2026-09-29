package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// echoSecretsRunner prints fixed text the way a playbook would print a variable it was given.
type echoSecretsRunner struct {
	// text is written to the run's output.
	text string
}

// Run writes the text and succeeds.
func (e *echoSecretsRunner) Run(_ context.Context, _ roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
	_, _ = io.WriteString(out, e.text)
	return roundhouse.Result{ExitCode: 0}, nil
}

// TestAPathInventorysSecretsAreMasked is the case that printed in clear: a hosts file on disk whose
// secret-named variables a stored inventory would have had masked. It covers a file and a directory
// holding group_vars, and it proves the mask held in the stored log.
func TestAPathInventorysSecretsAreMasked(t *testing.T) {
	t.Parallel()
	const token, become, groupPass = "FAKE-invfile-tok-4410", "FAKE-invfile-become-4411", "FAKE-group-pw-4412"
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(hosts, []byte("localhost ansible_connection=local api_token="+token+
		" ansible_become_pass="+become+"\n"), 0o600); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
	invDir := filepath.Join(dir, "inventory")
	if err := os.MkdirAll(filepath.Join(invDir, "group_vars"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(invDir, "hosts"), []byte("[web]\nweb01\n"), 0o600); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(invDir, "group_vars", "web.yml"),
		[]byte("ansible_password: "+groupPass+"\n"), 0o600); err != nil {
		t.Fatalf("write group_vars: %v", err)
	}

	tests := []struct {
		Inventory string
		Secrets   []string
	}{
		{Inventory: hosts, Secrets: []string{token, become}}, // Test 0: A hosts file.
		{Inventory: invDir, Secrets: []string{groupPass}},    // Test 1: A directory with group_vars.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			printed := "vars: " + strings.Join(test.Secrets, " ") + "\n"
			d := New(store, &echoSecretsRunner{text: printed}, nil)
			t.Cleanup(d.Close)
			r, err := d.Submit(ctx, "site.yml", test.Inventory)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			waitTerminal(t, store, r.ID)
			body, err := store.Log(ctx, r.ID)
			if err != nil {
				t.Fatalf("Log() error = %v", err)
			}
			for _, secret := range test.Secrets {
				if strings.Contains(string(body), secret) {
					t.Errorf("the log carries %s in clear:\n%s", secret, body)
				}
			}
			if !strings.Contains(string(body), "vars: ") {
				t.Errorf("the log lost the line itself:\n%s", body)
			}
		})
	}
}

// TestPathInventorySecretsLeavesScriptsAndGapsAlone covers what is deliberately not read: a missing
// path, an empty one, and an executable dynamic inventory, whose output rather than text holds hosts.
func TestPathInventorySecretsLeavesScriptsAndGapsAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "ec2.py")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n# api_token=FAKE-in-a-script\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	for testNum, path := range []string{"", filepath.Join(dir, "missing"), script} {
		if got := pathInventorySecrets(path); len(got) != 0 {
			t.Errorf("test %d: pathInventorySecrets(%q) = %v, want nothing", testNum, path, got)
		}
	}
}
