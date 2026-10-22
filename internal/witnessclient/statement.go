package witnessclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kordloom/switchtender/witness"
)

// statementPath returns the path a witness serves the signed statement of producer key keyID at.
func statementPath(keyID string) string { return "/v1/installs/" + keyID + "/attestation" }

// FetchStatement returns the signed statement of what the witness of enr holds for the install enr
// enrolled: the newest beat it countersigned and the tree it holds, which every later push must
// extend.
//
// A witness that holds nothing for the install yet answers with a signed statement of size 0, which
// is held to every check below and then returned as ErrNothingWitnessed. Any other answer that says
// so, an unsigned 404 among them, is not the witness's word and is ErrRejected. A witness that holds
// no record under the enrollment's key is ErrRejected wrapping witness.ErrNotEnrolled, and any other
// answer that is not 200 is ErrRejected. A witness that does not answer, or answers 5xx, is
// ErrUnreachable. A statement is returned only when its signature verifies, when its signer is a
// key the enrollment lists as the witness's word at the time it was minted, and when it names the
// enrollment's producer key and label. A signer the enrollment does not list, or a statement that
// does not verify, is ErrWitnessKey, and one about another producer key or label is ErrEnrollment.
// The enrollment is not read again, so a key list refreshed on disk is not taken mid-command.
func FetchStatement(ctx context.Context, client *http.Client, enr *Enrollment) (*witness.Statement, error) {
	code, raw, err := exchange(ctx, client, http.MethodGet, enr.Witness+statementPath(enr.KeyID), nil)
	if err != nil {
		return nil, err
	}
	switch reason := answerText(raw); {
	case code == http.StatusOK:
	case code == http.StatusNotFound && strings.HasPrefix(reason, witness.ErrNotEnrolled.Error()+":"):
		return nil, fmt.Errorf("%w: %w: the witness answered the statement with %d: %s", ErrRejected,
			witness.ErrNotEnrolled, code, reason)
	default:
		return nil, fmt.Errorf("%w: the witness answered the statement with %d: %s", ErrRejected, code,
			reason)
	}
	var st witness.Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%w: the witness's statement is not one: %w", ErrWitnessKey, err)
	}
	if err := checkStatement(&st, enr); err != nil {
		return nil, err
	}
	if st.Size == 0 {
		if st.LastBeat != 0 || st.LastSeq != 0 || st.LastHead != "" || st.Root != "" {
			return nil, fmt.Errorf("%w: the statement holds a tree of 0 entries and names beat %d at "+
				"entry %d, which no witness signs", ErrWitnessKey, st.LastBeat, st.LastSeq)
		}
		return nil, fmt.Errorf("%w: the witness signed that it holds no countersigned head for "+
			"install %s yet", ErrNothingWitnessed, enr.InstallID)
	}
	return &st, nil
}

// checkStatement verifies st's signature and holds its signer and its producer to enr: the signer
// must be a key enr lists, with the same public key, as the witness's word at the time st was
// minted, which is the rule a pushed countersignature is held to, and the producer key, its id, and
// the label must be enr's.
func checkStatement(st *witness.Statement, enr *Enrollment) error {
	if _, err := witness.VerifyStatement(st); err != nil {
		return fmt.Errorf("%w: %w", ErrWitnessKey, err)
	}
	minted, err := time.Parse(time.RFC3339Nano, st.MintedAt)
	if err != nil {
		return fmt.Errorf("%w: the statement's minted_at %q is not a time", ErrWitnessKey,
			st.MintedAt)
	}
	if k, ok := enr.Keys.KeyAt(st.WitnessKeyID, minted); !ok || k.PublicKey != st.WitnessPublicKey {
		return fmt.Errorf("%w: the statement is signed by witness key %s, which this install's "+
			"enrollment does not hold as the witness's key at %s", ErrWitnessKey, st.WitnessKeyID,
			st.MintedAt)
	}
	switch {
	case st.KeyID != enr.KeyID:
		return fmt.Errorf("%w: the statement is about producer key %s, and this install is enrolled "+
			"under %s", ErrEnrollment, st.KeyID, enr.KeyID)
	case st.PublicKey != enr.PublicKey:
		return fmt.Errorf("%w: the statement's producer key is not the one this install is enrolled "+
			"under", ErrEnrollment)
	case st.InstallID != enr.InstallID:
		return fmt.Errorf("%w: the statement is about install %s, and this install is enrolled as %s",
			ErrEnrollment, st.InstallID, enr.InstallID)
	}
	return nil
}
