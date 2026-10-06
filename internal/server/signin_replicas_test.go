package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestSignInBudgetsHoldAcrossReplicas spends a sign-in budget on one server and signs in again
// through a second server on the same database, on every backend.
//
// The limiter counted in each process's memory, so behind a load balancer every replica gave an
// address its whole budget again: with N replicas an attacker had N times the guesses at an account
// and N times the failures from one address that the limits promise. The budgets now live in the
// store's shared allowances, measured on the store's clock, so the second server refuses an address
// the first one already cut off.
func TestSignInBudgetsHoldAcrossReplicas(t *testing.T) {
	t.Parallel()
	signIn := func(h http.Handler, username string) int {
		body := fmt.Sprintf(`{"username":%q,"password":"wrong-password"}`, username)
		return hardenCall(h, "", http.MethodPost, "/v1/auth/login", body, nil).Code
	}
	tests := []struct {
		// Name labels the case.
		Name string
		// Spend signs in on replica A this many times before replica B is asked.
		Spend int
		// Distinct gives every attempt on A its own username, so only the address budget is spent.
		Distinct bool
		// WantStatus is replica B's answer to the next attempt from the same address.
		WantStatus int
		// WantBody is text B's refusal must contain.
		WantBody string
	}{{ // Test 0: Ten attempts at one account on A use up that account's window on B too.
		Name: "per account", Spend: loginWindowMax, WantStatus: http.StatusTooManyRequests,
		WantBody: "too many sign-in attempts",
	}, { // Test 1: Thirty failures from one address on A cut the address off on B too.
		Name: "per address", Spend: loginAddressMax, Distinct: true,
		WantStatus: http.StatusTooManyRequests, WantBody: "failed sign-in attempts from this address",
	}, { // Test 2: One failure leaves both budgets open, so B still checks the password.
		Name: "under budget", Spend: 1, WantStatus: http.StatusUnauthorized, WantBody: "bad credentials",
	}}
	for _, backend := range hardenBackends(true) {
		for testNum, test := range tests {
			t.Run(fmt.Sprintf("%s test %d %s", backend.Name, testNum, test.Name), func(t *testing.T) {
				t.Parallel()
				// The budgets count inside one sign-in window, so the attempts on A and the one on B
				// must all land in it. Each attempt checks a bcrypt hash, and on a machine loaded
				// enough to stretch them past the window the budget rolls over before B is asked,
				// which says nothing about replicas. Such a try is run again on a fresh database,
				// and three of them in a row fail the test by naming the load rather than a bug.
				var rec *httptest.ResponseRecorder
				for try := 1; ; try++ {
					start := time.Now()
					first, reopen := backend.Open(t)
					a, _ := hardenServer(t, first)
					b, _ := hardenServer(t, reopen())
					for i := 0; i < test.Spend; i++ {
						username := "target"
						if test.Distinct {
							username = fmt.Sprintf("sweep-%d", i)
						}
						if code := signIn(a, username); code != http.StatusUnauthorized {
							t.Fatalf("attempt %d on replica A = %d, want 401 while under budget", i, code)
						}
					}
					rec = hardenCall(b, "", http.MethodPost, "/v1/auth/login",
						`{"username":"target","password":"wrong-password"}`, nil)
					if time.Since(start) < loginWindowLength-5*time.Second {
						break
					}
					if try == 3 {
						t.Fatalf("three tries each outran the %s sign-in window, so the budget rolled "+
							"over before replica B was asked: the machine is too loaded for this test "+
							"to say anything", loginWindowLength)
					}
					t.Logf("try %d outran the %s sign-in window, trying again on a fresh database", try,
						loginWindowLength)
				}
				if diff := cmp.Diff(test.WantStatus, rec.Code); diff != "" {
					t.Errorf("replica B status mismatch (-want +got):\n%s\nbody: %s", diff,
						strings.TrimSpace(rec.Body.String()))
				}
				if !strings.Contains(rec.Body.String(), test.WantBody) {
					t.Errorf("replica B answered %q, want it to say %q",
						strings.TrimSpace(rec.Body.String()), test.WantBody)
				}
			})
		}
	}
}
