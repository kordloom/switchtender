package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// pullKindStore holds an ssh key named cred_ssh and a registry login named cred_reg, bound to
// ghcr.io, sealed with sealer.
func pullKindStore(t *testing.T, sealer *credential.Sealer) credential.Store {
	t.Helper()
	ctx := context.Background()
	store := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		{ID: "cred_ssh", Name: "deploy key", Kind: credential.KindSSHKey, Secret: "key"},
		{ID: "cred_reg", Name: "ghcr login", Kind: credential.KindRegistry,
			Secret:   "username=bot\npassword=pw",
			Settings: map[string]string{credential.SettingRegistryHost: "ghcr.io"}},
	} {
		sealed, err := sealer.Seal(c.Secret)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		c.Secret, c.CreatedAt = sealed, time.Now()
		if err := store.Save(ctx, c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	return store
}

// TestAPullCredentialMustBeARegistryLogin proves every request that names a pull credential is
// refused with the rule's text when the credential is not a registry login, and accepted when it
// is: project create and update, template create and update, and a run create, which the
// submission itself refuses through a real dispatcher.
func TestAPullCredentialMustBeARegistryLogin(t *testing.T) {
	t.Parallel()
	const (
		projectBody = `{"name":"infra","repo_url":"https://git.example.com/infra.git",` +
			`"pull_credential_id":"%s"}`
		templateBody = `{"name":"deploy","playbook":"site.yml","inventory":"hosts.ini",` +
			`"pull_credential_id":"%s"}`
		// runBody names a pinned image, since the handler keeps a pull credential only beside an
		// image, and a pinned one needs no digest lookup.
		runBody = `{"tool":"bash","command":"echo hi","image":"ghcr.io/acme/app@sha256:` +
			`0000000000000000000000000000000000000000000000000000000000000000",` +
			`"pull_credential_id":"%s"}`
	)
	tests := []struct {
		// Method is the request method.
		Method string
		// Path is the request path.
		Path string
		// Body is the request JSON with %s where the pull credential id goes.
		Body string
		// WantOK is the status a registry login gets.
		WantOK int
	}{{ // Test 0: Creating a project.
		Method: http.MethodPost, Path: "/v1/projects",
		Body:   projectBody,
		WantOK: http.StatusCreated,
	}, { // Test 1: Editing a project.
		Method: http.MethodPut, Path: "/v1/projects/prj_infra",
		Body:   projectBody,
		WantOK: http.StatusOK,
	}, { // Test 2: Creating a template.
		Method: http.MethodPost, Path: "/v1/templates",
		Body:   templateBody,
		WantOK: http.StatusCreated,
	}, { // Test 3: Editing a template.
		Method: http.MethodPut, Path: "/v1/templates/tpl_deploy",
		Body:   templateBody,
		WantOK: http.StatusOK,
	}, { // Test 4: Creating a run, refused by the submission.
		Method: http.MethodPost, Path: "/v1/runs",
		Body:   runBody,
		WantOK: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		for _, credID := range []string{"cred_ssh", "cred_reg"} {
			t.Run(fmt.Sprintf("test %d %s", testNum, credID), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				sealer := sharedSealer("pass", "salt")
				creds := pullKindStore(t, sealer)
				projects := project.NewMemStore()
				if err := projects.Save(ctx, &project.Project{ID: "prj_infra", Name: "infra",
					RepoURL: "https://git.example.com/infra.git", CreatedAt: time.Now()}); err != nil {
					t.Fatalf("projects.Save() error = %v", err)
				}
				templates := template.NewMemStore()
				if err := templates.Save(ctx, &template.Template{ID: "tpl_deploy", Name: "deploy",
					Playbook: "site.yml", Inventory: "hosts.ini", CreatedAt: time.Now()}); err != nil {
					t.Fatalf("templates.Save() error = %v", err)
				}
				runs := run.NewMemStore()
				runner := roundhouse.RunnerFunc(
					func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
						return roundhouse.Result{ExitCode: 0}, nil
					},
				)
				d := dispatch.New(runs, runner, zap.NewNop(), dispatch.WithCredentials(creds, sealer),
					dispatch.WithNoJanitor())
				t.Cleanup(d.Close)
				handler := New(runs, d, zap.NewNop(), WithCredentials(creds, sealer),
					WithProjects(projects), WithTemplates(templates)).Handler()
				body := fmt.Sprintf(test.Body, credID)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(test.Method, test.Path,
					strings.NewReader(body)))
				want := test.WantOK
				if credID == "cred_ssh" {
					want = http.StatusBadRequest
				}
				if diff := cmp.Diff(want, rec.Code); diff != "" {
					t.Fatalf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
				}
				if credID == "cred_ssh" &&
					!strings.Contains(rec.Body.String(), credential.ErrPullKind.Error()) {
					t.Errorf("body = %s, want it to name the rule %q", rec.Body.String(),
						credential.ErrPullKind.Error())
				}
			})
		}
	}
}

// TestNoEditLeavesARegistryLoginUnbound proves a credential update that would leave a registry
// login with no registry host is refused and changes nothing: a rename of one stored before the
// rule, and a kind change to registry that carries no settings.
func TestNoEditLeavesARegistryLoginUnbound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Stored is the credential before the update.
		Stored credential.Credential
		// Body is the update request JSON.
		Body string
		// WantCode is the response status.
		WantCode int
	}{{ // Test 0: A name-only update of a registry login with no host is refused.
		Stored:   credential.Credential{Name: "old", Kind: credential.KindRegistry},
		Body:     `{"name":"renamed"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 1: A kind change to registry with no settings is refused.
		Stored:   credential.Credential{Name: "old", Kind: credential.KindEnv},
		Body:     `{"name":"old","kind":"registry","secret":"username=bot\npassword=pw"}`,
		WantCode: http.StatusBadRequest,
	}, { // Test 2: The same kind change naming the host is accepted.
		Stored: credential.Credential{Name: "old", Kind: credential.KindEnv},
		Body: `{"name":"old","kind":"registry","secret":"username=bot\npassword=pw",` +
			`"settings":{"registry_host":"ghcr.io"}}`,
		WantCode: http.StatusOK,
	}, { // Test 3: A rename of a bound registry login is accepted.
		Stored: credential.Credential{Name: "old", Kind: credential.KindRegistry,
			Settings: map[string]string{credential.SettingRegistryHost: "ghcr.io"}},
		Body:     `{"name":"renamed"}`,
		WantCode: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			sealer := sharedSealer("pass", "salt")
			sealed, err := sealer.Seal("username=bot\npassword=pw")
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			stored := test.Stored
			stored.ID, stored.Secret, stored.CreatedAt = "cred_1", sealed, time.Now()
			store := credential.NewMemStore()
			if err := store.Save(ctx, &stored); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithCredentials(store, sealer)).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/credentials/cred_1",
				strings.NewReader(test.Body)))
			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			got, err := store.Get(ctx, "cred_1")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if test.WantCode != http.StatusOK {
				if got.Name != test.Stored.Name || got.Kind != test.Stored.Kind {
					t.Errorf("refused update changed the credential to %q %s, want %q %s",
						got.Name, got.Kind, test.Stored.Name, test.Stored.Kind)
				}
				if !strings.Contains(rec.Body.String(), credential.SettingRegistryHost) {
					t.Errorf("body = %s, want it to name %s", rec.Body.String(),
						credential.SettingRegistryHost)
				}
			}
		})
	}
}
