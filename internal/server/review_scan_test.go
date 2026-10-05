package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestReviewHookRefusesAPlanThatRunsAProgram proves a pull request whose Terraform configuration
// runs a program while it plans, or cannot be read in full, is not planned: a plan of it would run
// code from an unmerged branch with the template's credentials. No run is created, the refusal is
// recorded, the pull request is told what was found or could not be read, and its status is an
// error. The same template with a configuration that runs nothing is planned.
//
//nolint:funlen // Test function.
func TestReviewHookRefusesAPlanThatRunsAProgram(t *testing.T) {
	t.Parallel()
	const plan = "  + aws_instance.web\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
	tests := []struct {
		// Files is what the pull request's head carries.
		Files map[string]string
		// WantRefused is whether the plan is refused.
		WantRefused bool
		// WantComment are fragments the refusal comment must carry.
		WantComment []string
		// WantReason is the webhook answer's refusal reason.
		WantReason string
	}{{ // Test 0: An external data source in the configuration is refused, named by its address.
		Files: map[string]string{"infra/plan.txt": plan,
			"infra/main.tf": "data \"external\" \"lookup\" {\n  program = [\"curl\", \"x\"]\n}\n"},
		WantRefused: true,
		WantComment: []string{"runs a program while it plans",
			"data.external.lookup runs a program during plan (infra/main.tf line 1)"},
		WantReason: "the configuration runs a program while it plans",
	}, { // Test 1: One behind a module the pull request adds is refused too.
		Files: map[string]string{"infra/plan.txt": plan,
			"infra/main.tf":          "module \"probe\" {\n  source = \"../modules/probe\"\n}\n",
			"modules/probe/probe.tf": "data \"external\" \"p\" {\n  program = [\"true\"]\n}\n"},
		WantRefused: true,
		WantComment: []string{"module.probe.data.external.p"},
		WantReason:  "the configuration runs a program while it plans",
	}, { // Test 2: A registry module the gate cannot download, since this runner downloads
		// nothing, leaves the plan unclassified, so refused, with why.
		Files: map[string]string{"infra/plan.txt": plan,
			"infra/main.tf": "module \"vpc\" {\n  source = \"terraform-aws-modules/vpc/aws\"\n}\n"},
		WantRefused: true,
		WantComment: []string{"could not be read in full",
			`could not read module.vpc from "terraform-aws-modules/vpc/aws"`,
			"not downloaded, since this server's runner cannot download modules for the gate"},
		WantReason: "the configuration could not be read in full",
	}, { // Test 3: A configuration that runs nothing is planned.
		Files: map[string]string{"infra/plan.txt": plan,
			"infra/main.tf": "resource \"terraform_data\" \"x\" {\n}\n"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rs := newReviewServer(t, reviewSetup{PRFiles: test.Files})
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook = %d %s, want 202", rec.Code, rec.Body.String())
			}
			if !test.WantRefused {
				if r := rs.planRun(t, rec); !r.DryRun {
					t.Error("the plan is not a dry run")
				}
				return
			}
			if !strings.Contains(rec.Body.String(), test.WantReason) {
				t.Errorf("webhook answer %s, want the reason %q", rec.Body.String(), test.WantReason)
			}
			st := rs.waitStatus(t, rs.headSHA, "error")
			rs.srv.reviews.Wait()
			if !strings.HasPrefix(st.Description, "Not planned") {
				t.Errorf("status description = %q, want the refusal", st.Description)
			}
			list, err := rs.runs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 0 {
				t.Errorf("a refused plan created %d runs", len(list))
			}
			c := rs.forge.Comments(7)
			if len(c) != 1 {
				t.Fatalf("comments = %+v, want one", c)
			}
			for _, want := range test.WantComment {
				if !strings.Contains(c[0].Body, want) {
					t.Errorf("comment %q does not say %q", c[0].Body, want)
				}
			}
			if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/refused") {
				t.Error("the refusal is not on the chain")
			}
			if specs := rs.specs.all(); len(specs) != 0 {
				t.Errorf("a refused plan executed %d specs", len(specs))
			}
		})
	}
}
