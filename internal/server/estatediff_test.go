package server

import (
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestTheDiffAnswersWhatMovedSinceTheLastReview covers the question that follows the estate itself.
//
// An audit does not ask what the estate is. It asks what moved since the last review. Before the
// state history existed neither question could be answered, because every gather destroyed the
// reading before it.
func TestTheDiffAnswersWhatMovedSinceTheLastReview(t *testing.T) {
	t.Parallel()
	earlier := map[string]run.HostFacts{
		"web01": {Host: "web01", Facts: map[string]string{"kernel": "5.15.0", "role": "web"}},
		"old01": {Host: "old01", Facts: map[string]string{"kernel": "4.19.0"}},
		"same1": {Host: "same1", Facts: map[string]string{"kernel": "6.1.0"}},
	}
	later := map[string]run.HostFacts{
		"web01": {Host: "web01", Facts: map[string]string{"kernel": "6.8.0"}},
		"new01": {Host: "new01", Facts: map[string]string{"kernel": "6.8.0"}},
		"same1": {Host: "same1", Facts: map[string]string{"kernel": "6.1.0"}},
	}

	got := diffEstates(earlier, later)
	if got.Unchanged != 1 {
		t.Errorf("unchanged = %d, want 1. Without it a short list of differences reads as a query "+
			"that found nothing rather than a quiet estate", got.Unchanged)
	}
	byHost := map[string]hostDiff{}
	for _, h := range got.Hosts {
		byHost[h.Host] = h
	}
	if byHost["new01"].State != diffAdded {
		t.Errorf("new01 = %q, want %q", byHost["new01"].State, diffAdded)
	}
	if byHost["old01"].State != diffRemoved {
		t.Errorf("old01 = %q, want %q", byHost["old01"].State, diffRemoved)
	}

	web := byHost["web01"]
	if web.State != diffChanged {
		t.Fatalf("web01 = %q, want %q", web.State, diffChanged)
	}
	// The values at each end, because "kernel changed" is not an answer an auditor can use.
	if web.Facts["kernel"].From != "5.15.0" || web.Facts["kernel"].To != "6.8.0" {
		t.Errorf("kernel change = %+v, want 5.15.0 to 6.8.0", web.Facts["kernel"])
	}
	// A fact the host stopped reporting is a change too. Reading only the later side would miss it
	// and call the host unchanged on that key.
	if role, ok := web.Facts["role"]; !ok || role.From != "web" || role.To != "" {
		t.Errorf("a fact that disappeared was not reported as a change: %+v", web.Facts)
	}

	// Ordered, so two identical requests render the same way.
	for i := 1; i < len(got.Hosts); i++ {
		if got.Hosts[i-1].Host > got.Hosts[i].Host {
			t.Fatalf("hosts are not ordered: %v", got.Hosts)
		}
	}
}
