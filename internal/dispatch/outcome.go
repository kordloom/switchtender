package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// logDigestPage bounds how much log a single read pulls while the outcome digest is streamed, so a
// run with a large log is hashed in bounded memory rather than loaded whole.
const logDigestPage = 512

// runOutcome is the canonical, non-secret record of what a run did, the body an outcome entry
// commits a digest of. It holds only facts an auditor needs and nothing a run could carry a secret
// in: the raw log and events stay in the store, named here by their digest. The field set and its
// order are fixed, and the slices are sorted, so the same run reduces to the same bytes every time
// and a receipt holder can recompute the digest the chain committed.
type runOutcome struct {
	// RunID is the run this outcome belongs to.
	RunID string `json:"run_id"`
	// Status is the terminal status the run reached.
	Status string `json:"status"`
	// ExitCode is the tool's exit code, null when the run never produced one.
	ExitCode *int `json:"exit_code"`
	// Tool is the engine the run executed with.
	Tool string `json:"tool,omitempty"`
	// Playbook is the playbook a run executed, empty for a command tool.
	Playbook string `json:"playbook,omitempty"`
	// Inventory is the inventory the run targeted.
	Inventory string `json:"inventory,omitempty"`
	// Image is the container image reference the run executed in, empty for a host run.
	Image string `json:"image,omitempty"`
	// StartedAt is when execution began.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// EndedAt is when the run reached its terminal state.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// LogSHA256 is the SHA-256 of the run's captured log bytes in order, binding the output an
	// operator reads to this record.
	LogSHA256 string `json:"log_sha256"`
	// Hosts are the per-host outcomes, sorted by host.
	Hosts []outcomeHost `json:"hosts,omitempty"`
	// Tasks are the per-task durations, sorted by task.
	Tasks []outcomeTask `json:"tasks,omitempty"`
}

// outcomeHost is one host's result in a run, the counts an auditor reads.
type outcomeHost struct {
	// Host is the target host.
	Host string `json:"host"`
	// Worst is the most severe outcome the host reached.
	Worst string `json:"worst"`
	// OK, Changed, Failures, Unreachable, and Skipped are the per-outcome task counts.
	OK          int `json:"ok"`
	Changed     int `json:"changed"`
	Failures    int `json:"failures"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
}

// outcomeTask is one task's wall-clock cost in a run.
type outcomeTask struct {
	// Task is the task name.
	Task string `json:"task"`
	// Seconds is the task's summed wall-clock time across its occurrences.
	Seconds float64 `json:"seconds"`
}

// commitOutcome records a finished run's outcome as a tamper-evident entry, so the chain commits not
// only what was asked of the run but what it did. It is a no-op when no audit chain is configured and
// for a child of a split or pipeline, whose outcome is rolled into its parent's. The append is not
// fail-closed: the run has already happened, so a chain that cannot record it is logged loudly rather
// than pretended away, the same choice the relay makes for a worker's report.
func (d *Dispatcher) commitOutcome(r *run.Run) {
	if d.audits == nil || r.ParentID != nil {
		return
	}
	ctx := context.Background()
	body, err := d.outcomeBody(ctx, r)
	if err != nil {
		d.log.Error("dispatch: build run outcome: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		d.log.Error("dispatch: digest run outcome: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	// The outcome is the control node's own observation that the run reached a terminal state, made
	// on behalf of whoever fired it, so it is a system actor acting for the run's actor.
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(),
		Actor: "system:dispatcher", ActorType: "system", OnBehalfOf: r.Actor,
		Method: audit.MethodRun, Path: "/runs/" + r.ID + "/outcome/" + string(r.Status),
		ContentDigest: digest, Nonce: nonce,
	}
	if err := d.audits.Append(ctx, entry); err != nil {
		d.log.Error("dispatch: record run outcome: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	d.log.Debug("dispatch: committed run outcome",
		zap.String("run_id", r.ID), zap.String("receipt", audit.Receipt(entry)))
}

// outcomeBody assembles the canonical outcome record for r and returns its JSON. The host and task
// summaries are read from the store, where summarize wrote them before finalize, and the log is
// streamed to a digest so a large run is not held in memory.
func (d *Dispatcher) outcomeBody(ctx context.Context, r *run.Run) ([]byte, error) {
	logSHA, err := d.logDigest(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	hosts, err := d.store.RunHostSummaries(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	tasks, err := d.store.RunTaskSummaries(ctx, r.ID)
	if err != nil {
		return nil, err
	}

	out := runOutcome{
		RunID: r.ID, Status: string(r.Status), ExitCode: r.ExitCode,
		Tool: r.Tool, Playbook: r.Playbook, Inventory: r.Inventory, Image: r.Image,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, LogSHA256: logSHA,
	}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, outcomeHost{
			Host: h.Host, Worst: h.Worst, OK: h.OK, Changed: h.Changed,
			Failures: h.Failures, Unreachable: h.Unreachable, Skipped: h.Skipped,
		})
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, outcomeTask{Task: t.Task, Seconds: t.Seconds})
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Task < out.Tasks[j].Task })

	return json.Marshal(out)
}

// logDigest streams a run's log in order and returns the hex SHA-256 of its bytes. Paging keeps the
// memory bounded for a run whose log is large, and the ordering is the store's own log sequence.
func (d *Dispatcher) logDigest(ctx context.Context, runID string) (string, error) {
	h := sha256.New()
	var after int64
	for {
		chunks, err := d.store.LogAfter(ctx, runID, after, logDigestPage)
		if err != nil {
			return "", err
		}
		if len(chunks) == 0 {
			break
		}
		for _, c := range chunks {
			h.Write(c.Data)
			after = c.Seq
		}
		if len(chunks) < logDigestPage {
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
