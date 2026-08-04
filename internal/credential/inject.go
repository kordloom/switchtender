package credential

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Injection is the runtime material a typed credential contributes to a run: environment variables
// applied verbatim, and files whose written path is bound to environment variables so a tool that
// expects a credentials file, such as a Google service account JSON, finds it.
type Injection struct {
	// Env holds KEY=VALUE lines applied to the execution environment.
	Env []string
	// Files holds file material written to a private temp file at run time, each bound to the
	// environment variables named in the file.
	Files []InjectionFile
	// ExtraVars holds Ansible extra variables a custom credential type injects. The built-in cloud
	// injectors leave it empty.
	ExtraVars map[string]string
	// Secrets names the values to mask out of run output. It is set by a custom type, which knows
	// which of its fields are secret; a built-in injector leaves it nil, and the caller then masks
	// every environment value it produced, which is the prior behavior.
	Secrets []string
}

// InjectionFile is credential material written to a private file at run time. EnvVars names the
// environment variables set to the written file's path, so any tool that reads the credential from a
// path locates it.
type InjectionFile struct {
	// EnvVars are the environment variables set to the written file's path.
	EnvVars []string
	// Content is the file's contents.
	Content string
}

// Injector turns a credential's resolved plaintext secret into runtime material. It must not perform
// I/O: the caller writes any files and applies the environment, so an injector stays pure and testable.
type Injector func(secret string) (Injection, error)

// injectors holds the registered typed-credential injectors keyed by kind. It is written only from
// init and host registration, before any run executes and before any concurrent read, so it needs no
// lock.
var injectors = map[Kind]Injector{}

// init registers the built-in cloud credential injectors so their kinds resolve at run time.
func init() {
	RegisterInjector(KindAWS, awsInject)
	RegisterInjector(KindAzure, azureInject)
	RegisterInjector(KindGCP, gcpInject)
	RegisterInjector(KindVMware, vmwareInject)
	RegisterInjector(KindOpenStack, openstackInject)
}

// RegisterInjector registers an injector for a typed credential kind, letting a host add a credential
// type beyond the built-ins without forking the server. It must be called during startup, before any
// run executes. It panics on an empty kind, a nil injector, or a duplicate, each a developer error.
func RegisterInjector(kind Kind, inj Injector) {
	if kind == "" {
		panic("credential: RegisterInjector: empty kind")
	}
	if inj == nil {
		panic("credential: RegisterInjector: nil injector for " + string(kind))
	}
	if _, dup := injectors[kind]; dup {
		panic("credential: RegisterInjector: duplicate kind " + string(kind))
	}
	injectors[kind] = inj
}

// Injectable reports whether kind has a registered injector.
func Injectable(kind Kind) bool {
	_, ok := injectors[kind]
	return ok
}

// Inject applies the injector registered for kind to secret, or returns ErrBadKind when the kind has
// no injector.
func Inject(kind Kind, secret string) (Injection, error) {
	inj, ok := injectors[kind]
	if !ok {
		return Injection{}, fmt.Errorf("%w: %s", ErrBadKind, kind)
	}
	return inj(secret)
}

// Fields parses KEY=VALUE credential material into a map with lowercased keys, so a typed injector
// or the runner reads named fields regardless of the case the operator entered.
func Fields(secret string) map[string]string {
	m := make(map[string]string)
	for _, line := range EnvLines(secret) {
		k, v, _ := strings.Cut(line, "=")
		m[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return m
}

// awsInject maps AWS access-key fields to the standard AWS SDK environment variables.
func awsInject(secret string) (Injection, error) {
	f := Fields(secret)
	access, secretKey := f["access_key"], f["secret_key"]
	if access == "" || secretKey == "" {
		return Injection{}, fmt.Errorf("%w: aws needs access_key and secret_key", ErrBadField)
	}
	env := []string{
		"AWS_ACCESS_KEY_ID=" + access,
		"AWS_SECRET_ACCESS_KEY=" + secretKey,
	}
	if token := f["session_token"]; token != "" {
		env = append(env, "AWS_SESSION_TOKEN="+token)
	}
	if region := f["region"]; region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+region, "AWS_REGION="+region)
	}
	return Injection{Env: env}, nil
}

// azureInject maps Azure service-principal fields to both the ARM_ variables Terraform's azurerm
// provider reads and the AZURE_ variables the Ansible azure.azcollection modules read.
func azureInject(secret string) (Injection, error) {
	f := Fields(secret)
	client, pass := f["client_id"], f["secret"]
	sub, tenant := f["subscription_id"], f["tenant_id"]
	if client == "" || pass == "" || sub == "" || tenant == "" {
		return Injection{}, fmt.Errorf(
			"%w: azure needs client_id, secret, subscription_id, tenant_id", ErrBadField)
	}
	return Injection{Env: []string{
		"AZURE_CLIENT_ID=" + client,
		"AZURE_SECRET=" + pass,
		"AZURE_SUBSCRIPTION_ID=" + sub,
		"AZURE_TENANT=" + tenant,
		"ARM_CLIENT_ID=" + client,
		"ARM_CLIENT_SECRET=" + pass,
		"ARM_SUBSCRIPTION_ID=" + sub,
		"ARM_TENANT_ID=" + tenant,
	}}, nil
}

// gcpInject writes a Google service-account JSON to a file and binds it to the environment variables
// gcloud, Terraform's google provider, and the Ansible google.cloud modules read.
func gcpInject(secret string) (Injection, error) {
	secret = strings.TrimSpace(secret)
	if !json.Valid([]byte(secret)) {
		return Injection{}, fmt.Errorf("%w: gcp needs a service account JSON", ErrBadField)
	}
	return Injection{Files: []InjectionFile{{
		EnvVars: []string{"GOOGLE_APPLICATION_CREDENTIALS", "GCP_SERVICE_ACCOUNT_FILE"},
		Content: secret,
	}}}, nil
}

// openstackInject maps OpenStack auth fields to the OS_ environment variables openstacksdk and
// the openstack.cloud collection read. The domain names default to Default, which is what a stock
// Keystone ships with, so the common case needs four fields.
func openstackInject(secret string) (Injection, error) {
	f := Fields(secret)
	authURL, username, password := f["auth_url"], f["username"], f["password"]
	project := f["project_name"]
	if authURL == "" || username == "" || password == "" || project == "" {
		return Injection{}, fmt.Errorf(
			"%w: openstack needs auth_url, username, password, and project_name", ErrBadField)
	}
	userDomain, projectDomain := f["user_domain_name"], f["project_domain_name"]
	if userDomain == "" {
		userDomain = "Default"
	}
	if projectDomain == "" {
		projectDomain = "Default"
	}
	env := []string{
		"OS_AUTH_URL=" + authURL,
		"OS_USERNAME=" + username,
		"OS_PASSWORD=" + password,
		"OS_PROJECT_NAME=" + project,
		"OS_USER_DOMAIN_NAME=" + userDomain,
		"OS_PROJECT_DOMAIN_NAME=" + projectDomain,
		"OS_IDENTITY_API_VERSION=3",
	}
	if region := f["region_name"]; region != "" {
		env = append(env, "OS_REGION_NAME="+region)
	}
	return Injection{Env: env}, nil
}

// vmwareInject maps vCenter fields to the VMWARE_ environment variables the community.vmware modules
// read.
func vmwareInject(secret string) (Injection, error) {
	f := Fields(secret)
	host, user, pass := f["host"], f["user"], f["password"]
	if host == "" || user == "" || pass == "" {
		return Injection{}, fmt.Errorf("%w: vmware needs host, user, password", ErrBadField)
	}
	env := []string{
		"VMWARE_HOST=" + host,
		"VMWARE_USER=" + user,
		"VMWARE_PASSWORD=" + pass,
	}
	if certs := f["validate_certs"]; certs != "" {
		env = append(env, "VMWARE_VALIDATE_CERTS="+certs)
	}
	return Injection{Env: env}, nil
}
