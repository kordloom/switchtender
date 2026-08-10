// Package credential stores execution secrets, SSH private keys and vault passwords, encrypted at
// rest with AES-GCM. Secret material decrypts only at execution time, never leaves the process in
// API responses, and is wiped from temp files when the run finishes.
package credential

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/secretsource"
)

// Kind classifies what a credential holds and how the runner materializes it.
type Kind string

const (
	// KindSSHKey is an SSH private key handed to ansible-playbook as --private-key.
	KindSSHKey Kind = "ssh_key"
	// KindVaultPassword is an Ansible Vault password handed over as --vault-password-file.
	KindVaultPassword Kind = "vault_password"
	// KindEnv is newline separated KEY=VALUE pairs injected into the execution environment,
	// which is how cloud SDK credentials reach inventory plugins and playbook modules.
	KindEnv Kind = "env"
	// KindBecomePassword is a privilege escalation password handed to a run as the
	// ansible_become_password variable through a file, so it never appears on the command line.
	KindBecomePassword Kind = "become_password"
	// KindRegistry is a container registry username and password, the username on the first line
	// and the password on the rest, used to pull execution environment images.
	KindRegistry Kind = "registry"
	// KindToken is a single API token or JWT, exposed to a run as the TokenEnvVar environment
	// variable so any tool can send it as a bearer token without a KEY=VALUE wrapper.
	KindToken Kind = "token"
	// KindOpenStack is an OpenStack application credential or password auth. Its fields
	// (auth_url, username, password, project_name, optional user_domain_name, project_domain_name,
	// region_name) become the OS_ environment variables openstacksdk and the openstack.cloud
	// collection read.
	KindOpenStack Kind = "openstack"
	// KindAWS is an Amazon Web Services access key. Its fields (access_key, secret_key, optional
	// session_token and region) inject as the standard AWS_ environment variables that Ansible, the
	// aws_ec2 inventory plugin, and Terraform all read.
	KindAWS Kind = "aws"
	// KindAzure is an Azure service principal. Its fields (client_id, secret, subscription_id,
	// tenant_id) inject as both the ARM_ variables Terraform reads and the AZURE_ variables the
	// Ansible azure collection reads.
	KindAzure Kind = "azure"
	// KindGCP is a Google Cloud service account JSON, written to a private file and bound to the
	// GOOGLE_APPLICATION_CREDENTIALS and GCP_SERVICE_ACCOUNT_FILE environment variables.
	KindGCP Kind = "gcp"
	// KindVMware is a VMware vCenter login. Its fields (host, user, password, optional
	// validate_certs) inject as the VMWARE_ environment variables the community.vmware modules read.
	KindVMware Kind = "vmware"
	// KindSSHPassword is machine password authentication. Its fields (user, password) inject as the
	// ansible_user and ansible_password connection variables through a file, so the password stays
	// off the command line.
	KindSSHPassword Kind = "ssh_password"
	// KindBecome is privilege escalation richer than KindBecomePassword. Its fields (optional method,
	// optional user, required password) inject the non-empty subset of the ansible_become_method,
	// ansible_become_user, and ansible_become_password variables through a file.
	KindBecome Kind = "become"
	// KindNetwork is a network device login. Its fields (user, password, optional network_os,
	// optional connection defaulting to network_cli) inject the ansible_user, ansible_password,
	// ansible_network_os, and ansible_connection variables through a file.
	KindNetwork Kind = "network"
)

// TokenEnvVar is the environment variable a token credential is exposed under at run time.
const TokenEnvVar = "SWITCHTENDER_TOKEN"

var (
	// ErrNotFound is returned when a credential does not exist in the store.
	ErrNotFound = errors.New("credential not found")
	// ErrBadKind is returned when a credential kind is not recognized.
	ErrBadKind = errors.New("unknown credential kind")
	// ErrBadField is returned when a typed credential is missing a required field.
	ErrBadField = errors.New("credential missing required field")
	// ErrBadSetting is returned when a credential setting has an invalid key or value.
	ErrBadSetting = errors.New("invalid credential setting")
	// ErrNoKey is returned when encryption is attempted without an encryption key configured.
	ErrNoKey = errors.New("no encryption key: set SWITCHTENDER_ENCRYPTION_KEY")
)

// builtinKinds are the credential kinds SwitchTender ships with, in a stable display order. ValidKind
// also accepts any kind with a registered injector, so a host or plugin can add its own type without
// editing this list.
var builtinKinds = []Kind{
	KindSSHKey, KindSSHPassword, KindVaultPassword, KindBecomePassword, KindBecome, KindNetwork,
	KindEnv, KindToken, KindRegistry, KindAWS, KindAzure, KindGCP, KindVMware, KindOpenStack,
}

// ValidKind reports whether k names a supported credential kind: a built-in, or a typed or custom
// kind with a registered injector, so a host that registers its own credential type does not need to
// touch this function.
func ValidKind(k Kind) bool {
	return slices.Contains(builtinKinds, k) || Injectable(k)
}

// Kinds returns the built-in credential kinds in display order. Custom registered kinds are also
// valid via ValidKind but are not enumerated here.
func Kinds() []Kind {
	return slices.Clone(builtinKinds)
}

// KindList joins the built-in kinds into a comma-separated string for a user-facing error hint.
func KindList() string {
	out := make([]string, len(builtinKinds))
	for i, k := range builtinKinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// SourceList joins the supported credential sources into a comma-separated string for a user-facing
// error hint.
func SourceList() string {
	return strings.Join(secretsource.Kinds(), ", ")
}

// Settings bounds keep the non-secret settings map small and line-safe: it cannot become a covert
// store for large blobs, and a value cannot smuggle an extra line into an env or extra-vars file.
const (
	// maxSettings is the most settings entries one credential may carry.
	maxSettings = 32
	// maxSettingKeyLen is the longest allowed settings key, in bytes.
	maxSettingKeyLen = 64
	// maxSettingValueLen is the longest allowed settings value, in bytes.
	maxSettingValueLen = 512
)

// settingKeyPattern is the shape of a settings key: a letter followed by letters, digits, or
// underscores. That covers field names such as become_user and environment names such as AWS_REGION.
var settingKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// EncodeSettings renders a settings map as JSON for a store's text column, empty string for an
// empty map so an untouched credential stores the column default.
func EncodeSettings(settings map[string]string) (string, error) {
	if len(settings) == 0 {
		return "", nil
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encode settings: %w", err)
	}
	return string(b), nil
}

// DecodeSettings parses a stored settings column, treating empty as none.
func DecodeSettings(text string) (map[string]string, error) {
	if text == "" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ValidateSettings checks a settings map's shape: entry count, key pattern and length, and values
// that are non-empty, bounded, and free of control characters. A value carrying a newline could
// append an extra line to an env or extra-vars injection, the same trick a multi-line token pulls,
// so it is refused here rather than trusted downstream.
func ValidateSettings(settings map[string]string) error {
	if len(settings) > maxSettings {
		return fmt.Errorf("%w: at most %d entries, got %d", ErrBadSetting, maxSettings, len(settings))
	}
	for k, v := range settings {
		if len(k) > maxSettingKeyLen || !settingKeyPattern.MatchString(k) {
			return fmt.Errorf("%w: key %q must be a letter followed by letters, digits, or "+
				"underscores, at most %d bytes", ErrBadSetting, k, maxSettingKeyLen)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: key %q has an empty value", ErrBadSetting, k)
		}
		if len(v) > maxSettingValueLen {
			return fmt.Errorf("%w: key %q value exceeds %d bytes", ErrBadSetting, k, maxSettingValueLen)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%w: key %q value contains a control character", ErrBadSetting, k)
			}
		}
	}
	return nil
}

// ansibleOnlyKinds are credential kinds materialized through Ansible-specific flags or extra-vars
// files: a private key, a vault password file, and connection or become variables. They have no
// effect under any other tool, so attaching one to a bash, terraform, python, or similar run is a
// mistake worth rejecting at submit rather than silently ignoring at execution.
var ansibleOnlyKinds = []Kind{
	KindSSHKey, KindVaultPassword, KindBecomePassword, KindSSHPassword, KindBecome, KindNetwork,
}

// AnsibleOnly reports whether kind only takes effect under the Ansible tool.
func AnsibleOnly(k Kind) bool {
	return slices.Contains(ansibleOnlyKinds, k)
}

const (
	// SourceLocal means the sealed Secret is the value itself. It is the default.
	SourceLocal = secretsource.KindLocal
	// SourceCommand means the sealed Secret is a command whose stdout is the secret.
	SourceCommand = secretsource.KindCommand
	// SourceVault means the sealed Secret is a Vault config read over HTTP at run time.
	SourceVault = secretsource.KindVault
	// SourceGSM means the sealed Secret is a Google Secret Manager config read over HTTP at run time.
	SourceGSM = secretsource.KindGSM
	// SourceAWS means the sealed Secret is an AWS Secrets Manager config read with a SigV4 signed
	// request at run time.
	SourceAWS = secretsource.KindAWS
	// SourceVaultDynamic means the sealed Secret is a Vault dynamic secrets config. A short-lived
	// credential is minted for each run and revoked when the run ends.
	SourceVaultDynamic = secretsource.KindVaultDynamic
)

// NormalizeSource maps an empty source to the local default and otherwise returns source unchanged.
func NormalizeSource(source string) string { return secretsource.NormalizeKind(source) }

// vaultIDPattern bounds a vault label to what ansible-playbook parses unambiguously in
// label@file: no separator, no spaces, nothing a shell or the argument parser reinterprets.
var vaultIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidVaultID reports whether v can label a vault password. Empty is valid and means the classic
// unlabeled password file.
func ValidVaultID(v string) bool {
	return v == "" || vaultIDPattern.MatchString(v)
}

// ValidSource reports whether s names a supported credential source. Empty is valid and means local.
func ValidSource(s string) bool { return secretsource.ValidKind(s) }

// RegistryLogin splits registry credential material into its username and password. The first line
// is the username; everything after it is the password, so a password may contain any character.
func RegistryLogin(secret string) (username, password string) {
	username, password, found := strings.Cut(secret, "\n")
	if !found {
		return strings.TrimSpace(secret), ""
	}
	return strings.TrimSpace(username), password
}

// EnvLines splits env credential material into KEY=VALUE entries, dropping blanks and comments.
func EnvLines(secret string) []string {
	var out []string
	for line := range strings.SplitSeq(secret, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Credential is one stored secret. Secret carries ciphertext inside the store and plaintext only
// in the hands of the executor; it never serializes to JSON.
type Credential struct {
	// ID is the unique credential identifier.
	ID string `json:"id"`
	// Name labels the credential for humans, for example prod-fleet-key.
	Name string `json:"name"`
	// Kind classifies the secret.
	Kind Kind `json:"kind"`
	// TypeID names a custom credential type this credential is an instance of, empty for a built-in
	// kind. When set, the sealed Secret holds a JSON object of the type's field values rather than a
	// single value, and injection is driven by the type rather than by Kind.
	TypeID string `json:"type_id,omitempty"`
	// Source is where the secret value comes from: empty or local means Secret holds the sealed value
	// itself; command means Secret holds a command whose stdout is the secret, fetched at run time
	// from an external store such as Vault or a cloud CLI.
	Source string `json:"source,omitempty"`
	// Secret is the encrypted material at rest and never appears in API responses. For a command
	// source it is the sealed command, not the secret.
	Secret string `json:"-"`
	// VaultID labels an Ansible Vault password for --vault-id, so several vault credentials on
	// one run each unlock the secrets encrypted for their label. Empty passes the classic
	// --vault-password-file. Only meaningful on KindVaultPassword.
	VaultID string `json:"vault_id,omitempty"`
	// Settings carries the credential's non-secret fields, such as the connection user or a become
	// method. Unlike the sealed Secret they return from the API and show in the interface, so an
	// import can land them and an operator can see and edit them. At injection the connection kinds
	// merge them beneath the sealed fields, sealed values winning on conflict, and their values are
	// never added to the run's mask list. Other kinds carry them as reference metadata.
	Settings map[string]string `json:"settings,omitempty"`
	// OrgID is the owning organization. Empty means unowned, a global object that follows the role.
	// When set, members of the organization gain access to the credential and, under strict grants, it
	// is hidden from non-members who lack an explicit grant.
	OrgID string `json:"org_id,omitempty"`
	// CreatedAt is when the credential was created.
	CreatedAt time.Time `json:"created_at"`
}

// Store persists credentials with their secrets encrypted by the caller. Implementations must be
// safe for concurrent use.
type Store interface {
	// Save inserts or replaces the credential identified by c.ID.
	Save(ctx context.Context, c *Credential) error
	// Update changes an existing credential's name, kind, and sealed secret, preserving its creation
	// time, or returns ErrNotFound.
	Update(ctx context.Context, c *Credential) error
	// Get returns the credential with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Credential, error)
	// List returns all credentials ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Credential, error)
	// Delete removes the credential with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// NewID returns a random credential identifier prefixed with "cred_".
func NewID() string {
	return idgen.New("cred_", 6)
}
