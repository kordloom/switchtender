package policy

import "github.com/kordloom/switchtender/internal/run"

// accountOf returns the username of the account r was requested under, and whether that is known.
//
// A run carries the account it was requested under, and every run derived from the request copies
// it. One requested through a bound credential that does not carry the name, because the credential
// named an account id and the name was lost on the way, or an agent's run with no account at all,
// is unknown rather than unowned: nothing can say it was not the account a rule names. A run no
// account stands behind, such as a schedule or a webhook, is known to have none.
func accountOf(r *run.Run) (string, bool) {
	switch {
	case r.Account != "":
		return r.Account, true
	case r.ActorUserID != "" || AgentRequested(r):
		return "", false
	default:
		return "", true
	}
}

// matchesAccount reports whether the policy's account criterion matches r. A run whose account is
// unknown fails closed: a rule that holds or refuses covers it, and an exemption does not, so a
// name nobody can read never lets a run through and never lets one past a hold.
func (p *Policy) matchesAccount(r *run.Run) bool {
	if p.Account == "" {
		return true
	}
	account, known := accountOf(r)
	if !known {
		return !p.Exempts()
	}
	return account == p.Account
}
