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
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kordloom/loomseal/jcs"
)

// preimageDomain separates a license signature from every other signature this product makes, so a
// signed value from one surface can never be replayed as another.
const preimageDomain = "switchtender-license/1\n"

// Tiers a license may grant, in ascending order. Each tier includes everything below it.
const (
	TierPro        = "pro"
	TierTeam       = "team"
	TierEnterprise = "enterprise"
)

// Feature names one gated capability. The set is small on purpose: every gate in the product calls
// Allow with one of these, so the whole paid surface is enumerable in one place.
type Feature string

const (
	// FeatureSSO is directory sign-in: OIDC, SAML, LDAP, and JWT.
	FeatureSSO Feature = "sso"
	// FeaturePolicyFull is the full policy engine: Rego policies, distinct-approver separation of
	// duties, and rules scoped to an actor, an actor kind, or an account. One require-approval
	// policy stays Community, and an exemption is free on every tier.
	FeaturePolicyFull Feature = "policy-full"
	// FeaturePolicyRules is the set of rules Pro adds to a plain hold: deny rules, risk floors, and
	// reversibility floors.
	FeaturePolicyRules Feature = "policy-rules"
	// FeatureRegister is the period change register, the SOC 2 / ISO evidence export. Per-run
	// dossiers, receipts, anchoring, and verification stay Community: proofs are free, the
	// compliance packaging is paid.
	FeatureRegister Feature = "register"
	// FeatureWorkers is distributed execution: relay workers and queues beyond the server's own.
	FeatureWorkers Feature = "workers"
	// FeatureReconcile is the one-click drift reconcile proposal. Drift detection stays Community.
	FeatureReconcile Feature = "reconcile"
	// FeatureHostedWitness is enrolling the install with the KordLoom-operated witness. Enterprise
	// includes it, and Team buys it as an add-on.
	FeatureHostedWitness Feature = "hosted-witness"
)

// AddonHostedWitness is the add-on entry that grants FeatureHostedWitness to a Team license.
const AddonHostedWitness = "hosted-witness"

// Claims is the signed body of a license.
type Claims struct {
	// V is the license format version, 1.
	V int `json:"v"`
	// ID names this license, for support and revocation conversations.
	ID string `json:"id"`
	// Org is the organization the license was issued to, as it should appear in status output.
	Org string `json:"org"`
	// Tier is pro, team, or enterprise. Licenses minted before Pro existed carry team, and an
	// empty tier is read as team so no issued license is ever downgraded by a build upgrade.
	Tier string `json:"tier"`
	// Hosts is the self-reported band: "500", "1000", "2500", or "unlimited". Informational; nothing
	// counts, so a license minted at the retired "250" band still verifies.
	Hosts string `json:"hosts,omitempty"`
	// Issued and Expires bound the term, RFC 3339.
	Issued  string `json:"issued"`
	Expires string `json:"expires"`
	// Kid names the compiled-in key that signed this license.
	Kid string `json:"kid"`
	// Addons lists what was bought on top of the tier, such as AddonHostedWitness. The signature
	// covers it only when it is non-empty, so a license without add-ons is signed over the same
	// bytes it always was.
	Addons []string `json:"addons,omitempty"`
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

// tierRank orders the tiers for comparison. An unrecognized tier ranks below every real one, so a
// license naming a tier this build has never heard of covers nothing rather than everything.
func tierRank(tier string) int {
	switch tier {
	case TierPro:
		return 1
	case TierTeam:
		return 2
	case TierEnterprise:
		return 3
	}
	return 0
}

// normalizeTier maps a license's stored tier to the tier it grants. The empty string is a license
// minted before tiers below Team existed; every one of those was sold as Team.
func normalizeTier(tier string) string {
	if tier == "" {
		return TierTeam
	}
	return tier
}

// featureTier is the lowest tier that includes each feature. A feature absent from this map
// requires Team, so forgetting to place a new feature fails toward the paid side, never the free.
var featureTier = map[Feature]string{
	FeatureSSO:           TierPro,
	FeaturePolicyFull:    TierTeam,
	FeaturePolicyRules:   TierPro,
	FeatureRegister:      TierTeam,
	FeatureWorkers:       TierTeam,
	FeatureReconcile:     TierTeam,
	FeatureHostedWitness: TierEnterprise,
}

// featureAddon names the add-on that grants a feature below the tier that includes it. Only a tier
// of addonTier or above can carry one.
var featureAddon = map[Feature]string{
	FeatureHostedWitness: AddonHostedWitness,
}

// addonTier is the lowest tier an add-on is sold on.
const addonTier = TierTeam

// minTierFor returns the lowest tier that includes a feature.
func minTierFor(f Feature) string {
	if tier, ok := featureTier[f]; ok {
		return tier
	}
	return TierTeam
}

// tierLabel is the capitalized tier name gate errors print.
func tierLabel(tier string) string {
	switch tier {
	case TierPro:
		return "Pro"
	case TierTeam:
		return "Team"
	case TierEnterprise:
		return "Enterprise"
	}
	return tier
}

// Covers reports whether the license includes a feature, by its tier or, from addonTier up, by an
// add-on it lists. It looks at the claims alone and not at the term, so a lapsed license still
// covers what it covered while live, which is the question Allow settles before it asks the clock.
func (l *License) Covers(f Feature) bool {
	rank := tierRank(normalizeTier(l.Claims.Tier))
	if rank >= tierRank(minTierFor(f)) {
		return true
	}
	addon, ok := featureAddon[f]
	return ok && rank >= tierRank(addonTier) && slices.Contains(l.Claims.Addons, addon)
}

// needFor names what a feature requires as gate errors print it, article included: "a Team
// license", or for a feature sold as an add-on, "an Enterprise license or the Team add-on".
func needFor(f Feature) string {
	tier := minTierFor(f)
	need := "a " + tierLabel(tier) + " license"
	if tier == TierEnterprise {
		need = "an " + tierLabel(tier) + " license"
	}
	if _, ok := featureAddon[f]; ok {
		need += " or the " + tierLabel(addonTier) + " add-on"
	}
	return need
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
	if len(c.Addons) > 0 {
		addons := make([]any, len(c.Addons))
		for i, a := range c.Addons {
			addons[i] = a
		}
		m["addons"] = addons
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

// builtIn holds the features every build carries, because their code is part of the Community
// source. A feature in neither builtIn nor a registered paid slot is refused on every build, which
// fails toward the paid side the way featureTier does.
var builtIn = map[Feature]bool{
	FeatureHostedWitness: true,
}

// carried holds the features the paid module registered code for. Copy on write, because gates read
// it from request handlers while the paid module's init, and tests, extend it.
var carried atomic.Pointer[map[Feature]bool]

// Carry records that this build carries the code behind fs. The paid module's slots call it when
// they register, so registering an implementation is the declaration and nothing can drift from it.
func Carry(fs ...Feature) {
	for {
		old := carried.Load()
		next := make(map[Feature]bool, len(fs))
		if old != nil {
			for f := range *old {
				next[f] = true
			}
		}
		for _, f := range fs {
			next[f] = true
		}
		if carried.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Uncarry removes fs from what this build carries. Only tests call it, to stand for a build that
// holds a license but not the code.
func Uncarry(fs ...Feature) {
	for {
		old := carried.Load()
		next := make(map[Feature]bool)
		if old != nil {
			for f := range *old {
				if !slices.Contains(fs, f) {
					next[f] = true
				}
			}
		}
		if carried.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Carried reports whether this build carries the code behind f.
func Carried(f Feature) bool {
	if builtIn[f] {
		return true
	}
	m := carried.Load()
	return m != nil && (*m)[f]
}

// PaidBuild reports whether the paid module registered code in this binary, which is what a
// license file needs to unlock anything.
func PaidBuild() bool {
	m := carried.Load()
	return m != nil && len(*m) > 0
}

// Absent returns the refusal for a feature this build does not carry, whatever license the process
// holds. It is the one place the Community refusal is built.
func Absent(f Feature) error {
	return fmt.Errorf("%w: %w", ErrRequiresTier, Unlicensed(f))
}

// Allow reports whether a feature may be used right now, and if not, why not, in one line.
//
// The error is the entire user experience of hitting a gate, so it names the feature, the tier, and
// where to go, and never says more. Whether the build carries the feature is settled first, so a
// Community build refuses moved features with the same line whatever license is installed, and
// never calls a feature lapsed or uncovered when its code is not there. The clock is checked here,
// on every call, so a license lapsing while the server runs drops it to Community without a
// restart, exactly as the pricing page promises.
func Allow(f Feature) error {
	if !Carried(f) {
		return Absent(f)
	}
	return allowAt(current.Load(), f, time.Now())
}

// Unlicensed returns the refusal a Community install gets for f, whatever license this process
// holds. A Community build refuses a paid feature with this line because the code that does it is
// not in the binary, so a license file alone cannot turn it on.
func Unlicensed(f Feature) error {
	return allowAt(nil, f, time.Now())
}

// allowAt is Allow against an explicit license and clock, which is what makes the rule testable.
//
// The license leads each sentence and the feature follows it, because "is required for" agrees with
// every gate name. Leading with the name put a singular verb after plural ones, so a Pro gate read
// "Deny rules and risk and reversibility floors requires a Pro license".
func allowAt(l *License, f Feature, now time.Time) error {
	name := gateName(f)
	need := needFor(f)
	if l == nil {
		return fmt.Errorf("%s is required for %s; this install runs Community. "+
			"https://switchtender.com/pricing", capitalize(need), name)
	}
	// Coverage is settled before the clock, and the order is the whole meaning of ErrLapsed.
	//
	// ErrLapsed tells a caller that this install had the feature working yesterday, and callers
	// act on that: the policy file keeps serving rules rather than taking the install offline.
	// Reporting the lapse first made an expired license say "lapsed" about a feature its tier
	// never included, so an expired Pro license was read as having lapsed out of the Team-only
	// policy engine and was handed it. An expired license would then buy strictly more than the
	// same license bought while it was live.
	if !l.Covers(f) {
		return fmt.Errorf("%s is required for %s; this install's %s license does not "+
			"include it. https://switchtender.com/pricing",
			capitalize(need), name, tierLabel(normalizeTier(l.Claims.Tier)))
	}
	if l.Expired(now) {
		return fmt.Errorf("%w: %s is required for %s and this install's license for %s "+
			"lapsed on %s; everything Community keeps working. https://switchtender.com/pricing",
			ErrLapsed, need, name, l.Claims.Org, l.Claims.Expires)
	}
	return nil
}

// gateName is the name a gate error prints for a feature, lowercased to sit after "required for".
// No name starts with a proper noun, so lowercasing the first letter never misspells one. A feature
// with no name prints its identifier.
func gateName(f Feature) string {
	name, ok := featureNames[f]
	if !ok || name == "" {
		return string(f)
	}
	return strings.ToLower(name[:1]) + name[1:]
}

// capitalize returns s with its first letter upper case, so a requirement can open a sentence.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// featureNames are the human names gates print, written as they would open a sentence.
var featureNames = map[Feature]string{
	FeatureSSO:           "Directory sign-in (OIDC, SAML, LDAP, JWT)",
	FeaturePolicyFull:    "The full policy engine",
	FeaturePolicyRules:   "Deny rules and risk and reversibility floors",
	FeatureRegister:      "The period change register",
	FeatureWorkers:       "Running distributed workers",
	FeatureReconcile:     "One-click drift reconcile",
	FeatureHostedWitness: "The hosted witness",
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
