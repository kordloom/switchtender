package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestABackupCarriesNoFederationKeyMaterial pins that a backup of an install whose database holds
// federation signing keys carries none of their private material, neither in the clear envelope nor
// in the sealed payload a restore reads. The payload is sealed under the same encryption key the
// signing keys are, so the check opens it rather than trusting the seal to hide what is inside.
func TestABackupCarriesNoFederationKeyMaterial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sealer := credential.NewSealer("pass", "salt")
	issuer, err := federation.NewIssuer("https://st.example.com", db.FederationKeys(), sealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := issuer.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if _, err := issuer.Rotate(ctx); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if err := db.Credentials().Save(ctx, &credential.Credential{
		ID: "cred_marker", Name: "backup-content-marker", Kind: credential.KindOIDCToken,
		Settings: map[string]string{"audience": "vault"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("save credential: %v", err)
	}
	keys, err := db.FederationKeys().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var private []string
	for _, k := range keys {
		plain, err := sealer.Open(k.Sealed)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		private = append(private, k.Sealed, plain)
	}
	if len(private) != 4 {
		t.Fatalf("collected %d private values, want two keys' sealed halves and PEMs", len(private))
	}

	var buf bytes.Buffer
	if _, err := Write(ctx, snapshotStores(db), sealer, &buf); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	var env envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	compressed, err := sealer.Open(env.Sealed)
	if err != nil {
		t.Fatalf("open payload: %v", err)
	}
	zr, err := gzip.NewReader(strings.NewReader(compressed))
	if err != nil {
		t.Fatalf("gunzip payload: %v", err)
	}
	payload, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(payload), "backup-content-marker") {
		t.Fatal("the opened payload does not hold the credential saved before the backup, so the " +
			"scan below would scan nothing")
	}
	for _, p := range private {
		if strings.Contains(buf.String(), p) || strings.Contains(string(payload), p) {
			t.Error("the backup carries federation signing key material")
		}
	}
	if strings.Contains(string(payload), "PRIVATE KEY") {
		t.Error("the backup payload carries a PEM private key")
	}
}
