package imageref

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// testLogin is the registry login the cases below hand the resolver, less the host it is bound to.
var testLogin = Credentials{Username: "robot", Password: "pw-secret-value"}

// testBasic is the Authorization value testLogin makes for a basic challenge.
var testBasic = "Basic " + base64.StdEncoding.EncodeToString(
	[]byte(testLogin.Username+":"+testLogin.Password))

// logins records every Authorization value a fake server is sent.
type logins struct {
	// mu guards seen.
	mu sync.Mutex
	// seen holds the values in arrival order.
	seen []string
}

// note records req's Authorization value when it carries one.
func (l *logins) note(req *http.Request) {
	auth := req.Header.Get("Authorization")
	if auth == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, auth)
}

// all returns the recorded values.
func (l *logins) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

// transportFunc adapts a function to an http.RoundTripper.
type transportFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// hostOf returns the host and port a test server listens on.
func hostOf(srv *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
}

// basicRegistry starts a registry that asks for a basic login and serves the digest once it has
// one, recording each login it is sent.
func basicRegistry(t *testing.T) (*httptest.Server, *logins) {
	t.Helper()
	got := &logins{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		got.note(req)
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// TestSameRegistryComparesHosts covers which two host names are one registry.
func TestSameRegistryComparesHosts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// A and B are the two hosts.
		A, B string
		// WantSame reports whether they are one registry.
		WantSame bool
	}{{ // Test 0: The same host.
		A: "ghcr.io", B: "ghcr.io", WantSame: true,
	}, { // Test 1: Case does not matter.
		A: "GHCR.io", B: "ghcr.IO", WantSame: true,
	}, { // Test 2: Docker Hub's API host is Docker Hub.
		A: "docker.io", B: "registry-1.docker.io", WantSame: true,
	}, { // Test 3: Docker Hub's index host is Docker Hub.
		A: "index.docker.io", B: "docker.io", WantSame: true,
	}, { // Test 4: A port is part of the host.
		A: "registry.example.com", B: "registry.example.com:5000",
	}, { // Test 5: Two ports on one name are two registries.
		A: "registry.example.com:5000", B: "registry.example.com:5001",
	}, { // Test 6: A longer name that starts the same is another host.
		A: "ghcr.io", B: "ghcr.io.attacker.example",
	}, { // Test 7: A name that starts like Docker Hub is not Docker Hub.
		A: "docker.io", B: "docker.io.attacker.example",
	}, { // Test 8: No host matches nothing, itself included.
		A: "", B: "",
	}, { // Test 9: One side empty.
		A: "", B: "ghcr.io",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := SameRegistry(test.A, test.B); got != test.WantSame {
				t.Errorf("SameRegistry(%q, %q) = %t, want %t", test.A, test.B, got, test.WantSame)
			}
			if got := SameRegistry(test.B, test.A); got != test.WantSame {
				t.Errorf("SameRegistry(%q, %q) = %t, want %t", test.B, test.A, got, test.WantSame)
			}
		})
	}
}

// TestRegistryHostNamesWhereALoginWouldGo covers the host that serves a reference's registry API.
func TestRegistryHostNamesWhereALoginWouldGo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the image reference.
		In string
		// WantHost is the registry host.
		WantHost string
		// Want is the error, nil for a reference that parses.
		Want error
	}{{ // Test 0: A reference with no registry is served by Docker Hub.
		In: "alpine", WantHost: "registry-1.docker.io",
	}, { // Test 1: A reference that names docker.io is served by Docker Hub's API host.
		In: "docker.io/team/runner:2", WantHost: "registry-1.docker.io",
	}, { // Test 2: A named registry.
		In: "ghcr.io/org/runner:2", WantHost: "ghcr.io",
	}, { // Test 3: A named registry with a port.
		In: "registry.example.com:5000/x:1", WantHost: "registry.example.com:5000",
	}, { // Test 4: A reference pinned by digest.
		In: "ghcr.io/org/runner@" + testDigest, WantHost: "ghcr.io",
	}, { // Test 5: An empty reference is refused.
		In: "", Want: ErrReference,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := RegistryHost(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("RegistryHost(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if got != test.WantHost {
				t.Errorf("RegistryHost(%q) = %q, want %q", test.In, got, test.WantHost)
			}
		})
	}
}

// TestALoginLeavesOnlyForItsBoundRegistry pins the host binding. A registry that asks for a basic
// login is sent the run's login only when the login is bound to that registry's host. Bound to any
// other host, or to none, the registry is sent nothing and the lookup fails as an anonymous one.
func TestALoginLeavesOnlyForItsBoundRegistry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Bound is the host the login is bound to, where {registry} stands for the fake registry.
		Bound string
		// WantSent is every login the registry is sent.
		WantSent []string
		// Want is the error, nil when the tag resolves.
		Want error
	}{{ // Test 0: Bound to the registry the image names, the login is sent.
		Name: "bound here", Bound: "{registry}", WantSent: []string{testBasic},
	}, { // Test 1: Bound to another registry, nothing is sent.
		Name: "bound elsewhere", Bound: "registry.example.com", Want: ErrResolve,
	}, { // Test 2: Bound to no registry, nothing is sent.
		Name: "bound nowhere", Bound: "", Want: ErrResolve,
	}, { // Test 3: Bound to the same address on another port, nothing is sent.
		Name: "another port", Bound: "127.0.0.1:1", Want: ErrResolve,
	}, { // Test 4: Bound to the same address with no port, nothing is sent.
		Name: "no port", Bound: "127.0.0.1", Want: ErrResolve,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			srv, got := basicRegistry(t)
			host := hostOf(srv)
			creds := testLogin
			creds.Host = strings.ReplaceAll(test.Bound, "{registry}", host)
			r := NewResolver(srv.Client(), 0)
			_, err := r.Digest(context.Background(), host+"/team/runner:2", creds)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Digest() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantSent, got.all(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("logins the registry was sent mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// realmCase is one arrangement of a registry, a token realm, and a login.
type realmCase struct {
	// Name labels the case.
	Name string
	// ViaRedirect asks a second host that redirects to the registry, so the challenge comes from a
	// host the login is not bound to.
	ViaRedirect bool
	// BoundElsewhere binds the login to a registry other than the one asked.
	BoundElsewhere bool
	// PlainRealm makes the registry name a token realm served over http.
	PlainRealm bool
	// WantSent is every login the token realm is sent.
	WantSent []string
	// Want is the error, nil when the tag resolves.
	Want error
}

// TestATokenRealmGetsTheLoginOnlyFromItsBoundRegistry pins who may send the login to a token realm.
// The realm is sent the login only when the registry the login is bound to named that realm in its
// own challenge and the realm is https.
func TestATokenRealmGetsTheLoginOnlyFromItsBoundRegistry(t *testing.T) {
	t.Parallel()
	tests := []realmCase{{ // Test 0: The bound registry names an https realm, which is sent the login.
		Name: "bound registry", WantSent: []string{testBasic},
	}, { // Test 1: A registry the login is not bound to names the realm, which is sent nothing.
		Name: "bound elsewhere", BoundElsewhere: true, Want: ErrResolve,
	}, { // Test 2: The bound host redirects, and the host it redirects to names the realm.
		Name: "challenge after a redirect", ViaRedirect: true, Want: ErrResolve,
	}, { // Test 3: The bound registry names an http realm, which is refused before anything is sent.
		Name: "http realm", PlainRealm: true, Want: ErrResolve,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := lookupThroughRealm(t, test)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Digest() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantSent, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("logins the token realm was sent mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// lookupThroughRealm runs one lookup for test against a registry that demands a bearer token, and
// returns every login the token realm was sent with the lookup's error.
func lookupThroughRealm(t *testing.T, test realmCase) ([]string, error) {
	t.Helper()
	got := &logins{}
	realmHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got.note(req)
		if _, _, ok := req.BasicAuth(); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"t0k"}`))
	})
	start := httptest.NewTLSServer
	if test.PlainRealm {
		start = httptest.NewServer
	}
	realm := start(realmHandler)
	t.Cleanup(realm.Close)
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer t0k" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm.URL+`/token",service="registry.test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	t.Cleanup(registry.Close)
	asked := registry
	if test.ViaRedirect {
		asked = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, registry.URL+req.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(asked.Close)
	}
	creds := testLogin
	creds.Host = hostOf(asked)
	if test.BoundElsewhere {
		creds.Host = "registry.example.com"
	}
	r := NewResolver(registry.Client(), 0)
	_, err := r.Digest(context.Background(), hostOf(asked)+"/team/runner:2", creds)
	return got.all(), err
}

// TestALoginDoesNotFollowARedirectOffItsAddress pins the redirect rule. The bound registry takes
// the login and redirects the request to another port on its own host name. The standard client
// would carry the login there, because it compares host names without their ports. The resolver
// follows the redirect and leaves the login behind.
func TestALoginDoesNotFollowARedirectOffItsAddress(t *testing.T) {
	t.Parallel()
	elsewhere := &logins{}
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		elsewhere.note(req)
		w.Header().Set("Docker-Content-Digest", testDigest)
	}))
	t.Cleanup(other.Close)
	here := &logins{}
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		here.note(req)
		http.Redirect(w, req, other.URL+req.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(registry.Close)
	creds := testLogin
	creds.Host = hostOf(registry)
	r := NewResolver(registry.Client(), 0)
	got, err := r.Digest(context.Background(), hostOf(registry)+"/team/runner:2", creds)
	if err != nil || got != testDigest {
		t.Fatalf("Digest() = %q, %v, want the digest the redirect target serves", got, err)
	}
	if diff := cmp.Diff([]string{testBasic}, here.all()); diff != "" {
		t.Errorf("logins the bound registry was sent mismatch (-want +got):\n%s", diff)
	}
	if sent := elsewhere.all(); len(sent) != 0 {
		t.Errorf("the redirect target was sent %d login(s), want none", len(sent))
	}
}

// TestARegistryThatDoesNotAnswerGetsOneFixedError pins what a submitter is told when a request gets
// no answer. Whatever the transport's reason, a refused address, a refused connection, or a failed
// handshake, the error reads the same, and the reason is kept apart for the server's log.
func TestARegistryThatDoesNotAnswerGetsOneFixedError(t *testing.T) {
	t.Parallel()
	const wantText = "image tag not resolved to a digest: the registry did not answer"
	tests := []struct {
		// Reason is the transport's own error text.
		Reason string
	}{
		{Reason: `resolved address "127.0.0.1" is this server itself`}, // Test 0: A refused address.
		{Reason: `resolved address "169.254.169.254" is not allowed`},  // Test 1: A metadata address.
		{Reason: "connect: connection refused"},                        // Test 2: A closed port.
		{Reason: "tls: failed to verify certificate"},                  // Test 3: A failed handshake.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New(test.Reason)
			})}
			_, err := NewResolver(client, 0).Digest(context.Background(), "registry.example.com/x:1",
				Credentials{})
			if !errors.Is(err, ErrResolve) {
				t.Fatalf("Digest() error = %v, want ErrResolve", err)
			}
			if err.Error() != wantText {
				t.Errorf("Digest() error text = %q, want %q", err.Error(), wantText)
			}
			if detail := NoAnswerDetail(err); !strings.Contains(detail, test.Reason) {
				t.Errorf("NoAnswerDetail() = %q, want it to carry %q", detail, test.Reason)
			}
		})
	}
	if detail := NoAnswerDetail(ErrResolve); detail != "" {
		t.Errorf("NoAnswerDetail() of an error that is not a missing answer = %q, want empty", detail)
	}
}
