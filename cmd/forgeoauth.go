package cmd

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/trigger"
)

// serveForgeOAuth holds the --forge-oauth values, one forge OAuth application each.
var serveForgeOAuth []string

// forgeOAuthKeys are the settings a --forge-oauth value may carry.
var forgeOAuthKeys = map[string]bool{
	"provider": true, "client_id": true, "secret_env": true, "secret_file": true,
	"web_url": true, "api_url": true,
}

// parseForgeOAuth reads each --forge-oauth value into the forge OAuth application people link their
// forge accounts through. A value is comma-separated key=value settings: provider, github or
// gitlab, client_id, the client secret from exactly one of secret_env, an environment variable's
// name, or secret_file, a file's path, and optionally web_url and api_url for GitHub Enterprise
// Server or self-managed GitLab. getenv and readFile read the secret, so it never appears on a
// command line. Two applications for the same forge are refused.
func parseForgeOAuth(values []string, getenv func(string) string,
	readFile func(string) ([]byte, error)) ([]forgelink.App, error) {
	apps := make([]forgelink.App, 0, len(values))
	seen := map[string]bool{}
	for _, raw := range values {
		app, err := parseForgeApp(raw, getenv, readFile)
		if err != nil {
			return nil, err
		}
		key := app.Provider + " " + app.APIURL
		if seen[key] {
			return nil, fmt.Errorf("%w: --forge-oauth names %s at %s twice", ErrUsage, app.Provider,
				app.APIURL)
		}
		seen[key] = true
		apps = append(apps, app)
	}
	return apps, nil
}

// parseForgeApp reads one --forge-oauth value.
func parseForgeApp(raw string, getenv func(string) string,
	readFile func(string) ([]byte, error)) (forgelink.App, error) {
	settings := map[string]string{}
	for part := range strings.SplitSeq(raw, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k = strings.TrimSpace(k)
		if !ok || !forgeOAuthKeys[k] {
			return forgelink.App{}, fmt.Errorf("%w: --forge-oauth takes provider, client_id, "+
				"secret_env or secret_file, web_url, and api_url as key=value, not %q", ErrUsage, part)
		}
		settings[k] = strings.TrimSpace(v)
	}
	provider := settings["provider"]
	if provider != trigger.ProviderGitHub && provider != trigger.ProviderGitLab {
		return forgelink.App{}, fmt.Errorf("%w: --forge-oauth provider must be %q or %q, not %q",
			ErrUsage, trigger.ProviderGitHub, trigger.ProviderGitLab, provider)
	}
	if settings["client_id"] == "" {
		return forgelink.App{}, fmt.Errorf("%w: --forge-oauth for %s needs client_id", ErrUsage,
			provider)
	}
	secret, err := forgeOAuthSecret(provider, settings, getenv, readFile)
	if err != nil {
		return forgelink.App{}, err
	}
	web, api := forgeOAuthURLs(provider, settings["web_url"], settings["api_url"])
	for name, u := range map[string]string{"web_url": web, "api_url": api} {
		if err := checkForgeURL(u); err != nil {
			return forgelink.App{}, fmt.Errorf("%w: --forge-oauth %s %q: %v", ErrUsage, name, u, err)
		}
	}
	return forgelink.App{
		Provider: provider, WebURL: web, APIURL: forgelink.CanonicalAPIURL(provider, api),
		ClientID: settings["client_id"], ClientSecret: secret,
	}, nil
}

// forgeOAuthSecret reads the client secret a --forge-oauth value names, from exactly one of an
// environment variable or a file. The secret itself is never accepted on the command line, where
// every process listing would show it.
func forgeOAuthSecret(provider string, settings map[string]string, getenv func(string) string,
	readFile func(string) ([]byte, error)) (string, error) {
	env, file := settings["secret_env"], settings["secret_file"]
	if (env == "") == (file == "") {
		return "", fmt.Errorf("%w: --forge-oauth for %s needs exactly one of secret_env or "+
			"secret_file", ErrUsage, provider)
	}
	var secret string
	if env != "" {
		secret = getenv(env)
	} else {
		b, err := readFile(file)
		if err != nil {
			return "", fmt.Errorf("%w: --forge-oauth for %s: read secret_file: %v", ErrUsage,
				provider, err)
		}
		secret = string(b)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("%w: --forge-oauth for %s: the client secret is empty", ErrUsage,
			provider)
	}
	return secret, nil
}

// forgeOAuthURLs returns the web address and API base of a forge, filling in what was left out:
// the public service's for an empty web address, and for a named one the API base GitHub
// Enterprise Server or self-managed GitLab serves under it.
func forgeOAuthURLs(provider, web, api string) (string, string) {
	web = strings.TrimRight(web, "/")
	if web == "" {
		web = "https://github.com"
		if provider == trigger.ProviderGitLab {
			web = "https://gitlab.com"
		}
	}
	if api != "" {
		return web, strings.TrimRight(api, "/")
	}
	switch {
	case provider == trigger.ProviderGitHub && strings.EqualFold(web, "https://github.com"):
		return web, trigger.DefaultAPIURL(provider)
	case provider == trigger.ProviderGitHub:
		return web, web + "/api/v3"
	default:
		return web, web + "/api/v4"
	}
}

// checkForgeURL reports why u cannot name a forge: it must be an https address with a host and
// nothing after the path.
func checkForgeURL(u string) error {
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("must be an https address with a host and no query")
	}
	return nil
}
