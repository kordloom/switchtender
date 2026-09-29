package cmd

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"

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
		WantErr string
	}{{ // Test 0: The shape a real Pro trial takes.
		Name: "valid pro trial", Org: "Acme", Tier: license.TierPro, Hosts: "250", Days: 30,
	}, { // Test 1: A Team license at the top band.
		Name: "valid team unlimited", Org: "Acme", Tier: license.TierTeam, Hosts: "unlimited", Days: 365,
	}, { // Test 2: An empty organization would name nobody, and the field is what the binary matches.
		Name: "empty org", Org: "", Tier: license.TierPro, Hosts: "250", Days: 30,
		WantErr: "--org is required",
	}, { // Test 3: Whitespace is not an organization either.
		Name: "blank org", Org: "   ", Tier: license.TierPro, Hosts: "250", Days: 30,
		WantErr: "--org is required",
	}, { // Test 4: A tier that does not exist.
		Name: "bad tier", Org: "Acme", Tier: "platinum", Hosts: "250", Days: 30,
		WantErr: "is not pro, team, or enterprise",
	}, { // Test 5: An unset tier, which is what a forgotten flag leaves behind.
		Name: "empty tier", Org: "Acme", Tier: "", Hosts: "250", Days: 30,
		WantErr: "is not pro, team, or enterprise",
	}, { // Test 6: A band the pricing page does not publish.
		Name: "bad band", Org: "Acme", Tier: license.TierTeam, Hosts: "banana", Days: 30,
		WantErr: "is not a published band",
	}, { // Test 7: A plausible but unsold band. 500 sits between two real ones.
		Name: "unsold band", Org: "Acme", Tier: license.TierTeam, Hosts: "500", Days: 30,
		WantErr: "is not a published band",
	}, { // Test 8: A negative term signs a license that expired before it was issued.
		Name: "negative days", Org: "Acme", Tier: license.TierPro, Hosts: "250", Days: -30,
		WantErr: "--days must be positive",
	}, { // Test 9: A zero term is the same defect, and is what an unset flag now leaves.
		Name: "zero days", Org: "Acme", Tier: license.TierPro, Hosts: "250", Days: 0,
		WantErr: "--days must be positive",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			mintOrg, mintTier, mintHosts, mintDays = test.Org, test.Tier, test.Hosts, test.Days
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
