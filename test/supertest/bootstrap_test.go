package main

import (
	"fmt"
	"testing"
)

// TestBootstrapFindsTheInitialToken pins how the harness finds the first admin token in a server's
// boot log. A pod has no terminal, so the server writes the token to a file and names the file in
// its log instead of printing it. The harness looked only for a printed token, so every run after
// that change failed at the first phase with "no server pod printed an initial admin token".
func TestBootstrapFindsTheInitialToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Logs      string
		WantToken string
		WantPath  string
	}{{ // Test 0: The banner a pod writes, naming the file.
		Logs: `creating a new database at /data/switchtender.db.

  No API tokens exist, so :8080 would have served an unauthenticated API.
  Created an initial admin token and wrote it to a file readable by this account alone,
  rather than to this log, which keeps whatever it is given:

      /data/initial-admin-token

  Read it, delete the file, and use it as: Authorization: Bearer <token>
`,
		WantPath: "/data/initial-admin-token",
	}, { // Test 1: The banner a terminal gets, printing the token.
		Logs: `
  No API tokens exist, so :8080 would have served an unauthenticated API.
  Created an initial admin token instead. It is shown only this once:

      swt_abc123

  Use it as:     Authorization: Bearer swt_abc123
`,
		WantToken: "swt_abc123",
	}, { // Test 2: A pod that lost first-boot initialization says neither.
		Logs: `{"level":"info","msg":"switchtender serving","addr":":8080"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := parseInitialToken(test.Logs); got != test.WantToken {
				t.Errorf("parseInitialToken() = %q, want %q", got, test.WantToken)
			}
			if got := parseInitialTokenPath(test.Logs); got != test.WantPath {
				t.Errorf("parseInitialTokenPath() = %q, want %q", got, test.WantPath)
			}
		})
	}
}

// TestTokenInIgnoresKubectlNotices pins that reading the token file keeps only the token, since
// kubectl exec can print its own notices beside the file's contents.
func TestTokenInIgnoresKubectlNotices(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Out       string
		WantToken string
	}{{ // Test 0: The file alone.
		Out: "swt_abc123\n", WantToken: "swt_abc123",
	}, { // Test 1: A kubectl notice before it.
		Out: "Defaulted container \"server\" out of: server, init\nswt_abc123\n", WantToken: "swt_abc123",
	}, { // Test 2: No token at all.
		Out: "cat: can't open '/data/initial-admin-token': No such file or directory\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := tokenIn(test.Out); got != test.WantToken {
				t.Errorf("tokenIn() = %q, want %q", got, test.WantToken)
			}
		})
	}
}
