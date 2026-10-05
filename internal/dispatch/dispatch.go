// Package dispatch orchestrates run execution: it accepts run requests, schedules them across a
// bounded worker pool, drives status transitions, and streams output into the store.
package dispatch

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// DefaultWorkers is the number of concurrent runs when none is configured.
const DefaultWorkers = 4

// tailPollInterval is how often the event tailer checks the sidecar file for new lines.
const tailPollInterval = 75 * time.Millisecond

// costWindow is how many recent runs per host feed the average duration used to balance splits.
const costWindow = 5

const (
	// DefaultClaimInterval is how often an idle executor polls the store for pending runs.
	DefaultClaimInterval = 250 * time.Millisecond
	// watchInterval is how often an executing run renews its lease and checks for a cancel
	// request from another process.
	watchInterval = 3 * time.Second
	// leaseTTL is how stale a lease may grow before the janitor treats its holder as dead.
	leaseTTL = 30 * time.Second
	// janitorInterval is how often stale leases are swept.
	janitorInterval = 10 * time.Second
	// overrunGrace is how long past its own timeout a run is left alone before the control node ends
	// it. The executor should end its own run, with the real exit code and captured output, so this
	// waits until it has clearly failed to.
	overrunGrace = 2 * time.Minute
	// overrunScan bounds how many running runs one sweep examines, so the check stays cheap on an
	// install with a large fleet.
	overrunScan = 500
	// idleBackoffShift is how many times an idle claim wait may double, so the ceiling is the claim
	// interval shifted by it. A dispatcher with nothing to claim backs off toward that ceiling
	// rather than hammering the store, and drops back to the base interval the moment it claims.
	idleBackoffShift = 3
	// dedupeRetryShards names the shard-retry action in the idempotency keys it dedupes under.
	dedupeRetryShards = "retry-shards"
	// dedupeRelaunchHosts names the failed-host relaunch action it dedupes under.
	dedupeRelaunchHosts = "relaunch-hosts"
)

// Publisher receives live run output for streaming to clients. All methods must be safe for
// concurrent use and must not block.
type Publisher interface {
	// PublishEvents delivers newly parsed events for a run.
	PublishEvents(id string, events []event.Event)
	// PublishLog delivers a newly written log chunk for a run.
	PublishLog(id string, chunk []byte)
	// CloseRun signals that a run has finished producing output.
	CloseRun(id string)
}

// noopPublisher discards all output. It is the default when no Publisher is configured.
type noopPublisher struct{}

// PublishEvents discards events.
func (noopPublisher) PublishEvents(string, []event.Event) {}

// PublishLog discards a log chunk.
func (noopPublisher) PublishLog(string, []byte) {}

// CloseRun does nothing.
func (noopPublisher) CloseRun(string) {}

// Dispatcher accepts run requests and executes them across a bounded worker pool.
type Dispatcher struct {
	// store persists runs and their output.
	store run.Store
	// audits commits each run's outcome to the tamper-evident chain, nil when no trail is kept.
	audits audit.Store
	// runner executes a single playbook.
	runner roundhouse.Runner
	// log records dispatcher activity.
	log *zap.Logger
	// sem bounds the number of concurrently executing runs.
	sem chan struct{}
	// wg tracks in-flight workers so Close can wait for them.
	wg sync.WaitGroup
	// notifyWG tracks in-flight webhook and email deliveries so Close waits for them to finish
	// instead of cutting them off mid-send.
	notifyWG sync.WaitGroup
	// notifyHTTP dials notification targets, nil until the guarded default is built.
	notifyHTTP *http.Client
	// notifyHTTPOnce builds the guarded default exactly once, so every delivery shares one
	// connection pool rather than each one creating and abandoning its own.
	notifyHTTPOnce sync.Once
	// ctx is canceled by Close to stop in-flight and pending runs.
	ctx context.Context
	// cancel cancels ctx.
	cancel context.CancelCauseFunc
	// publisher receives live output for streaming.
	publisher Publisher
	// hostLister enumerates inventory hosts for split runs, nil when the runner cannot list hosts.
	hostLister roundhouse.HostLister
	// invLister lists inventories through ansible-inventory to resolve smart and constructed
	// inventories, nil when the runner cannot.
	invLister roundhouse.InventoryLister
	// invReader reads inventory files with the run's own Ansible, for the cross-check an Ansible
	// run against a natively resolved inventory makes before it executes, nil when the runner
	// cannot, which refuses such a run rather than letting it execute unchecked.
	invReader roundhouse.InventoryReader
	// ansibleCoreReporter reports the server's ansible-core version, nil when the runner cannot.
	ansibleCoreReporter roundhouse.AnsibleCoreReporter
	// cmu guards cancels.
	cmu sync.Mutex
	// cancels maps a pending or executing run id to its cancel func.
	cancels map[string]context.CancelCauseFunc
	// coordinators maps a workflow coordinated in this process to a channel closed when its
	// coordinator returns, so a resumed workflow never has two coordinators here at once.
	coordinators map[string]chan struct{}
	// owner identifies this process on the leases it takes.
	owner string
	// claimInterval is how often the claim loop polls when idle.
	claimInterval time.Duration
	// wakeCh nudges the idle claim loop to poll now instead of finishing its backoff. It holds one
	// token, because one pending token already means "there is work, poll again".
	wakeCh chan struct{}
	// maxShards caps how many groups a split fans out into.
	maxShards int
	// queues names the queues this process serves; empty serves the default pool.
	queues []string
	// claimGate refuses new claims while it returns an error, for a process whose claiming is a
	// licensed feature. Nil on a single node install, where claiming is not one.
	claimGate func() error
	// claimGateSaid makes the refusal print once rather than on every attempt, since the condition
	// persists until somebody renews.
	claimGateSaid sync.Once
	// presence records that this process is polling for work, nil when nothing records it.
	presence PresenceRecorder
	// credentials resolves stored execution secrets, nil when the feature is off.
	credentials credential.Store
	// credentialTypes resolves operator-defined credential types, nil when none are configured.
	credentialTypes credential.TypeStore
	// federation mints run identity tokens for federated credentials, nil when no issuer is set.
	federation *federation.Issuer
	// runFilesRoot is the directory each run's private credential directory is created under. Empty
	// uses runfiles.DefaultRoot.
	runFilesRoot string
	// sealer decrypts credential secrets.
	sealer *credential.Sealer
	// delivery hands a relay worker the secrets the control node sealed for each run at claim, nil on
	// an executor that opens them from its own store.
	delivery SecretDelivery
	// projects resolves git projects, nil when the feature is off.
	projects project.Store
	// syncer maintains project checkouts.
	syncer *project.Syncer
	// webhooks receive terminal run notifications.
	webhooks []string
	// slackWebhooks receive a Slack-formatted terminal run notification.
	slackWebhooks []string
	// mattermostWebhooks receive the Slack-compatible payload at a Mattermost incoming webhook.
	mattermostWebhooks []string
	// rocketChatWebhooks receive the Slack-compatible payload at a Rocket.Chat incoming webhook.
	rocketChatWebhooks []string
	// discordWebhooks receive a Discord-formatted terminal run notification.
	discordWebhooks []string
	// teamsWebhooks receive a Microsoft Teams Adaptive Card terminal run notification.
	teamsWebhooks []string
	// ntfyURLs receive a terminal run notification published to an ntfy topic.
	ntfyURLs []string
	// ntfyToken is an optional bearer token for a protected ntfy topic.
	ntfyToken string
	// pagerdutyKeys are PagerDuty Events API routing keys that receive an incident when a run fails.
	pagerdutyKeys []string
	// grafanaURLs are Grafana base URLs that receive an annotation when a run finishes.
	grafanaURLs []string
	// grafanaToken is the bearer token for the Grafana annotations API.
	grafanaToken string
	// twilioSID and twilioToken authenticate the Twilio SMS API; twilioFrom is the sender number and
	// twilioTo the recipients that receive an SMS when a run fails.
	twilioSID   string
	twilioToken string
	twilioFrom  string
	twilioTo    []string
	// pagerDutyEndpoint is the PagerDuty Events API enqueue URL, and twilioBaseURL the Twilio REST
	// host. Each defaults to the real service and is a field, not a global, so a test points one
	// dispatcher at its own server without racing another test through a shared package variable.
	pagerDutyEndpoint string
	twilioBaseURL     string
	// emailer sends terminal run notifications by email, nil when email is off.
	emailer Emailer
	// emailOnFailureOnly limits email notifications to failed runs.
	emailOnFailureOnly bool
	// router finds the named notification targets attached to what a run came from, nil when none
	// are configured.
	router NotificationRouter
	// outbox records the events named notification targets hear in each run's order and delivers
	// them, nil to deliver them directly through the router.
	outbox *named.Outbox
	// inventories resolves stored inventories, nil when the feature is off.
	inventories inventory.Store
	// invSources resolves dynamic inventory sources, nil when the feature is off.
	invSources invsource.Store
	// factCache holds the facts a template's fact cache serves, nil when no executor here can.
	factCache factcache.Store
	// syncSources runs the background scheduled-sync loop for dynamic inventory sources.
	syncSources bool
	// dumper renders inventory sources to JSON.
	dumper roundhouse.InventoryDumper
	// policies gate submitted runs by holding matches for approval, nil when enforcement is off.
	policies policy.Store
	// decisions keeps the record of each decision a person makes on a held run or an approval step:
	// the masked reason and the random value its commitment hides it under, its corrections, and the
	// separation-of-duties evaluation of an agent-initiated run. Never nil.
	decisions decision.Store
	// defaultImage is the fallback execution image used when a run, its template, and its project pin
	// none. Empty leaves an unpinned run on the host.
	defaultImage string
	// imageResolver resolves a run's image tag to the digest its registry serves when the run is
	// submitted. Nil leaves every image bound to its tag.
	imageResolver ImageResolver
	// moduleFetchTimeout bounds how long the gate's module download may run.
	moduleFetchTimeout time.Duration
	// moduleFetchMaxBytes bounds what the gate's module download may write.
	moduleFetchMaxBytes int64
	// planScans keeps the scans recent module downloads fed, so one submission downloads once.
	planScans *planScanCache
	// modules keeps the module trees the gate downloaded, so the run it judged executes them.
	modules *moduleStore
	// runTimeout bounds how long a single run may execute before it is canceled and finalized failed.
	// Zero disables the cap, so a run may take as long as it needs.
	runTimeout time.Duration
	// now reads the wall clock for a run's record and outcome timestamps. It is time.Now outside the
	// demo, which parks it in the past to seed runs with a believable, self-consistent history.
	now func() time.Time
}

// errRunTimeout is the cancellation cause when a run is stopped for exceeding runTimeout, so the
// outcome can record a timeout rather than a user cancel.
var errRunTimeout = errors.New("run exceeded its timeout")

// errShuttingDown is the cancellation cause when the dispatcher itself is stopping, so a run in flight
// during a restart is recorded as interrupted rather than canceled. Without the cause every graceful
// restart left the same record a person clicking cancel leaves, and a partial retry, which accepts the
// interrupted state an unclean kill produces, refused the tidy shutdown.
var errShuttingDown = errors.New("the server stopped while this run was executing")
