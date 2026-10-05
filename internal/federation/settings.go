package federation

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
)

// Deliveries a federated credential can choose between.
const (
	// DeliveryFile writes the token to a file and points the tool's own variables at it, so the
	// tool performs the exchange and SwitchTender makes no call to the cloud. It is the default.
	DeliveryFile = "file"
	// DeliveryExchange has SwitchTender exchange the token at launch and inject the short-lived
	// credentials the cloud returns, for a tool that cannot read a token file.
	DeliveryExchange = "exchange"
)

// Default audiences, the values each cloud's federation setup expects unless told otherwise.
const (
	// DefaultAWSAudience is the audience an AWS IAM OpenID Connect provider is usually registered
	// with.
	DefaultAWSAudience = "sts.amazonaws.com"
	// DefaultAzureAudience is the audience a Microsoft Entra federated identity credential expects.
	DefaultAzureAudience = "api://AzureADTokenExchange"
	// gcpIAMPrefix prefixes a workload identity pool provider's full resource name.
	gcpIAMPrefix = "//iam.googleapis.com/"
)

// Default cloud endpoints the exchange delivery and the generated credentials file name.
const (
	// defaultGCPSTSURL is Google's security token service token endpoint.
	defaultGCPSTSURL = "https://sts.googleapis.com/v1/token"
	// defaultGCPIAMURL is Google's IAM credentials service, which impersonates a service account.
	defaultGCPIAMURL = "https://iamcredentials.googleapis.com"
	// gcpCloudPlatformScope is the scope a federated or impersonated Google token is requested for.
	gcpCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
)

// Config is a federated credential's settings, parsed and checked. Each field maps to one settings
// key named in its comment, and a key a kind does not read is refused rather than ignored, so a
// misspelled key fails when the credential is saved instead of when a run is denied.
type Config struct {
	// Kind is the credential kind the settings belong to.
	Kind credential.Kind
	// Audience is the token's aud claim, from audience.
	Audience string
	// TokenTTL is how long the token is valid, from token_ttl.
	TokenTTL time.Duration
	// Environment is the token's environment claim and subject segment, from environment.
	Environment string
	// Delivery is file or exchange, from delivery.
	Delivery string
	// RoleARN is the AWS role to assume, from role_arn.
	RoleARN string
	// Region is the AWS region, from region.
	Region string
	// SessionName is the AWS role session name, from session_name. Empty names the session after
	// the run.
	SessionName string
	// SessionDuration is the AWS role session lifetime in seconds for the exchange delivery, from
	// session_duration. Zero leaves the STS default of one hour.
	SessionDuration int
	// STSEndpoint overrides the token service endpoint, from sts_endpoint: an AWS STS endpoint for
	// a partition, a FIPS endpoint, or a VPC endpoint, or Google's STS token URL.
	STSEndpoint string
	// Provider is the Google workload identity pool provider's full resource name, from provider.
	Provider string
	// ServiceAccount is the Google service account to impersonate, from service_account.
	ServiceAccount string
	// IAMEndpoint overrides Google's IAM credentials service base URL, from iam_endpoint.
	IAMEndpoint string
	// ClientID is the Microsoft Entra application (client) id, from client_id.
	ClientID string
	// TenantID is the Microsoft Entra tenant id, from tenant_id.
	TenantID string
	// SubscriptionID is the Azure subscription, from subscription_id.
	SubscriptionID string
	// AuthorityHost is the Microsoft Entra authority for a sovereign cloud, from authority_host.
	AuthorityHost string
}

// settingKeys lists the settings each federated kind reads.
var settingKeys = map[credential.Kind][]string{
	credential.KindAWSOIDC: {"role_arn", "audience", "region", "delivery", "session_name",
		"session_duration", "sts_endpoint", "token_ttl", "environment"},
	credential.KindGCPOIDC: {"provider", "service_account", "audience", "delivery", "sts_endpoint",
		"iam_endpoint", "token_ttl", "environment"},
	credential.KindAzureOIDC: {"client_id", "tenant_id", "subscription_id", "audience",
		"authority_host", "token_ttl", "environment"},
	credential.KindOIDCToken: {"audience", "token_ttl", "environment"},
}

// SettingKeys returns the settings a federated kind reads, sorted, or nil for any other kind.
func SettingKeys(kind credential.Kind) []string {
	keys := slices.Clone(settingKeys[kind])
	sort.Strings(keys)
	return keys
}

// Patterns the settings are checked against. Each refuses a colon, so no setting can reach into the
// subject a trust policy matches by position.
var (
	// environmentPattern bounds an environment name.
	environmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	// roleARNPattern is an IAM role ARN in any partition.
	roleARNPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]{1,512}$`)
	// regionPattern is an AWS region name.
	regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d{1,2}$`)
	// sessionNamePattern is the character set and length STS allows for a role session name.
	sessionNamePattern = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)
	// providerPattern is a workload identity pool provider resource name, with or without the
	// //iam.googleapis.com/ prefix.
	providerPattern = regexp.MustCompile(`^(//iam\.googleapis\.com/)?projects/\d+/locations/global/` +
		`workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)
	// serviceAccountPattern is a service account email.
	serviceAccountPattern = regexp.MustCompile(
		`^[a-z0-9-]{6,30}@[a-z0-9.-]+\.iam\.gserviceaccount\.com$`)
	// azureIDPattern is a Microsoft Entra client, tenant, or subscription id, or a tenant domain.
	azureIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,127}$`)
)

// ParseSettings checks a federated credential's settings for kind and returns them parsed, with the
// defaults filled in. It refuses a key the kind does not read, a required key that is missing, and
// a value of the wrong shape.
func ParseSettings(kind credential.Kind, settings map[string]string) (Config, error) {
	allowed, ok := settingKeys[kind]
	if !ok {
		return Config{}, fmt.Errorf("%w: %s is not a federated kind", ErrSetting, kind)
	}
	for k := range settings {
		if !slices.Contains(allowed, k) {
			return Config{}, fmt.Errorf("%w: %s does not read %q; it reads %s", ErrSetting, kind, k,
				strings.Join(SettingKeys(kind), ", "))
		}
	}
	get := func(k string) string { return strings.TrimSpace(settings[k]) }
	cfg := Config{Kind: kind, Audience: get("audience"), Environment: get("environment"),
		Delivery: get("delivery"), TokenTTL: DefaultTokenTTL}
	if raw := get("token_ttl"); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl < MinTokenTTL || ttl > MaxTokenTTL {
			return Config{}, fmt.Errorf("%w: token_ttl must be a duration from %s to %s, such as 15m",
				ErrSetting, MinTokenTTL, MaxTokenTTL)
		}
		cfg.TokenTTL = ttl
	}
	if cfg.Environment != "" && !environmentPattern.MatchString(cfg.Environment) {
		return Config{}, fmt.Errorf("%w: environment must be letters, digits, dots, hyphens, or "+
			"underscores, at most 64", ErrSetting)
	}
	if cfg.Delivery == "" {
		cfg.Delivery = DeliveryFile
	}
	var err error
	switch kind {
	case credential.KindAWSOIDC:
		err = parseAWS(&cfg, get)
	case credential.KindGCPOIDC:
		err = parseGCP(&cfg, get)
	case credential.KindAzureOIDC:
		err = parseAzure(&cfg, get)
	default:
		if cfg.Audience == "" {
			err = fmt.Errorf("%w: oidc_token needs audience, the relying party the token is for",
				ErrSetting)
		}
		if err == nil && cfg.Delivery != DeliveryFile {
			err = fmt.Errorf("%w: oidc_token delivers by file only", ErrSetting)
		}
	}
	if err != nil {
		return Config{}, err
	}
	if strings.ContainsAny(cfg.Audience, "\r\n") || len(cfg.Audience) > 512 {
		return Config{}, fmt.Errorf("%w: audience must be one line of at most 512 bytes", ErrSetting)
	}
	return cfg, nil
}

// parseAWS fills and checks the aws_oidc settings.
func parseAWS(cfg *Config, get func(string) string) error {
	cfg.RoleARN, cfg.Region, cfg.SessionName = get("role_arn"), get("region"), get("session_name")
	if !roleARNPattern.MatchString(cfg.RoleARN) {
		return fmt.Errorf("%w: aws_oidc needs role_arn, an IAM role ARN such as "+
			"arn:aws:iam::123456789012:role/deploy", ErrSetting)
	}
	if cfg.Region != "" && !regionPattern.MatchString(cfg.Region) {
		return fmt.Errorf("%w: region %q is not an AWS region name", ErrSetting, cfg.Region)
	}
	if cfg.SessionName != "" && !sessionNamePattern.MatchString(cfg.SessionName) {
		return fmt.Errorf("%w: session_name must be 2 to 64 letters, digits, or +=,.@_-", ErrSetting)
	}
	if raw := get("session_duration"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 900 || n > 43200 {
			return fmt.Errorf("%w: session_duration must be whole seconds from 900 to 43200", ErrSetting)
		}
		cfg.SessionDuration = n
	}
	if cfg.Audience == "" {
		cfg.Audience = DefaultAWSAudience
	}
	if err := checkDelivery(cfg, DeliveryFile, DeliveryExchange); err != nil {
		return err
	}
	if cfg.SessionDuration != 0 && cfg.Delivery != DeliveryExchange {
		return fmt.Errorf("%w: session_duration applies to the exchange delivery; with the file "+
			"delivery the tool's SDK chooses the session length", ErrSetting)
	}
	return checkEndpoint(&cfg.STSEndpoint, "sts_endpoint", get("sts_endpoint"))
}

// parseGCP fills and checks the gcp_oidc settings.
func parseGCP(cfg *Config, get func(string) string) error {
	provider := get("provider")
	if !providerPattern.MatchString(provider) {
		return fmt.Errorf("%w: gcp_oidc needs provider, the workload identity pool provider such as "+
			"projects/123456789/locations/global/workloadIdentityPools/pool/providers/switchtender",
			ErrSetting)
	}
	cfg.Provider = gcpIAMPrefix + strings.TrimPrefix(provider, gcpIAMPrefix)
	cfg.ServiceAccount = get("service_account")
	if cfg.ServiceAccount != "" && !serviceAccountPattern.MatchString(cfg.ServiceAccount) {
		return fmt.Errorf("%w: service_account must be a service account email ending in "+
			".iam.gserviceaccount.com", ErrSetting)
	}
	if cfg.Audience == "" {
		// Google accepts the provider's own https URL as an audience without listing it, which is
		// the audience its documentation tells a pool to expect by default.
		cfg.Audience = "https:" + cfg.Provider
	}
	if err := checkDelivery(cfg, DeliveryFile, DeliveryExchange); err != nil {
		return err
	}
	if err := checkEndpoint(&cfg.STSEndpoint, "sts_endpoint", get("sts_endpoint")); err != nil {
		return err
	}
	if cfg.STSEndpoint == "" {
		cfg.STSEndpoint = defaultGCPSTSURL
	}
	if err := checkEndpoint(&cfg.IAMEndpoint, "iam_endpoint", get("iam_endpoint")); err != nil {
		return err
	}
	if cfg.IAMEndpoint == "" {
		cfg.IAMEndpoint = defaultGCPIAMURL
	}
	cfg.IAMEndpoint = strings.TrimRight(cfg.IAMEndpoint, "/")
	return nil
}

// parseAzure fills and checks the azure_oidc settings.
func parseAzure(cfg *Config, get func(string) string) error {
	cfg.ClientID, cfg.TenantID = get("client_id"), get("tenant_id")
	cfg.SubscriptionID = get("subscription_id")
	if !azureIDPattern.MatchString(cfg.ClientID) || !azureIDPattern.MatchString(cfg.TenantID) {
		return fmt.Errorf("%w: azure_oidc needs client_id and tenant_id", ErrSetting)
	}
	if cfg.SubscriptionID != "" && !azureIDPattern.MatchString(cfg.SubscriptionID) {
		return fmt.Errorf("%w: subscription_id is not an Azure subscription id", ErrSetting)
	}
	if cfg.Audience == "" {
		cfg.Audience = DefaultAzureAudience
	}
	if err := checkDelivery(cfg, DeliveryFile); err != nil {
		return err
	}
	return checkEndpoint(&cfg.AuthorityHost, "authority_host", get("authority_host"))
}

// checkDelivery refuses a delivery the kind does not offer.
func checkDelivery(cfg *Config, allowed ...string) error {
	if !slices.Contains(allowed, cfg.Delivery) {
		return fmt.Errorf("%w: %s delivers by %s", ErrSetting, cfg.Kind, strings.Join(allowed, " or "))
	}
	return nil
}

// checkEndpoint validates an endpoint override and stores it in dst. A token travels to it, so it
// must be https, or http to a loopback host, which is how a local test service listens.
func checkEndpoint(dst *string, name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%w: %s must be an absolute URL with no credentials in it", ErrSetting, name)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return fmt.Errorf("%w: %s must use https, since a token is sent to it", ErrSetting, name)
	}
	*dst = raw
	return nil
}
