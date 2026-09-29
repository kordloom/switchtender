package server

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
)

// recordSignIn writes a successful sign-in to the audit trail, and does not fail the sign-in when it
// cannot.
//
// Sign-in is exempt from the fail-closed append that covers every other mutation, and it has to be:
// it is reachable without a credential, so recording each attempt lets a stranger append to the
// chain without bound, and a fail-closed append then answers the login itself with a 503 whenever
// the audit store is unhealthy, locking every user out of the install.
//
// Exempting the attempt is not a reason to lose the event. A sign-in that succeeded is one an
// identity provider already authenticated, so it is bounded by real accounts rather than by whoever
// can reach the port, and who signed in and when is exactly what an auditor asks for. Recording it
// here keeps it in the chain without putting the audit store in front of the door.
func recordSignIn(ctx context.Context, audits audit.Store, log *zap.Logger,
	method, username, path string) {
	if audits == nil || username == "" {
		return
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: method + ":" + username,
		Method: http.MethodPost, Path: path,
	}
	if err := audits.Append(ctx, entry); err != nil {
		// Logged rather than surfaced. Refusing the sign-in here would reintroduce the lockout this
		// exemption exists to prevent.
		log.Error("server: record sign-in: "+err.Error(), zap.String("method", method))
	}
}
