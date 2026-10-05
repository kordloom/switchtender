package outcome

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// ErrIssuance is returned when a token issuance is missing a part its chain entry has to name.
var ErrIssuance = errors.New("token issuance cannot be recorded")

// TokenIssuance is the evidence of one federated identity token: the run it was minted for, the
// credential it was minted under, the id of the key that signed it, the token's own id, and when it
// expires. The key is named by its public thumbprint and the token by its jti, so nothing here lets
// anyone rebuild either.
type TokenIssuance struct {
	// RunID is the run the token was minted for.
	RunID string
	// ParentRunID is the pipeline or split run that run belongs to, empty for a top-level run. It is
	// named in the entry so the receipt of the run that was launched discloses a step's tokens.
	ParentRunID string
	// CredentialID is the federated credential the token was minted under.
	CredentialID string
	// KeyID is the id of the key that signed the token, the kid in its header.
	KeyID string
	// TokenID is the token's jti.
	TokenID string
	// IssuedAt is when the token was signed, the time the entry records.
	IssuedAt time.Time
	// ExpiresAt is when the token stops being valid.
	ExpiresAt time.Time
	// OnBehalfOf is the launching actor the token names.
	OnBehalfOf string
}

// TokenPath is the chain path of a token issuance:
//
//	/runs/<run>/federation/<credential>/kid/<key id>/jti/<token id>/exp/<unix seconds>
//
// followed by /parent/<run> for a pipeline step or a shard. Every part is an identifier or a number
// and nothing is a name a person chose, so the path discloses which key signed which run's token
// and when it lapsed, and nothing a token or a key could be rebuilt from. Each identifier is path
// escaped, so none can add a segment. The run comes first and as a whole segment, so a sparse
// receipt and a dossier find the entry by the run's id like every other entry about the run.
func TokenPath(ti TokenIssuance) (string, error) {
	switch {
	case ti.RunID == "":
		return "", fmt.Errorf("%w: it names no run", ErrIssuance)
	case ti.CredentialID == "":
		return "", fmt.Errorf("%w: it names no credential", ErrIssuance)
	case ti.KeyID == "":
		return "", fmt.Errorf("%w: it names no signing key", ErrIssuance)
	case ti.TokenID == "":
		return "", fmt.Errorf("%w: it names no token id", ErrIssuance)
	case ti.ExpiresAt.IsZero():
		return "", fmt.Errorf("%w: it has no expiry", ErrIssuance)
	}
	path := "/runs/" + url.PathEscape(ti.RunID) + "/federation/" + url.PathEscape(ti.CredentialID) +
		"/kid/" + url.PathEscape(ti.KeyID) + "/jti/" + url.PathEscape(ti.TokenID) +
		"/exp/" + strconv.FormatInt(ti.ExpiresAt.Unix(), 10)
	if ti.ParentRunID != "" {
		path += "/parent/" + url.PathEscape(ti.ParentRunID)
	}
	return path, nil
}

// ParseTokenPath reads a path TokenPath wrote, reporting ok false for anything that does not
// round-trip exactly, so a near miss stays an ordinary entry rather than being read as an issuance.
// The returned issuance carries no time but its expiry and no actor, which the entry holds
// elsewhere.
func ParseTokenPath(path string) (TokenIssuance, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 10 && len(parts) != 12 {
		return TokenIssuance{}, false
	}
	if parts[0] != "runs" || parts[2] != "federation" || parts[4] != "kid" || parts[6] != "jti" ||
		parts[8] != "exp" {
		return TokenIssuance{}, false
	}
	ids := []string{parts[1], parts[3], parts[5], parts[7]}
	for i, raw := range ids {
		v, err := url.PathUnescape(raw)
		if err != nil {
			return TokenIssuance{}, false
		}
		ids[i] = v
	}
	ti := TokenIssuance{RunID: ids[0], CredentialID: ids[1], KeyID: ids[2], TokenID: ids[3]}
	exp, err := strconv.ParseInt(parts[9], 10, 64)
	if err != nil {
		return TokenIssuance{}, false
	}
	ti.ExpiresAt = time.Unix(exp, 0).UTC()
	if len(parts) == 12 {
		if parts[10] != "parent" {
			return TokenIssuance{}, false
		}
		parent, err := url.PathUnescape(parts[11])
		if err != nil {
			return TokenIssuance{}, false
		}
		ti.ParentRunID = parent
	}
	if again, err := TokenPath(ti); err != nil || again != path {
		return TokenIssuance{}, false
	}
	return ti, true
}

// CommitTokenIssuance records a token issuance as a chain entry, by committer acting on behalf of
// whoever launched the run. The entry carries no content digest: everything it attests is in its
// path, which the chain link commits and every export discloses as written, so a receipt shows
// which key signed the run's token with nothing further to rebuild. It is appended before the token
// leaves the issuer, and a failure is returned, so a token whose signing key is not on record is
// withheld.
func CommitTokenIssuance(ctx context.Context, audits audit.Store, ti TokenIssuance, committer string) error {
	path, err := TokenPath(ti)
	if err != nil {
		return err
	}
	return audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: ti.IssuedAt, Actor: committer, ActorType: "system",
		OnBehalfOf: ti.OnBehalfOf, Method: audit.MethodToken, Path: path,
	})
}
