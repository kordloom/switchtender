package server

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/federation"
)

// federationOff is the answer every federation route gives on an install with no issuer URL.
const federationOff = "workload identity federation is not configured: set --federation-issuer"

// federationCacheControl lets a relying party cache the discovery document and key set briefly. It
// is short so a fresh read is never far behind the store: a normal rotation publishes the next key
// a day before it signs, and an emergency rotation removes keys at once, which a long cache would
// hide.
const federationCacheControl = "public, max-age=60"

// WithFederation serves the issuer's discovery document and public key set and enables the signing
// key endpoints. Nil leaves federation off.
func WithFederation(issuer *federation.Issuer) Option {
	return func(srv *Server) { srv.federation = issuer }
}

// federationDiscoveryHandler serves the OpenID Connect discovery document a cloud reads to find the
// public key set. It is unauthenticated, since the cloud verifying a run's token has no account
// here.
func federationDiscoveryHandler(issuer *federation.Issuer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if issuer == nil {
			respondError(w, log, http.StatusNotFound, federationOff)
			return
		}
		w.Header().Set("Cache-Control", federationCacheControl)
		respondJSON(w, log, http.StatusOK, issuer.Discovery(), wantsPretty(r))
	}
}

// federationJWKSHandler serves the public key set: the key that signs and every key a rotation
// retired within the grace period. It holds public halves only.
func federationJWKSHandler(issuer *federation.Issuer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if issuer == nil {
			respondError(w, log, http.StatusNotFound, federationOff)
			return
		}
		set, err := issuer.JWKS(r.Context())
		if err != nil {
			log.Error("server: federation key set: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the federation keys")
			return
		}
		w.Header().Set("Cache-Control", federationCacheControl)
		respondJSON(w, log, http.StatusOK, set, wantsPretty(r))
	}
}

// federationKeysResponse lists the issuer's signing keys without private material.
type federationKeysResponse struct {
	// Issuer is the issuer URL every token names.
	Issuer string `json:"issuer"`
	// Keys describes each stored key, oldest first.
	Keys []federation.KeyInfo `json:"keys"`
}

// federationKeysHandler lists the signing keys with each one's state and its created, activated,
// retired, and removed times: which signs, which a rotation has published ahead of signing, and
// which are retired or removed.
func federationKeysHandler(issuer *federation.Issuer, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if issuer == nil {
			respondError(w, log, http.StatusNotFound, federationOff)
			return
		}
		keys, err := issuer.Keys(r.Context())
		if err != nil {
			log.Error("server: federation keys: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read the federation keys")
			return
		}
		respondJSON(w, log, http.StatusOK, federationKeysResponse{Issuer: issuer.URL(), Keys: keys},
			wantsPretty(r))
	}
}

// federationRotateResponse names the key a rotation created and lists every key as it stands after.
type federationRotateResponse struct {
	// Key is the new key: pending after a normal rotation, signing after an emergency one.
	Key federation.KeyInfo `json:"key"`
	// Emergency reports an emergency rotation.
	Emergency bool `json:"emergency"`
	// Keys lists every key after the rotation, with the schedule it set.
	Keys []federation.KeyInfo `json:"keys"`
}

// federationRotateHandler starts a normal rotation: the new key is published now and signs a day
// later, and the key it replaces signs until then and stays published a day after. A rotation
// already under way answers 409 with the time its key starts signing. It is admin work, and the
// request is recorded in the audit chain like every other change.
func federationRotateHandler(issuer *federation.Issuer, log *zap.Logger) http.HandlerFunc {
	return rotateHandler(issuer, log, false)
}

// federationEmergencyRotateHandler replaces the signing key at once for a key that may be
// compromised: the new key signs immediately and every other key leaves the published set now. Its
// own path puts the choice on the record, so the audit chain tells an emergency rotation from a
// routine one without disclosing anything else.
func federationEmergencyRotateHandler(issuer *federation.Issuer, log *zap.Logger) http.HandlerFunc {
	return rotateHandler(issuer, log, true)
}

// rotateHandler runs a normal or an emergency rotation and answers with the new key and the listing
// after it.
func rotateHandler(issuer *federation.Issuer, log *zap.Logger, emergency bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if issuer == nil {
			respondError(w, log, http.StatusNotFound, federationOff)
			return
		}
		rotate := issuer.Rotate
		if emergency {
			rotate = issuer.EmergencyRotate
		}
		info, err := rotate(r.Context())
		switch {
		case errors.Is(err, federation.ErrRotationPending):
			respondError(w, log, http.StatusConflict, err.Error())
			return
		case err != nil:
			log.Error("server: rotate federation key: "+err.Error(), zap.Bool("emergency", emergency))
			respondError(w, log, http.StatusInternalServerError, "could not rotate the federation key")
			return
		}
		keys, err := issuer.Keys(r.Context())
		if err != nil {
			log.Error("server: federation keys after a rotation: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "the key rotated, but the keys "+
				"could not be read back: list them again")
			return
		}
		respondJSON(w, log, http.StatusOK, federationRotateResponse{
			Key: info, Emergency: emergency, Keys: keys,
		}, wantsPretty(r))
	}
}
