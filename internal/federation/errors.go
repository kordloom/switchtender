package federation

import "errors"

var (
	// ErrIssuer is returned when the issuer URL cannot serve as an OpenID Connect issuer.
	ErrIssuer = errors.New("invalid federation issuer")
	// ErrNotConfigured is returned when a run needs a federated credential and this process has no
	// issuer to mint its token with.
	ErrNotConfigured = errors.New("workload identity federation is not configured")
	// ErrSetting is returned when a federated credential's settings are missing or malformed.
	ErrSetting = errors.New("invalid federated credential setting")
	// ErrSubject is returned when a run's identity cannot be written into a subject unambiguously.
	ErrSubject = errors.New("run identity cannot form a subject")
	// ErrExchange is returned when a cloud token service refuses or garbles a token exchange.
	ErrExchange = errors.New("token exchange refused")
	// ErrKeyRemoved is returned when a write to a key store would undo a removal: give back the
	// private half of a key whose private half was erased, or clear or postpone a key's removal.
	ErrKeyRemoved = errors.New("a removed federation key cannot be restored")
	// ErrDuplicate is returned when one run carries two federated credentials of the same kind,
	// whose environment variables would collide so that one silently replaced the other.
	ErrDuplicate = errors.New("one run carries two federated credentials of the same kind")
	// ErrRotationPending is returned when a normal rotation is asked for while the key the last one
	// published has not started signing yet.
	ErrRotationPending = errors.New("a key rotation is already under way")
	// ErrEvidence is returned when a minted token's issuance could not be recorded, so the token is
	// withheld rather than delivered without its signing key on record.
	ErrEvidence = errors.New("token issuance could not be recorded")
	// errChangeEnded is returned when a context outlives the key store change it carries and is
	// used to read or write after that change ended.
	errChangeEnded = errors.New("the federation key change has already ended")
)
