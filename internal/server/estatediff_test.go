package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestTheDiffAnswersWhatMovedSinceTheLastReview covers the question that follows the estate itself.
//
// An audit does not ask what the estate is. It asks what moved since the last review. Before the
// state history existed neither question could be answered, because every gather destroyed the
// reading before it.
func TestTheDiffAnswersWhatMovedSinceTheLastReview(t *testing.T) {
	t.Parallel()
	windowStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	gathered := windowStart.Add(24 * time.Hour)
	earlier := map[string]run.HostFacts{
		"web01": {Host: "web01", Facts: map[string]string{"kernel": "5.15.0", "role": "web"}},
		"old01": {Host: "old01", Facts: map[string]string{"kernel": "4.19.0"}},
		"same1": {Host: "same1", Facts: map[string]string{"kernel": "6.1.0"}},
	}
	later := map[string]run.HostFacts{
		"web01": {Host: "web01", GatheredAt: gathered, Facts: map[string]string{"kernel": "6.8.0"}},
		"new01": {Host: "new01", GatheredAt: gathered, Facts: map[string]string{"kernel": "6.8.0"}},
		"same1": {Host: "same1", GatheredAt: gathered, Facts: map[string]string{"kernel": "6.1.0"}},
		"old01": {Host: "old01", Facts: map[string]string{"kernel": "4.19.0"}},
	}

	got := diffEstates(windowStart, earlier, later)
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
	// old01 was in the estate at the start and nothing gathered it since, so its state is carried
	// forward rather than confirmed. It cannot be "removed": a host holds its last observed state,
	// so the earlier estate is always a subset of the later one and nothing ever leaves.
	if byHost["old01"].State != diffUnobserved {
		t.Errorf("old01 = %q, want %q", byHost["old01"].State, diffUnobserved)
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

// TestTheDiffIsBoundedByTheRequest covers a bug this endpoint was born with, of a class already
// fixed everywhere else in the product.
//
// Every list response is bounded by the request rather than by the size of the install. This one
// was not, and the window where it matters most is the one an operator is most likely to ask for:
// a fleet-wide upgrade differs on every host, so the response is largest exactly when reading a
// prefix as the whole answer is worst.
func TestTheDiffIsBoundedByTheRequest(t *testing.T) {
	t.Parallel()
	earlier := map[string]run.HostFacts{}
	later := map[string]run.HostFacts{}
	for i := 0; i < maxListRows+50; i++ {
		host := fmt.Sprintf("host%05d", i)
		earlier[host] = run.HostFacts{Host: host, Facts: map[string]string{"kernel": "5.15.0"}}
		later[host] = run.HostFacts{Host: host, Facts: map[string]string{"kernel": "6.8.0"}}
	}
	got := diffEstates(time.Time{}, earlier, later)
	shown, total := cappedList(got.Hosts)
	if total != maxListRows+50 {
		t.Fatalf("total = %d, want every differing host counted", total)
	}
	if len(shown) != maxListRows {
		t.Errorf("response carries %d hosts, want it capped at %d", len(shown), maxListRows)
	}
	if len(shown) >= total {
		t.Error("a capped response would not be reported as truncated, so a prefix reads as the " +
			"whole set of differences")
	}
}
