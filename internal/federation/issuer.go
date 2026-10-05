// Package federation makes SwitchTender an OpenID Connect issuer for the runs it executes, so a run
// reaches a cloud with short-lived credentials and nothing durable is stored.
//
// The install publishes a discovery document and a public key set at its external URL. Each run
// that carries a federated credential receives an identity token signed with a key of the issuer's
// own, with claims that name the run precisely: its organization, project, template, environment,
// who launched it, and whether an approver released it. A cloud's trust policy verifies the token
// against the published keys and keys on those claims, and exchanges it for credentials that expire
// on their own. The server makes no outbound call to make any of this work: the cloud fetches the
// keys, and in the default delivery the tool performs the exchange.
package federation

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/credential"
)

// Token lifetimes.
const (
	// DefaultTokenTTL is how long a run identity token is valid when the credential sets no
	// token_ttl. It covers a tool that reaches the cloud partway into a run, and it is still short
	// enough that a token copied out of a run is worthless within the hour.
	DefaultTokenTTL = 15 * time.Minute
	// MinTokenTTL is the shortest token_ttl a credential may set.
	MinTokenTTL = time.Minute
	// MaxTokenTTL is the longest token_ttl a credential may set.
	MaxTokenTTL = time.Hour
)

// Paths the issuer is served under, relative to the issuer URL.
const (
	// DiscoveryPath is where a relying party reads the OpenID Connect discovery document.
	DiscoveryPath = "/.well-known/openid-configuration"
	// JWKSPath is where a relying party reads the public key set.
	JWKSPath = "/.well-known/jwks.json"
)

// Option configures an Issuer.
type Option func(*Issuer)

// WithClock sets the issuer's own clock, for tests. The issuer judges keys, and stamps tokens and
// rotations, by its key store's clock, which for a database is the database server's, so every
// process sharing the keys agrees on when each key starts signing, stops, and leaves the published
// set. Its own clock stands in only for a key store with no clock of its own.
func WithClock(now func() time.Time) Option {
	return func(i *Issuer) {
		if now != nil {
			i.now = now
		}
	}
}

// Issuer mints run identity tokens and publishes the keys that verify them.
type Issuer struct {
	// url is the issuer URL with no trailing slash, the iss claim of every token.
	url string
	// keys persists the signing keys, shared by every process on the same database.
	keys KeyStore
	// sealer seals and opens the private half of each key.
	sealer *credential.Sealer
	// now is the issuer's own clock, which judges keys and stamps tokens only when the key store
	// keeps no clock of its own.
	now func() time.Time
	// recorder records each token as it is minted, nil when nothing records issuances.
	recorder IssuanceRecorder
}

// NewIssuer returns an issuer at rawURL that keeps its signing keys in keys, sealed with sealer.
// The URL must be https, or http on a loopback host for a local trial, with no query, fragment, or
// credentials, because a cloud fetches the discovery document from it and compares the token's iss
// claim to it byte for byte. Sealing needs an enabled sealer.
func NewIssuer(rawURL string, keys KeyStore, sealer *credential.Sealer, opts ...Option) (*Issuer, error) {
	u, err := IssuerURL(rawURL)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		return nil, fmt.Errorf("%w: no key store", ErrIssuer)
	}
	if sealer == nil || !sealer.Enabled() {
		return nil, fmt.Errorf("%w: the signing key is sealed under the encryption key: %w", ErrIssuer,
			credential.ErrNoKey)
	}
	i := &Issuer{url: u, keys: keys, sealer: sealer, now: time.Now}
	for _, opt := range opts {
		opt(i)
	}
	return i, nil
}

// IssuerURL validates and normalizes an issuer URL, returning it without a trailing slash.
func IssuerURL(rawURL string) (string, error) {
	raw := strings.TrimSpace(rawURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%w: %q is not an absolute URL", ErrIssuer, raw)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return "", fmt.Errorf("%w: %q must use https, since a cloud refuses to trust keys fetched "+
			"over plain http", ErrIssuer, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("%w: %q must not carry credentials, a query, or a fragment",
			ErrIssuer, raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// isLoopback reports whether host names the local machine.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// URL returns the issuer URL, the iss claim of every token.
func (i *Issuer) URL() string { return i.url }

// Discovery is the OpenID Connect discovery document a relying party reads before fetching keys. It
// carries what the token verification needs and nothing an interactive sign-in would, the same
// shape the CI providers that federate into clouds publish.
type Discovery struct {
	// Issuer is the issuer URL, which every token's iss claim matches exactly.
	Issuer string `json:"issuer"`
	// JWKSURI is where the public key set is served.
	JWKSURI string `json:"jwks_uri"`
	// ResponseTypesSupported lists id_token, the only thing this issuer produces.
	ResponseTypesSupported []string `json:"response_types_supported"`
	// SubjectTypesSupported lists public.
	SubjectTypesSupported []string `json:"subject_types_supported"`
	// IDTokenSigningAlgValuesSupported lists RS256.
	IDTokenSigningAlgValuesSupported []string `json:"id_token_signing_alg_values_supported"`
	// ScopesSupported lists openid.
	ScopesSupported []string `json:"scopes_supported"`
	// ClaimsSupported lists every claim a token carries.
	ClaimsSupported []string `json:"claims_supported"`
}

// ClaimNames lists every claim a run identity token carries, in the order Claims declares them.
func ClaimNames() []string {
	return []string{
		"iss", "sub", "aud", "exp", "iat", "nbf", "jti",
		"run_id", "purpose", "pull_request", "parent_run_id", "org_id", "project_id", "template_id",
		"environment",
		"credential_id", "tool", "run_type", "source", "commit_sha", "launcher_type",
		"actor", "actor_type", "actor_user_id", "approved", "approved_by", "approved_by_type",
		"approval_policy",
	}
}

// Discovery returns the discovery document for this issuer.
func (i *Issuer) Discovery() Discovery {
	return Discovery{
		Issuer:                           i.url,
		JWKSURI:                          i.url + JWKSPath,
		ResponseTypesSupported:           []string{"id_token"},
		SubjectTypesSupported:            []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{string(jose.RS256)},
		ScopesSupported:                  []string{"openid"},
		ClaimsSupported:                  ClaimNames(),
	}
}

// clock returns the time keys are judged by: the key store's clock, which every process sharing
// the store reads, so no one process's clock decides when a key starts signing, stops, leaves the
// published set, or loses its private half for all the others. A store with no clock of its own
// leaves it to the issuer's.
func (i *Issuer) clock(ctx context.Context) (time.Time, error) {
	t, err := i.keys.Now(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the federation key clock: %w", err)
	}
	if t.IsZero() {
		return i.now(), nil
	}
	return t, nil
}

// read returns every stored key and the time to judge them at. The time is read after the keys, so
// no key read is judged at a moment before it was written.
func (i *Issuer) read(ctx context.Context) ([]*Key, time.Time, error) {
	keys, err := i.keys.List(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list federation keys: %w", err)
	}
	now, err := i.clock(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	return keys, now, nil
}

// JWKS returns the public key set: every key from its creation until its removal, which is the key
// that signs, a key a normal rotation published ahead of signing, and a key retired within the
// grace period. It reads only public halves, so serving it never opens a private key.
func (i *Issuer) JWKS(ctx context.Context) (jose.JSONWebKeySet, error) {
	keys, now, err := i.read(ctx)
	if err != nil {
		return jose.JSONWebKeySet{}, err
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{}}
	for _, k := range keys {
		if !published(k, now) {
			continue
		}
		pub, err := parsePublic(k)
		if err != nil {
			return jose.JSONWebKeySet{}, err
		}
		set.Keys = append(set.Keys, jose.JSONWebKey{
			Key: pub, KeyID: k.ID, Algorithm: k.Algorithm, Use: "sig",
		})
	}
	return set, nil
}

// Keys describes every stored key, oldest first, with its state and its four times and no private
// material. A removed key stays listed, as the record of when it was trusted.
func (i *Issuer) Keys(ctx context.Context) ([]KeyInfo, error) {
	keys, now, err := i.read(ctx)
	if err != nil {
		return nil, err
	}
	signing := signer(keys, now)
	out := make([]KeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, infoOf(k, now, signing))
	}
	return out, nil
}

// Ensure makes sure a signing key exists, generating the first one on a new install, so the public
// key set is populated before the first run needs it and a cloud configured ahead of time finds a
// key. It also settles the keys, erasing the private half of any key whose removal has come.
func (i *Issuer) Ensure(ctx context.Context) error {
	_, _, _, err := i.signingKey(ctx)
	return err
}

// signingKey returns the key that signs new tokens, its private half, and the time it was judged
// at. When the keys are behind their schedule, or no key could sign, which is a new install, it
// settles them inside a change first, deciding again on the keys and the clock as they stand under
// the store's lock. A first key generated there signs at once: with nothing signing there is no
// relying party to give a day's notice to.
func (i *Issuer) signingKey(ctx context.Context) (*Key, *rsa.PrivateKey, time.Time, error) {
	keys, now, err := i.read(ctx)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	if signer(keys, now) == nil || len(settle(cloneKeys(keys), now)) > 0 {
		if keys, now, err = i.settleKeys(ctx, signer(keys, now) == nil); err != nil {
			return nil, nil, time.Time{}, err
		}
	}
	k := signer(keys, now)
	if k == nil {
		return nil, nil, time.Time{}, fmt.Errorf("%w: no federation key can sign", ErrIssuer)
	}
	priv, err := openPrivate(i.sealer, k)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return k, priv, now, nil
}

// settleKeys settles the keys inside one change, saving a first key when none can sign, and returns
// the keys as the change left them with the time it judged them at. The first key is generated
// before the change takes the store's lock, when generate says one will be needed, because
// generating an RSA key holds the lock long enough to stall every process that signs. It is
// discarded when another process saved a key first, and generated after all when the change finds
// no key that can sign though the read before it did.
func (i *Issuer) settleKeys(ctx context.Context, generate bool) ([]*Key, time.Time, error) {
	var fresh *Key
	for {
		if generate && fresh == nil {
			var err error
			if fresh, err = newKey(i.sealer); err != nil {
				return nil, time.Time{}, err
			}
		}
		var keys []*Key
		var now time.Time
		short := false
		err := i.keys.Change(ctx, func(ctx context.Context) error {
			var err error
			if keys, now, err = i.read(ctx); err != nil {
				return err
			}
			if err := i.saveSettled(ctx, keys, now); err != nil {
				return err
			}
			if signer(keys, now) != nil {
				return nil
			}
			if fresh == nil {
				short = true
				return nil
			}
			stamp(fresh, now, now)
			if err := i.keys.Save(ctx, fresh); err != nil {
				return fmt.Errorf("save federation key: %w", err)
			}
			keys = append(keys, fresh)
			return nil
		})
		if err != nil {
			return nil, time.Time{}, err
		}
		if !short {
			return keys, now, nil
		}
		generate = true
	}
}

// saveSettled settles keys at now and saves every key settling changed, inside the caller's change.
func (i *Issuer) saveSettled(ctx context.Context, keys []*Key, now time.Time) error {
	for _, k := range settle(keys, now) {
		if err := i.keys.Save(ctx, k); err != nil {
			return fmt.Errorf("settle federation key %s: %w", k.ID, err)
		}
	}
	return nil
}

// Rotate starts a normal rotation, the one for routine hygiene. It publishes a new key at once and
// starts signing with it RotationDelay later. The key it replaces keeps signing until then and
// stays published for RetiredKeyGrace after, so a relying party has a day to fetch the new key
// before any token needs it and every token the old key signed expires while that key is still
// published. The whole schedule is written now, so every process reads it from the store and
// nothing has to fire later for the rotation to complete. It returns the new key.
//
// A rotation already under way is refused with ErrRotationPending rather than replaced, since
// replacing the published key would cut short the day relying parties were given to fetch it. That
// holds across every process sharing the store, and the new key and the old key's retirement are
// written together or not at all, so a failure partway leaves the keys as they were.
func (i *Issuer) Rotate(ctx context.Context) (KeyInfo, error) {
	return i.rotate(ctx, false)
}

// EmergencyRotate replaces the signing key at once, for a key that may be compromised. The new key
// signs immediately, and every other key leaves the public key set at the same moment with its
// private half erased: the key that was signing, a key a normal rotation published and had not yet
// started signing with, and a retired key still in its grace period. They were all stored the same
// way, so a suspected compromise of one is treated as a compromise of all. It returns the new key.
//
// It trades availability for safety, and the trade is the caller's to make. A run holding a token
// the removed key signed fails at the cloud once the cloud reads the new key set, and a run that
// starts before a cloud has read the set naming the new key fails until it has. Nothing a removed
// key signed verifies against the published set again, and no process signs with a removed key
// again, whatever it read before the rotation.
func (i *Issuer) EmergencyRotate(ctx context.Context) (KeyInfo, error) {
	return i.rotate(ctx, true)
}

// rotate runs a normal or an emergency rotation as one decision every process sharing the store
// shares. It reads the keys first, so a store that cannot be read, or a normal rotation already
// under way, fails the request before a key is generated, and then generates the new key before
// taking the store's lock, because generating an RSA key holds the lock long enough to stall every
// process that signs. The decision itself is made inside a change, on the keys and the clock as
// they stand once the lock is held, so nothing another process wrote in between is overwritten by
// a stale copy, and every write of the rotation lands together or not at all.
func (i *Issuer) rotate(ctx context.Context, emergency bool) (KeyInfo, error) {
	keys, now, err := i.read(ctx)
	if err != nil {
		return KeyInfo{}, err
	}
	if !emergency {
		if err := refusePending(keys, now); err != nil {
			return KeyInfo{}, err
		}
	}
	fresh, err := newKey(i.sealer)
	if err != nil {
		return KeyInfo{}, err
	}
	var info KeyInfo
	err = i.keys.Change(ctx, func(ctx context.Context) error {
		keys, now, err := i.read(ctx)
		if err != nil {
			return err
		}
		if emergency {
			info, err = i.replaceAll(ctx, keys, now, fresh)
		} else {
			info, err = i.schedule(ctx, keys, now, fresh)
		}
		return err
	})
	if err != nil {
		return KeyInfo{}, err
	}
	return info, nil
}

// schedule writes a normal rotation inside the change it runs in: fresh is published now and signs
// RotationDelay later, at once when nothing signs, and every key that signs now retires when fresh
// starts and leaves the published set RetiredKeyGrace after.
func (i *Issuer) schedule(ctx context.Context, keys []*Key, now time.Time, fresh *Key) (KeyInfo, error) {
	if err := i.saveSettled(ctx, keys, now); err != nil {
		return KeyInfo{}, err
	}
	if err := refusePending(keys, now); err != nil {
		return KeyInfo{}, err
	}
	var current []*Key
	for _, k := range keys {
		if signs(k, now) {
			current = append(current, k)
		}
	}
	switchAt := now.Add(RotationDelay)
	if len(current) == 0 {
		// Nothing signs, so no relying party is relying on anything: the new key signs at once.
		switchAt = now
	}
	stamp(fresh, now, switchAt)
	if err := i.keys.Save(ctx, fresh); err != nil {
		return KeyInfo{}, fmt.Errorf("save federation key: %w", err)
	}
	for _, k := range current {
		retireBy(k, switchAt)
		if err := i.keys.Save(ctx, k); err != nil {
			return KeyInfo{}, fmt.Errorf("schedule retirement of federation key %s: %w", k.ID, err)
		}
	}
	return infoOf(fresh, now, signer(append(keys, fresh), now)), nil
}

// replaceAll writes an emergency rotation inside the change it runs in: fresh signs from now, and
// every other key not already removed and erased leaves the published set now with its private
// half erased. A key that was signing retires now, and a key that never started signing never will.
func (i *Issuer) replaceAll(ctx context.Context, keys []*Key, now time.Time, fresh *Key) (KeyInfo, error) {
	stamp(fresh, now, now)
	if err := i.keys.Save(ctx, fresh); err != nil {
		return KeyInfo{}, fmt.Errorf("save federation key: %w", err)
	}
	at := now.UTC()
	for _, k := range keys {
		if k.Sealed == "" && removed(k, now) {
			continue
		}
		switch {
		case !activated(k, now):
			// It never signed and now never will.
			k.ActivatedAt = nil
		case !retired(k, now):
			k.RetiredAt = cloneTime(&at)
		}
		if k.RemovedAt == nil || k.RemovedAt.After(at) {
			k.RemovedAt = cloneTime(&at)
		}
		k.Sealed = ""
		if err := i.keys.Save(ctx, k); err != nil {
			return KeyInfo{}, fmt.Errorf("remove federation key %s: %w", k.ID, err)
		}
	}
	return infoOf(fresh, now, fresh), nil
}

// mintAttempts bounds how many times Mint signs again after the key it signed with was removed
// while it signed. Each attempt reads the keys afresh, so a second attempt signs with the key the
// removing rotation installed, and only rotations racing each attempt in turn could exhaust it.
const mintAttempts = 3

// Mint signs an identity token carrying c for ttl, filling the registered claims: iss is this
// issuer, sub is built from the run's identity, and iat, nbf, exp, and jti are stamped now, on the
// clock the keys are judged by. c.Audience must be set. With a recorder, the issuance is recorded
// before the token is returned, and a token whose issuance cannot be recorded is withheld with
// ErrEvidence. The token is returned with its expiry.
//
// After signing, Mint reads the key it signed with again, and a token whose key lost its private
// half in the meantime is discarded unrecorded and signed again with the keys as they now stand. A
// process that read the keys just before an emergency rotation therefore never hands out, or puts
// on the record, a token signed after the rotation removed its key: a rotation that committed
// before the second read leaves the key erased there, and one that commits after it removed a key
// that was still trusted when the token was signed, like any token minted a moment before.
func (i *Issuer) Mint(ctx context.Context, c Claims, ttl time.Duration) (string, time.Time, error) {
	if c.Audience == "" {
		return "", time.Time{}, fmt.Errorf("%w: a token needs an audience", ErrSetting)
	}
	if ttl < MinTokenTTL || ttl > MaxTokenTTL {
		return "", time.Time{}, fmt.Errorf("%w: token lifetime %s is outside %s to %s", ErrSetting,
			ttl, MinTokenTTL, MaxTokenTTL)
	}
	sub, err := Subject(c)
	if err != nil {
		return "", time.Time{}, err
	}
	c.Issuer, c.Subject = i.url, sub
	for range mintAttempts {
		key, priv, now, err := i.signingKey(ctx)
		if err != nil {
			return "", time.Time{}, err
		}
		token, signed, err := sign(c, key.ID, priv, now, ttl)
		if err != nil {
			return "", time.Time{}, err
		}
		held, err := i.stillHeld(ctx, key.ID)
		if err != nil {
			return "", time.Time{}, err
		}
		if !held {
			continue
		}
		expiresAt := time.Unix(signed.ExpiresAt, 0).UTC()
		if i.recorder != nil {
			if err := i.recorder.RecordIssuance(ctx, Issuance{
				RunID: signed.RunID, ParentRunID: signed.ParentRunID, CredentialID: signed.CredentialID,
				KeyID: key.ID, TokenID: signed.ID, Actor: signed.Actor, IssuedAt: now,
				ExpiresAt: expiresAt,
			}); err != nil {
				return "", time.Time{}, fmt.Errorf("%w: %w", ErrEvidence, err)
			}
		}
		return token, expiresAt, nil
	}
	return "", time.Time{}, fmt.Errorf("sign token: the signing key was removed while the token was "+
		"signed, %d times running", mintAttempts)
}

// sign signs c with priv under kid, stamped at now and valid for ttl with a fresh jti, and returns
// the compact token and the claims it carries.
func sign(c Claims, kid string, priv *rsa.PrivateKey, now time.Time, ttl time.Duration) (string, Claims, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", Claims{}, fmt.Errorf("mint token id: %w", err)
	}
	c.IssuedAt, c.NotBefore, c.ExpiresAt = now.Unix(), now.Unix(), now.Add(ttl).Unix()
	c.ID = hex.EncodeToString(nonce[:])
	payload, err := json.Marshal(c)
	if err != nil {
		return "", Claims{}, fmt.Errorf("encode token claims: %w", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: kid},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", Claims{}, fmt.Errorf("build token signer: %w", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", Claims{}, fmt.Errorf("sign token: %w", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		return "", Claims{}, fmt.Errorf("serialize token: %w", err)
	}
	return token, c, nil
}

// stillHeld reports whether the stored key with id still holds its private half, which every
// removal takes from a key, by the time its removal has come or at once in an emergency rotation.
func (i *Issuer) stillHeld(ctx context.Context, id string) (bool, error) {
	keys, err := i.keys.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list federation keys: %w", err)
	}
	for _, k := range keys {
		if k.ID == id {
			return k.Sealed != "", nil
		}
	}
	return false, nil
}
