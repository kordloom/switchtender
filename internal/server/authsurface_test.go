package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRequiredRoleMap pins the whole role table the gate decides access with. Every entry here is a
// door: getting one wrong either hands management data to a viewer or locks an operator out of work
// they are supposed to do, and both failures are silent because nothing else in the stack
// re-derives the answer.
//
// The write cases below the read carve-outs matter as much as the reads. The schedule, trigger, and
// inventory-source families are lowered to operator for reads only; a method-blind lowering there
// would quietly drop every write on them from admin to operator, which is exactly the regression the
// function's own comment records.
//
//nolint:funlen // Test function.
func TestRequiredRoleMap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Method   string
		Path     string
		WantRole user.Role
	}{{ // Test 0: The audit trail is management data even to read, because a dossier quotes it.
		Name: "audit list", Method: http.MethodGet, Path: "/v1/audit", WantRole: user.RoleAdmin,
	}, { // Test 1: Everything under the trail takes the trail's role.
		Name: "audit verify", Method: http.MethodGet, Path: "/v1/audit/verify",
		WantRole: user.RoleAdmin,
	}, { // Test 2: The change register is drawn from the trail, so it is admin ground too.
		Name: "audit register", Method: http.MethodGet, Path: "/v1/audit/register",
		WantRole: user.RoleAdmin,
	}, { // Test 3: A run's evidence is operator at the gate, and the handler then decides whether
		// this is the caller's own run. A viewer is stopped here.
		Name: "run evidence", Method: http.MethodGet, Path: "/v1/runs/run_1/evidence",
		WantRole: user.RoleOperator,
	}, { // Test 4: A signed receipt quotes the same trail, so it takes the same role.
		Name: "run receipt", Method: http.MethodGet, Path: "/v1/runs/run_1/receipt",
		WantRole: user.RoleOperator,
	}, { // Test 5: The account list carries personal data, so reading it is management work.
		Name: "users list", Method: http.MethodGet, Path: "/v1/users", WantRole: user.RoleAdmin,
	}, { // Test 6: One account is the same data as the list of them.
		Name: "one user", Method: http.MethodGet, Path: "/v1/users/user_1", WantRole: user.RoleAdmin,
	}, { // Test 7: The token list is a map of every credential holding access to this install.
		Name: "tokens list", Method: http.MethodGet, Path: "/v1/tokens", WantRole: user.RoleAdmin,
	}, { // Test 8: Grants are the access map, so reading them is admin work.
		Name: "grants list", Method: http.MethodGet, Path: "/v1/grants", WantRole: user.RoleAdmin,
	}, { // Test 9: Approval policies say which changes nobody is watching.
		Name: "policies list", Method: http.MethodGet, Path: "/v1/policies", WantRole: user.RoleAdmin,
	}, { // Test 10: Organizations are the subject side of every grant.
		Name: "orgs list", Method: http.MethodGet, Path: "/v1/orgs", WantRole: user.RoleAdmin,
	}, { // Test 11: Team membership says who a grant to a team actually reaches.
		Name: "teams list", Method: http.MethodGet, Path: "/v1/teams", WantRole: user.RoleAdmin,
	}, { // Test 12: Credential types are management data even though they carry no secret.
		Name: "credential types", Method: http.MethodGet, Path: "/v1/credential-types",
		WantRole: user.RoleAdmin,
	}, { // Test 13: The doctor enumerates every object in the install by id and name.
		Name: "doctor", Method: http.MethodGet, Path: "/v1/doctor", WantRole: user.RoleAdmin,
	}, { // Test 14: Reading the configuration that runs work without a person is operator ground.
		Name: "schedules read", Method: http.MethodGet, Path: "/v1/schedules",
		WantRole: user.RoleOperator,
	}, { // Test 15: One schedule reads the same as the list.
		Name: "one schedule read", Method: http.MethodGet, Path: "/v1/schedules/sch_1",
		WantRole: user.RoleOperator,
	}, { // Test 16: Triggers read as operator for the same reason.
		Name: "triggers read", Method: http.MethodGet, Path: "/v1/triggers",
		WantRole: user.RoleOperator,
	}, { // Test 17: Inventory sources read as operator too.
		Name: "inventory sources read", Method: http.MethodGet, Path: "/v1/inventory-sources",
		WantRole: user.RoleOperator,
	}, { // Test 18: Writing a schedule stays admin. The operator lowering is for reads only, and a
		// method-blind version of it silently dropped every write on this family.
		Name: "schedule write", Method: http.MethodPut, Path: "/v1/schedules/sch_1",
		WantRole: user.RoleAdmin,
	}, { // Test 19: Creating a schedule stays admin.
		Name: "schedule create", Method: http.MethodPost, Path: "/v1/schedules",
		WantRole: user.RoleAdmin,
	}, { // Test 20: Deleting a trigger stays admin.
		Name: "trigger delete", Method: http.MethodDelete, Path: "/v1/triggers/trg_1",
		WantRole: user.RoleAdmin,
	}, { // Test 21: Writing an inventory source stays admin.
		Name: "inventory source write", Method: http.MethodPut, Path: "/v1/inventory-sources/src_1",
		WantRole: user.RoleAdmin,
	}, { // Test 22: An ordinary read that is not carved out is a viewer read.
		Name: "runs list", Method: http.MethodGet, Path: "/v1/runs", WantRole: user.RoleViewer,
	}, { // Test 23: The fleet view is a viewer read.
		Name: "fleet", Method: http.MethodGet, Path: "/v1/fleet", WantRole: user.RoleViewer,
	}, { // Test 24: HEAD is GET without a body, so it lands on the same role as the read.
		Name: "head a run", Method: http.MethodHead, Path: "/v1/runs/run_1",
		WantRole: user.RoleViewer,
	}, { // Test 25: HEAD on a carved-out read gets that read's role, not the write default.
		Name: "head the account list", Method: http.MethodHead, Path: "/v1/users",
		WantRole: user.RoleAdmin,
	}, { // Test 26: Launching work is operator ground.
		Name: "create run", Method: http.MethodPost, Path: "/v1/runs", WantRole: user.RoleOperator,
	}, { // Test 27: A pipeline is a run, so it takes the same role.
		Name: "create pipeline", Method: http.MethodPost, Path: "/v1/pipelines",
		WantRole: user.RoleOperator,
	}, { // Test 28: Stopping work is operator ground.
		Name: "cancel", Method: http.MethodPost, Path: "/v1/runs/run_1/cancel",
		WantRole: user.RoleOperator,
	}, { // Test 29: Retrying failed shards is operator ground.
		Name: "retry", Method: http.MethodPost, Path: "/v1/runs/run_1/retry",
		WantRole: user.RoleOperator,
	}, { // Test 30: Relaunching failed hosts is operator ground.
		Name: "relaunch", Method: http.MethodPost, Path: "/v1/runs/run_1/relaunch-failed",
		WantRole: user.RoleOperator,
	}, { // Test 31: Launching a template is the same as launching a run.
		Name: "template launch", Method: http.MethodPost, Path: "/v1/templates/tpl_1/launch",
		WantRole: user.RoleOperator,
	}, { // Test 32: Explaining a run is an advisory read, so a viewer may ask even though it is a
		// POST.
		Name: "explain", Method: http.MethodPost, Path: "/v1/runs/run_1/explain",
		WantRole: user.RoleViewer,
	}, { // Test 33: Asking about the fleet reads data a viewer can already see.
		Name: "ask", Method: http.MethodPost, Path: "/v1/ai/ask", WantRole: user.RoleViewer,
	}, { // Test 34: A draft feeds execution configuration, so it takes the launching role.
		Name: "draft", Method: http.MethodPost, Path: "/v1/ai/draft", WantRole: user.RoleOperator,
	}, { // Test 35: Proposing a run is operator work; releasing the held proposal stays admin.
		Name: "propose run", Method: http.MethodPost, Path: "/v1/ai/propose-run",
		WantRole: user.RoleOperator,
	}, { // Test 36: Proposing a reconcile is operator work.
		Name: "reconcile", Method: http.MethodPost, Path: "/v1/drift/reconcile",
		WantRole: user.RoleOperator,
	}, { // Test 37: Minting a stream ticket grants nothing by itself, and falling to the admin
		// default killed the live run view for every viewer and operator.
		Name: "stream ticket", Method: http.MethodPost, Path: "/v1/runs/run_1/stream-ticket",
		WantRole: user.RoleViewer,
	}, { // Test 38: Confirming your own credential works is not management.
		Name: "auth check", Method: http.MethodPost, Path: "/v1/auth/check",
		WantRole: user.RoleViewer,
	}, { // Test 39: Ending your own session acts on the credential you already hold, so every role
		// may do it. Requiring admin here left a viewer signed in with no way out.
		Name: "logout", Method: http.MethodPost, Path: "/v1/auth/logout", WantRole: user.RoleViewer,
	}, { // Test 40: Approving a held run is not in the operator list, so it falls to the admin
		// default, which is what keeps an agent from releasing its own held work.
		Name: "approve", Method: http.MethodPost, Path: "/v1/runs/run_1/approve",
		WantRole: user.RoleAdmin,
	}, { // Test 41: Rejecting a held run is admin for the same reason.
		Name: "reject", Method: http.MethodPost, Path: "/v1/runs/run_1/reject",
		WantRole: user.RoleAdmin,
	}, { // Test 42: Minting a token is admin, which keeps a capped agent token from minting a wider
		// one.
		Name: "create token", Method: http.MethodPost, Path: "/v1/tokens", WantRole: user.RoleAdmin,
	}, { // Test 43: Creating an account is admin.
		Name: "create user", Method: http.MethodPost, Path: "/v1/users", WantRole: user.RoleAdmin,
	}, { // Test 44: Creating a template is admin, the unlisted default.
		Name: "create template", Method: http.MethodPost, Path: "/v1/templates",
		WantRole: user.RoleAdmin,
	}, { // Test 45: Deleting a credential is admin.
		Name: "delete credential", Method: http.MethodDelete, Path: "/v1/credentials/cred_1",
		WantRole: user.RoleAdmin,
	}, { // Test 46: An unrouted mutation falls to admin rather than to something permissive, so a
		// route added without a role entry fails closed.
		Name: "unknown mutation", Method: http.MethodPost, Path: "/v1/something-new",
		WantRole: user.RoleAdmin,
	}, { // Test 47: The same table answers for an unversioned path, since the prefix is only
		// stripped when present.
		Name: "unversioned audit", Method: http.MethodGet, Path: "/audit", WantRole: user.RoleAdmin,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, test.Path, nil)
			if diff := cmp.Diff(test.WantRole, requiredRole(req)); diff != "" {
				t.Errorf("%s: role mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestRoleAllowsFailsClosedOnAnUnknownRole pins the ranking and, more importantly, what happens to a
// role the ranking does not recognize. An unreadable or empty role must satisfy nothing at all: if
// it ranked above viewer, a corrupted account row would be a free pass through every read.
func TestRoleAllowsFailsClosedOnAnUnknownRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Have      user.Role
		Need      user.Role
		WantAllow bool
	}{{ // Test 0: Admin satisfies admin.
		Name: "admin needs admin", Have: user.RoleAdmin, Need: user.RoleAdmin, WantAllow: true,
	}, { // Test 1: Admin outranks operator.
		Name: "admin needs operator", Have: user.RoleAdmin, Need: user.RoleOperator, WantAllow: true,
	}, { // Test 2: Admin outranks viewer.
		Name: "admin needs viewer", Have: user.RoleAdmin, Need: user.RoleViewer, WantAllow: true,
	}, { // Test 3: Operator does not reach admin.
		Name: "operator needs admin", Have: user.RoleOperator, Need: user.RoleAdmin,
		WantAllow: false,
	}, { // Test 4: Operator satisfies operator.
		Name: "operator needs operator", Have: user.RoleOperator, Need: user.RoleOperator,
		WantAllow: true,
	}, { // Test 5: Operator outranks viewer.
		Name: "operator needs viewer", Have: user.RoleOperator, Need: user.RoleViewer,
		WantAllow: true,
	}, { // Test 6: Viewer does not reach operator.
		Name: "viewer needs operator", Have: user.RoleViewer, Need: user.RoleOperator,
		WantAllow: false,
	}, { // Test 7: Viewer satisfies viewer.
		Name: "viewer needs viewer", Have: user.RoleViewer, Need: user.RoleViewer, WantAllow: true,
	}, { // Test 8: An empty role satisfies nothing, not even the lowest requirement.
		Name: "empty needs viewer", Have: user.Role(""), Need: user.RoleViewer, WantAllow: false,
	}, { // Test 9: A role nobody defined satisfies nothing either.
		Name: "made up role", Have: user.Role("superuser"), Need: user.RoleViewer, WantAllow: false,
	}, { // Test 10: A requirement nobody defined ranks zero, so any role clears it. This is why the
		// requirement side is always one of the three constants.
		Name: "unknown requirement", Have: user.RoleViewer, Need: user.Role("wizard"),
		WantAllow: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := roleAllows(test.Have, test.Need); got != test.WantAllow {
				t.Errorf("%s: roleAllows(%q, %q) = %v, want %v",
					test.Name, test.Have, test.Need, got, test.WantAllow)
			}
		})
	}
}

// TestCapAgentRoleLowersOnlyAdmin pins the ceiling an agent token runs under. An agent may launch
// and propose work but must never manage identity, access, or secrets, and must never approve its
// own held run. Every one of those is admin, so lowering admin to operator closes the whole surface
// at the door regardless of how the agent reaches the API.
func TestCapAgentRoleLowersOnlyAdmin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       user.Role
		WantRole user.Role
	}{{ // Test 0: Admin is the one role that is lowered.
		Name: "admin", In: user.RoleAdmin, WantRole: user.RoleOperator,
	}, { // Test 1: Operator is already at the ceiling and is unchanged.
		Name: "operator", In: user.RoleOperator, WantRole: user.RoleOperator,
	}, { // Test 2: A viewer is not raised to the ceiling, only capped by it.
		Name: "viewer", In: user.RoleViewer, WantRole: user.RoleViewer,
	}, { // Test 3: An unrecognized role is left as it is, and roleAllows then refuses it anyway.
		Name: "unknown", In: user.Role("wizard"), WantRole: user.Role("wizard"),
	}, { // Test 4: An empty role stays empty rather than becoming an operator.
		Name: "empty", In: user.Role(""), WantRole: user.Role(""),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantRole, capAgentRole(test.In)); diff != "" {
				t.Errorf("%s: mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestDelegatedObjectEdgeSpellings covers the path spellings the existing delegation test does not
// reach. The manage path is additive, so a match here widens access beyond the global role: an id
// returned for a shape that is not a single grantable object is a delegation reaching further than
// the grant model says it can.
func TestDelegatedObjectEdgeSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Method string
		Path   string
		WantID string
	}{{ // Test 0: A trailing slash leaves the id empty, which must be no object rather than a
		// wildcard the authorizer then looks up.
		Name: "trailing slash", Method: http.MethodPut, Path: "/v1/templates/", WantID: "",
	}, { // Test 1: A deeper path is not a single object, so a manage grant cannot reach a file tree.
		Name: "three segments", Method: http.MethodDelete, Path: "/v1/projects/proj_1/files",
		WantID: "",
	}, { // Test 2: Tokens are not grantable objects, so no grant can delegate revoking a credential.
		Name: "tokens are not grantable", Method: http.MethodDelete, Path: "/v1/tokens/tok_1",
		WantID: "",
	}, { // Test 3: Schedules are not grantable objects either.
		Name: "schedules are not grantable", Method: http.MethodPut, Path: "/v1/schedules/sch_1",
		WantID: "",
	}, { // Test 4: A POST on a single object is a sub-resource shape, never a delegable edit.
		Name: "post on an object", Method: http.MethodPost, Path: "/v1/templates/tpl_1",
		WantID: "",
	}, { // Test 5: Organizations are the subject side of a grant, not an object one can be given.
		Name: "orgs are not grantable", Method: http.MethodDelete, Path: "/v1/orgs/org_1",
		WantID: "",
	}, { // Test 6: A doubled slash still resolves to the object, because the leading slashes are
		// trimmed before the segments are counted. That widens nothing on its own: the request is a
		// mutation, so the role gate already demands admin and only a manage grant on this same id
		// gets past it, and the mux answers an uncleaned path with a redirect rather than the handler.
		Name: "doubled slash", Method: http.MethodPut, Path: "/v1//templates/tpl_1",
		WantID: "tpl_1",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, test.Path, nil)
			if diff := cmp.Diff(test.WantID, delegatedObject(req)); diff != "" {
				t.Errorf("%s: object mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestLooksLikeJWT pins how the gate decides to route a bearer credential to issuer validation
// instead of the token store. It is a shape test on purpose, so a SwitchTender token is never
// handed to the verifier and a JWT is never hashed and looked up as an opaque token.
func TestLooksLikeJWT(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		In      string
		WantJWT bool
	}{{ // Test 0: Three segments joined by two dots is the JWT shape.
		Name: "three segments", In: "aaa.bbb.ccc", WantJWT: true,
	}, { // Test 1: An opaque SwitchTender token carries no dots.
		Name: "opaque token", In: "st_abcdef0123456789", WantJWT: false,
	}, { // Test 2: One dot is not a JWT.
		Name: "one dot", In: "aaa.bbb", WantJWT: false,
	}, { // Test 3: Three dots is not a JWT either, so a JWE is not sent to the ID token verifier.
		Name: "three dots", In: "aaa.bbb.ccc.ddd", WantJWT: false,
	}, { // Test 4: An empty credential is not a JWT, and the gate refuses it before this anyway.
		Name: "empty", In: "", WantJWT: false,
	}, { // Test 5: Two dots with empty segments still reads as the JWT shape, and the verifier is
		// then the thing that refuses it.
		Name: "empty segments", In: "..", WantJWT: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := looksLikeJWT(test.In); got != test.WantJWT {
				t.Errorf("%s: looksLikeJWT(%q) = %v, want %v", test.Name, test.In, got, test.WantJWT)
			}
		})
	}
}

// TestHookPathAndAuditPathAgree pins the redaction of a webhook's secret from the audit chain. A
// trigger authenticates by a token in its path and the store keeps only that token's hash, so
// recording the raw path puts the secret back: hash-linked, unredactable without breaking the chain,
// and carried into every bundle handed to a third party.
//
// The spellings below are the ones that used to fall through: a different method, a doubled slash,
// a different case, and an encoded slash inside the token.
//
//nolint:funlen // Test function.
func TestHookPathAndAuditPathAgree(t *testing.T) {
	t.Parallel()
	const redacted = "/hooks/[redacted]"
	tests := []struct {
		Name      string
		Path      string
		WantHook  bool
		WantAudit string
	}{{ // Test 0: The ordinary delivery path is a hook and is redacted.
		Name: "plain hook", Path: "/hooks/sekret", WantHook: true, WantAudit: redacted,
	}, { // Test 1: The versioned spelling is the same hook.
		Name: "versioned hook", Path: "/v1/hooks/sekret", WantHook: true, WantAudit: redacted,
	}, { // Test 2: A doubled slash used to fall through and write the token verbatim.
		Name: "doubled slash", Path: "/hooks//sekret", WantHook: true, WantAudit: redacted,
	}, { // Test 3: A different case used to fall through the same way.
		Name: "uppercase", Path: "/HOOKS/sekret", WantHook: true, WantAudit: redacted,
	}, { // Test 4: Mixed case is redacted too.
		Name: "mixed case", Path: "/Hooks/SeKreT", WantHook: true, WantAudit: redacted,
	}, { // Test 5: A trailing slash is still a hook.
		Name: "trailing slash", Path: "/hooks/sekret/", WantHook: true, WantAudit: redacted,
	}, { // Test 6: Everything after the prefix is redacted, not just the first segment, so an
		// encoded slash inside a token cannot split it and leave the tail recorded.
		Name: "split token", Path: "/hooks/sek/ret", WantHook: true, WantAudit: redacted,
	}, { // Test 7: A path that merely starts with the word hooks is not a hook.
		Name: "hooksy path", Path: "/hooksy/thing", WantHook: false, WantAudit: "/hooksy/thing",
	}, { // Test 8: An ordinary API path is recorded as it stands.
		Name: "api path", Path: "/v1/runs", WantHook: false, WantAudit: "/v1/runs",
	}, { // Test 9: The bare prefix names no token, so there is nothing to redact.
		Name: "bare prefix", Path: "/hooks", WantHook: false, WantAudit: "/hooks",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := hookPath(test.Path); got != test.WantHook {
				t.Errorf("%s: hookPath(%q) = %v, want %v", test.Name, test.Path, got, test.WantHook)
			}
			req := httptest.NewRequest(http.MethodPost, "http://example.test"+test.Path, nil)
			req.URL.Path = test.Path
			if diff := cmp.Diff(test.WantAudit, auditPath(req)); diff != "" {
				t.Errorf("%s: audit path mismatch (-want +got):\n%s", test.Name, diff)
			}
			if strings.Contains(auditPath(req), "sekret") ||
				strings.Contains(auditPath(req), "SeKreT") {
				t.Errorf("%s: the webhook secret reached the audit path %q",
					test.Name, auditPath(req))
			}
		})
	}
}

// TestHookTokenSurvivesATraversalSpelling demonstrates a spelling of a webhook path that still
// writes the live token into the audit chain verbatim.
//
// hookPath cleans the path before matching, so a traversal segment resolves the path away from the
// hooks prefix and the redaction declines to fire. auditPath then records the raw request path,
// which still carries the token. The entry is hash-linked, cannot be redacted afterward without
// breaking the chain, and travels in every bundle handed to a third party, which is precisely the
// harm the redaction exists to prevent.
func TestHookTokenSurvivesATraversalSpelling(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "http://example.test/v1/runs", nil)
	req.URL.Path = "/hooks/live-secret/../../probe"
	if got := auditPath(req); strings.Contains(got, "live-secret") {
		t.Errorf("audit path = %q, want the webhook secret redacted out of it", got)
	}
}

// TestIsSignIn pins which unauthenticated mutations stay off the audit chain. These are reachable
// by anyone on the network, so recording each attempt let a stranger append without bound to the
// structure the integrity story rests on, and because the append is fail-closed, an unhealthy audit
// store then locked every account out of signing in.
func TestIsSignIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Path       string
		WantSignIn bool
	}{{ // Test 0: The password login.
		Name: "login", Path: "/v1/auth/login", WantSignIn: true,
	}, { // Test 1: Signing out is an authentication event too.
		Name: "logout", Path: "/v1/auth/logout", WantSignIn: true,
	}, { // Test 2: The whole OIDC handshake.
		Name: "oidc callback", Path: "/auth/oidc/callback", WantSignIn: true,
	}, { // Test 3: The SAML assertion consumer, whose omission was the worst of them: an unhealthy
		// store answered it 503, which in a SAML deployment locks every user out.
		Name: "saml acs", Path: "/v1/auth/saml/acs", WantSignIn: true,
	}, { // Test 4: The SAML metadata an administrator reads.
		Name: "saml metadata", Path: "/auth/saml/metadata", WantSignIn: true,
	}, { // Test 5: Confirming a token is not a sign-in, so it is recorded like any other mutation.
		Name: "auth check", Path: "/v1/auth/check", WantSignIn: false,
	}, { // Test 6: Reading who you are is not a sign-in.
		Name: "auth me", Path: "/v1/auth/me", WantSignIn: false,
	}, { // Test 7: An ordinary mutation is recorded.
		Name: "create run", Path: "/v1/runs", WantSignIn: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "http://example.test/", nil)
			req.URL.Path = test.Path
			if got := isSignIn(req); got != test.WantSignIn {
				t.Errorf("%s: isSignIn(%q) = %v, want %v", test.Name, test.Path, got,
					test.WantSignIn)
			}
		})
	}
}

// TestIsStreamAndStreamRunID pins the two things the ticket path depends on. isStream decides
// whether a missing Authorization header may be answered by a ticket at all, and streamRunID is the
// run the ticket is checked against, which is what stops a ticket for one run from opening another.
func TestIsStreamAndStreamRunID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Method     string
		Path       string
		WantStream bool
		WantRunID  string
	}{{ // Test 0: The versioned stream path.
		Name: "versioned", Method: http.MethodGet, Path: "/v1/runs/run_1/stream",
		WantStream: true, WantRunID: "run_1",
	}, { // Test 1: The unversioned spelling resolves the same run.
		Name: "unversioned", Method: http.MethodGet, Path: "/runs/run_1/stream",
		WantStream: true, WantRunID: "run_1",
	}, { // Test 2: A POST is not the stream, so it cannot be opened with a ticket.
		Name: "post", Method: http.MethodPost, Path: "/v1/runs/run_1/stream",
		WantStream: false, WantRunID: "run_1",
	}, { // Test 3: The events endpoint is not the stream, and the id it would derive is not a run
		// id either, so nothing about it could redeem a ticket even if it were reached.
		Name: "events", Method: http.MethodGet, Path: "/v1/runs/run_1/events",
		WantStream: false, WantRunID: "run_1/events",
	}, { // Test 4: A stream suffix outside the runs tree is not the run stream.
		Name: "other stream", Method: http.MethodGet, Path: "/v1/logs/stream", WantStream: false,
		WantRunID: "/logs",
	}, { // Test 5: A doubled slash leaves no run id at all, and redeem refuses an empty one, so this
		// spelling fails closed rather than matching some run.
		Name: "empty id", Method: http.MethodGet, Path: "/v1/runs//stream", WantStream: true,
		WantRunID: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, "http://example.test/", nil)
			req.URL.Path = test.Path
			if got := isStream(req); got != test.WantStream {
				t.Errorf("%s: isStream = %v, want %v", test.Name, got, test.WantStream)
			}
			if diff := cmp.Diff(test.WantRunID, streamRunID(req)); diff != "" {
				t.Errorf("%s: run id mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestUnauthenticatedActorNamesTheCallerClass pins the name written into the chain for a caller
// that presented no credential. The name is the only thing distinguishing a webhook delivery from a
// stranger probing the API in an entry that can never be edited afterward.
func TestUnauthenticatedActorNamesTheCallerClass(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Method   string
		Path     string
		WantName string
	}{{ // Test 0: A webhook delivery is named as one.
		Name: "hook", Method: http.MethodPost, Path: "/hooks/sekret", WantName: "webhook",
	}, { // Test 1: The versioned hook spelling is also a webhook.
		Name: "versioned hook", Method: http.MethodPost, Path: "/v1/hooks/sekret",
		WantName: "webhook",
	}, { // Test 2: A GET on a hook path is not a delivery, so it is an ordinary unauthenticated
		// caller.
		Name: "hook read", Method: http.MethodGet, Path: "/hooks/sekret",
		WantName: "unauthenticated",
	}, { // Test 3: The SAML assertion consumer is named saml.
		Name: "saml acs", Method: http.MethodPost, Path: "/v1/auth/saml/acs", WantName: "saml",
	}, { // Test 4: Anything else is simply unauthenticated.
		Name: "plain", Method: http.MethodPost, Path: "/v1/runs", WantName: "unauthenticated",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, "http://example.test/", nil)
			req.URL.Path = test.Path
			if diff := cmp.Diff(test.WantName, unauthenticatedActor(req)); diff != "" {
				t.Errorf("%s: actor mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestUploadPathAllowsOnlyTheLargerCapRoutes pins which routes are let past the ordinary body cap
// and past the audit gate's whole-body digest. A path that slips into this set is a mutation that
// can allocate twenty-six megabytes and go unrecorded by content, so the match is on the cleaned
// and lowercased path precisely so a traversal or a case trick cannot get in.
func TestUploadPathAllowsOnlyTheLargerCapRoutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Path       string
		WantUpload bool
	}{{ // Test 0: An import carries a whole export, so it gets the larger cap.
		Name: "import", Path: "/v1/import/switchtender", WantUpload: true,
	}, { // Test 1: An inbound webhook body is sized by the sender.
		Name: "hook", Path: "/hooks/sekret", WantUpload: true,
	}, { // Test 2: The match is case-insensitive.
		Name: "uppercase import", Path: "/V1/IMPORT/AWX", WantUpload: true,
	}, { // Test 3: A doubled slash still resolves to the import prefix.
		Name: "doubled slash", Path: "/v1//import/awx", WantUpload: true,
	}, { // Test 4: An ordinary mutation stays on the small cap.
		Name: "create run", Path: "/v1/runs", WantUpload: false,
	}, { // Test 5: A traversal out of the import tree does not keep the larger cap, so a caller
		// cannot dress an ordinary mutation up as an import.
		Name: "traversal out", Path: "/v1/import/../runs", WantUpload: false,
	}, { // Test 6: The bare import prefix with no format is not an import route.
		Name: "bare import", Path: "/v1/import", WantUpload: false,
	}, { // Test 7: A path that merely begins with the word is not a hook.
		Name: "hooksy", Path: "/hooksy/x", WantUpload: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := uploadPath(test.Path); got != test.WantUpload {
				t.Errorf("%s: uploadPath(%q) = %v, want %v",
					test.Name, test.Path, got, test.WantUpload)
			}
		})
	}
}

// TestActorKeysIdentifyTheSameCaller pins that the live stream limiter and the ticket limiter key on
// the same caller. The two bounds are documented as describing one caller, so if they keyed
// differently a caller could hold its ticket budget under one identity and its stream budget under
// another, and neither bound would mean what it says.
func TestActorKeysIdentifyTheSameCaller(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Actor      Actor
		HasActor   bool
		RemoteAddr string
		WantStream string
		WantTicket string
	}{{ // Test 0: An account is the strongest identity, so it wins over the credential label.
		Name: "account wins", Actor: Actor{UserID: "user_1", Name: "casey-laptop"}, HasActor: true,
		WantStream: "user:user_1", WantTicket: "user:user_1",
	}, { // Test 1: With no account, the credential label identifies the caller.
		Name: "name fallback", Actor: Actor{Name: "ci-token"}, HasActor: true,
		WantStream: "name:ci-token", WantTicket: "name:ci-token",
	}, { // Test 2: An authenticated actor with neither is counted per client address by the stream
		// limiter and as one anonymous bucket by the ticket store.
		Name: "empty actor", Actor: Actor{}, HasActor: true, RemoteAddr: "10.0.0.5:9000",
		WantStream: "addr:10.0.0.5:9000", WantTicket: "anon",
	}, { // Test 3: An install serving without authentication is still bounded per client rather
		// than only in total.
		Name: "no actor", HasActor: false, RemoteAddr: "192.0.2.9:443",
		WantStream: "addr:192.0.2.9:443", WantTicket: "anon",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_1/stream", nil)
			if test.RemoteAddr != "" {
				req.RemoteAddr = test.RemoteAddr
			}
			if test.HasActor {
				req = req.WithContext(context.WithValue(req.Context(), actorKey{}, test.Actor))
			}
			if diff := cmp.Diff(test.WantStream, actorKeyFor(req)); diff != "" {
				t.Errorf("%s: stream key mismatch (-want +got):\n%s", test.Name, diff)
			}
			if diff := cmp.Diff(test.WantTicket, ticketActorKey(test.Actor)); diff != "" {
				t.Errorf("%s: ticket key mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestSameActorComparesTheAccountFirst pins how "is this the same person" is answered when deciding
// whether a non-admin may read their own run's evidence. A person's API token and their browser
// session record different names, so comparing names alone answers the question wrongly in the
// direction that matters: it refuses the person their own evidence.
func TestSameActorComparesTheAccountFirst(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Actor     Actor
		RunActor  string
		RunUserID string
		WantSame  bool
	}{{ // Test 0: The same account through two different credentials is the same person.
		Name: "same account different label", Actor: Actor{UserID: "user_1", Name: "session casey"},
		RunActor: "casey-cli-token", RunUserID: "user_1", WantSame: true,
	}, { // Test 1: Different accounts are different people even when the labels match.
		Name: "different account same label", Actor: Actor{UserID: "user_2", Name: "agent"},
		RunActor: "agent", RunUserID: "user_1", WantSame: false,
	}, { // Test 2: With no account on either side, the credential label is all there is.
		Name: "label only", Actor: Actor{Name: "cli-admin"}, RunActor: "cli-admin", WantSame: true,
	}, { // Test 3: A different label with no accounts is a different caller.
		Name: "different label", Actor: Actor{Name: "cli-admin"}, RunActor: "other", WantSame: false,
	}, { // Test 4: An empty label matches nothing, so a run with no recorded actor is not everyone's.
		Name: "empty both", Actor: Actor{}, RunActor: "", WantSame: false,
	}, { // Test 5: An account on the caller and none on the run falls back to the label, which does
		// not match here.
		Name: "account against unowned run", Actor: Actor{UserID: "user_1", Name: "casey"},
		RunActor: "someone-else", WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rn := &run.Run{Actor: test.RunActor, ActorUserID: test.RunUserID}
			if got := sameActor(test.Actor, rn); got != test.WantSame {
				t.Errorf("%s: sameActor = %v, want %v", test.Name, got, test.WantSame)
			}
		})
	}
}
