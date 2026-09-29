package grant_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/run"
)

// TestScopesARunAdmitsExactlyWhatARunCanName holds the two sets to each other.
//
// ScopesARun narrows a grant reduction to the objects that can decide whether a run is readable.
// It is a hand-written prefix list beside another hand-written list, run.RunAuth.Objects, and the
// two drifting apart is silent in both directions. Admitting too little hides runs from the people
// who may read them. Admitting too much reports a caller as restricted who is not, which withholds
// every install-wide total from them and empties the metrics exposition, with nothing logged and
// nothing connecting the blackout to the grant that caused it.
//
// So this asks the run package what a run names, rather than restating it.
func TestScopesARunAdmitsExactlyWhatARunCanName(t *testing.T) {
	t.Parallel()
	auth := &run.RunAuth{
		ProjectID:        "proj_one",
		InventoryID:      "inv_one",
		PullCredentialID: "cred_pull",
		CredentialIDs:    []string{"cred_ssh", "cred_vault"},
	}
	objects := auth.Objects()
	if len(objects) == 0 {
		t.Fatal("RunAuth.Objects() returned nothing, so this guard compares against an empty set")
	}
	for _, id := range objects {
		if !grant.ScopesARun(id) {
			t.Errorf("a run names %q and ScopesARun refuses it, so a grant on that object would be "+
				"dropped from the reduction and the run it governs would stop being filtered", id)
		}
	}

	// The object kinds a grant may carry that a run never names. A grant on one of these is an
	// ordinary delegation and must not be counted as something that can hide a run.
	for _, id := range []string{"tpl_deploy", grant.QueueObject("prod")} {
		if grant.ScopesARun(id) {
			t.Errorf("ScopesARun admits %q, which no run references. Counting it reports every "+
				"non-subject caller as restricted, which withholds their install-wide totals and "+
				"serves an empty metrics exposition on an install where nothing is hidden at all", id)
		}
	}
	if !grant.ValidObject("tpl_deploy") || !grant.ValidObject(grant.QueueObject("prod")) {
		t.Error("the two kinds above are no longer valid grant objects, so this guard is comparing " +
			"against objects the product does not have")
	}
}
