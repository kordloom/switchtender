package cmd

import (
	"crypto/ed25519"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// TestValidateMintRefusesWhatIsNotSold covers the revenue path's only guard. A mint is
// irreversible: the sole revocation is retiring a signing key, which invalidates every license that
// key ever signed, so a license issued with the wrong tier, an empty organization, a band the
// product does not sell, or a negative term stands for its whole stated term at the customer.
//
// Before this guard the command defaulted --tier to team and --days to 365, so the shortest
// plausible invocation signed a full-year license for the most expensive tier with a blank
// organization on it.
func TestValidateMintRefusesWhatIsNotSold(t *testing.T) {
	// Not parallel: validateMint reads package-level flag variables.
	tests := []struct {
		Name    string
		Org     string
		Tier    string
		Hosts   string
		Days    int
		Addons  []string
		WantErr string
	}{{ // Test 0: The shape a real Pro trial takes.
		Name: "valid pro trial", Org: "Acme", Tier: license.TierPro, Hosts: "500", Days: 30,
	}, { // Test 1: A Team license at the top band.
		Name: "valid team unlimited", Org: "Acme", Tier: license.TierTeam, Hosts: "unlimited", Days: 365,
	}, { // Test 2: An empty organization would name nobody, and the field is what the binary matches.
		Name: "empty org", Org: "", Tier: license.TierPro, Hosts: "500", Days: 30,
		WantErr: "--org is required",
	}, { // Test 3: Whitespace is not an organization either.
		Name: "blank org", Org: "   ", Tier: license.TierPro, Hosts: "500", Days: 30,
		WantErr: "--org is required",
	}, { // Test 4: A tier that does not exist.
		Name: "bad tier", Org: "Acme", Tier: "platinum", Hosts: "500", Days: 30,
		WantErr: "is not pro, team, or enterprise",
	}, { // Test 5: An unset tier, which is what a forgotten flag leaves behind.
		Name: "empty tier", Org: "Acme", Tier: "", Hosts: "500", Days: 30,
		WantErr: "is not pro, team, or enterprise",
	}, { // Test 6: A band the pricing page does not publish.
		Name: "bad band", Org: "Acme", Tier: license.TierTeam, Hosts: "banana", Days: 30,
		WantErr: "is not a published band",
	}, { // Test 7: The retired 250 band. A license minted at it still verifies, but no new one is
		// signed.
		Name: "retired band", Org: "Acme", Tier: license.TierTeam, Hosts: "250", Days: 30,
		WantErr: "is not a published band",
	}, { // Test 8: A negative term signs a license that expired before it was issued.
		Name: "negative days", Org: "Acme", Tier: license.TierPro, Hosts: "500", Days: -30,
		WantErr: "--days must be positive",
	}, { // Test 9: A zero term is the same defect, and is what an unset flag now leaves.
		Name: "zero days", Org: "Acme", Tier: license.TierPro, Hosts: "500", Days: 0,
		WantErr: "--days must be positive",
	}, { // Test 10: Pro is sold to 500 hosts only, so Pro at a larger band is refused.
		Name: "pro above its band", Org: "Acme", Tier: license.TierPro, Hosts: "1000", Days: 365,
		WantErr: "is not a band Pro is sold at",
	}, { // Test 11: The same refusal at the top band, where the price gap is widest.
		Name: "pro unlimited", Org: "Acme", Tier: license.TierPro, Hosts: "unlimited", Days: 365,
		WantErr: "is not a band Pro is sold at",
	}, { // Test 12: Team at a band Pro refuses is still sold.
		Name: "valid team 1000", Org: "Acme", Tier: license.TierTeam, Hosts: "1000", Days: 365,
	}, { // Test 13: Pro is sold at one band, so a Pro license at the 2,500 band is refused too.
		Name: "pro at the 2500 band", Org: "Acme", Tier: license.TierPro, Hosts: "2500", Days: 365,
		WantErr: "is not a band Pro is sold at",
	}, { // Test 14: Team at the 2,500 band the ladder added.
		Name: "valid team 2500", Org: "Acme", Tier: license.TierTeam, Hosts: "2500", Days: 365,
	}, { // Test 15: A plausible but unsold band, between the 2,500 band and unlimited.
		Name: "unsold band", Org: "Acme", Tier: license.TierTeam, Hosts: "5000", Days: 30,
		WantErr: "is not a published band",
	}, { // Test 16: Team with the hosted witness add-on is what the Team card sells.
		Name: "team addon", Org: "Acme", Tier: license.TierTeam, Hosts: "1000", Days: 365,
		Addons: []string{license.AddonHostedWitness},
	}, { // Test 17: Pro cannot carry the add-on, so a Pro license listing it is refused.
		Name: "pro addon", Org: "Acme", Tier: license.TierPro, Hosts: "500", Days: 365,
		Addons:  []string{license.AddonHostedWitness},
		WantErr: "a pro license cannot carry an add-on",
	}, { // Test 18: An add-on the product does not sell.
		Name: "unsold addon", Org: "Acme", Tier: license.TierTeam, Hosts: "500", Days: 365,
		Addons:  []string{"custody"},
		WantErr: "is not one the product sells",
	}, { // Test 19: A misspelled add-on would sign a grant no build honors.
		Name: "misspelled addon", Org: "Acme", Tier: license.TierTeam, Hosts: "500", Days: 365,
		Addons:  []string{"hosted_witness"},
		WantErr: "is not one the product sells",
	}, { // Test 20: The same add-on twice.
		Name: "repeated addon", Org: "Acme", Tier: license.TierTeam, Hosts: "500", Days: 365,
		Addons:  []string{license.AddonHostedWitness, license.AddonHostedWitness},
		WantErr: "is listed twice",
	}, { // Test 21: An unsold add-on on Pro is named as unsold before the tier is.
		Name: "unsold addon on pro", Org: "Acme", Tier: license.TierPro, Hosts: "500", Days: 30,
		Addons:  []string{""},
		WantErr: "is not one the product sells",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			mintOrg, mintTier, mintHosts, mintDays = test.Org, test.Tier, test.Hosts, test.Days
			mintAddons = test.Addons
			t.Cleanup(func() { mintAddons = nil })
			err := validateMint()
			if test.WantErr == "" {
				if err != nil {
					t.Fatalf("validateMint() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateMint() = nil, want an error containing %q", test.WantErr)
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("validateMint() error = %q, want it to contain %q", err, test.WantErr)
			}
		})
	}
}

// TestMintIDIsUnique pins the identifier the terms and the support process rely on. The previous
// form was the issuer key prefix plus a timestamp to the second, so two licenses the same key signed
// inside one second carried byte-identical IDs. That is the field documented as the handle for
// revocation conversations, and two customers sharing one is the case where it matters most.
func TestMintIDIsUnique(t *testing.T) {
	t.Parallel()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	const runs = 500
	seen := make(map[string]bool, runs)
	for i := range runs {
		id, err := mintID(pub)
		if err != nil {
			t.Fatalf("mintID() error = %v", err)
		}
		if seen[id] {
			t.Fatalf("mintID() returned %q twice within %d calls in the same second", id, i+1)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "lic_") {
			t.Errorf("mintID() = %q, want it to start with lic_", id)
		}
	}
}

// TestMintFlagsHaveNoDefaultsThatSell guards against a default returning to the two flags that
// decide what was sold. A default tier of team and a default term of a year meant the shortest
// invocation produced the most expensive license, and the fix is only durable if it stays absent.
func TestMintFlagsHaveNoDefaultsThatSell(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"tier", "hosts"} {
		f := licenseMintCmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("license mint has no --%s flag", name)
		}
		if f.DefValue != "" {
			t.Errorf("--%s defaults to %q; it must be stated explicitly at every mint", name, f.DefValue)
		}
	}
	if f := licenseMintCmd.Flags().Lookup("days"); f == nil {
		t.Fatal("license mint has no --days flag")
	} else if f.DefValue != "0" {
		t.Errorf("--days defaults to %q; a term must be stated rather than inherited", f.DefValue)
	}
}

// TestMintPrintsAddons pins the add-ons in what the mint shows before it signs and in the line it
// prints after. The add-on is $3,000 a year, so the confirmation is where a wrong one is caught.
func TestMintPrintsAddons(t *testing.T) {
	// Not parallel: the output reads package-level flag variables.
	mintDays, mintOut = 365, "acme.json"
	t.Cleanup(func() { mintDays, mintOut = 0, "license.json" })
	base := license.Claims{
		ID: "lic_print", Org: "Acme", Tier: license.TierTeam, Hosts: "1000",
		Issued: "2026-09-01T00:00:00Z", Expires: "2027-09-01T00:00:00Z",
	}
	tests := []struct {
		Name       string
		Addons     []string
		WantPrompt string
		WantLine   string
	}{{ // Test 22: The hosted witness add-on is named in both.
		Name: "addon", Addons: []string{license.AddonHostedWitness},
		WantPrompt: "  addons  hosted-witness\n",
		WantLine: "minted lic_print team for Acme (1000 hosts, add-ons hosted-witness, 365 days, " +
			"expires 2027-09-01) -> acme.json\n",
	}, { // Test 23: No add-on reads as none, so the absence is a stated fact.
		Name: "none", WantPrompt: "  addons  none\n",
		WantLine: "minted lic_print team for Acme (1000 hosts, add-ons none, 365 days, " +
			"expires 2027-09-01) -> acme.json\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			c := base
			c.Addons = test.Addons
			if got := mintPrompt(c); !strings.Contains(got, test.WantPrompt) {
				t.Errorf("mintPrompt() = %q, want it to contain %q", got, test.WantPrompt)
			}
			if got := mintedLine(c); got != test.WantLine {
				t.Errorf("mintedLine() = %q, want %q", got, test.WantLine)
			}
		})
	}
}

// TestLicenseStatusPrintsAddons runs license status against signed licenses on disk and checks the
// add-ons line appears exactly when the license carries add-ons.
func TestLicenseStatusPrintsAddons(t *testing.T) {
	// Not parallel: it sets the license path in the environment and captures standard output.
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	const kid = "status-addons-test"
	license.RegisterKey(kid, pub)
	tests := []struct {
		Name       string
		Addons     []string
		WantAddons bool
	}{{ // Test 24: A Team license with the hosted witness add-on names it.
		Name: "addon", Addons: []string{license.AddonHostedWitness}, WantAddons: true,
	}, { // Test 25: A license with no add-ons prints no add-ons line.
		Name: "none",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			raw, err := license.Sign(license.Claims{
				V: 1, ID: "lic_status", Org: "Acme", Tier: license.TierTeam, Hosts: "250",
				Issued: "2026-09-01T00:00:00Z", Expires: "2099-09-01T00:00:00Z", Kid: kid,
				Addons: test.Addons,
			}, priv)
			if err != nil {
				t.Fatalf("Sign() error = %v", err)
			}
			path := filepath.Join(t.TempDir(), "license.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			t.Setenv("SWITCHTENDER_LICENSE", path)
			out, err := stdoutOf(t, func() error { return licenseStatusCmd.RunE(licenseStatusCmd, nil) })
			if err != nil {
				t.Fatalf("license status error = %v", err)
			}
			const line = "  addons   hosted-witness\n"
			if got := strings.Contains(out, line); got != test.WantAddons {
				t.Errorf("license status printed the add-ons line = %v, want %v:\n%s",
					got, test.WantAddons, out)
			}
			if !strings.Contains(out, "team license for Acme (valid)") {
				t.Errorf("license status did not name the license:\n%s", out)
			}
		})
	}
}

// TestMintClaimsCarryTheAddons checks the add-ons the mint was given are the ones it signs, since
// validating an add-on and then signing a license without it would sell a grant nobody receives.
func TestMintClaimsCarryTheAddons(t *testing.T) {
	// Not parallel: mintClaims reads package-level flag variables.
	mintOrg, mintTier, mintHosts, mintDays = "Acme", license.TierTeam, "1000", 365
	t.Cleanup(func() { mintOrg, mintTier, mintHosts, mintDays, mintAddons = "", "", "", 0, nil })
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		Addons     []string
		WantAddons string
	}{{ // Test 26: The hosted witness add-on is signed into the license.
		Addons: []string{license.AddonHostedWitness}, WantAddons: "[hosted-witness]",
	}, { // Test 27: No add-on signs none.
		Addons: nil, WantAddons: "[]",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			mintAddons = test.Addons
			c := mintClaims("lic_claims", now)
			if got := fmt.Sprint(c.Addons); got != test.WantAddons {
				t.Errorf("mintClaims().Addons = %s, want %s", got, test.WantAddons)
			}
			if c.Tier != license.TierTeam || c.Org != "Acme" || c.Hosts != "1000" ||
				c.Kid != license.IssuerKid || c.Expires != "2027-09-01T00:00:00Z" {
				t.Errorf("mintClaims() = %+v, want the flags it was given", c)
			}
		})
	}
}

// TestDocumentedMintsNameAPublishedBand holds every mint command the operator documents to a band
// the mint command signs. The witness runbook minted its test license at the 250 band after that
// band was retired, so the step it documents was refused at the one moment it was followed.
func TestDocumentedMintsNameAPublishedBand(t *testing.T) {
	t.Parallel()
	hosts := regexp.MustCompile(`--hosts[ =]"?([A-Za-z0-9]+)`)
	var mints int
	for _, dir := range []string{"../deploy", "../docs"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// A continued command is joined first, so a band on the line after the mint is read.
			text := strings.ReplaceAll(string(raw), "\\\n", " ")
			for line := range strings.Lines(text) {
				if !strings.Contains(line, "license mint") {
					continue
				}
				mints++
				m := hosts.FindStringSubmatch(line)
				if m == nil {
					t.Errorf("%s: a mint names no band: %s", path, strings.TrimSpace(line))
					continue
				}
				if !slices.Contains(mintBands, m[1]) {
					t.Errorf("%s: a mint names band %q, which is not one of %v", path, m[1],
						mintBands)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if mints == 0 {
		t.Fatal("no documented mint command was found, so this test checks nothing")
	}
}
