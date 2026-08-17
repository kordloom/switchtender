// Package outcome commits a finished run's outcome to the audit chain. It is the single definition
// both the in-process dispatcher and the relay control node call, so a run has exactly one outcome
// entry in the same shape however it executed, and a receipt drawn from either verifies the same way.
// It also defines the run's canonical spec record and the decision entry an approval commits, since
// those are the same kind of thing: a canonical body a chain entry commits and a verifier rebuilds.
package outcome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// logDigestPage bounds how much log a single read pulls while the outcome digest is streamed, so a
// run with a large log is hashed in bounded memory rather than loaded whole.
const logDigestPage = 512

// Record is the canonical, non-secret record of what a run did, the body an outcome entry
// commits a digest of. It holds only facts an auditor needs and nothing a run could carry a secret
// in: the raw log and events stay in the store, named here by their digest. The field set and its
// order are fixed, and the slices are sorted, so the same run reduces to the same bytes every time
// and a receipt holder can recompute the digest the chain committed.
type Record struct {
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
	// SpecDigest is the digest of the run's spec as it stood when the run finished. With the same
	// digest in the decision entry, the chain proves the spec an approver decided on is the spec
	// that executed, not merely that a run with the same id did.
	SpecDigest string `json:"spec_digest,omitempty"`
	// CommitSHA is the exact project commit the run executed, stamped after the project sync, so
	// the record names the content that ran rather than a branch that has since moved.
	CommitSHA string `json:"commit_sha,omitempty"`
	// DryRun reports the run executed in its tool's no-change mode, so a preview cannot later be
	// presented as the change itself.
	DryRun bool `json:"dry_run,omitempty"`
	// Hosts are the per-host outcomes, sorted by host.
	Hosts []RecordHost `json:"hosts,omitempty"`
	// Tasks are the per-task durations, sorted by task.
	Tasks []RecordTask `json:"tasks,omitempty"`
}

// RecordHost is one host's result in a run, the counts an auditor reads.
type RecordHost struct {
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

// RecordTask is one task's wall-clock cost in a run.
type RecordTask struct {
	// Task is the task name.
	Task string `json:"task"`
	// Milliseconds is the task's summed wall-clock time across its occurrences. The record keeps
	// integer milliseconds because the LoomSeal JCS profile refuses fractional numbers, so a float
	// here would make the record unsignable.
	Milliseconds int64 `json:"ms"`
}

// Commit records a finished run's outcome as a tamper-evident chain entry, so the chain commits not
// only what was asked of the run but what it did. committer names the process that observed the run
// finish, a system actor acting on behalf of whoever fired the run. The caller decides when to call
// this; Commit assumes the run is terminal and its evidence is in store.
func Commit(ctx context.Context, audits audit.Store, store run.Store, r *run.Run, committer string) error {
	body, err := Body(ctx, store, r)
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return err
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(),
		Actor: committer, ActorType: "system", OnBehalfOf: r.Actor,
		Method: audit.MethodRun, Path: "/runs/" + r.ID + "/outcome/" + string(r.Status),
		ContentDigest: digest, Nonce: nonce,
	}
	return audits.Append(ctx, entry)
}

// Body assembles the canonical outcome record for r and returns its JSON. The host and task
// summaries are read from the store, where they were written before the run finished, and the log is
// streamed to a digest so a large run is not held in memory. It is exported so a verifier can rebuild
// the same bytes and confirm they commit to the digest the chain holds.
func Body(ctx context.Context, store run.Store, r *run.Run) ([]byte, error) {
	logSHA, err := logDigest(ctx, store, r.ID)
	if err != nil {
		return nil, err
	}
	hosts, err := store.RunHostSummaries(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	tasks, err := store.RunTaskSummaries(ctx, r.ID)
	if err != nil {
		return nil, err
	}

	specDigest, err := SpecDigest(r)
	if err != nil {
		return nil, err
	}
	out := Record{
		RunID: r.ID, Status: string(r.Status), ExitCode: r.ExitCode,
		Tool: r.Tool, Playbook: r.Playbook, Inventory: r.Inventory, Image: r.Image,
		StartedAt: utcOrNil(r.StartedAt), EndedAt: utcOrNil(r.EndedAt), LogSHA256: logSHA,
		SpecDigest: specDigest, CommitSHA: r.CommitSHA, DryRun: r.DryRun,
	}
	for _, h := range hosts {
		out.Hosts = append(out.Hosts, RecordHost{
			Host: h.Host, Worst: h.Worst, OK: h.OK, Changed: h.Changed,
			Failures: h.Failures, Unreachable: h.Unreachable, Skipped: h.Skipped,
		})
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, RecordTask{Task: t.Task, Milliseconds: int64(math.Round(t.Seconds * 1000))})
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].Task < out.Tasks[j].Task })

	return json.Marshal(out)
}

// utcOrNil normalizes a timestamp to UTC, keeping nil as nil.
//
// The record has to reduce to the same bytes whichever copy of the run it is built from. A run in
// memory carries the server's local offset, and the store writes and reads every timestamp as UTC,
// so a record built at commit time and the same record rebuilt from the stored run differed by an
// offset on every install outside UTC. The digest is over these bytes, so the receipt that discloses
// the outcome reported it as not matching the chain: the flagship artifact failed to verify
// everywhere except one time zone, and every test built its timestamps in that zone.
func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// logDigest streams a run's log in order and returns the hex SHA-256 of its bytes. Paging keeps the
// memory bounded for a run whose log is large, and the ordering is the store's own log sequence.
func logDigest(ctx context.Context, store run.Store, runID string) (string, error) {
	h := sha256.New()
	var after int64
	for {
		chunks, err := store.LogAfter(ctx, runID, after, logDigestPage)
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

// Parse decodes a disclosed outcome body, the JSON Body produces, so a verifier can read what a run
// did after confirming the body matches the digest the chain committed.
func Parse(body []byte) (Record, error) {
	var rec Record
	err := json.Unmarshal(body, &rec)
	return rec, err
}
