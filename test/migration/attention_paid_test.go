//go:build paid

package migration

import "testing"

// TestNeedsAttentionNamesAQueueNoWorkerServes is the paid build's side of the attention scenario. A
// run on a queue no worker serves has to be named as having none, with the queue and a zero count,
// while the default queue the server's own dispatcher serves reads as served, and drop once it
// moves.
func TestNeedsAttentionNamesAQueueNoWorkerServes(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	a := in.startServer("a")

	unserved := in.must(a, "admin", "POST", "/v1/runs", map[string]any{
		"tool": "bash", "command": "true", "queue": "unserved",
	}, 202)
	queued := decodeRun(t, unserved.Body)
	lonely := in.waitAttention(a, "operator", queued.ID, "no_worker")
	if lonely.Main.Queue == nil || *lonely.Main.Queue != "unserved" ||
		lonely.Main.EligibleWorkers == nil || *lonely.Main.EligibleWorkers != 0 {
		t.Errorf("the unserved run's item = %+v, want queue unserved with no eligible worker", lonely)
	}
	if n := in.attention(a, "operator").Counts["no_worker"]; n != 1 {
		t.Errorf("no worker count = %d, want 1: the default queue the server serves is not missing", n)
	}

	in.must(a, "admin", "POST", "/v1/runs/"+queued.ID+"/cancel", nil, 200, 202)
	in.waitNotListed(a, "operator", queued.ID)
}
