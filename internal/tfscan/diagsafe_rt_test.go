package tfscan

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
)

// TestFirstErrorSanitizesDiagnosticText pins that a diagnostic summary passes through util.SafeText
// before it becomes an unread entry. A diagnostic can quote a token read from a file at a pull
// request's head, so an invalid-UTF-8 or NUL byte would otherwise flow into the entry, the pull
// request comment, the API response, and the stored run, where on PostgreSQL a NUL fails the write.
func TestFirstErrorSanitizesDiagnosticText(t *testing.T) {
	t.Parallel()
	diags := hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "unexpected token \x00\xff after value",
		Subject:  &hcl.Range{Start: hcl.Pos{Line: 3}},
	}}
	got := firstError(diags)
	if strings.ContainsRune(got, 0) || !strings.ContainsRune(got, '�') {
		t.Errorf("firstError = %q, want the NUL and invalid byte replaced by the safe marker", got)
	}
}
