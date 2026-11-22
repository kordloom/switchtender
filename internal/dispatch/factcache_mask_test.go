package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/run"
)

// TestFactCacheMasksTheRunsOwnSecrets proves the facts a run gathers are saved with the run's own
// secret values masked. A local play gathers ansible_env, which holds every variable an env
// credential injected, and the stored facts are read later by people who could not read that
// credential. A document that holds no secret is saved as the cache saved it before masking, its
// large numbers kept exact.
func TestFactCacheMasksTheRunsOwnSecrets(t *testing.T) {
	t.Parallel()
	const secret = "v4lue-xyz"
	withSecret := `{"ansible_env": {"AZURE_STORAGE_KEY": "` + secret + `", "HOME": "/root"}, ` +
		`"ansible_memtotal_mb": 15258, "note": "a <b> & c"}`
	plain := "{\n    \"ansible_distribution\": \"Debian\",\n    \"size\": 12345678901234567890\n}"
	compact := `{"ansible_distribution":"Debian","size":12345678901234567890}`
	tests := []struct {
		// Doc is the fact document the play writes for localhost.
		Doc string
		// WantStored is the stored document, or empty to check only WantAbsent and WantPresent.
		WantStored string
		// WantAbsent and WantPresent are strings the stored document must not and must hold.
		WantAbsent  []string
		WantPresent []string
	}{{ // Test 0: The injected value is masked, and the rest of the document is kept as written.
		Doc:        withSecret,
		WantAbsent: []string{secret},
		WantPresent: []string{`"AZURE_STORAGE_KEY":"` + MaskToken + `"`, `"ansible_memtotal_mb":15258`,
			`"a <b> & c"`},
	}, { // Test 1: A document with no secret in it is stored as the cache compacts it, unmasked.
		Doc: plain, WantStored: compact,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			sealer := credential.NewSealer("pass", "salt")
			sealed, err := sealer.Seal("AZURE_STORAGE_KEY=" + secret)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}
			creds := credential.NewMemStore()
			if err := creds.Save(ctx, &credential.Credential{ID: "cred_1", Name: "azure",
				Kind: credential.KindEnv, Source: credential.SourceLocal, Secret: sealed}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			inventories := inventory.NewMemStore()
			if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "local",
				Content: "localhost ansible_connection=local\n", CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			facts := factcache.NewMemStore()
			play := &factPlay{}
			play.next(map[string]string{"localhost": test.Doc})
			store := run.NewMemStore()
			d := New(store, play.runner(), nil, WithCredentials(creds, sealer), WithNoJanitor(),
				WithInventories(inventories), WithFactCache(facts))
			defer d.Close()
			r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"),
				run.WithCredentialIDs([]string{"cred_1"}), run.WithFactCache(true, 0))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			dispatchtest.WaitTerminal(t, store, r.ID)
			got, err := facts.Facts(ctx, "inv_1", "localhost")
			if err != nil {
				t.Fatalf("Facts() error = %v", err)
			}
			stored := string(got.Facts)
			if got.Bytes != len(got.Facts) {
				t.Errorf("stored Bytes = %d, want the stored document's length %d", got.Bytes,
					len(got.Facts))
			}
			if !json.Valid(got.Facts) {
				t.Errorf("stored document is not JSON: %s", stored)
			}
			if test.WantStored != "" {
				if diff := cmp.Diff(test.WantStored, stored); diff != "" {
					t.Errorf("stored document (-want +got):\n%s", diff)
				}
			}
			for _, absent := range test.WantAbsent {
				if strings.Contains(stored, absent) {
					t.Errorf("stored document holds %q: %s", absent, stored)
				}
			}
			for _, present := range test.WantPresent {
				if !strings.Contains(stored, present) {
					t.Errorf("stored document lacks %q: %s", present, stored)
				}
			}
		})
	}
}
