package run_test

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestClientKeysAreScopedToTheirOrg covers a cross-tenant leak through the politest header in the API.
// An idempotency key is chosen by the caller, and keys people choose are ordinary words: "nightly",
// "deploy-2026-08-17", "1". The stored key was global, so the moment two organizations on one install
// picked the same word, the second one's submission stopped being their own: the lookup found the first
// organization's run and returned it, handing over that run's id, command, actor, and status, and the
// change they actually asked for never ran. Neither side saw an error.
func TestClientKeysAreScopedToTheirOrg(t *testing.T) {
	t.Parallel()
	const word = "nightly-deploy"

	acme, err := run.ClientKey(word, "org_acme")
	if err != nil {
		t.Fatalf("ClientKey: %v", err)
	}
	other, err := run.ClientKey(word, "org_globex")
	if err != nil {
		t.Fatalf("ClientKey: %v", err)
	}
	if acme == other {
		t.Fatalf("two organizations sending the same idempotency key %q both store %q, so one "+
			"organization's submission resolves to the other's run", word, acme)
	}

	// The same organization repeating its own key still deduplicates, which is the whole feature.
	again, err := run.ClientKey(word, "org_acme")
	if err != nil {
		t.Fatalf("ClientKey: %v", err)
	}
	if again != acme {
		t.Errorf("the same key from the same organization stored as %q then %q, so a retry would "+
			"fire a second run", acme, again)
	}

	// An install with no organizations keeps the key exactly as the caller sent it, so a single-tenant
	// deployment's stored keys do not change shape.
	plain, err := run.ClientKey(word, "")
	if err != nil {
		t.Fatalf("ClientKey: %v", err)
	}
	if plain != word {
		t.Errorf("unowned key stored as %q, want the caller's key %q unchanged", plain, word)
	}

	// The server's own namespace stays reserved whatever organization asks for it.
	if _, err := run.ClientKey("st:internal", "org_acme"); err == nil {
		t.Error("a caller claimed the reserved internal key namespace")
	}

	// A key that already looks scoped cannot be used to impersonate another organization's namespace.
	forged, err := run.ClientKey("org_globex\x00nightly-deploy", "org_acme")
	if err != nil && !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err == nil && forged == other {
		t.Error("a caller in one organization forged another organization's stored key")
	}
}
