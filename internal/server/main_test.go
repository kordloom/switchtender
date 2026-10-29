package server

import (
	"os"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/user"
)

// TestMain runs the package under a Team license, carrying the paid code as the paid build does,
// because the suite exercises Team features throughout: deny policies, risk floors, reconcile
// proposals. The refusal paths are proven by the
// gate tests, which swap the license out and back around themselves and stay sequential for it.
//
// It also hashes passwords at the lowest bcrypt cost. The suite builds accounts by the hundred, and
// at the default cost hashing them was the largest single cost in its profile. The sign-in guard is
// built after this runs, so its stand-in comparison runs at the same cost as the hashes.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	license.Carry(license.FeatureSSO, license.FeaturePolicyFull, license.FeaturePolicyRules,
		license.FeatureRegister, license.FeatureWorkers, license.FeatureReconcile)
	user.UseMinCostForTests()
	os.Exit(m.Run())
}

// TestTheSuiteHashesAndComparesAtOneCost checks that TestMain lowered the cost on both sides of a
// sign-in: the hash user.New stores and the stand-in comparison the default guard makes for a missing
// username. A guard at another cost than the hashes answers a missing username in a different time
// than a real one.
func TestTheSuiteHashesAndComparesAtOneCost(t *testing.T) {
	t.Parallel()
	u, err := user.New("cost-probe", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	cost, err := bcrypt.Cost([]byte(u.PasswordHash))
	if err != nil {
		t.Fatalf("bcrypt.Cost() error = %v", err)
	}
	if cost != bcrypt.MinCost {
		t.Errorf("user.New hashed at cost %d, want %d from TestMain", cost, bcrypt.MinCost)
	}
	if got := user.DefaultGuard().Cost(); got != cost {
		t.Errorf("default guard compares at cost %d, want the hashes' cost %d", got, cost)
	}
}

// sealerPair names a passphrase and salt a test derives a Sealer from.
type sealerPair struct {
	// passphrase is the operator passphrase the key derives from.
	passphrase string
	// salt is the per-deployment salt.
	salt string
}

//nolint:gochecknoglobals // Guarded by sealersMu, simplifies testing.
var (
	// sealersMu guards sealers.
	sealersMu sync.Mutex
	// sealers holds one derivation per pair, run on first use.
	sealers = map[sealerPair]func() *credential.Sealer{}
)

// sharedSealer returns the package's one Sealer for passphrase and salt. Deriving a key runs
// argon2id at production strength, about 0.4 s under the race detector and over 2 s when ten tests
// derive at once, and the key depends on nothing but its inputs. A Sealer is never written after it
// is made, so every test that names the same pair can share one. Distinct pairs derive concurrently.
func sharedSealer(passphrase, salt string) *credential.Sealer {
	pair := sealerPair{passphrase: passphrase, salt: salt}
	sealersMu.Lock()
	derive, ok := sealers[pair]
	if !ok {
		derive = sync.OnceValue(func() *credential.Sealer {
			return credential.NewSealer(passphrase, salt)
		})
		sealers[pair] = derive
	}
	sealersMu.Unlock()
	return derive()
}
