package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/ai"
	"github.com/dcadolph/railwarden/internal/audit"
	"github.com/dcadolph/railwarden/internal/auth"
	"github.com/dcadolph/railwarden/internal/credential"
	"github.com/dcadolph/railwarden/internal/dispatch"
	"github.com/dcadolph/railwarden/internal/extplugin"
	"github.com/dcadolph/railwarden/internal/grant"
	"github.com/dcadolph/railwarden/internal/inventory"
	"github.com/dcadolph/railwarden/internal/invsource"
	"github.com/dcadolph/railwarden/internal/live"
	"github.com/dcadolph/railwarden/internal/logutil"
	"github.com/dcadolph/railwarden/internal/pgstore"
	"github.com/dcadolph/railwarden/internal/policy"
	"github.com/dcadolph/railwarden/internal/project"
	"github.com/dcadolph/railwarden/internal/retention"
	"github.com/dcadolph/railwarden/internal/roundhouse"
	"github.com/dcadolph/railwarden/internal/run"
	"github.com/dcadolph/railwarden/internal/schedule"
	"github.com/dcadolph/railwarden/internal/server"
	"github.com/dcadolph/railwarden/internal/sqlitestore"
	"github.com/dcadolph/railwarden/internal/team"
	"github.com/dcadolph/railwarden/internal/template"
	"github.com/dcadolph/railwarden/internal/trigger"
	"github.com/dcadolph/railwarden/internal/user"
)

const (
	// defaultServeAddr is the address the server listens on when --addr is not set. It binds loopback
	// so a fresh server is not exposed on the network before the operator creates the first token.
	defaultServeAddr = "127.0.0.1:8080"
	// defaultDBPath is the SQLite database file used when --db is not set.
	defaultDBPath = "railwarden.db"
	// defaultScheduleInterval is how often the scheduler checks for due schedules.
	defaultScheduleInterval = 15 * time.Second
	// shutdownTimeout bounds how long graceful HTTP shutdown waits for in-flight requests.
	shutdownTimeout = 15 * time.Second
	// readHeaderTimeout bounds how long the server waits to read request headers.
	readHeaderTimeout = 10 * time.Second
)

// serveAddr holds the value of the --addr flag.
var serveAddr string

// serveDB holds the value of the --db flag.
var serveDB string

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

// serveAllowContainerEE holds the value of the --allow-container-ee flag.
var serveAllowContainerEE bool

// serveStrictGrants holds the value of the --strict-grants flag.
var serveStrictGrants bool

// serveReadOnly holds the value of the --read-only flag.
var serveReadOnly bool

// serveMatrixCap holds the value of the --matrix-cap flag.
var serveMatrixCap int

// servePluginsDir holds the value of the --plugins-dir flag.
var servePluginsDir string

// serveOIDCIssuer, serveOIDCClientID, serveOIDCRedirectURL, and serveOIDCDefaultRole hold the
// OpenID Connect single sign-on flags. The client secret comes from RAILWARDEN_OIDC_CLIENT_SECRET.
var (
	serveOIDCIssuer      string
	serveOIDCClientID    string
	serveOIDCRedirectURL string
	serveOIDCDefaultRole string
)

// serveLDAP* hold the LDAP directory sign-in flags. The service bind password comes from
// RAILWARDEN_LDAP_PASSWORD.
var (
	serveLDAPURL         string
	serveLDAPBindDN      string
	serveLDAPBaseDN      string
	serveLDAPUserFilter  string
	serveLDAPDefaultRole string
	serveLDAPRoleMap     []string
)

// serveSAML* hold the SAML single sign-on flags. Railwarden is the service provider and the
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
// as by jwtmint, instead of a Railwarden token.
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

// retainRuns holds the value of the --retain-runs flag, a duration like 90d.
var retainRuns string

// retainEvents holds the value of the --retain-events flag, a duration like 30d.
var retainEvents string

// retentionInterval holds the value of the --retention-interval flag.
var retentionInterval time.Duration

// smtpAddr holds the value of the --smtp-addr flag, the SMTP server host:port.
var smtpAddr string

// smtpFrom holds the value of the --smtp-from flag, the sender address.
var smtpFrom string

// smtpTo holds the values of the repeatable --smtp-to flag, the recipient addresses.
var smtpTo []string

// smtpUsername holds the value of the --smtp-username flag; the password comes from the
// RAILWARDEN_SMTP_PASSWORD environment variable.
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
}

// containerLimitsFromFlags builds the ContainerLimits from the shared container flag values.
func containerLimitsFromFlags() roundhouse.ContainerLimits {
	return roundhouse.ContainerLimits{
		Memory: containerMemory, CPUs: containerCPUs,
		PidsLimit: containerPidsLimit, Network: containerNetwork,
	}
}

// pluginsDir returns the plugins directory to load: the flag when set, else the
// RAILWARDEN_PLUGINS_DIR environment variable. Empty means no plugins.
func pluginsDir(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("RAILWARDEN_PLUGINS_DIR")
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

// serveCmd runs the Railwarden HTTP server (the dispatcher).
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the Railwarden server.",
	RunE:  runServe,
}

// init registers serve command flags.
func init() {
	serveCmd.Flags().StringVar(&serveAddr, "addr", defaultServeAddr, "Address the server listens on. Loopback by default. Set 0.0.0.0 to expose it on the network.")
	serveCmd.Flags().StringVar(&serveDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN for the PostgreSQL backend.")
	serveCmd.Flags().StringVar(&serveTLSCert, "tls-cert", "",
		"TLS certificate file, to serve HTTPS directly. Requires --tls-key.")
	serveCmd.Flags().StringVar(&serveTLSKey, "tls-key", "",
		"TLS private key file, to serve HTTPS directly. Requires --tls-cert.")
	serveCmd.Flags().DurationVar(&scheduleInterval, "schedule-interval", defaultScheduleInterval,
		"How often the scheduler checks for due schedules.")
	serveCmd.Flags().StringArrayVar(&notifyWebhooks, "notify-webhook", nil,
		"URL that receives a JSON notification when a run finishes. Repeatable.")
	serveCmd.Flags().StringArrayVar(&notifySlack, "notify-slack", nil,
		"Slack incoming webhook URL that receives a message when a run finishes. Repeatable.")
	serveCmd.Flags().BoolVar(&serveAllowContainerEE, "allow-container-ee", false,
		"Allow runs whose project pins a container image to execute inside that image. Needs Docker.")
	registerContainerFlags(serveCmd)
	serveCmd.Flags().BoolVar(&serveStrictGrants, "strict-grants", false,
		"Deny non-admins access to an object that has no grants, instead of deferring to the role.")
	serveCmd.Flags().BoolVar(&serveReadOnly, "read-only", false,
		"Reject every mutating request, for a safely exposable instance.")
	serveCmd.Flags().IntVar(&serveMatrixCap, "matrix-cap", server.DefaultMatrixCap,
		"Largest host matrix, in cells, the UI draws before showing a notice. 0 means no limit.")
	serveCmd.Flags().StringVar(&servePluginsDir, "plugins-dir", "",
		"Directory of extension plugin binaries to load at startup. Empty loads none. Also RAILWARDEN_PLUGINS_DIR.")
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
	serveCmd.Flags().StringVar(&retainRuns, "retain-runs", "",
		"Delete terminal runs older than this, for example 90d. Empty keeps them forever.")
	serveCmd.Flags().StringVar(&retainEvents, "retain-events", "",
		"Drop run events and logs older than this, for example 30d. Empty keeps them forever.")
	serveCmd.Flags().DurationVar(&retentionInterval, "retention-interval", retention.DefaultInterval,
		"How often the retention sweeper runs.")
	serveCmd.Flags().StringVar(&smtpAddr, "smtp-addr", "",
		"SMTP server host:port for run notification emails. Empty disables email.")
	serveCmd.Flags().StringVar(&smtpFrom, "smtp-from", "", "Sender address for notification emails.")
	serveCmd.Flags().StringArrayVar(&smtpTo, "smtp-to", nil,
		"Recipient address for notification emails. Repeatable.")
	serveCmd.Flags().StringVar(&smtpUsername, "smtp-username", "",
		"SMTP username. The password comes from RAILWARDEN_SMTP_PASSWORD.")
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
		auth = smtp.PlainAuth("", smtpUsername, os.Getenv("RAILWARDEN_SMTP_PASSWORD"), host)
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
// both RAILWARDEN_ENCRYPTION_KEY and a stable RAILWARDEN_ENCRYPTION_SALT; when either is missing
// the Sealer is disabled and the reason is logged so the operator knows which value to set.
func newSealerFromEnv(log *zap.Logger) *credential.Sealer {
	key := os.Getenv("RAILWARDEN_ENCRYPTION_KEY")
	salt := os.Getenv("RAILWARDEN_ENCRYPTION_SALT")
	sealer := credential.NewSealer(key, salt)
	if sealer.Enabled() {
		return sealer
	}
	if key == "" {
		log.Warn("credentials disabled: set RAILWARDEN_ENCRYPTION_KEY to enable them")
	} else {
		log.Warn("credentials disabled: set a stable RAILWARDEN_ENCRYPTION_SALT alongside the key")
	}
	return sealer
}

// newAuditSignerFromEnv builds an audit export Signer from RAILWARDEN_AUDIT_KEY, a hex-encoded
// ed25519 seed. When it is unset, export signing is off; when it is malformed the server refuses to
// start so a bad key is caught, not silently ignored. The public key is logged so an operator can
// record it for offline verification.
func newAuditSignerFromEnv(log *zap.Logger) (*audit.Signer, error) {
	signer, err := audit.NewSigner(os.Getenv("RAILWARDEN_AUDIT_KEY"))
	if err != nil {
		return nil, err
	}
	if signer != nil {
		log.Info("audit export signing enabled", zap.String("public_key", signer.PublicKeyHex()))
	}
	return signer, nil
}

// projectCacheDir returns where project checkouts live: the user cache directory when available,
// the system temp directory otherwise.
func projectCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "railwarden", "projects")
}

// runServe builds the server dependencies and serves until interrupted.
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

	if n, cerr := bundle.Tokens().Count(cmd.Context()); cerr == nil && n == 0 {
		log.Warn("no API tokens exist. The API is UNAUTHENTICATED until you create one. Run: railwarden token new")
	}

	sealer := newSealerFromEnv(log)
	auditSigner, err := newAuditSignerFromEnv(log)
	if err != nil {
		return err
	}

	closePlugins, err := extplugin.Load(pluginsDir(servePluginsDir), log)
	if err != nil {
		return fmt.Errorf("load plugins: %w", err)
	}
	defer closePlugins()

	hub := live.NewHub()
	runner := roundhouse.NewSelectiveRunner(serveAllowContainerEE, containerLimitsFromFlags())
	syncer, err := project.NewSyncer(projectCacheDir())
	if err != nil {
		return fmt.Errorf("project cache: %w", err)
	}
	emailer, onFailureOnly := buildEmailer()
	disp := dispatch.New(store, runner, log, dispatch.WithPublisher(hub),
		dispatch.WithCredentials(bundle.Credentials(), sealer),
		dispatch.WithProjects(bundle.Projects(), syncer),
		dispatch.WithWebhooks(notifyWebhooks),
		dispatch.WithSlack(notifySlack),
		dispatch.WithEmail(emailer, onFailureOnly),
		dispatch.WithInventories(bundle.Inventories()),
		dispatch.WithInventorySources(bundle.InventorySources()),
		dispatch.WithPolicies(bundle.Policies()))
	defer disp.Close()

	scheduler := schedule.NewScheduler(schedules, disp, log,
		schedule.WithInterval(scheduleInterval), schedule.WithTemplates(bundle.Templates()))
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
		retention.WithInterval(retentionInterval))
	sweeper.Start()
	defer sweeper.Close()

	var oidcAuth *server.OIDCAuth
	if serveOIDCIssuer != "" {
		oidcAuth, err = server.NewOIDCAuth(cmd.Context(), serveOIDCIssuer, serveOIDCClientID,
			os.Getenv("RAILWARDEN_OIDC_CLIENT_SECRET"), serveOIDCRedirectURL,
			user.Role(serveOIDCDefaultRole), bundle.Users(), bundle.Tokens(), log)
		if err != nil {
			return err
		}
	}

	var ldapAuth *server.LDAPAuth
	if serveLDAPURL != "" {
		ldapAuth, err = server.NewLDAPAuth(serveLDAPURL, serveLDAPBindDN,
			os.Getenv("RAILWARDEN_LDAP_PASSWORD"), serveLDAPBaseDN, serveLDAPUserFilter,
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

	aiProvider, err := ai.New(serveAIProvider, serveAIModel, serveAIURL, os.Getenv("RAILWARDEN_AI_KEY"))
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr: serveAddr,
		Handler: server.New(store, disp, log, server.WithStreamer(hub),
			server.WithCanceler(disp), server.WithRetrier(disp), server.WithApprover(disp),
			server.WithSchedules(schedules), server.WithTokens(bundle.Tokens()),
			server.WithCredentials(bundle.Credentials(), sealer),
			server.WithProjects(bundle.Projects()),
			server.WithTemplates(bundle.Templates()),
			server.WithUsers(bundle.Users()),
			server.WithInventories(bundle.Inventories()),
			server.WithPolicies(bundle.Policies()),
			server.WithAudit(bundle.Audits()),
			server.WithAuditSigner(auditSigner),
			server.WithInventorySources(bundle.InventorySources(), disp),
			server.WithTriggers(bundle.Triggers(), sealer),
			server.WithTeams(bundle.Teams()),
			server.WithGrants(bundle.Grants(), serveStrictGrants),
			server.WithReadOnly(serveReadOnly),
			server.WithMatrixCap(serveMatrixCap),
			server.WithOIDC(oidcAuth),
			server.WithSAML(samlAuth),
			server.WithLDAP(ldapAuth),
			server.WithJWT(jwtAuth),
			server.WithAI(aiProvider),
			server.WithDocs(docsFS)).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
		log.Info("railwarden serving", zap.String("addr", serveAddr), zap.String("scheme", scheme))
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
