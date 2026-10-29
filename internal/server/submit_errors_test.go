package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// leakMarker stands in for a value an inventory holds, such as a host name or a variable's value,
// which the native engine's parse errors quote.
const leakMarker = "zz-inventory-value-9f3c"

// errLeakyInventory is an inventory error quoting the inventory, the way the native engine's do.
var errLeakyInventory = fmt.Errorf("%w: %q is not a YAML boolean", inventory.ErrInvalidInventory,
	leakMarker)

// errStoreOutage is a store fault met while composing an inventory, which carries no sentinel.
var errStoreOutage = fmt.Errorf("compose fleet: list inventories: %w",
	errors.New("connection refused"))

// errContentOutage is an outage of an inventory's content source, which reads the same as a
// refusal by it.
var errContentOutage = fmt.Errorf("%w: vault request failed: timeout", inventory.ErrContentRead)

// TestClassifySubmitNamesEveryKnownError pins the class of each error a submission can end in,
// alone, wrapped with detail, and wrapped in ErrResolve the way the composer wraps what it meets.
// Inside ErrResolve a known error of another class still decides, so an inventory that needs Ansible
// is the owner's to fix even when the composer found it.
func TestClassifySubmitNamesEveryKnownError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantClass submitClass
		Errs      []error
	}{{ // Test 0: What a person must change in the request or in what it names.
		WantClass: submitInput, Errs: []error{
			credential.ErrNotFound, credential.ErrNoKey, project.ErrNotFound, inventory.ErrNotFound,
			inventory.ErrNeedsAnsible, inventory.ErrInvalidInventory, inventory.ErrTooManyHosts,
			inventory.ErrHostFilter, inventory.ErrSourceVars, dispatch.ErrNoPlaybook,
			dispatch.ErrNoCommand, dispatch.ErrUnknownTool, dispatch.ErrToolCredential,
			dispatch.ErrNoSteps, dispatch.ErrTooManySteps, dispatch.ErrStepInput,
			dispatch.ErrUnnamedStep, dispatch.ErrDuplicateStep, dispatch.ErrUnknownDependency,
			dispatch.ErrDependencyCycle, run.ErrApprovalStep, run.ErrDenyPath, inventory.ErrResolve,
			inventory.ErrComposition,
		},
	}, { // Test 1: A credential this server cannot open.
		WantClass: submitSecret, Errs: []error{credential.ErrNoSecret, credential.ErrUnreadable},
	}, { // Test 2: A deny rule or the license.
		WantClass: submitPolicy, Errs: []error{dispatch.ErrPolicyDenied, dispatch.ErrQueueUnlicensed},
	}, { // Test 3: An inventory whose content could not be read from where it lives.
		WantClass: submitContent, Errs: []error{inventory.ErrContentRead},
	}, { // Test 4: A disk too full to record a new run.
		WantClass: submitCapacity, Errs: []error{dispatch.ErrLowSpace},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			for _, known := range test.Errs {
				for _, c := range []struct {
					Err       error
					WantKnown error
				}{
					{Err: known, WantKnown: known},
					{Err: fmt.Errorf("%w: %q", known, leakMarker), WantKnown: known},
					{Err: fmt.Errorf("%w: input web: %w", inventory.ErrResolve, known),
						WantKnown: known},
				} {
					class, got := classifySubmit(c.Err)
					if class != test.WantClass || got != c.WantKnown {
						t.Errorf("classifySubmit(%v) = %d, %v, want %d, %v", c.Err, class, got,
							test.WantClass, c.WantKnown)
					}
				}
			}
		})
	}
}

// TestClassifySubmitLeavesTheRestToTheServer pins what is a fault: any error no class names, and the
// two each path answers before it classes, so a path that does not check those answers them as its
// own failure rather than as another class's.
func TestClassifySubmitLeavesTheRestToTheServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err error
	}{{ // Test 0: A store fault.
		Err: errors.New("connection refused"),
	}, { // Test 1: Policies that cannot be read are a gate not passed, not a rule's refusal.
		Err: dispatch.ErrPolicyUnavailable,
	}, { // Test 2: A composed inventory matching no hosts, which each path answers first.
		Err: fmt.Errorf("%w: patch window", inventory.ErrNoHosts),
	}, { // Test 3: A callback already pending, which the callback answers first.
		Err: run.ErrCallbackPending,
	}, { // Test 4: A canceled request.
		Err: context.Canceled,
	}, { // Test 5: A snapshot this server could not take or reopen when the run was submitted.
		Err: dispatch.ErrInventorySnapshot,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if class, known := classifySubmit(test.Err); class != submitFault || known != nil {
				t.Errorf("classifySubmit(%v) = %d, %v, want a fault", test.Err, class, known)
			}
			if got := senderReason(test.Err); got != "an error on the server's side" {
				t.Errorf("senderReason(%v) = %q, want the server's side named", test.Err, got)
			}
		})
	}
}

// TestAPersonIsToldTheWholeReason pins how each path a person submits through answers an error that
// person must fix: with its whole reason at the path's status. Pipeline submit, fleet reconcile, and
// AI propose answered 500 for most of these, and rerun did for all but three.
func TestAPersonIsToldTheWholeReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reply := ai.ProviderFunc(func(context.Context, string, string) (string, error) {
		return `{"tool":"bash","command":"systemctl restart nginx","summary":"Restart nginx"}`, nil
	})
	const pipeline = `{"name":"p","steps":[{"name":"a","tool":"bash","command":"true"}]}`
	noSecret := fmt.Errorf("%w: %q", credential.ErrNoSecret, "deploy-key")
	lowSpace := fmt.Errorf("%w: only 12 MiB free at /data, under the 256 MiB floor",
		dispatch.ErrLowSpace)
	sealFault := fmt.Errorf("%w: seal the snapshot of inventory web: %w",
		dispatch.ErrInventorySnapshot, errors.New("cipher: message authentication failed"))
	tests := []struct {
		Err        error
		WantStatus int
		WantText   string
		WantHidden string
		Path       string
		Body       string
	}{{ // Test 0: Run submit.
		Err: errLeakyInventory, WantStatus: http.StatusBadRequest, WantText: leakMarker,
		Path: "/v1/runs", Body: `{"playbook":"hello.yml"}`,
	}, { // Test 1: Template launch.
		Err: errLeakyInventory, WantStatus: http.StatusBadRequest, WantText: leakMarker,
		Path: "/v1/templates/tpl_1/launch", Body: `{}`,
	}, { // Test 2: Pipeline submit.
		Err: errLeakyInventory, WantStatus: http.StatusBadRequest, WantText: leakMarker,
		Path: "/v1/pipelines", Body: pipeline,
	}, { // Test 3: Rerun, where the error is a conflict with what the replayed spec names.
		Err: errLeakyInventory, WantStatus: http.StatusConflict, WantText: leakMarker,
		Path: "/v1/runs/run_done/rerun",
	}, { // Test 4: Fleet reconcile.
		Err: errLeakyInventory, WantStatus: http.StatusBadRequest, WantText: leakMarker,
		Path: "/v1/drift/reconcile", Body: `{"host":"web01"}`,
	}, { // Test 5: AI propose.
		Err: errLeakyInventory, WantStatus: http.StatusBadRequest, WantText: leakMarker,
		Path: "/v1/ai/propose-run", Body: `{"intent":"restart nginx"}`,
	}, { // Test 6: Rerun of a spec whose credential was deleted, which was a 500.
		Err: fmt.Errorf("%w: cred_gone", credential.ErrNotFound), WantStatus: http.StatusConflict,
		WantText: "cred_gone", Path: "/v1/runs/run_done/rerun",
	}, { // Test 7: A credential with no secret behind an AI proposal.
		Err: noSecret, WantStatus: http.StatusConflict, WantText: "deploy-key",
		Path: "/v1/ai/propose-run", Body: `{"intent":"restart nginx"}`,
	}, { // Test 8: A pipeline step graph error.
		Err:        fmt.Errorf("%w: step a needs itself", dispatch.ErrDependencyCycle),
		WantStatus: http.StatusBadRequest, WantText: "step a needs itself", Path: "/v1/pipelines",
		Body: pipeline,
	}, { // Test 9: A store fault while composing an inventory is the server's, which it was not.
		Err: errStoreOutage, WantStatus: http.StatusInternalServerError,
		WantText: "could not launch template", WantHidden: "connection refused",
		Path: "/v1/templates/tpl_1/launch", Body: `{}`,
	}, { // Test 10: A fault of the server's own is a 500 that keeps its detail to the log.
		Err: errors.New("disk full"), WantStatus: http.StatusInternalServerError,
		WantText: "could not create the proposal", WantHidden: "disk full",
		Path: "/v1/drift/reconcile", Body: `{"host":"web01"}`,
	}, { // Test 11: Content that could not be read is told whole to the person launching.
		Err: errContentOutage, WantStatus: http.StatusBadRequest, WantText: "vault request failed",
		Path: "/v1/templates/tpl_1/launch", Body: `{}`,
	}, { // Test 12: A snapshot this server could not seal is its own failure, which was a 400.
		Err: sealFault, WantStatus: http.StatusInternalServerError, WantText: "could not submit run",
		WantHidden: "cipher", Path: "/v1/runs", Body: `{"playbook":"hello.yml"}`,
	}, { // Test 13: And on a template launch.
		Err: sealFault, WantStatus: http.StatusInternalServerError,
		WantText: "could not launch template", WantHidden: "cipher",
		Path: "/v1/templates/tpl_1/launch", Body: `{}`,
	}, { // Test 14: And on a rerun, which was a 409, though a rerun pins no snapshot to conflict with.
		Err: sealFault, WantStatus: http.StatusInternalServerError, WantText: "could not rerun",
		WantHidden: "cipher", Path: "/v1/runs/run_done/rerun",
	}, { // Test 15: A disk under the free space floor is a 503 naming the space, not a 500.
		Err: lowSpace, WantStatus: http.StatusServiceUnavailable,
		WantText: "only 12 MiB free at /data", Path: "/v1/runs", Body: `{"playbook":"hello.yml"}`,
	}, { // Test 16: And on a rerun, whose input errors are a 409.
		Err: lowSpace, WantStatus: http.StatusServiceUnavailable,
		WantText: "under the 256 MiB floor", Path: "/v1/runs/run_done/rerun",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			if err := templates.Save(ctx, &template.Template{
				ID: "tpl_1", Name: "Demo Job Template", Playbook: "hello.yml",
			}); err != nil {
				t.Fatalf("save template: %v", err)
			}
			runs := run.NewMemStore()
			if err := runs.Save(ctx, &run.Run{
				ID: "run_done", Playbook: "hello.yml", Status: run.StatusFailed,
			}); err != nil {
				t.Fatalf("save run: %v", err)
			}
			seedDrift(t, runs, "chk_web", "", "web01", 3)
			handler := New(runs, &fakeSubmitter{err: test.Err}, zap.NewNop(),
				WithTemplates(templates), WithAI(reply), WithReconciler(&testReconciler{})).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, test.Path,
				strings.NewReader(test.Body)))
			if rec.Code != test.WantStatus {
				t.Fatalf("POST %s = %d %s, want %d", test.Path, rec.Code, rec.Body.String(),
					test.WantStatus)
			}
			if !strings.Contains(rec.Body.String(), test.WantText) {
				t.Errorf("POST %s answered %s, want it to contain %q", test.Path, rec.Body.String(),
					test.WantText)
			}
			if test.WantHidden != "" && strings.Contains(rec.Body.String(), test.WantHidden) {
				t.Errorf("POST %s answered %s, want the fault's detail kept to the log", test.Path,
					rec.Body.String())
			}
		})
	}
}

// TestAWebhookIsToldTheErrorByName pins the webhook trigger's answer to an error the template's
// owner must fix: a 409 naming the error, on the answer and on the trigger's last error, and nothing
// the error wraps, since the inventory's values are not the sender's to read. It was a 502, which a
// sender retries and which said nothing about why. A store fault stays a 502 the sender retries,
// and so does content that could not be read, named, since the same read fails for an outage.
func TestAWebhookIsToldTheErrorByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err        error
		WantStatus int
		WantText   string
	}{{ // Test 0: An inventory the product cannot run.
		Err: errLeakyInventory, WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: invalid inventory document",
	}, { // Test 1: A project that was deleted.
		Err: fmt.Errorf("%w: %s", project.ErrNotFound, leakMarker), WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: project not found",
	}, { // Test 2: An inventory that needs Ansible, found by the composer.
		Err: fmt.Errorf("%w: %w: input %s", inventory.ErrResolve, inventory.ErrNeedsAnsible,
			leakMarker), WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: this inventory needs Ansible",
	}, { // Test 3: A store outage while the inventory was composed.
		Err:        fmt.Errorf("compose %s: %w", leakMarker, errStoreOutage),
		WantStatus: http.StatusBadGateway, WantText: "could not launch the template",
	}, { // Test 4: Content that could not be read, which was the same 502 with no name.
		Err:        fmt.Errorf("%w: input %s", errContentOutage, leakMarker),
		WantStatus: http.StatusBadGateway,
		WantText:   "this template cannot launch: inventory content could not be read",
	}, { // Test 5: A constructed inventory naming an input that is gone, which was a 502.
		Err: fmt.Errorf("%w: input %s: %w", inventory.ErrComposition, leakMarker,
			inventory.ErrNotFound), WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: inventory not found",
	}, { // Test 6: An input the composer cannot parse, which was a 502.
		Err: fmt.Errorf("%w: input %s: %w", inventory.ErrResolve, leakMarker,
			errors.New("yaml: line 3: did not find expected key")), WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: composed inventory could not be resolved",
	}, { // Test 7: A disk under the free space floor, whose path a sender is not told.
		Err: fmt.Errorf("%w: only 12 MiB free at %s, under the 256 MiB floor", dispatch.ErrLowSpace,
			leakMarker), WantStatus: http.StatusBadGateway, WantText: "could not launch the template",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := newSurveyHook(t, &template.Template{ID: "tpl_s", Name: "deploy",
				Playbook: "site.yml"}, &fakeSubmitter{err: test.Err})
			rec := h.deliver("d-1")
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Fatalf("delivery = %d %s, want %d %q", rec.Code, rec.Body.String(), test.WantStatus,
					test.WantText)
			}
			last := h.trigger(t).LastError
			if !strings.Contains(last, test.WantText) {
				t.Errorf("trigger last_error = %q, want it to contain %q", last, test.WantText)
			}
			if strings.Contains(rec.Body.String(), leakMarker) || strings.Contains(last, leakMarker) {
				t.Errorf("the sender read the inventory: answer %s, last_error %q", rec.Body.String(),
					last)
			}
		})
	}
}

// TestACallbackIsToldTheErrorByName pins the provisioning callback's answer to the same errors. The
// host holds the key and nothing more, so it is told the error's name. A store fault stays a 502,
// which curl --retry, the client AWX's callback examples use, retries, and so does content that
// could not be read, named.
func TestACallbackIsToldTheErrorByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err        error
		WantStatus int
		WantText   string
	}{{ // Test 0: An inventory the product cannot run.
		Err: errLeakyInventory, WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: invalid inventory document",
	}, { // Test 1: No encryption key for the template's credentials.
		Err: fmt.Errorf("%w: %s", credential.ErrNoKey, leakMarker), WantStatus: http.StatusConflict,
		WantText: "this template cannot launch: no encryption key",
	}, { // Test 2: A store outage while the inventory was composed.
		Err:        fmt.Errorf("compose %s: %w", leakMarker, errStoreOutage),
		WantStatus: http.StatusBadGateway, WantText: "could not launch the template",
	}, { // Test 3: Content that could not be read.
		Err:        fmt.Errorf("%w: input %s", errContentOutage, leakMarker),
		WantStatus: http.StatusBadGateway,
		WantText:   "this template cannot launch: inventory content could not be read",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			f := newCallbackFixture(t, &fakeSubmitter{err: test.Err}, fakeResolver{}, nil)
			key := f.mintKey(t, "tpl_cb", nil)
			rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Fatalf("callback = %d %s, want %d %q", rec.Code, rec.Body.String(), test.WantStatus,
					test.WantText)
			}
			if strings.Contains(rec.Body.String(), leakMarker) {
				t.Errorf("the host read the inventory: %s", rec.Body.String())
			}
		})
	}
}

// TestACallbackIsNotShownTheInventoryItCannotRead pins the callback's answer to a stored inventory
// that does not read as a host list, which is checked before any launch. It quoted the parse error,
// and with it whatever the inventory holds, to any host holding the key.
func TestACallbackIsNotShownTheInventoryItCannotRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := "[web]\nweb01 ansible_host=10.0.0.11\n[" + leakMarker + ":vars]\nrole=web\n"
	if _, err := inventory.Hosts(content); !errors.Is(err, inventory.ErrInvalidInventory) ||
		!strings.Contains(err.Error(), leakMarker) {
		t.Fatalf("inventory.Hosts() error = %v, want an invalid inventory quoting %q", err, leakMarker)
	}
	inventories := inventory.NewMemStore()
	if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: content, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_cb", Status: run.StatusPending}}
	f := newCallbackFixture(t, sub, fakeResolver{}, nil, WithInventories(inventories))
	key := f.mintKey(t, "tpl_cb", nil)
	rec := f.callBack("tpl_cb", "10.0.0.11", "application/json", keyBody(key))
	want := "this template's inventory could not be read as a host list: invalid inventory document"
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("callback = %d %s, want 409 %q", rec.Code, rec.Body.String(), want)
	}
	if strings.Contains(rec.Body.String(), leakMarker) {
		t.Errorf("the host read the inventory: %s", rec.Body.String())
	}
	if sub.gotRun != nil {
		t.Error("a callback against an inventory it cannot read launched a run")
	}
}

// TestAPullRequestPlanIsToldTheErrorByName pins the pull request plan's answer to an error the
// template's owner must fix: a 409 to the forge and a refusal on the pull request, each naming the
// error and nothing it wraps, since anybody who can read the pull request reads the comment. A
// store fault stays a 502, and so does content that could not be read, named to the forge, and
// neither is told to the pull request.
func TestAPullRequestPlanIsToldTheErrorByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err         error
		WantStatus  int
		WantText    string
		WantComment string
	}{{ // Test 0: An inventory the product cannot run.
		Err: errLeakyInventory, WantStatus: http.StatusConflict,
		WantText:    "this template cannot launch: invalid inventory document",
		WantComment: "This plan cannot run: invalid inventory document.",
	}, { // Test 1: A store outage while the inventory was composed.
		Err:        fmt.Errorf("compose %s: %w", leakMarker, errStoreOutage),
		WantStatus: http.StatusBadGateway, WantText: "could not plan the pull request",
	}, { // Test 2: Content that could not be read.
		Err:        fmt.Errorf("%w: input %s", errContentOutage, leakMarker),
		WantStatus: http.StatusBadGateway,
		WantText:   "this template cannot launch: inventory content could not be read",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rs := newReviewServer(t, reviewSetup{SubmitErr: test.Err})
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Fatalf("webhook = %d %s, want %d %q", rec.Code, rec.Body.String(), test.WantStatus,
					test.WantText)
			}
			if test.WantComment != "" {
				rs.waitStatus(t, rs.headSHA, "error")
			}
			rs.srv.reviews.Wait()
			var told []string
			for _, c := range rs.forge.Comments(7) {
				told = append(told, c.Body)
			}
			all := strings.Join(told, "\n")
			if test.WantComment != "" && (len(told) != 1 || !strings.Contains(all, test.WantComment)) {
				t.Errorf("comments = %q, want one saying %q", told, test.WantComment)
			}
			if test.WantComment == "" && len(told) != 0 {
				t.Errorf("comments = %q, want none for a fault the forge retries", told)
			}
			if strings.Contains(rec.Body.String(), leakMarker) || strings.Contains(all, leakMarker) {
				t.Errorf("the pull request read the inventory: answer %s, comments %q",
					rec.Body.String(), told)
			}
		})
	}
}

// TestACommentApplyIsToldTheErrorByName pins a comment apply's answer to an error the plan's owner
// must fix: a refusal, answered 202 so the forge does not redeliver it, with a reply naming the error
// and nothing it wraps. It was a failure telling the commenter to comment again, which cannot fix a
// configuration error. A store fault stays a failure on the server's side, and so does content that
// could not be read, with a reply that names it.
func TestACommentApplyIsToldTheErrorByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err        error
		WantStatus int
		WantResult string
		WantReply  string
	}{{ // Test 0: An inventory the product cannot run.
		Err: errLeakyInventory, WantStatus: http.StatusAccepted, WantResult: "refused/configuration",
		WantReply: "This apply cannot run: invalid inventory document. Nothing was applied.",
	}, { // Test 1: A credential with no secret, named only by the error's own words.
		Err:        fmt.Errorf("%w: %q", credential.ErrNoSecret, leakMarker),
		WantStatus: http.StatusAccepted, WantResult: "refused/configuration",
		WantReply: "This apply cannot run: credential has no secret yet. Nothing was applied.",
	}, { // Test 2: A store outage while the inventory was composed.
		Err:        fmt.Errorf("compose %s: %w", leakMarker, errStoreOutage),
		WantStatus: http.StatusBadGateway, WantResult: "failed/propose", WantReply: failedReply,
	}, { // Test 3: Content that could not be read, which got the same reply as a store outage.
		Err:        fmt.Errorf("%w: input %s", errContentOutage, leakMarker),
		WantStatus: http.StatusBadGateway, WantResult: "failed/propose",
		WantReply: "This apply could not start: inventory content could not be read. Nothing was " +
			"applied.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServerFrom(t, reviewSetup{ApplyErr: test.Err})
			cs.planReported(t)
			rec, out := cs.post(t, 501, forgeApproverOne, "/switchtender apply")
			if rec.Code != test.WantStatus || out["result"] != test.WantResult {
				t.Fatalf("apply comment = %d %s, want %d %s", rec.Code, rec.Body.String(),
					test.WantStatus, test.WantResult)
			}
			replies := cs.replies()
			if len(replies) != 1 || !strings.Contains(replies[0], test.WantReply) {
				t.Errorf("replies = %q, want one saying %q", replies, test.WantReply)
			}
			if strings.Contains(strings.Join(replies, "\n"), leakMarker) {
				t.Errorf("the pull request read the inventory: %q", replies)
			}
			if applies := cs.applyRuns(t); len(applies) != 0 {
				t.Errorf("a refused apply created %d runs", len(applies))
			}
		})
	}
}

// TestAPreviewTellsAFaultFromTheInventory pins the inventory preview's answers: a 422 with the
// reason for a definition that cannot resolve or content that could not be read, which the person
// previewing must look into, and a 500 with the detail kept to the log for a failure of this
// server's stores, which was a 422 quoting the store's error.
func TestAPreviewTellsAFaultFromTheInventory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Err        error
		WantStatus int
		WantText   string
		WantHidden string
	}{{ // Test 0: A store outage while the inventory was composed.
		Err: errStoreOutage, WantStatus: http.StatusInternalServerError,
		WantText: "could not resolve inventory", WantHidden: "connection refused",
	}, { // Test 1: Content that could not be read.
		Err: errContentOutage, WantStatus: http.StatusUnprocessableEntity,
		WantText: "inventory content could not be read: vault request failed",
	}, { // Test 2: A constructed inventory naming an input that is gone.
		Err: fmt.Errorf("%w: input inv_gone: %w", inventory.ErrComposition,
			inventory.ErrNotFound), WantStatus: http.StatusUnprocessableEntity,
		WantText: "input inv_gone",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithInventories(inventory.NewMemStore()),
				WithInventoryPreviewer(boundPreviewer{err: test.Err})).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/inventories/preview",
				strings.NewReader(`{"name":"web","kind":"smart","host_filter":"name__icontains=web"}`)))
			if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantText) {
				t.Fatalf("preview = %d %s, want %d %q", rec.Code, rec.Body.String(), test.WantStatus,
					test.WantText)
			}
			if test.WantHidden != "" && strings.Contains(rec.Body.String(), test.WantHidden) {
				t.Errorf("preview answered %s, want the fault's detail kept to the log", rec.Body.String())
			}
		})
	}
}
