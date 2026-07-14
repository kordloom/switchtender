package auth_test

import (
	"strings"
	"testing"

	"github.com/dcadolph/railwarden/internal/auth"
	"github.com/dcadolph/railwarden/internal/authtest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	authtest.Contract(t, func() auth.Store { return auth.NewMemStore() })
}

func TestNewToken(t *testing.T) {
	t.Parallel()
	plain, tok, err := auth.New("ci")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !strings.HasPrefix(plain, "ymt_") {
		t.Errorf("plaintext = %q, want ymt_ prefix", plain)
	}
	if tok.Hash != auth.HashToken(plain) {
		t.Error("stored hash does not match the plaintext hash")
	}
	if tok.Name != "ci" || !strings.HasPrefix(tok.ID, "tok_") {
		t.Errorf("token = %+v, want name ci and tok_ id", tok)
	}
}

func TestFromHeader(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
	}{
		{In: "Bearer ymt_abc", Want: "ymt_abc"},   // Test 0: Standard bearer.
		{In: "bearer ymt_abc", Want: "ymt_abc"},   // Test 1: Case-insensitive scheme.
		{In: "Basic dXNlcg==", Want: ""},          // Test 2: Wrong scheme.
		{In: "", Want: ""},                        // Test 3: Absent header.
		{In: "Bearer  ymt_abc ", Want: "ymt_abc"}, // Test 4: Padding trimmed.
	}
	for i, test := range tests {
		if got := auth.FromHeader(test.In); got != test.Want {
			t.Errorf("test %d: FromHeader(%q) = %q, want %q", i, test.In, got, test.Want)
		}
	}
}
