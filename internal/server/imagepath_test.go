package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

const (
	// imagePathPinned is an image pinned by digest, so no digest lookup happens.
	imagePathPinned = "ghcr.io/acme/app@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000000"
	// imagePathSentence is the text a refusal carries.
	imagePathSentence = "takes its paths from a project"
)

// imagePathRig is a server over a real dispatcher with an operator token and an admin token.
type imagePathRig struct {
	handler   http.Handler
	runs      run.Store
	schedules schedule.Store
	operator  string
	admin     string
}

// newImagePathRig builds the rig, with defaultImage as the dispatcher's server default image.
func newImagePathRig(t *testing.T, defaultImage string) *imagePathRig {
	t.Helper()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	mint := func(name string, role user.Role) string {
		u, err := user.New(name, "pw", role)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save() error = %v", err)
		}
		plain, tok, err := auth.New(name)
		if err != nil {
			t.Fatalf("auth.New() error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save() error = %v", err)
		}
		return plain
	}
	rig := &imagePathRig{
		runs: run.NewMemStore(), schedules: schedule.NewMemStore(),
		operator: mint("ops", user.RoleOperator), admin: mint("root", user.RoleAdmin),
	}
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	opts := []dispatch.Option{dispatch.WithNoJanitor()}
	if defaultImage != "" {
		opts = append(opts, dispatch.WithDefaultImage(defaultImage))
	}
	d := dispatch.New(rig.runs, runner, zap.NewNop(), opts...)
	t.Cleanup(d.Close)
	rig.handler = New(rig.runs, d, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithTemplates(template.NewMemStore()), WithSchedules(rig.schedules)).Handler()
	return rig
}

// do sends one request with the token and returns the recorder.
func (g *imagePathRig) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	g.handler.ServeHTTP(rec, req)
	return rec
}

// idOf reads the id out of a response body.
func idOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID == "" {
		t.Fatalf("no id in %s (error = %v)", rec.Body.String(), err)
	}
	return got.ID
}

// waitTerminal blocks until the run has finished, so it may be rerun.
func (g *imagePathRig) waitTerminal(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rn, err := g.runs.Get(context.Background(), id); err == nil && rn.Status.Terminal() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
}

// TestImagePathCallerRule proves a run that mounts host paths into an image is refused below admin
// and accepted for an admin, through templates, reruns, and schedules.
func TestImagePathCallerRule(t *testing.T) {
	t.Parallel()
	image := `"image":"` + imagePathPinned + `"`
	ansible := `{"playbook":"site.yml","inventory":"hosts.ini"`
	terraform := `{"tool":"terraform","command":"/srv/tf/prod"`
	tests := []struct {
		// Name labels the case.
		Name string
		// Default is the dispatcher's server default image.
		Default string
		// Admin sends the request with the admin token.
		Admin bool
		// Body is the run request.
		Body string
		// WantCode is the status wanted.
		WantCode int
	}{{ // Test 0: An operator naming an image and Ansible paths is refused.
		Name: "operator ansible image", Admin: false, Body: ansible + "," + image + "}",
		WantCode: http.StatusForbidden,
	}, { // Test 1: An operator naming an image and a Terraform directory is refused.
		Name: "operator terraform image", Admin: false, Body: terraform + "," + image + "}",
		WantCode: http.StatusForbidden,
	}, { // Test 2: With no image anywhere, an operator's Ansible run is accepted.
		Name: "operator ansible no image", Admin: false, Body: ansible + "}",
		WantCode: http.StatusAccepted,
	}, { // Test 3: With no image anywhere, an operator's Terraform run is accepted.
		Name: "operator terraform no image", Admin: false, Body: terraform + "}",
		WantCode: http.StatusAccepted,
	}, { // Test 4: A server default image counts as an image.
		Name: "operator ansible default image", Default: imagePathPinned, Admin: false,
		Body: ansible + "}", WantCode: http.StatusForbidden,
	}, { // Test 5: An admin's Ansible image run is accepted.
		Name: "admin ansible image", Admin: true, Body: ansible + "," + image + "}",
		WantCode: http.StatusAccepted,
	}, { // Test 6: An admin's Terraform image run is accepted.
		Name: "admin terraform image", Admin: true, Body: terraform + "," + image + "}",
		WantCode: http.StatusAccepted,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			rig := newImagePathRig(t, test.Default)
			token := rig.operator
			if test.Admin {
				token = rig.admin
			}
			rec := rig.do(http.MethodPost, "/v1/runs", token, test.Body)
			if rec.Code != test.WantCode {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body.String(), test.WantCode)
			}
			if test.WantCode == http.StatusForbidden && !strings.Contains(rec.Body.String(), imagePathSentence) {
				t.Errorf("body = %s, want it to contain %q", rec.Body.String(), imagePathSentence)
			}
		})
	}

	t.Run("template launch", func(t *testing.T) {
		t.Parallel()
		rig := newImagePathRig(t, "")
		tpl := `{"name":"deploy","playbook":"site.yml","inventory":"hosts.ini",` + image + `}`
		rec := rig.do(http.MethodPost, "/v1/templates", rig.admin, tpl)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create template = %d %s", rec.Code, rec.Body.String())
		}
		id := idOf(t, rec)
		launch := "/v1/templates/" + id + "/launch"
		if rec := rig.do(http.MethodPost, launch, rig.operator, `{}`); rec.Code != http.StatusAccepted {
			t.Fatalf("operator launch = %d %s, want 202", rec.Code, rec.Body.String())
		}
		// A launch cannot override a path, so the override is refused at the door.
		rec = rig.do(http.MethodPost, launch, rig.operator, `{"playbook":"other.yml"}`)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("override launch = %d %s, want a 4xx", rec.Code, rec.Body.String())
		}
	})

	t.Run("rerun", func(t *testing.T) {
		t.Parallel()
		rig := newImagePathRig(t, "")
		rec := rig.do(http.MethodPost, "/v1/runs", rig.admin, ansible+","+image+"}")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("admin submit = %d %s, want 202", rec.Code, rec.Body.String())
		}
		id := idOf(t, rec)
		rig.waitTerminal(t, id)
		rec = rig.do(http.MethodPost, "/v1/runs/"+id+"/rerun", rig.operator, ``)
		if rec.Code != http.StatusAccepted {
			t.Errorf("operator rerun = %d %s, want 202", rec.Code, rec.Body.String())
		}
	})

	t.Run("schedules", func(t *testing.T) {
		t.Parallel()
		rig := newImagePathRig(t, "")
		inline := `{"name":"nightly","cron":"0 3 * * *","playbook":"site.yml","inventory":"hosts.ini"}`
		tplRec := rig.do(http.MethodPost, "/v1/templates", rig.admin,
			`{"name":"deploy","playbook":"site.yml","inventory":"hosts.ini"}`)
		if tplRec.Code != http.StatusCreated {
			t.Fatalf("create template = %d %s", tplRec.Code, tplRec.Body.String())
		}
		named := `{"name":"nightly","cron":"0 3 * * *","template_id":"` + idOf(t, tplRec) + `"}`
		tests := []struct {
			// Name labels the case.
			Name string
			// Admin saves with the admin token.
			Admin bool
			// Body is the schedule request.
			Body string
			// Want is the stored fact.
			Want bool
		}{{ // Test 0: An admin's inline schedule carries the fact.
			Name: "admin inline", Admin: true, Body: inline, Want: true,
		}, { // Test 1: A schedule naming a template does not, even from an admin.
			Name: "admin template", Admin: true, Body: named, Want: false,
		}}
		for testNum, test := range tests {
			token := rig.operator
			if test.Admin {
				token = rig.admin
			}
			rec := rig.do(http.MethodPost, "/v1/schedules", token, test.Body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("test %d %s: create = %d %s", testNum, test.Name, rec.Code, rec.Body.String())
			}
			got, err := rig.schedules.Get(context.Background(), idOf(t, rec))
			if err != nil {
				t.Fatalf("test %d %s: Get() error = %v", testNum, test.Name, err)
			}
			if got.PathsByAdmin != test.Want {
				t.Errorf("test %d %s: PathsByAdmin = %t, want %t", testNum, test.Name,
					got.PathsByAdmin, test.Want)
			}
		}
	})
}
