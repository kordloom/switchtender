package federation

import (
	"context"
	"time"
)

// Issuance is the evidence of one minted token: the run it was minted for, the credential it was
// minted under, the key that signed it, and its validity. It names the key by its id, the public
// thumbprint, and the token by its jti, so it carries nothing a key or a token could be rebuilt
// from.
type Issuance struct {
	// RunID is the run the token was minted for.
	RunID string
	// ParentRunID is the pipeline or split run that run belongs to, empty for a top-level run.
	ParentRunID string
	// CredentialID is the federated credential the token was minted under.
	CredentialID string
	// KeyID is the id of the key that signed the token, the kid in its header.
	KeyID string
	// TokenID is the token's jti, unique per token.
	TokenID string
	// Actor is the launching actor the token names, so the evidence says on whose behalf it was
	// minted.
	Actor string
	// IssuedAt is when the token was signed.
	IssuedAt time.Time
	// ExpiresAt is when the token stops being valid.
	ExpiresAt time.Time
}

// IssuanceRecorder records a token issuance as evidence. The issuer calls it after signing and
// before the token is returned, and a recorder that fails stops the token from leaving the issuer,
// so no token reaches a tool or a cloud without its signing key being on record.
type IssuanceRecorder interface {
	// RecordIssuance records is, returning an error when it could not be recorded.
	RecordIssuance(ctx context.Context, is Issuance) error
}

// IssuanceRecorderFunc adapts a function to IssuanceRecorder.
type IssuanceRecorderFunc func(ctx context.Context, is Issuance) error

// RecordIssuance calls f.
func (f IssuanceRecorderFunc) RecordIssuance(ctx context.Context, is Issuance) error {
	return f(ctx, is)
}

// WithIssuanceRecorder has the issuer record every token it mints through r before returning it.
func WithIssuanceRecorder(r IssuanceRecorder) Option {
	return func(i *Issuer) { i.recorder = r }
}
