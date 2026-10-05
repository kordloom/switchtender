// Package trigger fires job templates from inbound git webhooks. A trigger holds a secret token
// embedded in a webhook URL; a push to that URL launches the trigger's template, which syncs its
// project fresh and so runs the commit that was just pushed.
package trigger

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// tokenPrefix marks trigger webhook tokens so leaked strings are recognizable.
const tokenPrefix = "whk_"

// secretPrefix marks trigger signing secrets so leaked strings are recognizable.
const secretPrefix = "whs_"

// signaturePrefix is the algorithm label GitHub and GitLab prepend to the hex HMAC digest in the
// X-Hub-Signature-256 header.
const signaturePrefix = "sha256="

// ErrNotFound is returned when a trigger does not exist in the store.
var ErrNotFound = errors.New("trigger not found")

// ErrBadReview is returned when a review trigger's configuration cannot work.
var ErrBadReview = errors.New("invalid pull request review configuration")

// Trigger launches a template when its webhook URL is hit.
type Trigger struct {
	// ID is the unique trigger identifier.
	ID string `json:"id"`
	// Name labels the trigger for humans.
	Name string `json:"name"`
	// TemplateID is the template this trigger launches.
	TemplateID string `json:"template_id"`
	// TokenHash is the hex encoded SHA-256 of the webhook token; the token itself never persists.
	TokenHash string `json:"-"`
	// SigningSecret is the AES-GCM sealed HMAC secret used to verify X-Hub-Signature-256. It is
	// separate from the URL token so a leaked webhook URL does not also leak the signing key, and
	// never serializes to JSON. Empty when no encryption key was configured at creation.
	SigningSecret string `json:"-"`
	// RequireSignature rejects an inbound webhook whose HMAC signature is missing or wrong.
	RequireSignature bool `json:"require_signature"`
	// Review, when set, makes this a pull request review trigger: a pull_request or merge_request
	// webhook plans the template at the proposed commit, never applies it, and posts the result back
	// to the pull request. Nil for a push trigger, which fires the template as it always has.
	Review *Review `json:"review,omitempty"`
	// LastFiredAt is when the trigger last launched a run.
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	// LastError says why a delivery started no run, such as a required survey question nobody was
	// present to answer, and is empty once a delivery starts one. A push trigger records it for every
	// delivery that starts no run, and a review trigger for a plan refused over its survey, since the
	// pull request already hears every other refusal. A refused delivery answers the sender too, but
	// the sender's delivery log is the forge's, and an operator looking at the trigger here would
	// otherwise see only a fire time that stopped moving.
	LastError string `json:"last_error,omitempty"`
	// LastErrorAt is when the delivery LastError describes arrived.
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// CreatedBy names the actor who created the trigger, empty for one that predates the field.
	//
	// A trigger's token is a bearer credential belonging to the trigger, not to a person, so removing
	// an account does not revoke it and deliberately does not stop the trigger. Recording who created
	// it is what lets an offboarding review find the ones worth rotating.
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedAt is when the trigger was created.
	CreatedAt time.Time `json:"created_at"`
}

// Review providers a review trigger can report to.
const (
	// ProviderGitHub reports to GitHub or GitHub Enterprise Server through the REST API.
	ProviderGitHub = "github"
	// ProviderGitLab reports to GitLab, hosted or self-managed, through the REST API.
	ProviderGitLab = "gitlab"
)

// Review configures how a review trigger reaches the forge that sent the pull request.
type Review struct {
	// Provider is github or gitlab.
	Provider string `json:"provider"`
	// APIURL is the forge's REST API base, empty for the public service: https://api.github.com
	// for GitHub and https://gitlab.com/api/v4 for GitLab. A self-hosted forge sets its own, such as
	// https://github.example.com/api/v3 or https://gitlab.example.com/api/v4.
	APIURL string `json:"api_url,omitempty"`
	// Repository names the repository the pull requests belong to: owner/name on GitHub, the full
	// group/project path on GitLab. A webhook for any other repository is refused.
	Repository string `json:"repository"`
	// CredentialID names the token credential the trigger posts comments and statuses with. The
	// token is sealed like every other credential and never returned.
	CredentialID string `json:"credential_id"`
	// AllowForks plans pull requests whose head lives in a fork. Off by default: a plan runs the
	// proposed code with the template's credentials, so anybody able to open a pull request from a
	// fork could otherwise read them.
	AllowForks bool `json:"allow_forks,omitempty"`
}

// DefaultAPIURL returns the provider's public REST API base.
func DefaultAPIURL(provider string) string {
	switch provider {
	case ProviderGitHub:
		return "https://api.github.com"
	case ProviderGitLab:
		return "https://gitlab.com/api/v4"
	default:
		return ""
	}
}

// BaseURL returns the REST API base the review talks to: its own, or the provider's public one.
func (r *Review) BaseURL() string {
	if r.APIURL != "" {
		return strings.TrimRight(r.APIURL, "/")
	}
	return DefaultAPIURL(r.Provider)
}

// Validate checks the review configuration names a known provider, a repository, a token
// credential, and an https API base, so a review that could never report is refused where it is
// written rather than discovered on the first pull request.
func (r *Review) Validate() error {
	switch r.Provider {
	case ProviderGitHub, ProviderGitLab:
	default:
		return fmt.Errorf("%w: provider must be %q or %q, not %q", ErrBadReview, ProviderGitHub,
			ProviderGitLab, r.Provider)
	}
	repo := strings.Trim(r.Repository, "/")
	if repo == "" || !strings.Contains(repo, "/") || strings.ContainsAny(repo, " \t\r\n?#") ||
		strings.Contains(repo, "..") {
		return fmt.Errorf("%w: repository must be owner/name or group/project, not %q", ErrBadReview,
			r.Repository)
	}
	if r.Provider == ProviderGitHub && strings.Count(repo, "/") != 1 {
		return fmt.Errorf("%w: a GitHub repository is owner/name, not %q", ErrBadReview, r.Repository)
	}
	if r.CredentialID == "" {
		return fmt.Errorf("%w: credential_id must name a token credential", ErrBadReview)
	}
	if r.APIURL != "" {
		u, err := url.Parse(r.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("%w: api_url must be an https URL with no credentials in it", ErrBadReview)
		}
	}
	return nil
}

// VerifyToken reports whether header equals secret, the check GitLab's X-Gitlab-Token needs. GitLab
// sends the configured secret itself rather than a signature over the body, so the compare is the
// whole verification. It is constant time, and an empty secret or header never matches.
func VerifyToken(secret, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	return hmac.Equal([]byte(secret), []byte(header))
}

// Store persists triggers. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the trigger identified by t.ID.
	Save(ctx context.Context, t *Trigger) error
	// Get returns the trigger with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Trigger, error)
	// List returns all triggers ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Trigger, error)
	// Delete removes the trigger with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// FindByTokenHash returns the trigger with the given token hash, or ErrNotFound.
	FindByTokenHash(ctx context.Context, hash string) (*Trigger, error)
	// TouchFired stamps when the trigger last fired, updating that column alone and clearing the
	// last error, since the newest delivery started a run. A deleted trigger is a no-op, never a
	// resurrection: the fire path used to write its whole stale snapshot back through Save, so a
	// webhook in flight while an admin revoked the trigger re-inserted it, token and all, and a fire
	// racing a secret rotation reverted the rotation.
	TouchFired(ctx context.Context, id string, at time.Time) error
	// RecordRefusal stamps why a delivery started no run and when it arrived, updating those two
	// columns alone. A deleted trigger is a no-op, for the reason TouchFired gives.
	RecordRefusal(ctx context.Context, id string, at time.Time, reason string) error
}

// New mints a trigger for a template: the plaintext token to embed in the webhook URL exactly
// once, and the stored record.
func New(name, templateID string) (string, *Trigger, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	plain := tokenPrefix + hex.EncodeToString(b[:])
	var id [6]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", nil, err
	}
	return plain, &Trigger{
		ID:         "trg_" + hex.EncodeToString(id[:]),
		Name:       name,
		TemplateID: templateID,
		TokenHash:  HashToken(plain),
		CreatedAt:  time.Now(),
	}, nil
}

// HashToken returns the hex encoded SHA-256 of a webhook token.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// NewSigningSecret returns a fresh webhook signing secret. The plaintext is shown once, sealed at
// rest with credential.Sealer, and set as the secret on the git host so its HMAC signatures verify.
// Rotation mints a new one.
func NewSigningSecret() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return secretPrefix + hex.EncodeToString(b[:]), nil
}

// SignBody returns the X-Hub-Signature-256 value for body under secret: the string sha256=<hex> the
// git host sends and the hook recomputes to authenticate the payload.
func SignBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature reports whether header is a valid X-Hub-Signature-256 for body under secret. The
// compare is constant time; an empty secret or header, or a digest of the wrong length, never
// matches.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	return hmac.Equal([]byte(SignBody(secret, body)), []byte(header))
}
