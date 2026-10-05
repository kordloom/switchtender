package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/util"
)

// Kinds of report a record carries.
const (
	// KindPlan reports a plan run through its phases to its final result.
	KindPlan = "plan"
	// KindRefusal reports that a pull request was not planned, with a comment and a commit status.
	KindRefusal = "refusal"
	// KindFork reports that a pull request from a fork was not planned, with a commit status alone,
	// so opening pull requests from forks can never make the operator's token write comments.
	KindFork = "fork"
	// KindNoHosts reports that a pull request's plan was skipped because the template's inventory
	// matched no hosts, with a commit status alone, the way a schedule or a webhook fire that matched
	// no hosts is skipped rather than failed.
	KindNoHosts = "no_hosts"
)

// Record is one report's state: what its pull request was last told, and the claim and retry
// bookkeeping that sends each report once across every process sharing the store. A plan has one
// record for its whole life, keyed by its run id. A refusal has one keyed by what was refused.
type Record struct {
	// ID identifies the record: the plan's run id, or for a refusal an id derived from the trigger,
	// the pull request, the commit, and the reason, so a redelivered webhook finds the record its
	// first delivery made.
	ID string
	// Kind is plan, refusal, fork, or no_hosts.
	Kind string
	// RunID is the plan run, empty for a refusal.
	RunID string
	// TriggerID is the review trigger the report posts through.
	TriggerID string
	// PullRequest is the pull request number on GitHub, the merge request iid on GitLab.
	PullRequest int
	// Provider is the forge the pull request is on, github or gitlab, fixed from the trigger when
	// the record is made. Empty on a record an earlier release made, which takes its trigger's
	// destination at its first report and keeps it from then on.
	Provider string
	// APIURL is the API base of the forge the pull request is on, fixed with Provider.
	APIURL string
	// Repository is the repository the pull request belongs to, fixed with Provider. The report
	// goes there even when the trigger is pointed at another repository later, since pull request
	// numbers repeat across repositories and the plan describes this one's proposal.
	Repository string
	// StatusContext is the commit status context the record's first status was set under. Every
	// later status of the record is set under it too, so a template renamed while its plan is in
	// flight cannot leave the first status pending on the commit for good.
	StatusContext string
	// CommitSHA is the commit a refusal describes. A plan's commit is read from its run.
	CommitSHA string
	// Reason explains a refusal in one sentence, empty for a plan.
	Reason string
	// Receipt is the chain receipt of the webhook a refusal answers, empty for a plan, whose
	// receipt is on its run.
	Receipt string
	// Phase is the phase last reported, empty until a report has landed.
	Phase string
	// HeldBy is the rule the last held report named.
	HeldBy string
	// StatusState is the commit status state last set, empty when none was.
	StatusState string
	// CommentSHA256 is the hex SHA-256 of the comment body last written, empty when the last report
	// left the comment alone.
	CommentSHA256 string
	// RecordedSHA256 is the hex SHA-256 of the report content last committed to the audit chain, so
	// a retry of the same report is not recorded a second time.
	RecordedSHA256 string
	// ReportedAt is when the last report landed, by the store's clock, zero until one has.
	ReportedAt time.Time
	// Done reports that nothing more will be reported: the final report landed, the record closed
	// with nothing left to say, or reporting gave up.
	Done bool
	// Version moves on with every claim and every settle. A claim is a compare-and-set on the
	// version the claimer read, and a settle is fenced on the version its claim produced.
	Version int64
	// ClaimedBy names the process holding the record, empty when none does.
	ClaimedBy string
	// ClaimedUntil is when the claim lapses, by the store's clock.
	ClaimedUntil time.Time
	// Attempts counts the consecutive attempts that failed, zero once a report lands.
	Attempts int
	// FailingSince is when the current run of failed attempts began, zero when the last landed.
	FailingSince time.Time
	// RetryAt is the earliest the next attempt may go out after a failure, by the store's clock.
	RetryAt time.Time
	// LastError is what the last attempt failed on, or why reporting stopped. The run shows it.
	LastError string
	// CreatedAt is when the record was made.
	CreatedAt time.Time
}

// claimedAt reports whether another process's claim on the record is still live at now.
func (r *Record) claimedAt(now time.Time) bool {
	return r.ClaimedBy != "" && r.ClaimedUntil.After(now)
}

// sameLane reports whether two records report to the same pull request through the same trigger,
// which is to say they write the same comment.
func (r *Record) sameLane(o *Record) bool {
	return r.TriggerID == o.TriggerID && r.PullRequest == o.PullRequest
}

// State returns what a plan's run shows about its pull request.
func (r *Record) State() *ReportState {
	s := &ReportState{
		PullRequest: r.PullRequest, Phase: r.Phase, StatusState: r.StatusState,
		CommentSHA256: r.CommentSHA256, Done: r.Done, Attempts: r.Attempts, LastError: r.LastError,
	}
	if !r.ReportedAt.IsZero() {
		at := r.ReportedAt.UTC()
		s.ReportedAt = &at
	}
	if !r.Done && !r.RetryAt.IsZero() {
		at := r.RetryAt.UTC()
		s.RetryAt = &at
	}
	return s
}

// ReportState is what a plan's run shows about its pull request: what the pull request was last
// told, and the forge failure a report is retrying, if there is one.
type ReportState struct {
	// PullRequest is the pull request or merge request number.
	PullRequest int `json:"pull_request"`
	// Phase is the phase last reported, empty until a report has landed.
	Phase string `json:"phase,omitempty"`
	// StatusState is the commit status state last set.
	StatusState string `json:"status_state,omitempty"`
	// CommentSHA256 is the SHA-256 of the comment body last written.
	CommentSHA256 string `json:"comment_sha256,omitempty"`
	// ReportedAt is when the last report landed.
	ReportedAt *time.Time `json:"reported_at,omitempty"`
	// Done reports that nothing more will be reported.
	Done bool `json:"done"`
	// Attempts counts the consecutive attempts that failed.
	Attempts int `json:"attempts,omitempty"`
	// RetryAt is when the next attempt goes out, while one is pending.
	RetryAt *time.Time `json:"retry_at,omitempty"`
	// LastError is what the last attempt failed on, or why reporting stopped.
	LastError string `json:"last_error,omitempty"`
}

// PlanRecord returns a new record for the plan runID, reporting to pull request number through the
// trigger tg, on the forge and in the repository tg reviews now.
func PlanRecord(runID string, tg *trigger.Trigger, number int, at time.Time) *Record {
	rec := &Record{ID: runID, Kind: KindPlan, RunID: runID, TriggerID: tg.ID,
		PullRequest: number, CreatedAt: at}
	rec.setDestination(tg.Review)
	return rec
}

// RefusalRecord returns a new record for a pull request that was not planned. A fork's refusal is
// its own kind, which sets the commit status alone. The id is derived from everything the report
// says, so the same refusal delivered twice is one record and is reported once.
func RefusalRecord(kind string, tg *trigger.Trigger, number int, sha, reason, receipt string,
	at time.Time) *Record {
	sum := sha256.Sum256([]byte(kind + "\x00" + tg.ID + "\x00" + strconv.Itoa(number) +
		"\x00" + sha + "\x00" + reason))
	rec := &Record{
		ID: "rfs_" + hex.EncodeToString(sum[:12]), Kind: kind, TriggerID: tg.ID,
		PullRequest: number, CommitSHA: sha, Reason: reason, Receipt: receipt, CreatedAt: at,
	}
	rec.setDestination(tg.Review)
	return rec
}

// Sanitize replaces anything in the record's text that a text column cannot hold, a NUL byte or a
// byte sequence that is not UTF-8, so every backend stores the same record. Every store calls it
// before it writes.
//
// The text is somebody else's. LastError carries what a forge answered, a refusal's reason and a
// holding rule's name come from a webhook and a policy, and the destination from a trigger. SQLite
// stores such bytes and PostgreSQL refuses them with SQLSTATE 22021, and a settle PostgreSQL refused
// left the record claimed until the claim lapsed and then failing the same way on every attempt
// after, so the pull request was never told anything more.
func (r *Record) Sanitize() {
	if r == nil {
		return
	}
	r.CommitSHA, r.Reason, r.Receipt = util.SafeText(r.CommitSHA), util.SafeText(r.Reason),
		util.SafeText(r.Receipt)
	r.Phase, r.HeldBy = util.SafeText(r.Phase), util.SafeText(r.HeldBy)
	r.StatusState, r.LastError = util.SafeText(r.StatusState), util.SafeText(r.LastError)
	r.Provider, r.APIURL = util.SafeText(r.Provider), util.SafeText(r.APIURL)
	r.Repository, r.StatusContext = util.SafeText(r.Repository), util.SafeText(r.StatusContext)
}

// setDestination fixes the forge and repository the record reports to from cfg, a trigger's
// review configuration. A nil cfg leaves them empty.
func (r *Record) setDestination(cfg *trigger.Review) {
	if cfg == nil {
		return
	}
	r.Provider, r.APIURL, r.Repository = cfg.Provider, cfg.BaseURL(), cfg.Repository
}

// Store persists report records. Implementations must be safe for concurrent use, and Claim must be
// atomic across every process sharing the store.
type Store interface {
	// Create inserts rec when no record with its id exists and reports whether it did. A second
	// create for the same id changes nothing, so a redelivered webhook, or two replicas adopting
	// the same plan, leave one record.
	Create(ctx context.Context, rec *Record) (bool, error)
	// Get returns the record with id, or ErrRecordNotFound.
	Get(ctx context.Context, id string) (*Record, error)
	// Pending returns up to limit records that are not done, oldest first. A limit of zero or less
	// returns all of them.
	Pending(ctx context.Context, limit int) ([]*Record, error)
	// Claim takes the record for owner until until and reports whether this caller won. It is the
	// scheduler's claim: a compare-and-set on the version the caller read, so of every process
	// that read the same version exactly one wins, and a won claim moves the version on by one. It
	// also loses while another record of the same pull request and trigger holds a claim still
	// live at now, so one pull request's comment is written by one process at a time. A record
	// that is done, or gone, loses without an error.
	Claim(ctx context.Context, id string, version int64, owner string, now, until time.Time) (bool, error)
	// Settle writes rec's reporting state and releases its claim, fenced on version being the one
	// the claim produced, and reports whether it landed. A false means the claim lapsed and another
	// process took the record, so nothing was written.
	Settle(ctx context.Context, rec *Record, version int64) (bool, error)
}

// memStore is an in-memory Store guarded by a mutex. It serves a single process: a server with no
// database keeps its reports here, and they do not survive a restart.
type memStore struct {
	// mu guards records.
	mu sync.Mutex
	// records maps record id to the stored record.
	records map[string]*Record
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{records: map[string]*Record{}}
}

// Create inserts rec when its id is new.
func (m *memStore) Create(_ context.Context, rec *Record) (bool, error) {
	rec.Sanitize()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[rec.ID]; ok {
		return false, nil
	}
	cp := *rec
	m.records[rec.ID] = &cp
	return true, nil
}

// Get returns a copy of the record with id.
func (m *memStore) Get(_ context.Context, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return nil, ErrRecordNotFound
	}
	cp := *r
	return &cp, nil
}

// Pending returns copies of the records not yet done, oldest first.
func (m *memStore) Pending(_ context.Context, limit int) ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*Record{}
	for _, r := range m.records {
		if r.Done {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	SortRecords(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Claim takes the record for owner when its version is still version and no other record of its
// pull request holds a live claim.
func (m *memStore) Claim(_ context.Context, id string, version int64, owner string, now,
	until time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok || r.Done || r.Version != version {
		return false, nil
	}
	for _, o := range m.records {
		if o.ID != id && o.sameLane(r) && o.claimedAt(now) {
			return false, nil
		}
	}
	r.Version++
	r.ClaimedBy, r.ClaimedUntil = owner, until
	return true, nil
}

// Settle writes rec's reporting state when the stored version is still version.
func (m *memStore) Settle(_ context.Context, rec *Record, version int64) (bool, error) {
	rec.Sanitize()
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[rec.ID]
	if !ok || r.Version != version {
		return false, nil
	}
	applySettle(r, rec)
	return true, nil
}

// applySettle copies what a settle writes from rec onto stored, releases the claim, and moves the
// version on, the single definition the in-memory store follows and the SQL stores mirror.
func applySettle(stored, rec *Record) {
	stored.Phase, stored.HeldBy, stored.StatusState = rec.Phase, rec.HeldBy, rec.StatusState
	stored.CommentSHA256, stored.RecordedSHA256 = rec.CommentSHA256, rec.RecordedSHA256
	stored.ReportedAt, stored.Done = rec.ReportedAt, rec.Done
	stored.Provider, stored.APIURL, stored.Repository = rec.Provider, rec.APIURL, rec.Repository
	stored.StatusContext = rec.StatusContext
	stored.Attempts, stored.FailingSince = rec.Attempts, rec.FailingSince
	stored.RetryAt, stored.LastError = rec.RetryAt, rec.LastError
	stored.ClaimedBy, stored.ClaimedUntil = "", time.Time{}
	stored.Version++
}

// SortRecords orders records oldest first, by creation time and then id, the order Pending answers
// in on every store.
func SortRecords(list []*Record) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
			return list[i].CreatedAt.Before(list[j].CreatedAt)
		}
		return list[i].ID < list[j].ID
	})
}
