package imageref

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds one resolution, so a registry that does not answer delays a submission by
// seconds and then leaves the run bound to its tag.
const DefaultTimeout = 10 * time.Second

// DefaultDownFor is how long a registry that did not answer is left alone before it is asked again,
// so a dead registry costs one timeout a minute rather than one per submission.
const DefaultDownFor = time.Minute

// maxManifest bounds the manifest read when a registry answers without a digest header, so a hostile
// registry cannot make a submission buffer an unbounded body.
const maxManifest = 4 << 20

// manifestAccept lists every manifest form the resolver accepts, index forms first, so the digest is
// the one a runtime resolves the tag to on any platform.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// ErrResolve is returned when a registry did not resolve a tag to a digest.
var ErrResolve = errors.New("image tag not resolved to a digest")

// Credentials is a registry login, empty for an anonymous pull.
type Credentials struct {
	// Username is the registry user.
	Username string
	// Password is the registry password or token. It is never logged or returned in an error.
	Password string `json:"-"`
}

// Resolver asks a registry which digest a tag names, over the distribution API every registry
// serves. It remembers a registry that did not answer and leaves it alone for a while, so a dead
// registry does not hold every submission for the full timeout.
type Resolver struct {
	// client sends the requests. It must refuse the addresses a server-side request forgery aims at.
	client *http.Client
	// timeout bounds one resolution.
	timeout time.Duration
	// downFor is how long a registry that did not answer is not asked again.
	downFor time.Duration
	// now reads the clock.
	now func() time.Time
	// mu guards down.
	mu sync.Mutex
	// down holds, for each registry host that did not answer, when it may be asked again.
	down map[string]time.Time
}

// NewResolver returns a resolver that sends its requests through client and bounds each resolution
// by timeout, DefaultTimeout when timeout is zero or less. A registry that does not answer is not
// asked again for DefaultDownFor. It panics on a nil client, a wiring error.
func NewResolver(client *http.Client, timeout time.Duration) *Resolver {
	if client == nil {
		panic("imageref: http client required")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Resolver{client: client, timeout: timeout, downFor: DefaultDownFor, now: time.Now,
		down: map[string]time.Time{}}
}

// Digest returns the digest the registry serves for ref's tag. A reference that already pins a
// digest returns it without asking. Every failure, an unreachable registry, a refused login, an
// unknown tag, wraps ErrResolve, and none quotes the password.
//
// The lookup is bounded by ctx as well as by the resolver's timeout, so it ends when the request
// that submitted the run ends, and then wraps ctx's error. A registry that did not answer within
// the timeout is not asked again until DefaultDownFor has passed, and a lookup for it fails at once
// meanwhile. A lookup cut short by ctx says nothing about the registry and does not count against
// it.
func (r *Resolver) Digest(ctx context.Context, ref string, creds Credentials) (string, error) {
	parsed, err := Parse(ref)
	if err != nil {
		return "", err
	}
	if parsed.Digest != "" {
		return parsed.Digest, nil
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: the request ended before the registry was asked: %w", ErrResolve, err)
	}
	host := parsed.registryHost()
	if until, down := r.downUntil(host); down {
		return "", fmt.Errorf("%w: %s did not answer a lookup in the last %s, so it is not asked again "+
			"until %s", ErrResolve, host, r.downFor, until.UTC().Format(time.RFC3339))
	}
	lookupCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	digest, err := r.lookup(lookupCtx, parsed, ref, creds)
	if cerr := ctx.Err(); cerr != nil && err != nil {
		return "", fmt.Errorf("%w: the request ended during the lookup: %w", ErrResolve, cerr)
	}
	var unanswered *noAnswerError
	if errors.As(err, &unanswered) {
		r.markDown(host)
	}
	return digest, err
}

// downUntil reports whether host is being left alone after not answering, and until when.
func (r *Resolver) downUntil(host string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.down[host]
	if !ok {
		return time.Time{}, false
	}
	if !r.now().Before(until) {
		delete(r.down, host)
		return time.Time{}, false
	}
	return until, true
}

// markDown leaves host alone for downFor after it did not answer.
func (r *Resolver) markDown(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down[host] = r.now().Add(r.downFor)
}

// noAnswerError is a request to a registry or its token service that got no answer at all, a
// refused connection or a timeout, as opposed to an answer refusing the lookup.
type noAnswerError struct {
	// what names who did not answer.
	what string
	// err is the transport's error.
	err error
}

// Error states who did not answer and why.
func (e *noAnswerError) Error() string {
	return "the " + e.what + " did not answer: " + e.err.Error()
}

// Unwrap returns the transport's error.
func (e *noAnswerError) Unwrap() error { return e.err }

// lookup asks the registry for ref's manifest digest within ctx.
func (r *Resolver) lookup(ctx context.Context, parsed Ref, ref string, creds Credentials) (string, error) {
	manifest := "https://" + parsed.registryHost() + "/v2/" + parsed.Path + "/manifests/" +
		url.PathEscape(parsed.Tag)
	status, header, err := r.head(ctx, manifest, "")
	if err != nil {
		return "", err
	}
	auth := ""
	if status == http.StatusUnauthorized {
		if auth, err = r.authorize(ctx, header.Get("WWW-Authenticate"), parsed, creds); err != nil {
			return "", err
		}
		if status, header, err = r.head(ctx, manifest, auth); err != nil {
			return "", err
		}
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%w: the registry answered %d for %s", ErrResolve, status, ref)
	}
	if d := header.Get("Docker-Content-Digest"); digestPattern.MatchString(d) {
		return d, nil
	}
	return r.digestByBody(ctx, manifest, auth)
}

// head sends a HEAD for a manifest with the accepted forms and an optional Authorization value, and
// returns the status and header the registry answered with.
func (r *Resolver) head(ctx context.Context, manifest, auth string) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, manifest, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	req.Header.Set("Accept", manifestAccept)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrResolve, &noAnswerError{what: "registry", err: err})
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header, nil
}

// digestByBody reads a manifest whose registry sent no digest header and digests its bytes, which is
// how the distribution API defines a manifest's digest.
func (r *Resolver) digestByBody(ctx context.Context, manifest, auth string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, err)
	}
	req.Header.Set("Accept", manifestAccept)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, &noAnswerError{what: "registry", err: err})
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: the registry answered %d for the manifest", ErrResolve, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return "", fmt.Errorf("%w: read the manifest: %w", ErrResolve, err)
	}
	if len(body) > maxManifest {
		return "", fmt.Errorf("%w: the manifest is larger than %d bytes", ErrResolve, maxManifest)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// authorize answers a registry's challenge: a bearer token fetched from the realm it names, for a
// pull of the repository, or the login itself for a basic challenge. It returns the Authorization
// value to send.
func (r *Resolver) authorize(ctx context.Context, challenge string, ref Ref, creds Credentials) (string,
	error) {
	scheme, params := parseChallenge(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		if creds.Username == "" {
			return "", fmt.Errorf("%w: the registry asks for a login and the run names none", ErrResolve)
		}
		return "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(creds.Username+":"+creds.Password)), nil
	case "bearer":
		return r.bearer(ctx, params, ref, creds)
	default:
		return "", fmt.Errorf("%w: the registry asks for an unsupported login scheme", ErrResolve)
	}
}

// bearer fetches a pull token from the realm a bearer challenge names.
func (r *Resolver) bearer(ctx context.Context, params map[string]string, ref Ref,
	creds Credentials) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", fmt.Errorf("%w: the registry's token realm is not an https address", ErrResolve)
	}
	q := realm.Query()
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Path + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if creds.Username != "" {
		req.SetBasicAuth(creds.Username, creds.Password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrResolve, &noAnswerError{what: "token service", err: err})
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: the token service answered %d", ErrResolve, resp.StatusCode)
	}
	var tok struct {
		// Token is the bearer token.
		Token string `json:"token"`
		// AccessToken is the same token under the name OAuth registries use.
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("%w: the token service's answer does not decode", ErrResolve)
	}
	token := tok.Token
	if token == "" {
		token = tok.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("%w: the token service returned no token", ErrResolve)
	}
	return "Bearer " + token, nil
}

// parseChallenge splits a WWW-Authenticate value into its scheme and its quoted parameters.
func parseChallenge(v string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(v), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				break
			}
			value, rest = after[1:end+1], after[end+2:]
		} else {
			value, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return scheme, params
}
