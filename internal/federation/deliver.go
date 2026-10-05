package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/credential"
)

// TokenFileEnvVar names the file holding the raw identity token the generic oidc_token kind
// delivers. The token is handed over by file alone: a variable holding it would be inherited by
// every process the run starts and read by anything that prints its environment.
const TokenFileEnvVar = "SWITCHTENDER_OIDC_TOKEN_FILE"

// Delivery is what one federated credential contributes to a run.
type Delivery struct {
	// Env holds KEY=VALUE entries for the run's environment.
	Env []string
	// Files are the paths written inside the run's private directory. A containerized run mounts
	// each, so the path an environment variable names resolves inside the container too.
	Files []string
	// Secrets are the values to mask out of the run's output: the token, and any credential an
	// exchange returned.
	Secrets []string
}

// Deliver mints the run's identity token under cfg and turns it into what the tool reads, writing
// any file into dir, which the caller created private and removes when the run ends. With the file
// delivery nothing leaves this process; with the exchange delivery the token is sent to the cloud's
// token service and the credentials it returns are delivered instead.
func (i *Issuer) Deliver(ctx context.Context, cfg Config, c Claims, dir string) (Delivery, error) {
	token, err := i.MintFor(ctx, cfg, c)
	if err != nil {
		return Delivery{}, err
	}
	return DeliverToken(ctx, cfg, c, token, dir)
}

// MintFor mints the identity token a federated credential asks for, carrying c with the audience
// and environment cfg names, for cfg's token lifetime. It is the half of Deliver that needs the
// signing key, which is why a control node runs it for a relay worker and seals the token to the
// worker's pool rather than handing the worker anything it could mint with.
func (i *Issuer) MintFor(ctx context.Context, cfg Config, c Claims) (string, error) {
	c.Audience, c.Environment = cfg.Audience, cfg.Environment
	token, _, err := i.Mint(ctx, c, cfg.TokenTTL)
	if err != nil {
		return "", err
	}
	return token, nil
}

// DeliverToken turns an identity token already minted under cfg for the claims c into what the tool
// reads, writing any file into dir. c names the run and the purpose the token was minted for, which
// is what an AWS session is named after. It is the half of Deliver that needs no signing key, so a
// relay worker runs it on a token the control node minted and sealed to its pool.
func DeliverToken(ctx context.Context, cfg Config, c Claims, token, dir string) (Delivery, error) {
	var err error
	d := Delivery{Secrets: []string{token}}
	switch cfg.Kind {
	case credential.KindAWSOIDC:
		err = deliverAWS(ctx, cfg, c, token, dir, &d)
	case credential.KindGCPOIDC:
		err = deliverGCP(ctx, cfg, token, dir, &d)
	case credential.KindAzureOIDC:
		err = deliverAzure(cfg, token, dir, &d)
	case credential.KindOIDCToken:
		var path string
		path, err = writePrivate(dir, "oidc-token", token, &d)
		if err == nil {
			d.Env = append(d.Env, TokenFileEnvVar+"="+path)
		}
	default:
		err = fmt.Errorf("%w: %s is not a federated kind", ErrSetting, cfg.Kind)
	}
	return d, err
}

// deliverAWS hands the run an AWS role. The file delivery sets the variables every AWS SDK reads to
// assume a role with a web identity token, so boto3 under Ansible, the Terraform and OpenTofu
// providers, and the AWS CLI all exchange the token themselves. The exchange delivery calls
// AssumeRoleWithWebIdentity here and injects the session credentials it returns.
func deliverAWS(ctx context.Context, cfg Config, c Claims, token, dir string, d *Delivery) error {
	session := cfg.SessionName
	if session == "" {
		session = defaultSessionName(c)
	}
	if cfg.Region != "" {
		d.Env = append(d.Env, "AWS_REGION="+cfg.Region, "AWS_DEFAULT_REGION="+cfg.Region)
	}
	if cfg.Delivery == DeliveryExchange {
		creds, err := exchangeAWS(ctx, cfg, token, session)
		if err != nil {
			return err
		}
		d.Env = append(d.Env,
			"AWS_ACCESS_KEY_ID="+creds.AccessKeyID,
			"AWS_SECRET_ACCESS_KEY="+creds.SecretAccessKey,
			"AWS_SESSION_TOKEN="+creds.SessionToken)
		d.Secrets = append(d.Secrets, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
		return nil
	}
	path, err := writePrivate(dir, "aws-web-identity-token", token, d)
	if err != nil {
		return err
	}
	d.Env = append(d.Env,
		"AWS_ROLE_ARN="+cfg.RoleARN,
		"AWS_WEB_IDENTITY_TOKEN_FILE="+path,
		"AWS_ROLE_SESSION_NAME="+session)
	if cfg.STSEndpoint != "" {
		// botocore, the AWS SDK for Go v2, and the Terraform and OpenTofu S3 backends read this
		// service-specific endpoint override, so their own exchange goes where the credential says
		// rather than to the public endpoint. A tool that builds its STS client another way may not.
		d.Env = append(d.Env, "AWS_ENDPOINT_URL_STS="+cfg.STSEndpoint)
	}
	return nil
}

// defaultSessionName names an AWS role session after the run, so CloudTrail shows which run used
// the role, and after why the token was minted when it was not for a run executing: the gate's
// module download names the run it was judging, and a review pre-check names its pull request,
// since no run exists. STS caps a session name at 64 characters.
func defaultSessionName(c Claims) string {
	name := "switchtender-" + c.RunID
	switch c.Purpose {
	case PurposeGateDownload:
		name = "switchtender-gate-" + c.RunID
	case PurposeReviewPrecheck:
		name = "switchtender-review-" + c.PullRequest
	}
	if len(name) > 64 {
		name = name[:64]
	}
	if len(name) < 2 || !sessionNamePattern.MatchString(name) {
		return "switchtender"
	}
	return name
}

// gcpExternalAccount is the external_account credentials file Google's client libraries, gcloud,
// and the Terraform google provider read to exchange a token from a file for Google credentials.
type gcpExternalAccount struct {
	// Type is external_account.
	Type string `json:"type"`
	// Audience is the workload identity pool provider's full resource name.
	Audience string `json:"audience"`
	// SubjectTokenType says the file holds a JWT.
	SubjectTokenType string `json:"subject_token_type"`
	// TokenURL is the security token service endpoint the library exchanges at.
	TokenURL string `json:"token_url"`
	// CredentialSource names the token file.
	CredentialSource gcpCredentialSource `json:"credential_source"`
	// ServiceAccountImpersonationURL, when set, has the library impersonate a service account with
	// the federated token.
	ServiceAccountImpersonationURL string `json:"service_account_impersonation_url,omitempty"`
}

// gcpCredentialSource points an external_account file at the token.
type gcpCredentialSource struct {
	// File is the path of the token file.
	File string `json:"file"`
	// Format says the file holds the token as plain text.
	Format gcpCredentialFormat `json:"format"`
}

// gcpCredentialFormat is the format of an external_account token file.
type gcpCredentialFormat struct {
	// Type is text.
	Type string `json:"type"`
}

// gcpJWTTokenType is the RFC 8693 token type of an OpenID Connect identity token.
const gcpJWTTokenType = "urn:ietf:params:oauth:token-type:jwt"

// impersonationURL is the IAM credentials endpoint that mints an access token for account.
func impersonationURL(cfg Config) string {
	return cfg.IAMEndpoint + "/v1/projects/-/serviceAccounts/" + cfg.ServiceAccount +
		":generateAccessToken"
}

// deliverGCP hands the run Google Cloud credentials. The file delivery writes an external_account
// file over the token file and binds it to GOOGLE_APPLICATION_CREDENTIALS, so the library in the
// tool runs the exchange. The exchange delivery runs it here and injects the access token as
// GOOGLE_OAUTH_ACCESS_TOKEN, and as GCP_ACCESS_TOKEN beside GCP_AUTH_KIND=accesstoken, the pair the
// Ansible google.cloud collection's modules, inventory plugin, and lookups read.
func deliverGCP(ctx context.Context, cfg Config, token, dir string, d *Delivery) error {
	if cfg.Delivery == DeliveryExchange {
		access, err := exchangeGCP(ctx, cfg, token)
		if err != nil {
			return err
		}
		d.Env = append(d.Env,
			"GOOGLE_OAUTH_ACCESS_TOKEN="+access,
			"GCP_ACCESS_TOKEN="+access,
			"GCP_AUTH_KIND=accesstoken")
		d.Secrets = append(d.Secrets, access)
		return nil
	}
	tokenPath, err := writePrivate(dir, "gcp-identity-token", token, d)
	if err != nil {
		return err
	}
	account := gcpExternalAccount{
		Type: "external_account", Audience: cfg.Provider, SubjectTokenType: gcpJWTTokenType,
		TokenURL: cfg.STSEndpoint,
		CredentialSource: gcpCredentialSource{
			File: tokenPath, Format: gcpCredentialFormat{Type: "text"},
		},
	}
	if cfg.ServiceAccount != "" {
		account.ServiceAccountImpersonationURL = impersonationURL(cfg)
	}
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return fmt.Errorf("encode external_account file: %w", err)
	}
	credPath, err := writePrivate(dir, "gcp-external-account.json", string(body), d)
	if err != nil {
		return err
	}
	d.Env = append(d.Env,
		"GOOGLE_APPLICATION_CREDENTIALS="+credPath,
		"CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE="+credPath,
		"GCP_AUTH_KIND=application")
	return nil
}

// deliverAzure hands the run a Microsoft Entra federated identity: the token in a file and the
// variables the Azure workload identity convention defines, which azure-identity in every language
// reads, beside the ARM_ variables the Terraform azurerm provider reads for OpenID Connect.
func deliverAzure(cfg Config, token, dir string, d *Delivery) error {
	path, err := writePrivate(dir, "azure-federated-token", token, d)
	if err != nil {
		return err
	}
	d.Env = append(d.Env,
		"AZURE_CLIENT_ID="+cfg.ClientID,
		"AZURE_TENANT_ID="+cfg.TenantID,
		"AZURE_FEDERATED_TOKEN_FILE="+path,
		"ARM_CLIENT_ID="+cfg.ClientID,
		"ARM_TENANT_ID="+cfg.TenantID,
		"ARM_USE_OIDC=true",
		"ARM_OIDC_TOKEN_FILE_PATH="+path)
	if cfg.SubscriptionID != "" {
		d.Env = append(d.Env, "AZURE_SUBSCRIPTION_ID="+cfg.SubscriptionID,
			"ARM_SUBSCRIPTION_ID="+cfg.SubscriptionID)
	}
	if cfg.AuthorityHost != "" {
		d.Env = append(d.Env, "AZURE_AUTHORITY_HOST="+cfg.AuthorityHost)
	}
	return nil
}

// writePrivate creates name inside dir with mode 0600, refusing to follow or replace anything
// already there, and records the path on d. It returns the path.
func writePrivate(dir, name, content string, d *Delivery) (string, error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	d.Files = append(d.Files, path)
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return path, nil
}
