package grant_test

import (
	"testing"

	"github.com/dcadolph/yardmaster/internal/grant"
	"github.com/dcadolph/yardmaster/internal/granttest"
)

func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	granttest.Contract(t, func() grant.Store { return grant.NewMemStore() })
}

func TestSatisfies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Have grant.Access
		Want grant.Access
		OK   bool
	}{
		{Have: grant.AccessManage, Want: grant.AccessUse, OK: true},    // Test 0: Manage implies use.
		{Have: grant.AccessManage, Want: grant.AccessManage, OK: true}, // Test 1: Manage meets manage.
		{Have: grant.AccessUse, Want: grant.AccessUse, OK: true},       // Test 2: Use meets use.
		{Have: grant.AccessUse, Want: grant.AccessManage, OK: false},   // Test 3: Use does not meet manage.
	}
	for i, test := range tests {
		if got := grant.Satisfies(test.Have, test.Want); got != test.OK {
			t.Errorf("test %d: Satisfies(%q,%q) = %v, want %v", i, test.Have, test.Want, got, test.OK)
		}
	}
}

func TestValidators(t *testing.T) {
	t.Parallel()
	if !grant.ValidSubject("user_1") || !grant.ValidSubject("team_1") {
		t.Error("ValidSubject rejected a valid subject")
	}
	if grant.ValidSubject("proj_1") {
		t.Error("ValidSubject accepted a non-subject")
	}
	if !grant.ValidObject("proj_1") || !grant.ValidObject("tpl_1") ||
		!grant.ValidObject("inv_1") || !grant.ValidObject("cred_1") {
		t.Error("ValidObject rejected a valid object")
	}
	if grant.ValidObject("user_1") {
		t.Error("ValidObject accepted a non-object")
	}
	if !grant.ValidAccess(grant.AccessUse) || grant.ValidAccess(grant.Access("bogus")) {
		t.Error("ValidAccess wrong")
	}
}
