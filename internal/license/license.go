// Package license reads and checks SwitchTender's signed license files.
//
// A license is a small JSON document signed offline by KordLoom. The binary verifies it against
// keys compiled in, so nothing ever checks the network: no phone-home, no seat counting, no audit.
// An install with no license, an unreadable one, or a lapsed one runs the Community tier, which is
// complete on its own; a gate never crashes and never withholds data, it declines one paid feature
// with one line.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kordloom/loomseal/jcs"
)

// preimageDomain separates a license signature from every other signature this product makes, so a
// signed value from one surface can never be replayed as another.
const preimageDomain = "switchtender-license/1\n"

// Tiers a license may grant. Enterprise includes everything Team does.
const (
	TierTeam       = "team"
	TierEnterprise = "enterprise"
)

// Feature names one gated capability. The set is small on purpose: every gate in the product calls
// Allow with one of these, so the whole paid surface is enumerable in one place.
type Feature string

const (
	// FeatureSSO is directory sign-in: OIDC, SAML, and LDAP.
	FeatureSSO Feature = "sso"
	// FeaturePolicyFull is the full policy engine: deny rules, risk floors, actor scoping, and
	// distinct-approver separation of duties. One require-approval policy stays Community.
	FeaturePolicyFull Feature = "policy-full"
	// FeatureRegister is the period change register, the SOC 2 / ISO evidence export. Per-run
	// dossiers, receipts, anchoring, and verification stay Community: proofs are free, the
	// compliance packaging is paid.
	FeatureRegister Feature = "register"
	// FeatureWorkers is distributed execution: relay workers and queues beyond the server's own.
	FeatureWorkers Feature = "workers"
	// FeaturePostgresInit is initializing a NEW PostgreSQL schema. Opening an existing schema is
	// never gated, in any state, because a lapsed license must take nothing.
	FeaturePostgresInit Feature = "postgres-init"
	// FeatureReconcile is the one-click drift reconcile proposal. Drift detection stays Community.
	FeatureReconcile Feature = "reconcile"
)

// Claims is the signed body of a license.
type Claims struct {
	// V is the license format version, 1.
	V int `json:"v"`
	// ID names this license, for support and revocation conversations.
	ID string `json:"id"`
	// Org is the organization the license was issued to, as it should appear in status output.
	Org string `json:"org"`
	// Tier is team or enterprise.
	Tier string `json:"tier"`
	// Hosts is the self-reported band: "250", "1000", or "unlimited". Informational; nothing counts.
	Hosts string `json:"hosts,omitempty"`
	// Issued and Expires bound the term, RFC 3339.
	Issued  string `json:"issued"`
	Expires string `json:"expires"`
	// Kid names the compiled-in key that signed this license.
	Kid string `json:"kid"`
}

// File is a license as it sits on disk: the claims and a signature over their canonical form.
type File struct {
	// Claims is the signed body.
	Claims Claims `json:"claims"`
	// Sig is the base64 ed25519 signature over the domain-tagged canonical claims.
	Sig string `json:"sig"`
}

// License is a verified license plus where it came from.
type License struct {
	// Claims is the verified body.
	Claims Claims
	// Path is the file it was read from.
	Path string
}

// Expired reports whether the license term has lapsed as of now. A lapsed license is still a real
// license: status names it, and the install runs Community without a restart, because Allow checks
// the clock on every call rather than once at load.
func (l *License) Expired(now time.Time) bool {
	exp, err := time.Parse(time.RFC3339, l.Claims.Expires)
	if err != nil {
		return true
	}
	return now.After(exp)
}

// covers reports whether the license's tier includes a feature. Every feature in this file is Team;
// Enterprise includes Team.
func (l *License) covers(Feature) bool {
	return l.Claims.Tier == TierTeam || l.Claims.Tier == TierEnterprise
}

// Verify checks a license file's signature against the compiled-in keys and returns the license.
//
// It deliberately does not check the term: a lapsed license verifies and reports as lapsed, which
// is different from a forged one, and status output has to be able to say which.
func Verify(raw []byte, path string) (*License, error) {
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("license is not a license file: %w", err)
	}
	if f.Claims.V != 1 {
		return nil, fmt.Errorf("license version %d is not one this build reads", f.Claims.V)
	}
	pub, ok := keyFor(f.Claims.Kid)
	if !ok {
		return nil, fmt.Errorf("license names signing key %q, which this build does not trust",
			f.Claims.Kid)
	}
	sig, err := base64.StdEncoding.DecodeString(f.Sig)
	if err != nil {
		return nil, fmt.Errorf("license signature is not base64: %w", err)
	}
	canonical, err := jcs.Serialize(claimsObject(f.Claims))
	if err != nil {
		return nil, fmt.Errorf("canonicalize license: %w", err)
	}
	if !ed25519.Verify(pub, append([]byte(preimageDomain), canonical...), sig) {
		return nil, fmt.Errorf("license signature does not verify")
	}
	return &License{Claims: f.Claims, Path: path}, nil
}

// claimsObject renders claims as the map the canonical form is computed over, shared by the signer
// and the verifier so the two can never disagree about what is signed.
func claimsObject(c Claims) map[string]any {
	m := map[string]any{
		"v": c.V, "id": c.ID, "org": c.Org, "tier": c.Tier,
		"issued": c.Issued, "expires": c.Expires, "kid": c.Kid,
	}
	if c.Hosts != "" {
		m["hosts"] = c.Hosts
	}
	return m
}

// Sign produces a license file for claims. It lives here beside Verify so their preimages are the
// same bytes by construction; the private key never ships in a release binary, only the mint
// command loads one.
func Sign(c Claims, priv ed25519.PrivateKey) ([]byte, error) {
	canonical, err := jcs.Serialize(claimsObject(c))
	if err != nil {
		return nil, fmt.Errorf("canonicalize license: %w", err)
	}
	sig := ed25519.Sign(priv, append([]byte(preimageDomain), canonical...))
	return json.MarshalIndent(File{Claims: c, Sig: base64.StdEncoding.EncodeToString(sig)}, "", "  ")
}

// Load reads and verifies the license at path. A missing file is not an error state worth an error
// type: it returns nil, and nil means Community.
func Load(path string) (*License, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read license: %w", err)
	}
	return Verify(raw, path)
}

// current is the process-wide license. Nil is Community. Atomic because gates read it from
// request handlers while tests, and one day a reload signal, swap it.
var current atomic.Pointer[License]

// Set installs the process's license. Serve and the CLI commands call it once after Load.
func Set(l *License) { current.Store(l) }

// Current returns the process's license, nil for Community.
func Current() *License { return current.Load() }

// Allow reports whether a feature may be used right now, and if not, why not, in one line.
//
// The error is the entire user experience of hitting a gate, so it names the feature, the tier, and
// where to go, and never says more. The clock is checked here, on every call, so a license lapsing
// while the server runs drops it to Community without a restart, exactly as the pricing page
// promises.
func Allow(f Feature) error {
	return allowAt(current.Load(), f, time.Now())
}

// allowAt is Allow against an explicit license and clock, which is what makes the rule testable.
func allowAt(l *License, f Feature, now time.Time) error {
	name := featureNames[f]
	if name == "" {
		name = string(f)
	}
	if l == nil {
		return fmt.Errorf("%s requires a Team license; this install runs Community. "+
			"https://switchtender.com/pricing", name)
	}
	if l.Expired(now) {
		return fmt.Errorf("%s requires a Team license and this install's license for %s lapsed "+
			"on %s; everything Community keeps working. https://switchtender.com/pricing",
			name, l.Claims.Org, l.Claims.Expires)
	}
	if !l.covers(f) {
		return fmt.Errorf("%s is not covered by this install's %s license. "+
			"https://switchtender.com/pricing", name, l.Claims.Tier)
	}
	return nil
}

// featureNames are the human names gates print.
var featureNames = map[Feature]string{
	FeatureSSO:          "Directory sign-in (OIDC, SAML, LDAP)",
	FeaturePolicyFull:   "The full policy engine",
	FeatureRegister:     "The period change register",
	FeatureWorkers:      "Distributed workers",
	FeaturePostgresInit: "Initializing a new PostgreSQL database",
	FeatureReconcile:    "One-click drift reconcile",
}

// PathFor returns where an install's license lives. SWITCHTENDER_LICENSE overrides everything.
// A SQLite install keeps it beside the database file, the same rule the producer identity follows,
// so backup and restore carry both or neither. A DSN has no directory, so a PostgreSQL install
// reads it from the working directory unless the override says otherwise.
func PathFor(db string) string {
	if p := os.Getenv("SWITCHTENDER_LICENSE"); p != "" {
		return p
	}
	if strings.Contains(db, "://") {
		return "./switchtender-license.json"
	}
	dir := "."
	if i := strings.LastIndexByte(db, '/'); i >= 0 {
		dir = db[:i]
	}
	return dir + "/switchtender-license.json"
}
