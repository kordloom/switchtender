package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/term"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/evidence"
	"github.com/kordloom/switchtender/internal/extplugin"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/forward"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/imageref"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/retention"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/safedial"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/util"
	"github.com/kordloom/switchtender/spanbeat"
)

const (
	// defaultServeAddr is the address the server listens on when --addr is not set. It binds loopback
	// so a fresh server is not exposed on the network before the operator creates the first token.
	defaultServeAddr = "127.0.0.1:8080"
	// defaultDBPath is the SQLite database file used when --db is not set.
	defaultDBPath = "switchtender.db"
	// shutdownTimeout bounds how long graceful HTTP shutdown waits for in-flight requests.
	shutdownTimeout = 15 * time.Second
	// readHeaderTimeout bounds how long the server waits to read request headers, closing a
	// slowloris connection that dribbles them.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds how long the server waits to read a full request, closing a client that
	// dribbles a body to hold a connection open. It is generous enough for a large import upload.
	readTimeout = 2 * time.Minute
	// idleTimeout bounds how long a keep-alive connection may sit idle before it is closed, so
	// abandoned connections do not accumulate. No WriteTimeout is set, since live SSE streams are
	// long-lived by design and a write deadline would sever them.
	idleTimeout = 2 * time.Minute
)

// serveAddr holds the value of the --addr flag.
var serveAddr string

// serveDB holds the value of the --db flag.
var serveDB string

// policyFile names a YAML file holding the approval policies, making the file the source of truth
// instead of the database. A policy decides which runs a person has to approve, so who may change it
// is the whole question, and a file answers it with review and a commit history.
var policyFile string

// serveListener, when set, is the already-bound listener the server uses instead of binding
// serveAddr itself. The desktop command sets it so the port it chose cannot be taken by another
// process before the server starts.
var serveListener net.Listener

// serveTLSCert and serveTLSKey hold the TLS certificate and key file paths. When both are set the
// server speaks HTTPS, so it needs no reverse proxy in front of it.
var (
	serveTLSCert string
	serveTLSKey  string
)

// serveTrustedProxy names networks in CIDR form whose forwarding headers the server believes, and
// serveClientIPHeader names the header they set. Without these, every client behind one reverse
// proxy shares a rate-limit key, so one stranger's failed sign-ins refuse everybody.
var (
	serveTrustedProxy   []string
	serveClientIPHeader string
)

// scheduleInterval holds the value of the --schedule-interval flag.
var scheduleInterval time.Duration

// notifyWebhooks holds the values of the repeatable --notify-webhook flag.
var notifyWebhooks []string

// notifySlack holds the values of the repeatable --notify-slack flag.
var notifySlack []string

// notifyMattermost holds the values of the repeatable --notify-mattermost flag.
var notifyMattermost []string

// notifyRocketChat holds the values of the repeatable --notify-rocketchat flag.
var notifyRocketChat []string

// notifyDiscord holds the values of the repeatable --notify-discord flag.
var notifyDiscord []string

// notifyTeams holds the values of the repeatable --notify-teams flag.
var notifyTeams []string

// notifyNtfy holds the values of the repeatable --notify-ntfy flag.
var notifyNtfy []string

// notifyNtfyToken holds the value of the --notify-ntfy-token flag.
var notifyNtfyToken string

// notifyPagerDuty holds the values of the repeatable --notify-pagerduty flag.
var notifyPagerDuty []string

// notifyGrafana holds the values of the repeatable --notify-grafana flag.
var notifyGrafana []string

// notifyGrafanaToken holds the value of the --notify-grafana-token flag.
var notifyGrafanaToken string

// notifyTwilioSID, notifyTwilioToken, and notifyTwilioFrom hold the Twilio account and sender flags.
var notifyTwilioSID, notifyTwilioToken, notifyTwilioFrom string

// notifyTwilioTo holds the values of the repeatable --notify-twilio-to flag.
var notifyTwilioTo []string

// serveAllowContainerEE holds the value of the --allow-container-ee flag.
var serveAllowContainerEE bool

// serveDefaultImage holds the value of the --default-image flag, the fallback execution image used
// when a run, its template, and its project pin none.
var serveDefaultImage string

// serveRequireImageDigest holds the value of the --require-image-digest flag.
var serveRequireImageDigest bool

// serveImageDigestLookup holds the value of the --image-digest-lookup flag: whether a submitted run's
// image tag is resolved to the digest its registry serves, so the run executes by that digest.
var serveImageDigestLookup bool

// serveWorkers holds the value of the serve --workers flag.
var serveWorkers int

// serveRunTimeout holds the value of the --run-timeout flag, the default cap on how long a run may
// execute. Zero disables the cap.
var serveRunTimeout time.Duration

// serveStrictGrants holds the value of the --strict-grants flag.
var serveStrictGrants bool

// serveReadOnly holds the value of the --read-only flag.
var serveReadOnly bool

// serveWorkerPools holds the value of the --worker-pools flag. It confines each worker token to the
// queues it may lease from, which is what turns a queue from a routing hint into a boundary.
var serveWorkerPools string

// serveWorkerToken holds the value of the --worker-token flag. The mesh relay worker endpoints turn
// on when it or SWITCHTENDER_WORKER_TOKEN is set.
var serveWorkerToken string

// serveMatrixCap holds the value of the --matrix-cap flag.
var serveMatrixCap int

// serveMaxShards holds the value of the --max-shards flag.
var serveMaxShards int

// servePluginsDir holds the value of the --plugins-dir flag.
var servePluginsDir string

// serveOIDCIssuer, serveOIDCClientID, serveOIDCRedirectURL, and serveOIDCDefaultRole hold the
// OpenID Connect single sign-on flags. The client secret comes from SWITCHTENDER_OIDC_CLIENT_SECRET.
var (
	serveOIDCIssuer      string
	serveOIDCClientID    string
	serveOIDCRedirectURL string
	serveOIDCDefaultRole string
)

// serveLDAP* hold the LDAP directory sign-in flags. The service bind password comes from
// SWITCHTENDER_LDAP_PASSWORD.
var (
	serveLDAPURL         string
	serveLDAPBindDN      string
	serveLDAPBaseDN      string
	serveLDAPUserFilter  string
	serveLDAPDefaultRole string
	serveLDAPRoleMap     []string
)

// servePublicURL is the externally reachable address of this server, used to link a pull request
// review comment and commit status to the run they describe. Empty posts run ids without links.
var servePublicURL string

// serveSAML* hold the SAML single sign-on flags. SwitchTender is the service provider and the
// certificate and key files are its PEM keypair.
var (
	serveSAMLIDPMetadataURL string
	serveSAMLBaseURL        string
	serveSAMLCert           string
	serveSAMLKey            string
	serveSAMLUsernameAttr   string
	serveSAMLGroupsAttr     string
	serveSAMLDefaultRole    string
	serveSAMLRoleMap        []string
)

// serveJWT* hold the bearer JWT sign-in flags, so a service can present a JWT minted elsewhere, such
// as by jwtmint, instead of a SwitchTender token.
var (
	serveJWTJWKSURL       string
	serveJWTIssuer        string
	serveJWTAudience      string
	serveJWTUsernameClaim string
	serveJWTGroupsClaim   string
	serveJWTDefaultRole   string
	serveJWTRoleMap       []string
	serveAIProvider       string
	serveAIModel          string
	serveAIURL            string
)

// forwardURL holds --forward-url, an HTTP endpoint audit events stream to as NDJSON.
var forwardURL string

// forwardHeaders holds --forward-header pairs set on every forwarded batch.
var forwardHeaders []string

// forwardSyslog holds --forward-syslog, a TCP syslog collector audit events stream to.
var forwardSyslog string

// forwardSyslogTLS wraps the syslog connection in TLS.
var forwardSyslogTLS bool

// forwardState holds --forward-state, the durable cursor file.
var forwardState string

// forwardInterval holds --forward-interval, how often the tail polls when caught up.
var forwardInterval time.Duration

// evidenceDir holds the value of the --evidence-dir flag, where periodic change registers land.
var evidenceDir string

// evidenceCadence holds the value of the --evidence-cadence flag, how long each register covers.
var evidenceCadence time.Duration

// spanCadence holds the value of the --span-cadence flag, how often a span beat is appended to the
// audit chain. Zero leaves beats off; when set it must be a whole number of seconds of at least
// one, since the cadence is committed into every beat entry.
var spanCadence time.Duration

// serveAnchorTSAURL holds the value of the --anchor-tsa-url flag, the RFC 3161 timestamp authority
// that anchors each span beat. Empty emits beats without anchors, which still verify.
var serveAnchorTSAURL string

// retainRuns holds the value of the --retain-runs flag, a duration like 90d.
var retainRuns string

// retainEvents holds the value of the --retain-events flag, a duration like 30d.
var retainEvents string

// retainHistory holds the value of the --retain-history flag, a count of summaries per host.
var retainHistory int

// factsInterval holds the value of the --facts-interval flag, the minimum spacing between retained
// host state snapshots.
var factsInterval time.Duration

// retainFacts holds the value of the --retain-facts flag, a count of host state snapshots per host.
var retainFacts int

// retentionInterval holds the value of the --retention-interval flag.
var retentionInterval time.Duration

// smtpAddr holds the value of the --smtp-addr flag, the SMTP server host:port.
var smtpAddr string

// smtpFrom holds the value of the --smtp-from flag, the sender address.
var smtpFrom string

// smtpTo holds the values of the repeatable --smtp-to flag, the recipient addresses.
var smtpTo []string

// smtpUsername holds the value of the --smtp-username flag; the password comes from the
// SWITCHTENDER_SMTP_PASSWORD environment variable.
var smtpUsername string

// notifyWhen is when every chat and webhook channel flag is told about a run, the same moments the
// configuration reference lists for each.
const notifyWhen = "a run finishes, is held for approval, waits at a workflow approval step, or " +
	"needs attention past its alert threshold"

// notifyOn holds the value of the --notify-on flag: failure or finish.
var notifyOn string

// Container execution limit flags, shared by serve and worker so both executors cap the same way.
var (
	// containerMemory holds the --container-memory flag, the docker --memory cap.
	containerMemory string
	// containerCPUs holds the --container-cpus flag, the docker --cpus cap.
	containerCPUs string
	// containerPidsLimit holds the --container-pids-limit flag, the docker --pids-limit cap.
	containerPidsLimit int
	// containerNetwork holds the --container-network flag, the docker --network mode.
	containerNetwork string
	// containerRuntime holds the --container-runtime flag, the container CLI (docker or podman).
	containerRuntime string
	// containerPullPolicy holds the --container-pull-policy flag, the docker --pull policy.
	containerPullPolicy string
)

// registerContainerFlags adds the container resource and network flags to cmd, defaulting to the
// bounded ContainerLimits so runs stay capped even when an operator sets nothing.
func registerContainerFlags(cmd *cobra.Command) {
	d := roundhouse.DefaultContainerLimits()
	cmd.Flags().StringVar(&containerMemory, "container-memory", d.Memory,
		"Memory cap for containerized runs, as docker --memory. Empty removes the cap.")
	cmd.Flags().StringVar(&containerCPUs, "container-cpus", d.CPUs,
		"CPU cap for containerized runs, as docker --cpus. Empty removes the cap.")
	cmd.Flags().IntVar(&containerPidsLimit, "container-pids-limit", d.PidsLimit,
		"Process cap for containerized runs, as docker --pids-limit. Zero removes the cap.")
	cmd.Flags().StringVar(&containerNetwork, "container-network", d.Network,
		"Network mode for containerized runs, as docker --network, for example bridge or none. "+
			"With network access a run can reach the host's cloud metadata service and read the "+
			"instance identity, so prefer none for runs that do not need the network.")
	cmd.Flags().StringVar(&containerRuntime, "container-runtime", "docker",
		"Container CLI for containerized runs: docker or podman.")
	cmd.Flags().StringVar(&containerPullPolicy, "container-pull-policy", "missing",
		"Image pull policy for containerized runs, as docker --pull: always, missing, or never.")
	cmd.Flags().StringVar(&containerRunFilesSize, "container-runfiles-size", d.RunFilesSize,
		"Size of the in-memory filesystem a containerized run's private directory is mounted as, "+
			"for example 64m. It counts against --container-memory as it fills.")
}

// containerLimitsFromFlags builds the ContainerLimits from the shared container flag values.
func containerLimitsFromFlags() roundhouse.ContainerLimits {
	return roundhouse.ContainerLimits{
		Memory: containerMemory, CPUs: containerCPUs,
		PidsLimit: containerPidsLimit, Network: containerNetwork,
		RunFilesSize: containerRunFilesSize,
	}
}

// checkChoice refuses a flag value outside the set the flag accepts. The choice flags coerced any value
// they did not recognize to their default, so a typo in --container-runtime ran docker and one in
// --notify-on emailed on failures only, with nothing saying the value had been ignored.
func checkChoice(flag, value string, allowed ...string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("%w: --%s %q is not one of %s", ErrUsage, flag, value, strings.Join(allowed, ", "))
}

// moduleFetchTimeout holds the value of the --module-fetch-timeout flag, which serve and worker
// share.
var moduleFetchTimeout time.Duration

// moduleFetchMaxMiB holds the value of the --module-fetch-max-mib flag, which serve and worker
// share.
var moduleFetchMaxMiB int

// moduleKeepFor holds the value of the --module-keep-for flag, which serve and worker share.
var moduleKeepFor time.Duration

// moduleKeepMaxMiB holds the value of the --module-keep-max-mib flag, which serve and worker share.
var moduleKeepMaxMiB int

// Bounds the module download flags accept. The download runs while a submission waits for its
// answer, so its time is held to minutes, and its size to what a module tree can plausibly be.
const (
	// minModuleFetchTimeout is the shortest download time the flag accepts.
	minModuleFetchTimeout = time.Second
	// maxModuleFetchTimeout is the longest download time the flag accepts.
	maxModuleFetchTimeout = time.Hour
	// maxModuleFetchMiB is the largest download size, in MiB, the flag accepts.
	maxModuleFetchMiB = 64 << 10
	// minModuleKeepFor is the shortest time the flag keeps a module tree for its run. Shorter has
	// every run that waits at all download its modules again.
	minModuleKeepFor = time.Hour
	// maxModuleKeepFor is the longest time the flag keeps a module tree for its run.
	maxModuleKeepFor = 90 * 24 * time.Hour
	// maxModuleKeepMiB is the most space, in MiB, the flag lets the kept module trees occupy.
	maxModuleKeepMiB = 1 << 20
)

// registerModuleFetchFlags registers the bounds on the module download the approval gate runs
// before it reads a Terraform or OpenTofu plan, which a run repeats when the gate's copy of the
// modules is not kept where it executes. serve and worker share them.
func registerModuleFetchFlags(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&moduleFetchTimeout, "module-fetch-timeout",
		dispatch.DefaultModuleFetchTimeout,
		"How long one module download may run, for example 2m, from 1s to 1h. Past it the "+
			"download stops and the plan stays unclassified, so it is held or refused.")
	cmd.Flags().IntVar(&moduleFetchMaxMiB, "module-fetch-max-mib",
		dispatch.DefaultModuleFetchMaxBytes>>20,
		"How many MiB one module download may write, from 1 to 65536. The download also stops past "+
			"50000 files. Past either the plan stays unclassified, so it is held or refused.")
	cmd.Flags().DurationVar(&moduleKeepFor, "module-keep-for", dispatch.DefaultModuleKeepFor,
		"How long the module trees the gate downloads are kept for the run it judged, for example "+
			"168h, from 1h to 2160h. A run whose tree is no longer kept downloads its modules again "+
			"and is refused unless they match what the gate read.")
	cmd.Flags().IntVar(&moduleKeepMaxMiB, "module-keep-max-mib",
		dispatch.DefaultModuleKeepMaxBytes>>20,
		"How many MiB the kept module trees may occupy together, from 1 to 1048576, the oldest "+
			"dropped first.")
}

// checkModuleFetchLimits refuses module download bounds outside what the flags accept.
func checkModuleFetchLimits(timeout time.Duration, maxMiB int) error {
	if timeout < minModuleFetchTimeout || timeout > maxModuleFetchTimeout {
		return fmt.Errorf("%w: --module-fetch-timeout %s is outside %s to %s", ErrUsage, timeout,
			minModuleFetchTimeout, maxModuleFetchTimeout)
	}
	if maxMiB < 1 || maxMiB > maxModuleFetchMiB {
		return fmt.Errorf("%w: --module-fetch-max-mib %d is outside 1 to %d", ErrUsage, maxMiB,
			maxModuleFetchMiB)
	}
	return nil
}

// checkModuleKeep refuses module keep bounds outside what the flags accept.
func checkModuleKeep(keepFor time.Duration, maxMiB int) error {
	if keepFor < minModuleKeepFor || keepFor > maxModuleKeepFor {
		return fmt.Errorf("%w: --module-keep-for %s is outside %s to %s", ErrUsage, keepFor,
			minModuleKeepFor, maxModuleKeepFor)
	}
	if maxMiB < 1 || maxMiB > maxModuleKeepMiB {
		return fmt.Errorf("%w: --module-keep-max-mib %d is outside 1 to %d", ErrUsage, maxMiB,
			maxModuleKeepMiB)
	}
	return nil
}

// moduleFetchOption is the dispatcher option the module download flags select.
func moduleFetchOption() dispatch.Option {
	return dispatch.WithModuleFetchLimits(moduleFetchTimeout, int64(moduleFetchMaxMiB)<<20)
}

// moduleKeepOption is the dispatcher option the module keep flags select.
func moduleKeepOption() dispatch.Option {
	return dispatch.WithModuleKeep(moduleKeepFor, int64(moduleKeepMaxMiB)<<20)
}

// checkContainerChoices refuses a container runtime or pull policy the runner does not know, for
// serve and worker alike.
func checkContainerChoices() error {
	if err := checkChoice("container-runtime", containerRuntime, "docker", "podman"); err != nil {
		return err
	}
	if err := checkContainerRunFilesSize(); err != nil {
		return err
	}
	return checkChoice("container-pull-policy", containerPullPolicy, "always", "missing", "never")
}

// checkServeChoices refuses any serve choice flag holding a value it does not know.
func checkServeChoices() error {
	if err := checkContainerChoices(); err != nil {
		return err
	}
	if err := checkCallbackFlags(); err != nil {
		return err
	}
	if err := checkModuleFetchLimits(moduleFetchTimeout, moduleFetchMaxMiB); err != nil {
		return err
	}
	if err := checkModuleKeep(moduleKeepFor, moduleKeepMaxMiB); err != nil {
		return err
	}
	return checkChoice("notify-on", notifyOn, "failure", "finish")
}

// containerRuntimeFromFlags returns the container CLI selected by the flag, coercing any value other
// than podman to docker so the runner never gets an unexpected binary.
func containerRuntimeFromFlags() string {
	if containerRuntime == "podman" {
		return "podman"
	}
	return "docker"
}

// containerPullPolicyFromFlags returns the image pull policy selected by the flag, coercing any
// value other than always or never to missing so the runner never gets an unexpected policy.
func containerPullPolicyFromFlags() string {
	switch containerPullPolicy {
	case "always", "never":
		return containerPullPolicy
	default:
		return "missing"
	}
}

// newSelectiveRunnerFromFlags builds the run executor shared by serve and worker. The container
// flags decide the runtime, the pull policy, and the resource caps, while the caller passes the
// command's own execution-environment and digest-pinning flags. Both commands go through here so a
// cap or a policy can never reach one executor and miss the other.
func newSelectiveRunnerFromFlags(allowContainer, requireDigest bool) roundhouse.Runner {
	return roundhouse.NewSelectiveRunner(allowContainer, containerRuntimeFromFlags(),
		containerPullPolicyFromFlags(), requireDigest, containerLimitsFromFlags())
}

// galaxyServer holds the --galaxy-server flag: a private Ansible Galaxy or Automation Hub URL.
var galaxyServer string

// registerGalaxyFlag adds the --galaxy-server flag, shared by serve and worker. The token comes from
// the SWITCHTENDER_GALAXY_TOKEN environment variable so it never appears on the command line.
func registerGalaxyFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&galaxyServer, "galaxy-server", os.Getenv("SWITCHTENDER_GALAXY_SERVER"),
		"Private Ansible Galaxy or Automation Hub URL for project collection installs. "+
			"Token from SWITCHTENDER_GALAXY_TOKEN.")
}

// galaxySyncerOpts returns the project.Syncer options for a configured galaxy server and its token, or
// nil when no server is set.
func galaxySyncerOpts() []project.SyncerOption {
	if galaxyServer == "" {
		return nil
	}
	return []project.SyncerOption{project.WithGalaxy(galaxyServer, os.Getenv("SWITCHTENDER_GALAXY_TOKEN"))}
}

// pluginsDir returns the plugins directory to load: the flag when set, else the
// SWITCHTENDER_PLUGINS_DIR environment variable. Empty means no plugins.
func pluginsDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("SWITCHTENDER_PLUGINS_DIR")
}

// workerToken returns the mesh relay worker token: the flag when set, else SWITCHTENDER_WORKER_TOKEN.
// It resolves the environment lazily rather than as the flag default so the secret never appears in
// help output. Empty leaves the relay endpoints off.
func workerToken() string {
	if serveWorkerToken != "" {
		return serveWorkerToken
	}
	return os.Getenv("SWITCHTENDER_WORKER_TOKEN")
}

// parseRoleMap turns repeated groupDN=role entries into a lowercased group to role map, so a directory
// group can drive a user's role. It splits on the last equals sign, since a group DN itself contains
// equals signs, and drops an entry with an unknown role.
func parseRoleMap(entries []string) map[string]user.Role {
	m := make(map[string]user.Role, len(entries))
	for _, e := range entries {
		i := strings.LastIndex(e, "=")
		if i < 0 {
			continue
		}
		group := strings.ToLower(strings.TrimSpace(e[:i]))
		role := user.Role(strings.TrimSpace(e[i+1:]))
		if group != "" && user.ValidRole(role) {
			m[group] = role
		}
	}
	return m
}

// serveCmd runs the SwitchTender HTTP server (the dispatcher).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the SwitchTender server.",
	Args:  cobra.NoArgs,
	RunE:  runServe,
}

// init registers serve command flags.
func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", defaultServeAddr, "Address the server listens on. Loopback by default. Set 0.0.0.0 to expose it on the network.")
	serveCmd.Flags().StringVar(&policyFile, "policy-file", "",
		"YAML file holding the approval policies. When set, the file is the source of truth and the "+
			"API refuses policy changes, so a change to what needs approval is a reviewed diff. A "+
			"malformed file stops the server rather than silently gating nothing.")
	serveCmd.Flags().StringVar(&serveDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend. "+dbEnvVar+" sets it when "+
			"this flag is absent, which keeps a DSN's password off the command line.")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "",
		"TLS certificate file, to serve HTTPS directly. Requires --tls-key.")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "",
		"TLS private key file, to serve HTTPS directly. Requires --tls-cert.")
	serveCmd.Flags().DurationVar(&scheduleInterval, "schedule-interval", schedule.DefaultInterval,
		"How often the scheduler checks for due schedules.")
	serveCmd.Flags().StringArrayVar(&notifyWebhooks, "notify-webhook", nil,
		"URL that receives a JSON notification when "+notifyWhen+". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifySlack, "notify-slack", nil,
		"Slack incoming webhook URL that receives a message when "+notifyWhen+". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyMattermost, "notify-mattermost", nil,
		"Mattermost incoming webhook URL that receives a message when "+notifyWhen+
			". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyRocketChat, "notify-rocketchat", nil,
		"Rocket.Chat incoming webhook URL that receives a message when "+notifyWhen+
			". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyDiscord, "notify-discord", nil,
		"Discord incoming webhook URL that receives a message when "+notifyWhen+". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyTeams, "notify-teams", nil,
		"Microsoft Teams incoming webhook URL that receives an Adaptive Card when "+notifyWhen+
			". Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyNtfy, "notify-ntfy", nil,
		"ntfy topic URL that receives a notification when "+notifyWhen+
			", such as https://ntfy.sh/my-topic. Repeatable.")
	serveCmd.Flags().StringVar(&notifyNtfyToken, "notify-ntfy-token", "",
		"Optional bearer token for a protected ntfy topic, applied to every --notify-ntfy URL. Prefer SWITCHTENDER_NOTIFY_NTFY_TOKEN, which a run cannot read, since a flag is visible in the process list.")
	serveCmd.Flags().StringArrayVar(&notifyPagerDuty, "notify-pagerduty", nil,
		"PagerDuty Events API routing key that triggers an incident when a run fails. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyGrafana, "notify-grafana", nil,
		"Grafana base URL that receives an annotation when a run finishes. Repeatable.")
	serveCmd.Flags().StringVar(&notifyGrafanaToken, "notify-grafana-token", "",
		"Bearer token for the Grafana annotations API, applied to every --notify-grafana URL. Prefer SWITCHTENDER_NOTIFY_GRAFANA_TOKEN, which a run cannot read, since a flag is visible in the process list.")
	serveCmd.Flags().StringVar(&notifyTwilioSID, "notify-twilio-sid", "",
		"Twilio Account SID for SMS notifications on a failed run.")
	serveCmd.Flags().StringVar(&notifyTwilioToken, "notify-twilio-token", "",
		"Twilio Auth Token, paired with --notify-twilio-sid. Prefer SWITCHTENDER_NOTIFY_TWILIO_TOKEN, which a run cannot read, since a flag is visible in the process list.")
	serveCmd.Flags().StringVar(&notifyTwilioFrom, "notify-twilio-from", "",
		"Twilio sender phone number that texts run failures.")
	serveCmd.Flags().StringArrayVar(&notifyTwilioTo, "notify-twilio-to", nil,
		"Phone number that receives an SMS when a run fails. Repeatable.")
	serveCmd.Flags().BoolVar(&serveAllowContainerEE, "allow-container-ee", false,
		"Allow runs whose project pins a container image to execute inside that image. Needs Docker.")
	serveCmd.Flags().StringVar(&serveDefaultImage, "default-image", "",
		"Fallback execution image for runs that pin none at the run, template, or project level. "+
			"Empty leaves an unpinned run on the host.")
	serveCmd.Flags().BoolVar(&serveRequireImageDigest, "require-image-digest", false,
		"Reject a container run whose image is not pinned to an @sha256: digest.")
	serveCmd.Flags().BoolVar(&serveImageDigestLookup, "image-digest-lookup", true,
		"Resolve a submitted run's image tag to the digest its registry serves, so the run executes "+
			"by that digest. Turn off where no registry is reachable from this server.")
	registerContainerFlags(serveCmd)
	registerRunFilesFlag(serveCmd)
	registerModuleFetchFlags(serveCmd)
	registerGalaxyFlag(serveCmd)
	registerFederationFlag(serveCmd)
	registerCallbackFlags(serveCmd)
	serveCmd.Flags().StringSliceVar(&serveTrustedProxy, "trusted-proxy", nil,
		"CIDR of a reverse proxy whose client IP header to believe, repeatable. Required behind a "+
			"proxy: without it every client shares one rate-limit key.")
	serveCmd.Flags().StringVar(&serveClientIPHeader, "client-ip-header", "",
		"Header carrying the real client address from a trusted proxy. Defaults to the leftmost "+
			"X-Forwarded-For entry.")
	serveCmd.Flags().BoolVar(&serveStrictGrants, "strict-grants", false,
		"Deny non-admins access to an object that has no grants, instead of deferring to the role.")
	serveCmd.Flags().BoolVar(&serveReadOnly, "read-only", false,
		"Reject every mutating request, for a safely exposable instance.")
	serveCmd.Flags().StringVar(&serveWorkerPools, "worker-pools", "",
		"YAML file binding each worker token to the queues it may lease from. Without it every "+
			"worker token may lease from every queue.")
	serveCmd.Flags().StringVar(&serveWorkerToken, "worker-token", "",
		"Bearer token that authenticates mesh relay workers and enables the relay endpoints. "+
			"Also SWITCHTENDER_WORKER_TOKEN. Keep it secret.")
	serveCmd.Flags().IntVar(&serveWorkers, "workers", dispatch.DefaultWorkers,
		"Concurrent runs this process executes at once.")
	serveCmd.Flags().IntVar(&serveMatrixCap, "matrix-cap", server.DefaultMatrixCap,
		"Largest host matrix, in cells, the UI draws before showing a notice. 0 means no limit.")
	serveCmd.Flags().IntVar(&serveMaxShards, "max-shards", dispatch.DefaultMaxShards,
		"Most groups a split fans out into. A split is always bounded by the host count.")
	serveCmd.Flags().DurationVar(&serveRunTimeout, "run-timeout", 0,
		"Default cap on how long a run may execute before it is canceled and failed, for example 1h. "+
			"A run may set a shorter timeout. Zero leaves runs uncapped.")
	serveCmd.Flags().StringVar(&servePluginsDir, "plugins-dir", "",
		"Directory of extension plugin binaries to load at startup. Empty loads none. Also SWITCHTENDER_PLUGINS_DIR.")
	serveCmd.Flags().StringVar(&serveOIDCIssuer, "oidc-issuer", "",
		"OpenID Connect issuer URL to enable single sign-on. Empty leaves SSO off.")
	serveCmd.Flags().StringVar(&serveOIDCClientID, "oidc-client-id", "", "OIDC client id.")
	serveCmd.Flags().StringVar(&serveOIDCRedirectURL, "oidc-redirect-url", "",
		"OIDC redirect URL, for example https://host/auth/oidc/callback.")
	serveCmd.Flags().StringVar(&serveOIDCDefaultRole, "oidc-default-role", "viewer",
		"Role granted to an account created on first SSO sign-in: admin, operator, or viewer.")
	serveCmd.Flags().StringVar(&serveLDAPURL, "ldap-url", "",
		"LDAP directory URL to enable directory sign-in, for example ldaps://ldap.example.com:636.")
	serveCmd.Flags().StringVar(&serveLDAPBindDN, "ldap-bind-dn", "",
		"Service account DN used to search for a user, empty for an anonymous search.")
	serveCmd.Flags().StringVar(&serveLDAPBaseDN, "ldap-base-dn", "",
		"Search base for finding a user, for example ou=people,dc=example,dc=com.")
	serveCmd.Flags().StringVar(&serveLDAPUserFilter, "ldap-user-filter", "(uid=%s)",
		"Search filter with one %s for the username.")
	serveCmd.Flags().StringVar(&serveLDAPDefaultRole, "ldap-default-role", "viewer",
		"Role granted to an account created on first directory sign-in: admin, operator, or viewer.")
	serveCmd.Flags().StringArrayVar(&serveLDAPRoleMap, "ldap-role-map", nil,
		"Map a directory group to a role as groupDN=role, for example cn=admins,dc=x=admin. "+
			"A matched group sets the user's role on every sign-in. Repeatable.")
	serveCmd.Flags().StringVar(&servePublicURL, "public-url", os.Getenv("SWITCHTENDER_PUBLIC_URL"),
		"Public base URL of this server, such as https://switchtender.example.com, used to link a pull "+
			"request review back to its run. Also SWITCHTENDER_PUBLIC_URL.")
	serveCmd.Flags().StringVar(&serveSAMLIDPMetadataURL, "saml-idp-metadata-url", "",
		"SAML IdP metadata URL to enable SAML sign-in. Empty leaves SAML off.")
	serveCmd.Flags().StringVar(&serveSAMLBaseURL, "saml-base-url", "",
		"Public base URL of this server, used to build the SAML entity id and ACS endpoint.")
	serveCmd.Flags().StringVar(&serveSAMLCert, "saml-cert", "",
		"Path to the service provider certificate, PEM.")
	serveCmd.Flags().StringVar(&serveSAMLKey, "saml-key", "",
		"Path to the service provider RSA private key, PEM.")
	serveCmd.Flags().StringVar(&serveSAMLUsernameAttr, "saml-username-attr", "",
		"Assertion attribute used as the username. Empty uses the subject NameID.")
	serveCmd.Flags().StringVar(&serveSAMLGroupsAttr, "saml-groups-attr", "groups",
		"Assertion attribute holding the user's groups, used with --saml-role-map.")
	serveCmd.Flags().StringVar(&serveSAMLDefaultRole, "saml-default-role", "viewer",
		"Role granted to an account created on first SAML sign-in: admin, operator, or viewer.")
	serveCmd.Flags().StringArrayVar(&serveSAMLRoleMap, "saml-role-map", nil,
		"Map an asserted group to a role as group=role, for example platform-admins=admin. "+
			"A matched group sets the user's role on every sign-in. Repeatable.")
	serveCmd.Flags().StringVar(&serveJWTJWKSURL, "jwt-jwks-url", "",
		"JWKS URL to enable bearer JWT sign-in, for example https://jwtmint.example.com/jwks.")
	serveCmd.Flags().StringVar(&serveJWTIssuer, "jwt-issuer", "", "Expected token issuer, the iss claim.")
	serveCmd.Flags().StringVar(&serveJWTAudience, "jwt-audience", "",
		"Expected token audience, empty to skip the audience check.")
	serveCmd.Flags().StringVar(&serveJWTUsernameClaim, "jwt-username-claim", "sub",
		"Claim naming the account, for example sub or email.")
	serveCmd.Flags().StringVar(&serveJWTGroupsClaim, "jwt-groups-claim", "",
		"Claim holding the user's groups, used with --jwt-role-map.")
	serveCmd.Flags().StringVar(&serveJWTDefaultRole, "jwt-default-role", "viewer",
		"Role granted to an account created on first JWT sign-in.")
	serveCmd.Flags().StringVar(&serveAIProvider, "ai-provider", "",
		"Enable advisory AI features with a provider: ollama, anthropic, or openai. Empty leaves AI off.")
	serveCmd.Flags().StringVar(&serveAIModel, "ai-model", "",
		"Model name for the AI provider. Empty uses the provider's default.")
	serveCmd.Flags().StringVar(&serveAIURL, "ai-url", "",
		"Base URL for the AI provider, for a self-hosted Ollama or a proxy. Empty uses the default.")
	serveCmd.Flags().StringArrayVar(&serveJWTRoleMap, "jwt-role-map", nil,
		"Map a token group to a role as group=role. A matched group sets the role on every request. Repeatable.")
	serveCmd.Flags().StringVar(&forwardURL, "forward-url", "",
		"HTTP endpoint audit events are forwarded to as NDJSON, each with its chain receipt.")
	serveCmd.Flags().StringArrayVar(&forwardHeaders, "forward-header", nil,
		"Header set on every forwarded batch, as 'Name: value'. Repeatable.")
	serveCmd.Flags().StringVar(&forwardSyslog, "forward-syslog", "",
		"TCP syslog collector (host:port) audit events are forwarded to as RFC 5424.")
	serveCmd.Flags().BoolVar(&forwardSyslogTLS, "forward-syslog-tls", false,
		"Wrap the syslog connection in TLS.")
	serveCmd.Flags().StringVar(&forwardState, "forward-state", "switchtender-forward.json",
		"Durable cursor file recording the last delivered chain position.")
	serveCmd.Flags().DurationVar(&forwardInterval, "forward-interval", 5*time.Second,
		"How often the forwarder polls the chain when caught up. At least 1s.")
	serveCmd.Flags().StringVar(&evidenceDir, "evidence-dir", "",
		"Directory to write periodic change registers into. Requires --evidence-cadence.")
	serveCmd.Flags().DurationVar(&evidenceCadence, "evidence-cadence", 0,
		"How long each change register covers, and how often one is written, for example 2160h "+
			"for a quarter. Zero writes none.")
	serveCmd.Flags().DurationVar(&spanCadence, "span-cadence", 0,
		"Append a span beat to the audit chain this often, for example 60s. Whole seconds only. "+
			"Zero leaves beats off.")
	serveCmd.Flags().StringVar(&serveAnchorTSAURL, "anchor-tsa-url", "",
		"RFC 3161 timestamp authority that anchors each span beat, for example "+defaultTSA+". "+
			"Empty emits beats without anchors.")
	serveCmd.Flags().StringVar(&retainRuns, "retain-runs", "",
		"Delete terminal runs older than this, for example 90d. Empty keeps them forever.")
	serveCmd.Flags().StringVar(&retainEvents, "retain-events", "",
		"Drop run events and logs older than this, for example 30d. Empty keeps them forever.")
	serveCmd.Flags().IntVar(&retainHistory, "retain-history", 0,
		"Keep only this many per-host and per-task summaries for each host and task, for example "+
			"500. Summaries outlive the runs they came from, so this is the only bound on them. "+
			"Zero keeps every summary forever. Values below "+
			strconv.Itoa(run.MinRetainSummaries)+" are raised to it.")
	serveCmd.Flags().DurationVar(&factsInterval, "facts-interval", run.DefaultFactsInterval,
		"Minimum spacing between retained host state snapshots, for example 24h. The estate history "+
			"keeps the newest gather in each period, which is what answers what a host looked like "+
			"on a date. Zero keeps every gather, at roughly a hundred times the disk.")
	serveCmd.Flags().IntVar(&retainFacts, "retain-facts", run.DefaultFactsDepth,
		"Keep only this many host state snapshots for each host. A fact set is hundreds of "+
			"kilobytes, so unlike summaries this is bounded by default. Zero keeps every snapshot "+
			"forever.")
	serveCmd.Flags().DurationVar(&retentionInterval, "retention-interval", retention.DefaultInterval,
		"How often the retention sweeper runs.")
	serveCmd.Flags().StringVar(&smtpAddr, "smtp-addr", "",
		"SMTP server host:port for run notification emails. Empty disables email.")
	serveCmd.Flags().StringVar(&smtpFrom, "smtp-from", "", "Sender address for notification emails.")
	serveCmd.Flags().StringArrayVar(&smtpTo, "smtp-to", nil,
		"Recipient address for notification emails. Repeatable.")
	serveCmd.Flags().StringVar(&smtpUsername, "smtp-username", "",
		"SMTP username. The password comes from SWITCHTENDER_SMTP_PASSWORD.")
	serveCmd.Flags().StringVar(&notifyOn, "notify-on", "failure",
		"When to email: failure for failed runs only, or finish for every finished run, every "+
			"run held for approval, and every workflow waiting at an approval step. An attention "+
			"alert is emailed under either setting.")
}

// buildEmailer constructs the SMTP notifier from the flags, or returns nil when email is not
// configured. It reports whether notifications should fire only on failure.
func buildEmailer() (dispatch.Emailer, bool) {
	if smtpAddr == "" || smtpFrom == "" || len(smtpTo) == 0 {
		return nil, true
	}
	var auth smtp.Auth
	if smtpUsername != "" {
		host, _, _ := net.SplitHostPort(smtpAddr)
		auth = smtp.PlainAuth("", smtpUsername, os.Getenv("SWITCHTENDER_SMTP_PASSWORD"), host)
	}
	return dispatch.NewSMTPEmailer(smtpAddr, smtpFrom, smtpTo, auth), notifyOn != "finish"
}

// parseRetention converts a retention flag into a duration. An empty value means no window. A
// trailing d counts whole days; otherwise the Go duration syntax applies.
func parseRetention(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid retention %q: %w", s, err)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid retention %q: %w", s, err)
	}
	return d, nil
}

// storeBundle is the store set both database backends expose.
type storeBundle interface {
	// Runs returns the run store.
	Runs() run.Store
	// Schedules returns the schedule store.
	Schedules() schedule.Store
	// Tokens returns the API token store.
	Tokens() auth.Store
	// Credentials returns the execution secret store.
	Credentials() credential.Store
	// CredentialTypes returns the operator-defined credential type store.
	CredentialTypes() credential.TypeStore
	// FederationKeys returns the workload identity federation signing key store.
	FederationKeys() federation.KeyStore
	// Projects returns the git project store.
	Projects() project.Store
	// Templates returns the job template store.
	Templates() template.Store
	// Users returns the account store.
	Users() user.Store
	// Inventories returns the stored inventory store.
	Inventories() inventory.Store
	// Policies returns the approval policy store.
	Policies() policy.Store
	// Audits returns the audit trail store.
	Audits() audit.Store
	// InventorySources returns the dynamic inventory source store.
	InventorySources() invsource.Store
	// Triggers returns the webhook trigger store.
	Triggers() trigger.Store
	// Notifications returns the named notification target store.
	Notifications() notification.Store
	// Teams returns the team store.
	Teams() team.Store
	// Orgs returns the organization store.
	Orgs() org.Store
	// Grants returns the per-object access grant store.
	Grants() grant.Store
	// FactCache returns the per-host Ansible fact cache store.
	FactCache() factcache.Store
	// ReviewReports returns the pull request review report store.
	ReviewReports() review.Store
	// Decisions returns the approval decision record store: approver reasons, their corrections and
	// redactions, and the separation-of-duties evaluations of agent-initiated runs.
	Decisions() decision.Store
	// Attention returns the store of worker reports and raised attention alerts.
	Attention() attention.Store
	// Close closes the underlying database.
	Close() error
}

// notificationRouter builds the router that finds the named notification targets attached to what a
// run came from, reading a run's template through the schedule, trigger, or run that fired it, and
// an organization's targets through the organization that owns that template. The sealer opens each
// target's secrets at delivery; without a key a target that carries one fails to deliver, recorded.
func notificationRouter(bundle storeBundle, sealer *credential.Sealer,
	log *zap.Logger) *notification.Router {
	templates := bundle.Templates()
	return notification.NewRouter(bundle.Notifications(), notificationSealerOf(sealer),
		notification.SourceLineage(bundle.Schedules(), bundle.Triggers(), bundle.Runs()), log,
		notification.WithTemplateOrgs(func(ctx context.Context, id string) string {
			t, err := templates.Get(ctx, id)
			if err != nil {
				return ""
			}
			return t.OrgID
		}))
}

// notificationOutbox builds the outbox every process that runs work records its runs' events in and
// delivers them from: in each run's order, per target, retried, and kept as failed when the
// attempts run out. Claims and retries are measured by the database's clock, so the processes
// sharing it agree on when a claim lapses.
func notificationOutbox(bundle storeBundle, sealer *credential.Sealer,
	log *zap.Logger) *notification.Outbox {
	runs := bundle.Runs()
	return notification.NewOutbox(bundle.Notifications(), notificationRouter(bundle, sealer, log),
		notificationSealerOf(sealer), log, notification.WithClock(runs.Now))
}

// notificationSealerOf returns the credential sealer as a notification.Sealer, keeping a nil one
// nil rather than a typed nil the notification package would call through.
func notificationSealerOf(sealer *credential.Sealer) notification.Sealer {
	if sealer == nil {
		return nil
	}
	return sealer
}

// openBundle opens the stores for the --db value: a postgres:// or postgresql:// DSN selects the
// PostgreSQL backend, anything else is a SQLite file path.
func openBundle(db string) (storeBundle, error) {
	// The license loads before the store opens, because initializing a new PostgreSQL schema is
	// itself gated, and it loads here so init, import, the server, and the workers all read the
	// same answer from one place. A file that does not verify is loud and non-fatal: broken
	// licensing fails toward Community, never toward a command that will not run.
	if lic, lerr := license.Load(license.PathFor(db)); lerr != nil {
		fmt.Fprintln(os.Stderr, "license file did not verify, running Community: "+lerr.Error())
	} else if lic != nil {
		license.Set(lic)
	}
	if isPostgresDSN(db) {
		return pgstore.Open(db)
	}
	return sqlitestore.Open(db)
}

// openExisting opens the store a command works on, refusing a SQLite file that does not exist.
// Opening creates a missing file, so a command run from another directory, or with --db left off,
// minted its token or user into a new empty database the server never reads and reported success.
// Only serve, init, demo, restore, and an import create a database. A PostgreSQL DSN names a database
// that must already exist, so it goes straight through.
func openExisting(db string) (storeBundle, error) {
	if !isPostgresDSN(db) && !fileExists(db) {
		where := db
		if abs, err := filepath.Abs(db); err == nil {
			where = abs
		}
		return nil, fmt.Errorf("%w at %s, and this command does not create one. Pass --db with the "+
			"path your server uses, the one in its serve command or service file, or create an install "+
			"with switchtender init", errNoDatabase, where)
	}
	return openBundle(db)
}

// isPostgresDSN reports whether a --db value names a PostgreSQL database rather than a SQLite file.
func isPostgresDSN(db string) bool {
	return strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://")
}

// fileExists reports whether path names something on disk. Only a missing path answers false, so a
// file that cannot be read is left for the open to report.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// publishedKeyValues are the encryption values the documentation printed in its examples. They are
// public, so credentials sealed under them are sealed under a key anyone can read.
var publishedKeyValues = map[string]bool{"change-me": true, "change-me-too": true}

// refusePublishedKey refuses to start when the encryption key or salt is a value the documentation
// printed. The quickstart used them for months, so they are in shell histories and deploy scripts,
// and a server started with them sealed every credential it was given under a pair anyone can look
// up: a copy of the database was all it took to open them. Refusing here says so before anything is
// sealed, rather than after.
func refusePublishedKey() error {
	for _, name := range []string{"SWITCHTENDER_ENCRYPTION_KEY", "SWITCHTENDER_ENCRYPTION_SALT"} {
		if publishedKeyValues[strings.TrimSpace(os.Getenv(name))] {
			return fmt.Errorf("%w: %s is set to an example from the documentation, which anyone can "+
				"use to open the credentials this server seals. Generate your own with "+
				"`openssl rand -hex 32`, keep it with your other secrets, and set it before starting",
				errPublishedKey, name)
		}
	}
	return nil
}

// refuseBadAuditKey refuses to start when SWITCHTENDER_AUDIT_KEY is set to something that cannot
// sign. The configuration reference says a malformed value stops startup, and it did not: the server
// logged a warning and ran with no producer identity, so its chain bound no entry to this install and
// every receipt it issued could be lifted onto another. An operator who set the variable meant it,
// so a value that cannot be used is a mistake to stop on rather than a default to fall back from.
func refuseBadAuditKey() error {
	seed := os.Getenv(identity.KeyEnv)
	if seed == "" {
		return nil
	}
	if err := identity.CheckSeed(seed); err != nil {
		return fmt.Errorf("%w: %w. Set it to 32 bytes of hex, such as the output of "+
			"`openssl rand -hex 32`, or unset it and the server keeps its own key", errBadAuditKey, err)
	}
	return nil
}

// newSealerFromEnv builds a credential Sealer from the encryption environment. Credentials need
// both SWITCHTENDER_ENCRYPTION_KEY and a stable SWITCHTENDER_ENCRYPTION_SALT; when either is missing
// the Sealer is disabled and the reason is logged so the operator knows which value to set.
func newSealerFromEnv(log *zap.Logger) *credential.Sealer {
	key := os.Getenv("SWITCHTENDER_ENCRYPTION_KEY")
	salt := os.Getenv("SWITCHTENDER_ENCRYPTION_SALT")
	sealer := credential.NewSealer(key, salt)
	if sealer.Enabled() {
		// Key derivation allocates a 64 MiB argon2id arena that is dead the moment the key exists.
		// Hand it back to the OS now, once at startup, so a long-running process idles at its real
		// footprint instead of carrying the derivation arena in resident memory for its lifetime.
		debug.FreeOSMemory()
		return sealer
	}
	if key == "" {
		log.Warn("credentials disabled: set SWITCHTENDER_ENCRYPTION_KEY to enable them")
	} else {
		log.Warn("credentials disabled: set a stable SWITCHTENDER_ENCRYPTION_SALT alongside the key")
	}
	return sealer
}

// projectCacheDir returns where project checkouts live: the user cache directory when available,
// the system temp directory otherwise.
func projectCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "switchtender", "projects")
}

// producerInstallID returns the install id an anchor should record, empty when this install has no
// identity. An anchor records it so a later check can tell a chain read under a different identity, the
// shape a restore without its key file takes, from a chain that was rewritten.
func producerInstallID(id *audit.Identity) string {
	if id == nil {
		return ""
	}
	return id.InstallID
}

// identityDirEnv names the environment variable that places the producer signing identity
// explicitly, for an install whose account has no home directory to derive one from.
const identityDirEnv = "SWITCHTENDER_IDENTITY_DIR"

// identityDir returns the directory holding the producer signing identity for a database target.
// serve, which signs the bundles it serves, and the bundle command, which signs the bundle it
// emits, both derive it here so one install mints a single key and every tool reads that same key. A
// SQLite file keeps its identity beside it, so a copied database carries its key. A postgres DSN has
// no filesystem home: filepath.Dir on a DSN yields a cwd-relative junk directory whose name embeds
// the DSN's user:password@host, both wrong and a credential leak, so the identity falls back to a
// stable per-user directory instead.
//
// When there is no per-user directory to fall back to, this refuses rather than choosing one. It
// used to answer with the system temp directory, which is the one place a signing key must never
// live. os.UserConfigDir fails when the account has no home, which is the ordinary shape of a
// container running a postgres-backed server, so the fallback was not a remote branch: on those
// installs the key was minted in a world-writable directory that the next restart empties. A key
// that vanishes is not an outage, it is a new install identity, and since every audit entry is bound
// to the install that wrote it, the chain would silently start attributing entries to a different
// install on every restart while continuing to report itself sound.
func identityDir(db string) (string, error) {
	if dir := strings.TrimSpace(os.Getenv(identityDirEnv)); dir != "" {
		return dir, nil
	}
	// A seed supplied in SWITCHTENDER_AUDIT_KEY signs directly: that path neither reads nor writes
	// the key file, so it needs no durable directory. Refusing below broke the documented way to
	// sign a shared postgres chain, since a keyed container with no home was turned away over a
	// directory its key never touches, and serve then ran with an unattributed chain and unsigned
	// bundles while an audit anchor job hard-failed.
	if os.Getenv(identity.KeyEnv) != "" {
		return ".", nil
	}
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			return "", fmt.Errorf("%w: this account has no configuration directory to keep the "+
				"producer signing identity in, and the system temp directory is not one, because a "+
				"key that a restart deletes silently becomes a second install: set %s to a durable "+
				"path this server owns", errNoIdentityHome, identityDirEnv)
		}
		return filepath.Join(base, "switchtender", "identity"), nil
	}
	dir := filepath.Dir(db)
	if dir == "" || dir == "." {
		return ".", nil
	}
	return dir, nil
}

// runServe builds the server dependencies and serves until interrupted.
// externalAuthConfigured reports whether any single sign-on or federated auth provider is set, in
// which case the API is not wide open even before an API token exists.
func externalAuthConfigured() bool {
	return serveOIDCIssuer != "" || serveLDAPURL != "" ||
		serveSAMLIDPMetadataURL != "" || serveJWTJWKSURL != ""
}

// disableExternalAuth clears every directory sign-in setting, so the server comes up on local
// accounts instead of refusing to start. It is called only for a license that lapsed, never for one
// that was never there: an install that never had SSO and configured it anyway is a misconfiguration
// the operator should see at startup rather than have silently ignored.
func disableExternalAuth() {
	serveOIDCIssuer = ""
	serveLDAPURL = ""
	serveSAMLIDPMetadataURL = ""
	serveJWTJWKSURL = ""
}

// isLoopbackAddr reports whether addr binds only the loopback interface. An empty or wildcard host
// binds every interface and is not loopback, so exposing an unauthenticated API on it is refused.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// enforcedAuthOption turns external auth into the server option that keeps the gate enforcing, or a
// no-op when there is none, so the call site reads as one decision.
func enforcedAuthOption(externalAuth bool) server.Option {
	if !externalAuth {
		return func(*server.Server) {}
	}
	return server.WithEnforcedAuth()
}

// servePosture is what the API token count says the server must do before it binds.
type servePosture int

const (
	// postureReady means authentication is already in place and the server starts silently.
	postureReady servePosture = iota
	// postureWarn means the API is unauthenticated but only reachable where that is acceptable,
	// so the server starts and says so.
	postureWarn
	// postureBootstrap means a public bind has no authentication yet, so the server mints an
	// initial admin token and prints it before serving.
	postureBootstrap
)

// tokenCountGuard decides what the server must do about authentication given the API token count.
//
// A count error is fail-closed: an unreadable token store refuses to start rather than binding, since
// the only alternative is to guess there are no tokens and expose the network on that guess. The
// former code skipped the whole guard on a count error and fell through to bind, which is the one
// direction this must never fail.
//
// A readable empty store on a public bind used to be refused outright. That was safe and unusable:
// the documented first command binds a public address on a fresh database, so the quickstart, the
// README, and the homepage all opened with a command that exited 1, and the quickstart went on to
// promise the API was open until the first token. Minting the token instead keeps the bind
// authenticated from the first request and lets the documented command work as written.
func tokenCountGuard(count int, countErr error, accounts int, readOnly, externalAuth, loopback bool,
	addr string) (servePosture, error) {
	if countErr != nil {
		return postureReady, fmt.Errorf("refusing to serve on %s: cannot determine whether any API "+
			"tokens exist, so the API cannot be safely exposed: %w", addr, countErr)
	}
	if count > 0 {
		return postureReady, nil
	}
	// An account turns authentication on exactly as a token does, and the documented production
	// path creates one and no token: switchtender init, then serve. Judging on tokens alone, that
	// operator read "the API is UNAUTHENTICATED until you create one" in their log about an install
	// that was in fact answering 401 to everything, and on a public bind got an admin token minted
	// they never needed.
	if accounts > 0 {
		return postureReady, nil
	}
	// An install with external auth enforces from the first request, so there is nothing
	// unauthenticated to warn about even with an empty token table.
	if externalAuth {
		return postureReady, nil
	}
	// Read-only serves no mutation, and loopback is reachable only from the host, so both are
	// allowed to run open. Everything else gets a token minted for it.
	if readOnly || loopback {
		return postureWarn, nil
	}
	return postureBootstrap, nil
}

// initialTokenFile is the file the first admin token is handed over in when nobody is at a terminal
// to read it, beside the database it opens.
const initialTokenFile = "initial-admin-token"

// bootstrapAdminToken mints the initial admin token for an install that has none and hands it over
// once.
//
// At a terminal it is printed to stderr with fmt rather than through the logger. A token is exactly
// what the logging rules say never to log, and this is a one-time handover to the person at the
// terminal, the same disclosure 'token new' makes. Nobody is at a terminal when the server runs in a
// container, a pod, or under a service manager, and there stderr is the log, which keeps the token
// for as long as the log is kept and ships it wherever the logs go: a never-expiring admin token sat
// in docker logs and kubectl logs, and survived restarts there. So it goes to a file beside the
// database, readable by this account alone, and the log says where.
//
// The mint is recorded in the audit chain like any other token creation, so an install cannot come
// into existence with an admin credential the trail does not mention.
func bootstrapAdminToken(ctx context.Context, bundle storeBundle, addr, db string) error {
	plain, tok, err := auth.New("initial")
	if err != nil {
		return fmt.Errorf("mint the initial admin token: %w", err)
	}
	// Nobody minted it but the server's own first start, and the issuer says so rather than naming
	// whoever happens to own the process.
	tok.CreatedBy, tok.CreatedByType = "system:first-start", "system"
	if err := bundle.Tokens().Save(ctx, tok); err != nil {
		return fmt.Errorf("save the initial admin token: %w", err)
	}
	if err := recordCLI(ctx, bundle.Audits(), db, "/cli/serve/bootstrap-token"); err != nil {
		return err
	}
	next := fmt.Sprintf(`  Name more:     switchtender token new --user <account> --name ci %s
  Revoke this:   switchtender token revoke %s %s
`, dbFlag(db), tok.ID, dbFlag(db))
	if initialTokenToFile(term.IsTerminal(int(os.Stderr.Fd()))) {
		path, werr := writeInitialToken(db, plain)
		if werr == nil {
			fmt.Fprintf(os.Stderr, `
  No API tokens exist, so %s would have served an unauthenticated API.
  Created an initial admin token and wrote it to a file readable by this account alone,
  rather than to this log, which keeps whatever it is given:

      %s

  Read it, delete the file, and use it as: Authorization: Bearer <token>
%s
`, addr, path, next)
			return nil
		}
		fmt.Fprintf(os.Stderr, "\n  Could not write the initial admin token to a file (%v), so it is printed "+
			"below, and this log keeps it. Revoke it once you have named another.\n", werr)
	}
	fmt.Fprintf(os.Stderr, `
  No API tokens exist, so %s would have served an unauthenticated API.
  Created an initial admin token instead. It is shown only this once:

      %s

  Use it as:     Authorization: Bearer %s
%s
`, addr, plain, plain, next)
	return nil
}

// initialTokenToFile reports whether the initial admin token is written to a file rather than
// printed. A terminal shows it once and keeps nothing, so it is printed there. Anything else keeps
// it: a service log, and a container's log even when a terminal is attached, since docker logs
// records the terminal's output. The banner said the token was shown only once while the container
// log kept it for as long as the container existed.
func initialTokenToFile(isTerminal bool) bool {
	return !isTerminal || BuildChannel == buildChannelContainer
}

// writeInitialToken writes the initial admin token beside the database, readable by this account
// alone, and returns the file's path. A PostgreSQL install keeps it in its identity directory, since
// a DSN has no directory of its own.
func writeInitialToken(db, plain string) (string, error) {
	dir, err := identityDir(db)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(dir, initialTokenFile))
	if err != nil {
		return "", err
	}
	// A file left from an earlier database is replaced rather than appended to or trusted, and it is
	// created fresh so the permissions are this function's, not whatever the old file had.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(plain + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	return path, f.Close()
}

// dbFlag returns the --db argument a command printed for the operator needs to reach this server's
// database. A command without it opens switchtender.db in whatever directory it is run from, and
// quietly creates one when there is none, so a token minted by following a hint worked nowhere. A
// PostgreSQL DSN carries its password, so it is named by a placeholder rather than printed.
func dbFlag(db string) string {
	if isPostgresDSN(db) {
		return "--db <this server's database DSN>"
	}
	if abs, err := filepath.Abs(db); err == nil {
		db = abs
	}
	return "--db " + util.ShellArg(db)
}

// newDatabaseNote is what serve says when its database does not exist yet, empty when it does or when
// there is nothing to say.
//
// Starting on a database that is not there is a legitimate first run, and the quickstart, the
// container image, and the switching guide all do exactly that. It is also what a mistyped --db
// looks like, and then the server comes up healthy on an empty chain with no admin account and
// authentication off while the operator's real install sits unserved. So it is said out loud rather
// than refused, because refusing breaks every documented first run. Desktop makes its database in a
// per-user directory on its first launch by design and takes no --db, so telling its user to check
// --db named a flag they cannot pass.
func newDatabaseNote(db string, desktop bool) string {
	if desktop || isPostgresDSN(db) || fileExists(db) {
		return ""
	}
	return fmt.Sprintf("creating a new database at %s. If you meant to serve an existing install, "+
		"stop now and check --db: this one starts empty, with no admin account and no tokens.", db)
}

func runServe(cmd *cobra.Command, _ []string) error {
	serveDB = dbFromEnv(cmd, serveDB)
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()
	protectProcess(log)
	if err := applyEgressProxy(log); err != nil {
		return err
	}
	if err := refusePublishedKey(); err != nil {
		return err
	}
	if err := refuseBadAuditKey(); err != nil {
		return err
	}
	if err := checkServeChoices(); err != nil {
		return err
	}
	runFiles, runFilesReport, err := prepareRunFiles(log)
	if err != nil {
		return err
	}

	if note := newDatabaseNote(serveDB, serveDesktop); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	if err := checkWorkers(serveWorkers, serveWorkersHint); err != nil {
		return err
	}
	var proxies []*net.IPNet
	for _, c := range serveTrustedProxy {
		_, n, perr := net.ParseCIDR(strings.TrimSpace(c))
		if perr != nil {
			return fmt.Errorf("--trusted-proxy %q is not a CIDR: %w", c, perr)
		}
		proxies = append(proxies, n)
	}
	run.SetFactsInterval(factsInterval)
	run.SetFactsDepth(retainFacts)
	server.SetTrustedProxies(proxies)
	server.SetClientIPHeader(serveClientIPHeader)

	bundle, err := openBundle(serveDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	if lic := license.Current(); lic != nil {
		log.Info("licensed: " + lic.Claims.Tier + " (" + lic.Claims.Org + "), expires " +
			lic.Claims.Expires)
	}
	// SSO is configured explicitly by flag, so a missing license here is a misconfiguration worth
	// refusing at startup with one line, not a silently unauthenticated directory.
	//
	// A LAPSED license is the other case, and it is the one the pricing page makes a promise about:
	// "a lapsed license takes nothing: sign-in falls back to local accounts". Refusing to start
	// broke that promise in the worst possible way, by taking a working install offline on the day
	// its renewal slipped. Falling back is not a loosening either: local accounts are how every
	// Community install authenticates, so the install keeps enforcing, just not through the
	// directory. It is said loudly, once, because an operator has to know why their users are
	// suddenly signing in differently.
	if externalAuthConfigured() {
		if aerr := license.Allow(license.FeatureSSO); aerr != nil {
			lic := license.Current()
			if lic == nil || !lic.Expired(time.Now()) {
				return aerr
			}
			log.Warn("the license for directory sign-in lapsed on " + lic.Claims.Expires +
				", so single sign-on is off and this install is signing in with local accounts. " +
				"Everything else keeps working. Renew to turn the directory back on: " +
				"https://switchtender.com/pricing")
			disableExternalAuth()
		}
	}
	store, schedules := bundle.Runs(), bundle.Schedules()

	// The producer identity signs LoomSeal bundles and is published so a relying party can pin its
	// fingerprint. It is created on first start in the install's identity directory, the same one the
	// bundle command reads, so a bundle is signed with the key serve publishes. A failure to create
	// it is not fatal: the server still runs and still records the audit chain, and refusing to start
	// would take an install offline over evidence it may not be exporting yet.
	//
	// It does cost more than attribution. Without an identity there is no install to bind entries to,
	// so the chain commits to no producer and its receipts can be lifted onto another install. The
	// warning says so rather than naming only the export, because a shared database reaches this path
	// by design and an operator reading about bundles would not know the binding went with it. The
	// error it carries already ends with the remedy and the path to put the key in.
	//
	// It is bound before the initial admin token is minted, so that entry, the first change to who can
	// act that an install records, commits to the install like every entry after it.
	var producer *audit.Identity
	producerProblem := ""
	if id, err := loadProducerIdentity(cmd.Context(), bundle.Audits(), serveDB); err != nil {
		producerProblem = err.Error()
		log.Warn("producer identity unavailable, so bundles cannot be attributed and entries are not " +
			"bound to this install, which lets a receipt be lifted onto another one: " + err.Error())
	} else {
		producer = &id
		log.Info("producer identity ready", zap.String("key_id", id.KeyID()),
			zap.String("install_id", id.InstallID))
		// Every entry appended from here carries the install, so its link commits to who produced
		// it. Without this a published receipt could be lifted whole by a second install: keep the
		// claims and the genuine third-party anchor, rewrite the producer, re-sign, and a relying
		// party pinning that second key reads somebody else's history as its own.
		if binder, ok := bundle.Audits().(audit.InstallBinder); ok {
			binder.BindInstall(id.InstallID)
		} else {
			log.Warn("audit store cannot be bound to this install, so its entries will not commit " +
				"to who produced them")
		}
	}

	n, cerr := bundle.Tokens().Count(cmd.Context())
	// A failure to list accounts is not fatal here: the token count already decided the dangerous
	// direction, and an unreadable user store leaves this exactly where it was before accounts were
	// consulted rather than refusing to start.
	accounts := 0
	if users, uerr := bundle.Users().List(cmd.Context()); uerr == nil {
		accounts = len(users)
	} else {
		log.Warn("cannot count accounts, so the startup authentication notice may be wrong: " + uerr.Error())
	}
	posture, gerr := tokenCountGuard(n, cerr, accounts, serveReadOnly, externalAuthConfigured(),
		isLoopbackAddr(serveAddr), serveAddr)
	if gerr != nil {
		return gerr
	}
	switch posture {
	case postureWarn:
		log.Warn("no API tokens exist. The API is UNAUTHENTICATED until you create one. Run: " +
			"switchtender token new --user <account> --name <name> " + dbFlag(serveDB))
	case postureBootstrap:
		if err := bootstrapAdminToken(cmd.Context(), bundle, serveAddr, serveDB); err != nil {
			return err
		}
	case postureReady:
	}

	sealer := newSealerFromEnv(log)
	// The issuer is built before anything serves, and its first key generated, so a cloud
	// configured ahead of the first run finds a key set to read, and a URL or a missing encryption
	// key that would leave federation unable to sign stops the start rather than the first run that
	// needs it.
	issuer, err := newFederationIssuer(federationIssuer, bundle.FederationKeys(), sealer,
		bundle.Audits())
	if err != nil {
		return err
	}
	if issuer != nil {
		if err := issuer.Ensure(cmd.Context()); err != nil {
			return fmt.Errorf("federation signing key: %w", err)
		}
		log.Info("serve: workload identity federation on", zap.String("issuer", issuer.URL()))
	}
	closePlugins, err := extplugin.Load(pluginsDir(servePluginsDir), log)
	if err != nil {
		return fmt.Errorf("load plugins: %w", err)
	}
	defer closePlugins()

	hub := live.NewHub()
	runner := newSelectiveRunnerFromFlags(serveAllowContainerEE, serveRequireImageDigest)
	syncer, err := project.NewSyncer(projectCacheDir(), galaxySyncerOpts()...)
	if err != nil {
		return fmt.Errorf("project cache: %w", err)
	}
	emailer, onFailureOnly := buildEmailer()
	// Policies come from a file when one is configured, so a change to what needs approval is a
	// reviewed diff rather than an API call. A malformed file stops the server: degrading to no
	// policies would turn a typo into an install where nothing is gated and nothing says so.
	policies := bundle.Policies()
	if policyFile != "" {
		filePolicies, perr := policy.NewFileStore(policyFile)
		if perr != nil {
			return perr
		}
		policies = filePolicies
		// The error mattered and was dropped: List is where an unreadable file and the license
		// refusals surface, so discarding it made the startup checks below run against an empty
		// list and pass. A server given explicit policy it cannot read must not start, because
		// what it would run is not what the operator wrote down.
		count, lerr := filePolicies.List(cmd.Context())
		if lerr != nil {
			return fmt.Errorf("read --policy-file %s: %w", policyFile, lerr)
		}
		// The in-force digest of what this replica loaded, said at startup on purpose: file-pinned
		// policies are loaded per replica with nothing comparing them, so during a rollout two
		// servers can gate differently and every run stamps whichever set its server held. The
		// digest makes replica drift a one-line diff between two startup logs instead of a
		// forensic exercise, and it matches the policy_set digest stamped on runs.
		log.Info("policy file loaded",
			zap.String("path", policyFile),
			zap.Int("rules", len(count)),
			zap.String("in_force_digest", policy.InForce(count).Digest))
		// The file is explicit configuration, so a license gap here is a misconfiguration worth
		// one line at startup, the same treatment SSO gets.
		needsFull := false
		for _, fp := range count {
			if fp.Advanced() {
				needsFull = true
			}
		}
		// A lapse is not a misconfiguration and must not stop the server from starting. Returning
		// here meant a paid install with a policy file could not restart once its term ran out,
		// and the same refusal on every read stopped its runs, so the install went dark rather
		// than dropping to Community. The terms rule that out: a lapsed license takes nothing and
		// a running install is not bricked over a billing dispute. The rules stay in force either
		// way, because dropping them would silently ungate the runs they were written to hold.
		var lapse error
		if needsFull {
			if aerr := license.Allow(license.FeaturePolicyFull); aerr != nil {
				if !errors.Is(aerr, license.ErrLapsed) {
					return aerr
				}
				lapse = aerr
			}
		}
		if aerr := license.AllowPolicies(len(count)); aerr != nil {
			if !errors.Is(aerr, license.ErrLapsed) {
				return aerr
			}
			lapse = aerr
		}
		if lapse != nil {
			log.Warn("serve: the license term has lapsed and the policy file stays in force, "+
				"because a lapse takes nothing that was already holding runs. Renewing is what "+
				"allows new paid policy to be authored",
				zap.String("detail", lapse.Error()))
		}
		log.Info("serve: approval policies read from file",
			zap.String("path", policyFile), zap.Int("policies", len(count)))
	}

	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub),
		dispatch.WithRunFilesRoot(runFilesReport.Root),
		dispatch.WithAudits(bundle.Audits()),
		dispatch.WithWorkers(serveWorkers),
		dispatch.WithMaxShards(serveMaxShards),
		dispatch.WithRunTimeout(serveRunTimeout),
		moduleFetchOption(),
		moduleKeepOption(),
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithCredentialTypes(bundle.CredentialTypes()),
		dispatch.WithFederation(issuer),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithDefaultImage(serveDefaultImage),
		imageResolverOption(),
		dispatch.WithWebhooks(notifyWebhooks),
		dispatch.WithSlack(notifySlack),
		dispatch.WithMattermost(notifyMattermost),
		dispatch.WithRocketChat(notifyRocketChat),
		dispatch.WithDiscord(notifyDiscord),
		dispatch.WithTeams(notifyTeams),
		dispatch.WithNtfy(notifyNtfy, notifySecret(notifyNtfyToken, "SWITCHTENDER_NOTIFY_NTFY_TOKEN")),
		dispatch.WithPagerDuty(notifyPagerDuty),
		dispatch.WithGrafana(notifyGrafana,
			notifySecret(notifyGrafanaToken, "SWITCHTENDER_NOTIFY_GRAFANA_TOKEN")),
		dispatch.WithTwilio(notifyTwilioSID,
			notifySecret(notifyTwilioToken, "SWITCHTENDER_NOTIFY_TWILIO_TOKEN"),
			notifyTwilioFrom, notifyTwilioTo),
		dispatch.WithEmail(emailer, onFailureOnly),
		dispatch.WithNotificationOutbox(notificationOutbox(bundle, sealer, log)),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()),
		dispatch.WithFactCache(bundle.FactCache()),
		dispatch.WithSourceSync(),
		dispatch.WithPolicies(policies),
		dispatch.WithDecisions(bundle.Decisions()),
		dispatch.WithPresence(bundle.Attention()))
	defer disp.Close()

	// The Needs attention view, the doctor, and the alert monitor read the same source, so the three
	// cannot disagree about what is stuck. A thresholds file that does not parse stops the server,
	// the same choice the policy file makes.
	attentionCfg, err := loadAttentionConfig(log)
	if err != nil {
		return err
	}
	attentionSrc := attentionSource(bundle, attentionCfg)
	attentionMonitor := attention.NewMonitor(attentionSrc, bundle.Attention(), disp, log, 0)
	attentionMonitor.Start()
	defer attentionMonitor.Close()

	scheduler := schedule.NewScheduler(schedules, disp, log,
		schedule.WithInterval(scheduleInterval), schedule.WithTemplates(bundle.Templates()),
		schedule.WithAudits(bundle.Audits()),
		// A fire whose inventory matched no hosts is skipped, and the targets attached to the
		// schedule hear about it through the same named-target path its runs take.
		schedule.WithSkipNotifier(disp),
		// A schedule waits for its own previous run rather than stacking a second copy of the same
		// work on the same hosts.
		schedule.WithRunActive(schedule.ActiveIn(store)))
	scheduler.Start()
	defer scheduler.Close()

	runsWindow, err := parseRetention(retainRuns)
	if err != nil {
		return err
	}
	eventsWindow, err := parseRetention(retainEvents)
	if err != nil {
		return err
	}
	sweeper := retention.NewSweeper(store, log,
		retention.WithRetainRuns(runsWindow), retention.WithRetainEvents(eventsWindow),
		retention.WithRetainHistory(retainHistory), retention.WithInterval(retentionInterval))
	sweeper.Start()
	defer sweeper.Close()

	if spanCadence != 0 && (spanCadence < time.Second || spanCadence%time.Second != 0) {
		return fmt.Errorf("--span-cadence must be a whole number of seconds, at least 1s, got %s",
			spanCadence)
	}
	if spanCadence > 0 {
		var beatOpts []spanbeat.Option
		if serveAnchorTSAURL != "" {
			anchors, ok := bundle.Audits().(audit.AnchorStore)
			if !ok {
				return fmt.Errorf("--anchor-tsa-url is set but this store keeps no anchors")
			}
			client := &http.Client{Timeout: anchorTimeout}
			beatOpts = append(beatOpts, spanbeat.WithAnchorFunc(
				func(ctx context.Context, b spanbeat.AppendedBeat) error {
					ctx, cancel := context.WithTimeout(ctx, anchorTimeout)
					defer cancel()
					a, err := audit.NewAnchor(ctx, client, audit.AnchorRFC3161, serveAnchorTSAURL,
						audit.AnchorShapeLinear, producerInstallID(producer), b.Seq, b.Hash, time.Now())
					if err != nil {
						return err
					}
					if err := audit.CheckAnchorTime(a, b.At); err != nil {
						return err
					}
					return anchors.SaveAnchor(ctx, a)
				}))
			log.Info("span beats will be anchored", zap.String("tsa", serveAnchorTSAURL))
		}
		beats := spanbeat.NewEmitter(auditBeatStore{store: bundle.Audits()}, spanCadence, log, beatOpts...)
		beats.Start()
		defer beats.Close()
		log.Info("span beats enabled", zap.Duration("cadence", spanCadence))
	}

	// The evidence a review samples from is produced on a cadence rather than on the day it is
	// asked for. A pack nobody generated is the same as no pack when an auditor asks.
	// A negative cadence used to pass the pairing check and then fall through the positive guard,
	// so the feature was silently off while the flags said it was on.
	if evidenceCadence < 0 {
		return fmt.Errorf("--evidence-cadence must be positive, got %s", evidenceCadence)
	}
	if (evidenceDir == "") != (evidenceCadence == 0) {
		return fmt.Errorf("--evidence-dir and --evidence-cadence are set together; " +
			"a directory with no cadence writes nothing and a cadence with no directory has " +
			"nowhere to write")
	}
	if evidenceCadence > 0 {
		if evidenceCadence < time.Hour {
			return fmt.Errorf("--evidence-cadence must be at least 1h, got %s", evidenceCadence)
		}
		// The register is what Team buys, so an install that never bought it is refused at startup
		// rather than started to quietly write a paid artifact every cadence.
		//
		// A lapse is not that install. This said it matched how the SSO flags behave, and it did
		// not: SSO turns itself off, says so once, and keeps serving, because a lapsed license
		// takes nothing and a running install is not bricked over a billing dispute. Here it
		// returned, so a lapsed Team install running with an evidence cadence exited before it
		// bound a listener. Dark at midnight, which is the outcome the terms name.
		licensed := true
		if aerr := license.Allow(license.FeatureRegister); aerr != nil {
			if !errors.Is(aerr, license.ErrLapsed) {
				return aerr
			}
			log.Warn("serve: the license for the period change register lapsed, so the register " +
				"is off and no evidence packs are written. Everything else keeps working, and " +
				"every pack already written stays valid and verifies offline. Renew to turn it " +
				"back on: https://switchtender.com/pricing")
			licensed = false
		}
		if licensed {
			var packProducer audit.Identity
			if producer != nil {
				packProducer = *producer
			}
			packs := evidence.NewEmitter(bundle.Runs(), bundle.Audits(), packProducer, evidenceDir,
				evidenceCadence,
				log, evidence.WithNotify(func(path string, from, to time.Time) {
					log.Info("evidence pack ready", zap.String("path", path),
						zap.Time("from", from), zap.Time("to", to))
				}))
			if err := packs.Start(); err != nil {
				return err
			}
			defer packs.Close()
			log.Info("periodic change registers enabled",
				zap.String("dir", evidenceDir), zap.Duration("cadence", evidenceCadence))
		}
	}

	// The SIEM is where operators already look; a receipt on every forwarded event means any
	// sampled event can be held against the live chain. The cursor advances only on delivery,
	// so an outage delays events rather than dropping them.
	if forwardURL != "" || forwardSyslog != "" {
		if bundle.Audits() == nil {
			return fmt.Errorf("--forward-url and --forward-syslog need the audit trail enabled")
		}
		if forwardInterval < time.Second {
			return fmt.Errorf("--forward-interval must be at least 1s, got %s", forwardInterval)
		}
		var sinks []forward.Sink
		if forwardURL != "" {
			parsed, err := url.Parse(forwardURL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return fmt.Errorf("--forward-url must be an http or https URL")
			}
			headers := map[string]string{}
			for _, raw := range forwardHeaders {
				name, value, ok := strings.Cut(raw, ":")
				if !ok || strings.TrimSpace(name) == "" {
					return fmt.Errorf("--forward-header %q must look like 'Name: value'", raw)
				}
				headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
			}
			sinks = append(sinks, forward.NewHTTPSink(forwardURL, headers, nil))
		}
		if forwardSyslog != "" {
			if _, _, err := net.SplitHostPort(forwardSyslog); err != nil {
				return fmt.Errorf("--forward-syslog must be host:port: %w", err)
			}
			host, _ := os.Hostname()
			sinks = append(sinks, forward.NewSyslogSink(forwardSyslog, forwardSyslogTLS, host))
		}
		tail := forward.NewForwarder(bundle.Audits(), sinks, forwardState, forwardInterval, log)
		if err := tail.Start(); err != nil {
			return err
		}
		defer tail.Close()
		log.Info("audit forwarding enabled", zap.String("state", forwardState),
			zap.Int("sinks", len(sinks)))
	}

	var oidcAuth *server.OIDCAuth
	if serveOIDCIssuer != "" {
		oidcAuth, err = server.NewOIDCAuth(cmd.Context(), serveOIDCIssuer, serveOIDCClientID,
			os.Getenv("SWITCHTENDER_OIDC_CLIENT_SECRET"), serveOIDCRedirectURL,
			user.Role(serveOIDCDefaultRole), bundle.Users(), bundle.Tokens(), log)
		if err != nil {
			return err
		}
	}

	var ldapAuth *server.LDAPAuth
	if serveLDAPURL != "" {
		ldapAuth, err = server.NewLDAPAuth(serveLDAPURL, serveLDAPBindDN,
			os.Getenv("SWITCHTENDER_LDAP_PASSWORD"), serveLDAPBaseDN, serveLDAPUserFilter,
			user.Role(serveLDAPDefaultRole), parseRoleMap(serveLDAPRoleMap), bundle.Users(), log)
		if err != nil {
			return err
		}
	}

	var samlAuth *server.SAMLAuth
	if serveSAMLIDPMetadataURL != "" {
		samlAuth, err = server.NewSAMLAuth(cmd.Context(), serveSAMLIDPMetadataURL, serveSAMLBaseURL,
			serveSAMLCert, serveSAMLKey, serveSAMLUsernameAttr, serveSAMLGroupsAttr,
			user.Role(serveSAMLDefaultRole), parseRoleMap(serveSAMLRoleMap),
			bundle.Users(), bundle.Tokens(), log)
		if err != nil {
			return err
		}
	}

	// Organizations without separation are a shape somebody gets wrong quietly. Grants default open,
	// so an object nobody has granted falls back to the caller's global role, and an operator who
	// created a second organization and assumed it was a boundary has none: every tenant reads and
	// cancels every other tenant's runs. That is documented, and a default is stronger than a
	// document, so an install that has more than one organization and has not turned separation on
	// is told at startup rather than finding out from a customer.
	if !serveStrictGrants {
		if orgs, err := bundle.Orgs().List(cmd.Context()); err == nil && len(orgs) > 1 {
			log.Warn("this install has more than one organization and separation between them is "+
				"off, so a member of one can read and cancel another's runs: pass --strict-grants "+
				"to deny access to an object that has no grant instead of falling back to the "+
				"caller's global role", zap.Int("organizations", len(orgs)))
		}
	}

	var jwtAuth *server.JWTAuth
	if serveJWTJWKSURL != "" {
		// An issuer usually mints tokens for more than one application, and the audience claim is
		// what says which one a token was for. Without it every token that issuer signs is accepted
		// here, including one minted for a different application entirely, so a service holding a
		// token for something else at the same provider can sign in as whatever its claims map to.
		// The check is optional because a single-application issuer does not need it, not because
		// leaving it off is equivalent.
		if serveJWTAudience == "" {
			log.Warn("jwt sign-in accepts any audience: set --jwt-audience so a token minted for " +
				"another application at the same issuer is not accepted here")
		}
		jwtAuth, err = server.NewJWTAuth(cmd.Context(), serveJWTJWKSURL, serveJWTIssuer,
			serveJWTAudience, serveJWTUsernameClaim, serveJWTGroupsClaim,
			user.Role(serveJWTDefaultRole), parseRoleMap(serveJWTRoleMap), bundle.Users(), log)
		if err != nil {
			return err
		}
	}

	aiProvider, err := ai.New(serveAIProvider, serveAIModel, serveAIURL, os.Getenv("SWITCHTENDER_AI_KEY"))
	if err != nil {
		return err
	}

	// Worker pools are loaded before the server is built, so a malformed file refuses to start
	// rather than quietly falling back to one token that may lease from every queue.
	var workerPools *relay.Pools
	if serveWorkerPools != "" {
		workerPools, err = relay.LoadPools(serveWorkerPools)
		if err != nil {
			return err
		}
	}
	switch {
	case workerPools != nil:
		log.Info("mesh relay worker endpoints enabled, each token confined to its own queues",
			zap.String("pools", serveWorkerPools))
		// A pool that opted in to sealed secret delivery is named with the key its runs are sealed
		// to, so a key rotation can be confirmed from the log.
		keyIDs := workerPools.DeliveryKeyIDs()
		for _, name := range slices.Sorted(maps.Keys(keyIDs)) {
			log.Info("relay secret delivery enabled for a worker pool", zap.String("pool", name),
				zap.String("delivery_key", keyIDs[name]))
		}
	case workerToken() != "":
		log.Info("mesh relay worker endpoints enabled; every worker token may lease from every " +
			"queue, so set --worker-pools to confine them")
	}

	// The signal context is established before the server is built so live streams can watch it and
	// end themselves the moment draining starts, rather than holding shutdown open to its timeout.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(store, disp, log, server.WithStreamer(hub),
		server.WithShutdown(ctx),
		server.WithRunFiles(runFiles.Source, runFilesReport),
		server.WithCanceler(disp), server.WithRetrier(disp), server.WithApprover(disp),
		server.WithDecisions(bundle.Decisions()),
		// Relay workers hold no notification channels, so the control node announces for them.
		server.WithAnnouncer(disp),
		// Relay workers hold no credential store either, so the control node opens a claimed run's
		// secrets and seals them to the claiming pool's delivery key, when the pool registered one.
		server.WithSecretOpener(disp),
		// A worker's plan hands its saved plan file to the control node, which seals it onto the
		// apply it proposes, so the apply carries out exactly that plan.
		server.WithPlanSealer(disp),
		server.WithSchedules(schedules), server.WithTokens(bundle.Tokens()),
		server.WithCredentials(bundle.Credentials(), sealer),
		server.WithCredentialTypes(bundle.CredentialTypes()),
		server.WithFederation(issuer),
		server.WithProjects(bundle.Projects()),
		server.WithProjectFiles(syncer),
		server.WithTemplates(bundle.Templates()),
		server.WithUsers(bundle.Users()),
		server.WithInventories(bundle.Inventories()),
		server.WithPolicies(policies),
		server.WithAudit(bundle.Audits()),
		server.WithProducerIdentity(producer, resolveVersion()),
		server.WithProducerUnavailable(producerProblem),
		server.WithInventorySources(bundle.InventorySources(), disp),
		server.WithInventoryPreviewer(disp),
		server.WithTriggers(bundle.Triggers(), sealer),
		server.WithNotificationTargets(bundle.Notifications()),
		server.WithFactCache(bundle.FactCache()),
		callbackServeOption(disp),
		server.WithAttention(attentionSrc),
		server.WithReviewReporting(servePublicURL, nil, 0),
		server.WithReviewStore(bundle.ReviewReports()),
		server.WithTeams(bundle.Teams()),
		server.WithOrgs(bundle.Orgs()),
		server.WithGrants(bundle.Grants(), serveStrictGrants),
		server.WithReadOnly(serveReadOnly),
		server.WithRelay(store, workerToken()),
		server.WithWorkerPools(workerPools),
		server.WithMatrixCap(serveMatrixCap),
		server.WithOIDC(oidcAuth),
		server.WithSAML(samlAuth),
		server.WithLDAP(ldapAuth),
		server.WithJWT(jwtAuth),
		server.WithAI(aiProvider),
		server.WithDocs(docsFS),
		// An install whose way in is single sign-on authenticates from the first request. Its
		// token and account tables are empty until somebody signs in, and deriving enforcement
		// from them served the whole API to anonymous callers as admin in the meantime.
		enforcedAuthOption(externalAuthConfigured()))
	// A review plan still in flight when the server last stopped is reported on again, so its pull
	// request is not left showing a plan that never finishes.
	srv.ResumeReviews(ctx)
	httpServer := &http.Server{
		Addr:              serveAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}

	if (serveTLSCert == "") != (serveTLSKey == "") {
		return fmt.Errorf("both --tls-cert and --tls-key are required to serve HTTPS")
	}
	tls := serveTLSCert != "" && serveTLSKey != ""

	errCh := make(chan error, 1)
	go func() {
		scheme := "http"
		if tls {
			scheme = "https"
		}
		log.Info("switchtender serving", zap.String("addr", serveAddr), zap.String("scheme", scheme))
		var serveErr error
		switch {
		case serveListener != nil && tls:
			serveErr = httpServer.ServeTLS(serveListener, serveTLSCert, serveTLSKey)
		case serveListener != nil:
			serveErr = httpServer.Serve(serveListener)
		case tls:
			serveErr = httpServer.ListenAndServeTLS(serveTLSCert, serveTLSKey)
		default:
			serveErr = httpServer.ListenAndServe()
		}
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		errCh <- serveErr
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received: draining")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		// A webhook answered before its launch finished is carried through rather than cut off. Its
		// delivery is on the chain either way, so one still running when the drain gives up shows a
		// fire with no run after it, and a redelivery launches it.
		if !srv.WaitForHooks(shutdownCtx) {
			log.Warn("shutdown: webhook launches still running were cut off; their deliveries are " +
				"recorded and a redelivery launches them")
		}
		return <-errCh
	}
}

// notifySecret returns a notification secret, preferring the environment variable so it never has to
// appear on the command line.
//
// These were flag-only. A run executes as a child of this process under the same uid, so an
// operator-role user could read the server's argv from inside a bash run and lift a third-party
// credential their role was never granted: the ntfy bearer, the Grafana API token, the Twilio auth
// token. The environment is already closed against exactly that, because filterRunEnv drops every
// SWITCHTENDER_ variable before a run sees it, so moving the secret there puts it behind the boundary
// the rest of the configuration already sits behind. The flag stays for compatibility and for a
// deployment that does not care, and its help now says which channel is safe.
func notifySecret(flagValue, env string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return flagValue
}

// loadProducerIdentity reads the install's producer signing identity, resolving where it lives
// first so a server with nowhere durable to keep it says so rather than signing with a key the next
// restart throws away.
func loadProducerIdentity(ctx context.Context, audits audit.Store, db string) (audit.Identity, error) {
	dir, err := identityDir(db)
	if err != nil {
		return audit.Identity{}, err
	}
	return identityForChain(ctx, audits, db, dir)
}

// identityForChain loads the producer identity in dir for the chain audits holds. It creates one only
// for a chain that no install has claimed yet. A chain already bound to an install whose key is not
// in dir means the key was lost or the directory is the wrong one, and minting a new key there
// started a second install: every bundle signed afterward named an install the earlier entries do
// not, so none of them could verify, while the command printed a fingerprint to publish. A key that
// names a different install than the chain is refused for the same reason.
func identityForChain(ctx context.Context, audits audit.Store, db, dir string) (audit.Identity, error) {
	bound := ""
	if reader, ok := audits.(audit.InstallReader); ok {
		var err error
		if bound, err = reader.BoundInstall(ctx); err != nil {
			return audit.Identity{}, err
		}
	}
	if bound != "" && !audit.IdentityPresent(dir) {
		where := dir
		if abs, err := filepath.Abs(dir); err == nil {
			where = abs
		}
		return audit.Identity{}, fmt.Errorf("%w: the audit chain belongs to install %s, and its key "+
			"is not in %s. Restore %s there from a backup, or set %s to the seed this install signs "+
			"with. A new key would sign as a different install, and no earlier entry would verify "+
			"under it", errLostIdentity, bound, where, audit.IdentityFile, identity.KeyEnv)
	}
	id, err := audit.LoadIdentityForStore(db, dir)
	if err != nil {
		return audit.Identity{}, err
	}
	if bound != "" && id.InstallID != bound {
		return audit.Identity{}, fmt.Errorf("%w: the key in use names install %s, and the audit chain "+
			"belongs to install %s. Restore that install's %s, or set %s to its seed",
			errForeignIdentity, id.InstallID, bound, audit.IdentityFile, identity.KeyEnv)
	}
	return id, nil
}

// dbEnvVar names the database when --db is not given.
const dbEnvVar = "SWITCHTENDER_DB"

// dbFromEnv returns the database SWITCHTENDER_DB names when --db was not given, and the flag's value
// otherwise. A PostgreSQL DSN carries its password, and on the command line it showed in the process
// list and in the pod spec of every chart install, readable by anyone allowed to list pods.
func dbFromEnv(cmd *cobra.Command, flagValue string) string {
	if cmd.Flags().Changed("db") {
		return flagValue
	}
	if v := strings.TrimSpace(os.Getenv(dbEnvVar)); v != "" {
		return v
	}
	return flagValue
}

// imageResolverOption returns the dispatch option that resolves each submitted run's image tag to
// the digest its registry serves, through a client that refuses the addresses a server-side request
// forgery aims at. With --image-digest-lookup off it sets none, and every image stays bound to its
// tag.
func imageResolverOption() dispatch.Option {
	if !serveImageDigestLookup {
		return dispatch.WithImageResolver(nil)
	}
	client := &http.Client{Transport: safedial.Transport(), Timeout: imageref.DefaultTimeout}
	return dispatch.WithImageResolver(imageref.NewResolver(client, imageref.DefaultTimeout))
}
