package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/evidence"
	"github.com/kordloom/switchtender/internal/extplugin"
	"github.com/kordloom/switchtender/internal/forward"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/logutil"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/retention"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
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
		"Network mode for containerized runs, as docker --network, for example bridge or none.")
	cmd.Flags().StringVar(&containerRuntime, "container-runtime", "docker",
		"Container CLI for containerized runs: docker or podman.")
	cmd.Flags().StringVar(&containerPullPolicy, "container-pull-policy", "missing",
		"Image pull policy for containerized runs, as docker --pull: always, missing, or never.")
}

// containerLimitsFromFlags builds the ContainerLimits from the shared container flag values.
func containerLimitsFromFlags() roundhouse.ContainerLimits {
	return roundhouse.ContainerLimits{
		Memory: containerMemory, CPUs: containerCPUs,
		PidsLimit: containerPidsLimit, Network: containerNetwork,
	}
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
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "",
		"TLS certificate file, to serve HTTPS directly. Requires --tls-key.")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "",
		"TLS private key file, to serve HTTPS directly. Requires --tls-cert.")
	serveCmd.Flags().DurationVar(&scheduleInterval, "schedule-interval", schedule.DefaultInterval,
		"How often the scheduler checks for due schedules.")
	serveCmd.Flags().StringArrayVar(&notifyWebhooks, "notify-webhook", nil,
		"URL that receives a JSON notification when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifySlack, "notify-slack", nil,
		"Slack incoming webhook URL that receives a message when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyMattermost, "notify-mattermost", nil,
		"Mattermost incoming webhook URL that receives a message when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyRocketChat, "notify-rocketchat", nil,
		"Rocket.Chat incoming webhook URL that receives a message when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyDiscord, "notify-discord", nil,
		"Discord incoming webhook URL that receives a message when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyTeams, "notify-teams", nil,
		"Microsoft Teams incoming webhook URL that receives an Adaptive Card when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyNtfy, "notify-ntfy", nil,
		"ntfy topic URL that receives a notification when a run finishes, such as https://ntfy.sh/my-topic. Repeatable.")
	serveCmd.Flags().StringVar(&notifyNtfyToken, "notify-ntfy-token", "",
		"Optional bearer token for a protected ntfy topic, applied to every --notify-ntfy URL.")
	serveCmd.Flags().StringArrayVar(&notifyPagerDuty, "notify-pagerduty", nil,
		"PagerDuty Events API routing key that triggers an incident when a run fails. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifyGrafana, "notify-grafana", nil,
		"Grafana base URL that receives an annotation when a run finishes. Repeatable.")
	serveCmd.Flags().StringVar(&notifyGrafanaToken, "notify-grafana-token", "",
		"Bearer token for the Grafana annotations API, applied to every --notify-grafana URL.")
	serveCmd.Flags().StringVar(&notifyTwilioSID, "notify-twilio-sid", "",
		"Twilio Account SID for SMS notifications on a failed run.")
	serveCmd.Flags().StringVar(&notifyTwilioToken, "notify-twilio-token", "",
		"Twilio Auth Token, paired with --notify-twilio-sid.")
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
	registerContainerFlags(serveCmd)
	registerGalaxyFlag(serveCmd)
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
		"When to email: failure for failed runs only, or finish for every terminal run.")
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
	// Teams returns the team store.
	Teams() team.Store
	// Orgs returns the organization store.
	Orgs() org.Store
	// Grants returns the per-object access grant store.
	Grants() grant.Store
	// Close closes the underlying database.
	Close() error
}

// openBundle opens the stores for the --db value: a postgres:// or postgresql:// DSN selects the
// PostgreSQL backend, anything else is a SQLite file path.
func openBundle(db string) (storeBundle, error) {
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		return pgstore.Open(db)
	}
	return sqlitestore.Open(db)
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

// identityDir returns the directory holding the producer signing identity for a database target.
// serve, which signs the bundles it serves, and the bundle command, which signs the bundle it
// emits, both derive it here so one install mints a single key and every tool reads that same key. A
// SQLite file keeps its identity beside it, so a copied database carries its key. A postgres DSN has
// no filesystem home: filepath.Dir on a DSN yields a cwd-relative junk directory whose name embeds
// the DSN's user:password@host, both wrong and a credential leak, so the identity falls back to a
// stable per-user directory instead.
func identityDir(db string) string {
	if strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://") {
		base, err := os.UserConfigDir()
		if err != nil || base == "" {
			base = os.TempDir()
		}
		return filepath.Join(base, "switchtender", "identity")
	}
	dir := filepath.Dir(db)
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

// runServe builds the server dependencies and serves until interrupted.
// externalAuthConfigured reports whether any single sign-on or federated auth provider is set, in
// which case the API is not wide open even before an API token exists.
func externalAuthConfigured() bool {
	return serveOIDCIssuer != "" || serveLDAPURL != "" ||
		serveSAMLIDPMetadataURL != "" || serveJWTJWKSURL != ""
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
func tokenCountGuard(count int, countErr error, readOnly, externalAuth, loopback bool,
	addr string) (servePosture, error) {
	if countErr != nil {
		return postureReady, fmt.Errorf("refusing to serve on %s: cannot determine whether any API "+
			"tokens exist, so the API cannot be safely exposed: %w", addr, countErr)
	}
	if count > 0 {
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

// bootstrapAdminToken mints the initial admin token for an install that has none and prints it once.
//
// The value goes to stderr with fmt rather than through the logger. A token is exactly what the
// logging rules say never to log, and the run log is the wrong place for a credential; this is a
// one-time handover to the person at the terminal, the same disclosure 'token new' makes.
//
// The mint is recorded in the audit chain like any other token creation, so an install cannot come
// into existence with an admin credential the trail does not mention.
func bootstrapAdminToken(ctx context.Context, bundle storeBundle, addr string) error {
	plain, tok, err := auth.New("initial")
	if err != nil {
		return fmt.Errorf("mint the initial admin token: %w", err)
	}
	if err := bundle.Tokens().Save(ctx, tok); err != nil {
		return fmt.Errorf("save the initial admin token: %w", err)
	}
	if err := recordCLI(ctx, bundle.Audits(), "/cli/serve/bootstrap-token"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, `
  No API tokens exist, so %s would have served an unauthenticated API.
  Created an initial admin token instead. It is shown only this once:

      %s

  Use it as:     Authorization: Bearer %s
  Name more:     switchtender token new --name ci
  Revoke this:   switchtender token revoke %s

`, addr, plain, plain, tok.ID)
	return nil
}

func runServe(cmd *cobra.Command, _ []string) error {
	log, err := logutil.New()
	if err != nil {
		return fmt.Errorf("init logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	bundle, err := openBundle(serveDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = bundle.Close() }()
	store, schedules := bundle.Runs(), bundle.Schedules()

	n, cerr := bundle.Tokens().Count(cmd.Context())
	posture, gerr := tokenCountGuard(n, cerr, serveReadOnly, externalAuthConfigured(),
		isLoopbackAddr(serveAddr), serveAddr)
	if gerr != nil {
		return gerr
	}
	switch posture {
	case postureWarn:
		log.Warn("no API tokens exist. The API is UNAUTHENTICATED until you create one. Run: switchtender token new")
	case postureBootstrap:
		if err := bootstrapAdminToken(cmd.Context(), bundle, serveAddr); err != nil {
			return err
		}
	case postureReady:
	}

	sealer := newSealerFromEnv(log)
	// The producer identity signs LoomSeal bundles and is published so a relying party can pin its
	// fingerprint. It is created on first start in the install's identity directory, the same one the
	// bundle command reads, so a bundle is signed with the key serve publishes. A failure to create
	// it is not fatal: the server still runs and still records the audit chain, it just cannot
	// attribute a bundle, which is better than refusing to start over an export feature.
	var producer *audit.Identity
	if id, err := audit.LoadIdentityForStore(serveDB, identityDir(serveDB)); err != nil {
		log.Warn("producer identity unavailable, bundles cannot be attributed: " + err.Error())
	} else {
		producer = &id
		log.Info("producer identity ready", zap.String("key_id", id.KeyID()),
			zap.String("install_id", id.InstallID))
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
		count, _ := filePolicies.List(cmd.Context())
		log.Info("serve: approval policies read from file",
			zap.String("path", policyFile), zap.Int("policies", len(count)))
	}

	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub),
		dispatch.WithAudits(bundle.Audits()),
		dispatch.WithWorkers(serveWorkers),
		dispatch.WithMaxShards(serveMaxShards),
		dispatch.WithRunTimeout(serveRunTimeout),
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithCredentialTypes(bundle.CredentialTypes()),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithDefaultImage(serveDefaultImage),
		dispatch.WithWebhooks(notifyWebhooks),
		dispatch.WithSlack(notifySlack),
		dispatch.WithMattermost(notifyMattermost),
		dispatch.WithRocketChat(notifyRocketChat),
		dispatch.WithDiscord(notifyDiscord),
		dispatch.WithTeams(notifyTeams),
		dispatch.WithNtfy(notifyNtfy, notifyNtfyToken),
		dispatch.WithPagerDuty(notifyPagerDuty),
		dispatch.WithGrafana(notifyGrafana, notifyGrafanaToken),
		dispatch.WithTwilio(notifyTwilioSID, notifyTwilioToken, notifyTwilioFrom, notifyTwilioTo),
		dispatch.WithEmail(emailer, onFailureOnly),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()),
		dispatch.WithSourceSync(),
		dispatch.WithPolicies(policies))
	defer disp.Close()

	scheduler := schedule.NewScheduler(schedules, disp, log,
		schedule.WithInterval(scheduleInterval), schedule.WithTemplates(bundle.Templates()),
		schedule.WithAudits(bundle.Audits()),
		// A schedule waits for its own previous run rather than stacking a second copy of the same
		// work on the same hosts.
		schedule.WithRunActive(func(ctx context.Context, runID string) (bool, error) {
			r, err := store.Get(ctx, runID)
			if errors.Is(err, run.ErrNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return !r.Status.Terminal(), nil
		}))
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
		var packInstallID string
		if producer != nil {
			packInstallID = producer.InstallID
		}
		packs := evidence.NewEmitter(bundle.Runs(), bundle.Audits(), packInstallID, evidenceDir,
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

	var jwtAuth *server.JWTAuth
	if serveJWTJWKSURL != "" {
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
		server.WithCanceler(disp), server.WithRetrier(disp), server.WithApprover(disp),
		server.WithSchedules(schedules), server.WithTokens(bundle.Tokens()),
		server.WithCredentials(bundle.Credentials(), sealer),
		server.WithCredentialTypes(bundle.CredentialTypes()),
		server.WithProjects(bundle.Projects()),
		server.WithProjectFiles(syncer),
		server.WithTemplates(bundle.Templates()),
		server.WithUsers(bundle.Users()),
		server.WithInventories(bundle.Inventories()),
		server.WithPolicies(policies),
		server.WithAudit(bundle.Audits()),
		server.WithProducerIdentity(producer, resolveVersion()),
		server.WithInventorySources(bundle.InventorySources(), disp),
		server.WithTriggers(bundle.Triggers(), sealer),
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
		return <-errCh
	}
}
