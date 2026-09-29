package server

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// trustDocument is what a relying party fetches to learn which key legitimately signs this install's
// bundles. It carries no secret: the same public key already travels inside every bundle.
//
// A bundle is verifiable offline without this endpoint, because it embeds the key that signed it.
// What this adds is attribution. Without an out of band fingerprint, anyone can generate a key, sign
// a fabricated bundle, and have it verify as signed by a key nobody has any reason to trust. Fetching
// this once and pinning key_id is what lets a verifier say a bundle came from this install rather
// than from someone who merely owns a keypair.
type trustDocument struct {
	// Product names the software that emits the bundles.
	Product string `json:"product"`
	// ProductVersion is the running build.
	ProductVersion string `json:"product_version"`
	// InstallID identifies this install, matching producer.install_id in its bundles.
	InstallID string `json:"install_id"`
	// PublicKey is the raw ed25519 public key in standard base64, matching producer.public_key.
	PublicKey string `json:"public_key"`
	// KeyID is the fingerprint to pin, matching producer.key_id.
	KeyID string `json:"key_id"`
	// Format names the bundle format this install emits.
	Format string `json:"format"`
	// ChainProfile names the link construction its bundles declare.
	ChainProfile string `json:"chain_profile"`
}

// trustHandler serves the install's signing identity so a verifier can pin it.
//
// It is deliberately unauthenticated. A relying party checking a bundle has no account here, and
// requiring one would defeat the point of a record that is verifiable without our involvement. The
// response holds only public values.
func trustHandler(id *audit.Identity, version string, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if id == nil {
			respondError(w, log, http.StatusNotFound, "this install has no signing identity")
			return
		}
		// Cached briefly rather than not at all: the identity changes only when an operator rotates
		// the key, and a verifier fetching it repeatedly should not be answered from scratch.
		w.Header().Set("Cache-Control", "public, max-age=300")
		respondJSON(w, log, http.StatusOK, trustDocument{
			Product:        audit.ProductName,
			ProductVersion: version,
			InstallID:      id.InstallID,
			PublicKey:      id.PublicKeyBase64(),
			KeyID:          id.KeyID(),
			Format:         "loomseal/0.1",
			ChainProfile:   audit.ChainProfile,
		}, wantsPretty(r))
	}
}
