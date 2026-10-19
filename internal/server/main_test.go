package server

import (
	"os"
	"sync"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/license"
)

// TestMain runs the package under a Team license, because the suite exercises Team features
// throughout: deny policies, risk floors, reconcile proposals. The refusal paths are proven by the
// gate tests, which swap the license out and back around themselves and stay sequential for it.
func TestMain(m *testing.M) {
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	os.Exit(m.Run())
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
