package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/trigger"
)

// reviewRegistryHost is the registry host the pull requests' configurations name. It resolves
// nowhere: the CLI configuration the template's credential carries points it at the test registry.
const reviewRegistryHost = "registry.example.test"

// reviewModules are the modules the test registry serves: one that runs nothing, and one hiding an
// external data source behind a nested module.
func reviewModules() map[string]map[string]string {
	return map[string]map[string]string{
		"clean": {"main.tf": "output \"o\" {\n  value = 1\n}\n"},
		"hiding": {"main.tf": "module \"inner\" {\n  source = \"./nested\"\n}\n",
			"nested/main.tf": "data \"external\" \"x\" {\n  program = [\"true\"]\n}\n"},
	}
}

// reviewRegistry serves the Terraform module registry protocol for its modules, each named
// acme/<name>/null at version 1.0.0 and downloaded as a tar.gz archive from the same server.
type reviewRegistry struct {
	// modules holds each module's files by path, by module name.
	modules map[string]map[string]string
	// onDownload, when set, runs as each archive download starts.
	onDownload func()
	// mu guards downloads.
	mu sync.Mutex
	// downloads counts the archive downloads.
	downloads int
}

// serve answers the version listing, the download redirect, and the archive itself. Only a GET of
// the archive counts as a download: the downloader asks its size first.
func (reg *reviewRegistry) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 6 && parts[5] == "versions" && reg.modules[parts[3]] != nil:
		_, _ = io.WriteString(w, `{"modules":[{"versions":[{"version":"1.0.0"}]}]}`)
	case len(parts) == 7 && parts[6] == "download":
		w.Header().Set("X-Terraform-Get", "http://"+r.Host+"/archive/"+parts[3]+".tar.gz")
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[0] == "archive" &&
		reg.modules[strings.TrimSuffix(parts[1], ".tar.gz")] != nil:
		archive := tarGz(reg.modules[strings.TrimSuffix(parts[1], ".tar.gz")])
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		if r.Method == http.MethodHead {
			return
		}
		reg.mu.Lock()
		reg.downloads++
		reg.mu.Unlock()
		if reg.onDownload != nil {
			reg.onDownload()
		}
		_, _ = w.Write(archive)
	default:
		http.NotFound(w, r)
	}
}

// count returns how many archives were downloaded.
func (reg *reviewRegistry) count() int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return reg.downloads
}

// tarGz packs files into a tar.gz archive.
func tarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)),
			Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// reviewFetchRunner plans with the fake plan runner and downloads modules with the real runner, so
// the gate's download is real terraform's while the plan itself stays the fake.
type reviewFetchRunner struct {
	// Runner is the fake plan runner.
	roundhouse.Runner
	// fetch is the real runner's module download.
	fetch roundhouse.ModuleFetcher
}

// FetchModules downloads with the real runner.
func (f reviewFetchRunner) FetchModules(ctx context.Context, spec roundhouse.Spec,
	out io.Writer) (roundhouse.Result, error) {
	return f.fetch.FetchModules(ctx, spec, out)
}

// newRegistryReviewServer builds a review server whose pull request calls module from reg, whose
// template's credential carries the CLI configuration that reaches reg, and whose gate downloads
// with real terraform. It returns the server and the log of what the fake plan runner executed.
// A nil audits keeps the default store.
func newRegistryReviewServer(t *testing.T, reg *reviewRegistry, module string,
	audits audit.Store) (*reviewServer, *specLog) {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and terraform is not installed, so the " +
				"gate's download cannot run")
		}
		t.Skip("terraform not on PATH")
	}
	hub := httptest.NewServer(http.HandlerFunc(reg.serve))
	t.Cleanup(hub.Close)
	cli := filepath.Join(t.TempDir(), "cli.tfrc")
	if err := os.WriteFile(cli, []byte(fmt.Sprintf(
		"host %q {\n  services = {\n    \"modules.v1\" = %q\n  }\n}\n", reviewRegistryHost,
		hub.URL+"/v1/modules/")), 0o600); err != nil {
		t.Fatalf("write CLI configuration: %v", err)
	}
	fetch, ok := roundhouse.NewAnsibleRunner().(roundhouse.ModuleFetcher)
	if !ok {
		t.Fatal("the runner cannot fetch modules")
	}
	executed := &specLog{}
	rs := newReviewServer(t, reviewSetup{
		Runner: reviewFetchRunner{Runner: planRunner(executed), fetch: fetch},
		PRFiles: map[string]string{
			"infra/plan.txt": "  + aws_instance.web\nPlan: 1 to add, 0 to change, 0 to destroy.\n",
			"infra/main.tf": fmt.Sprintf("module \"net\" {\n  source  = \"%s/acme/%s/null\"\n"+
				"  version = \"1.0.0\"\n}\n", reviewRegistryHost, module),
		},
		Audits: audits,
	})
	sealed, err := credential.NewSealer("pass", "salt").Seal("CLOUD_SECRET=" + reviewCloudSecret +
		"\nTF_CLI_CONFIG_FILE=" + cli)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if err := rs.srv.credentials.Save(context.Background(), &credential.Credential{ID: "cred_cloud",
		Name: "cred_cloud", Kind: credential.KindEnv, Secret: sealed}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return rs, executed
}

// TestReviewHookDownloadsModulesBeforeItPlans proves pull request review reads the registry modules
// a configuration calls before it plans. A fresh commit holds no downloaded modules, so a pull
// request calling a registry module was refused whatever the module held. The gate now downloads
// them first, with real terraform and the template's own credential, which carries the CLI
// configuration that reaches the registry. A clean module is planned with the download in the
// plan's evidence, downloaded once for the review's question and the plan's submission together,
// and a module hiding an external data source is still refused, by its address.
func TestReviewHookDownloadsModulesBeforeItPlans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Module is the registry module the pull request's configuration calls.
		Module string
		// WantRefused is whether the plan is refused.
		WantRefused bool
		// WantComment is a fragment the refusal comment must carry.
		WantComment string
	}{{ // Test 0: A clean registry module is planned.
		Module: "clean",
	}, { // Test 1: One hiding an external data source is refused, named by its address.
		Module: "hiding", WantRefused: true,
		WantComment: "module.net.module.inner.data.external.x runs a program during plan " +
			"(infra/.terraform/modules/net/nested/main.tf line 1)",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			reg := &reviewRegistry{modules: reviewModules()}
			rs, executed := newRegistryReviewServer(t, reg, test.Module, nil)
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook = %d %s, want 202", rec.Code, rec.Body.String())
			}
			if !test.WantRefused {
				planned := rs.planRun(t, rec)
				if len(planned.DryRunScans) != 1 || !run.ScansChangeFree(planned.DryRunScans) {
					t.Fatalf("scans = %+v, want the gate's one change free scan",
						planned.DryRunScans)
				}
				if f := planned.DryRunScans[0].Fetch; !f.Fetched() || f.Command != "terraform get" {
					t.Errorf("fetch = %+v, want the gate's terraform get recorded as done", f)
				}
				if n := reg.count(); n != 1 {
					t.Errorf("the module was downloaded %d times for one review, want 1", n)
				}
				return
			}
			if !strings.Contains(rec.Body.String(), "the configuration runs a program while it "+
				"plans") {
				t.Errorf("webhook answer %s, want the refusal", rec.Body.String())
			}
			rs.waitStatus(t, rs.headSHA, "error")
			rs.srv.reviews.Wait()
			if c := rs.forge.Comments(7); len(c) != 1 ||
				!strings.Contains(c[0].Body, test.WantComment) {
				t.Errorf("comments = %+v, want one saying %q", c, test.WantComment)
			}
			if list, err := rs.runs.List(ctx); err != nil || len(list) != 0 {
				t.Errorf("a refused plan created %d runs (%v)", len(list), err)
			}
			if specs := executed.all(); len(specs) != 0 {
				t.Errorf("a refused plan executed %d specs", len(specs))
			}
		})
	}
}

// hangUpAudits refuses an append whose context is done, as a database store does, so a review that
// stays bound to a connection the forge closed loses its record.
type hangUpAudits struct {
	// Store is the store appended to.
	audit.Store
}

// Append refuses when ctx is done and appends otherwise.
func (h hangUpAudits) Append(ctx context.Context, e *audit.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.Store.Append(ctx, e)
}

// TestAReviewOutlastsAForgeThatStopsWaiting covers a download slower than the forge's patience. A
// forge waits about ten seconds for a webhook's answer and then hangs up, and the gate's module
// download can take longer. Here the forge hangs up the moment the download starts. The review
// still records its plan, launches it, and posts the pull request's status, where a review bound to
// the forge's connection lost its record and left the pull request with nothing.
func TestAReviewOutlastsAForgeThatStopsWaiting(t *testing.T) {
	t.Parallel()
	hungUp, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	reg := &reviewRegistry{modules: reviewModules(), onDownload: hangUp}
	rs, _ := newRegistryReviewServer(t, reg, "clean", hangUpAudits{Store: audit.NewMemStore()})
	body := rs.payload(rs.opened(), rs.headSHA, "")
	req := httptest.NewRequestWithContext(hungUp, http.MethodPost, rs.hookPath,
		bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", rs.eventName())
	req.Header.Set("X-Hub-Signature-256", trigger.SignBody(rs.secret, body))
	rec := httptest.NewRecorder()
	rs.handler.ServeHTTP(rec, req)
	if hungUp.Err() == nil {
		t.Fatal("the forge never hung up, so the test shows nothing")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook = %d %s, want the plan launched after the forge hung up", rec.Code,
			rec.Body.String())
	}
	if planned := rs.planRun(t, rec); !run.ScansChangeFree(planned.DryRunScans) {
		t.Errorf("scans = %+v, want the downloaded module read as change free",
			planned.DryRunScans)
	}
	rs.waitStatus(t, rs.headSHA, successState)
}
