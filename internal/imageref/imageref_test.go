package imageref

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// testDigest is a well-formed digest the fake registries serve.
var testDigest = "sha256:" + strings.Repeat("c", 64)

// TestParseReadsReferencesTheWayTheRuntimeDoes covers the shapes an image reference takes.
func TestParseReadsReferencesTheWayTheRuntimeDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the reference.
		In string
		// WantRef is how it parses.
		WantRef Ref
		// Want is the error, nil for a reference that parses.
		Want error
	}{{ // Test 0: A Docker Hub official image takes the library path and the latest tag.
		In:      "alpine",
		WantRef: Ref{Domain: "docker.io", Path: "library/alpine", Tag: "latest", Name: "alpine"},
	}, { // Test 1: A registry with a port, a nested path, and a tag.
		In: "registry.example.com:5000/team/runner:2.1",
		WantRef: Ref{Domain: "registry.example.com:5000", Path: "team/runner", Tag: "2.1",
			Name: "registry.example.com:5000/team/runner"},
	}, { // Test 2: A reference pinned by digest keeps its tag beside it.
		In: "ghcr.io/org/runner:2@" + testDigest,
		WantRef: Ref{Domain: "ghcr.io", Path: "org/runner", Tag: "2", Digest: testDigest,
			Name: "ghcr.io/org/runner"},
	}, { // Test 3: localhost is a registry domain although it has no dot.
		In:      "localhost/runner",
		WantRef: Ref{Domain: "localhost", Path: "runner", Tag: "latest", Name: "localhost/runner"},
	}, { // Test 4: A digest that is not sha256 is refused.
		In: "runner@md5:abc", Want: ErrReference,
	}, { // Test 5: A dangling tag separator is refused.
		In: "runner:", Want: ErrReference,
	}, { // Test 6: An empty reference is refused.
		In: "", Want: ErrReference,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Parse(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if test.Want != nil {
				return
			}
			if diff := cmp.Diff(test.WantRef, got); diff != "" {
				t.Errorf("Parse(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestWithDigestPinsATagAndKeepsIt covers writing a resolved digest onto a reference.
func TestWithDigestPinsATagAndKeepsIt(t *testing.T) {
	t.Parallel()
	got, err := WithDigest("registry.example.com/runner:2", testDigest)
	if err != nil {
		t.Fatalf("WithDigest() error = %v", err)
	}
	if want := "registry.example.com/runner:2@" + testDigest; got != want {
		t.Errorf("WithDigest() = %q, want %q", got, want)
	}
	if !Pinned(got) || DigestOf(got) != testDigest {
		t.Errorf("the pinned reference does not read as pinned: %q", got)
	}
	if _, err := WithDigest("runner:2", "sha256:short"); !errors.Is(err, ErrReference) {
		t.Errorf("WithDigest() accepted a malformed digest: %v", err)
	}
}

// fakeRegistry serves the distribution API for one repository: a manifest HEAD and GET, and when
// Token is set, a bearer challenge whose token service checks the login.
type fakeRegistry struct {
	// Token is the bearer token the registry demands, empty for an anonymous registry.
	Token string
	// User and Password are the login the token service accepts.
	User, Password string
	// NoDigestHeader makes the registry answer without Docker-Content-Digest.
	NoDigestHeader bool
	// Manifest is the manifest body served.
	Manifest string
	// heads counts manifest requests.
	heads atomic.Int64
}

// serve answers the registry and token endpoints. The token service shares the server.
func (f *fakeRegistry) serve(base *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			user, pass, ok := r.BasicAuth()
			if !ok || user != f.User || pass != f.Password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("scope") != "repository:team/runner:pull" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{"token":%q}`, f.Token)
		case "/v2/team/runner/manifests/2":
			f.heads.Add(1)
			if f.Token != "" && r.Header.Get("Authorization") != "Bearer "+f.Token {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+*base+`/token",service="registry.test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !f.NoDigestHeader {
				w.Header().Set("Docker-Content-Digest", testDigest)
			}
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(f.Manifest))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// TestResolverPinsATagToTheDigestTheRegistryServes covers resolution against a registry that answers
// anonymously, one that demands a bearer token for a login, and one that sends no digest header, and
// the failures that leave a run bound to its tag.
func TestResolverPinsATagToTheDigestTheRegistryServes(t *testing.T) {
	t.Parallel()
	const manifest = `{"schemaVersion":2}`
	bodySum := sha256.Sum256([]byte(manifest))
	bodyDigest := "sha256:" + hex.EncodeToString(bodySum[:])
	tests := []struct {
		// Name labels the case.
		Name string
		// Registry is the fake the resolver asks, nil for one that does not exist.
		Registry *fakeRegistry
		// Repo is the repository and tag asked for.
		Repo string
		// Creds is the login the run names.
		Creds Credentials
		// WantDigest is the digest resolved.
		WantDigest string
		// Want is the error, nil when the tag resolves.
		Want error
	}{{ // Test 0: An anonymous registry answers with the digest header.
		Name: "anonymous", Registry: &fakeRegistry{}, Repo: "team/runner:2", WantDigest: testDigest,
	}, { // Test 1: A registry demanding a token issues one for the run's login.
		Name:     "bearer token",
		Registry: &fakeRegistry{Token: "t0k", User: "robot", Password: "pw-secret-value"},
		Repo:     "team/runner:2", Creds: Credentials{Username: "robot", Password: "pw-secret-value"},
		WantDigest: testDigest,
	}, { // Test 2: A registry that sends no digest header has its manifest digested.
		Name: "no header", Registry: &fakeRegistry{NoDigestHeader: true, Manifest: manifest},
		Repo: "team/runner:2", WantDigest: bodyDigest,
	}, { // Test 3: A wrong login is refused and the run stays bound to its tag.
		Name:     "wrong login",
		Registry: &fakeRegistry{Token: "t0k", User: "robot", Password: "right"},
		Repo:     "team/runner:2", Creds: Credentials{Username: "robot", Password: "pw-secret-value"},
		Want: ErrResolve,
	}, { // Test 4: An unknown tag is refused.
		Name: "unknown tag", Registry: &fakeRegistry{}, Repo: "team/runner:9", Want: ErrResolve,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var base string
			srv := httptest.NewTLSServer(test.Registry.serve(&base))
			t.Cleanup(srv.Close)
			base = srv.URL
			host := strings.TrimPrefix(srv.URL, "https://")
			r := NewResolver(srv.Client(), 0)
			got, err := r.Digest(context.Background(), host+"/"+test.Repo, test.Creds)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Digest() error = %v, want %v", err, test.Want)
			}
			if err != nil && strings.Contains(err.Error(), test.Creds.Password) && test.Creds.Password != "" {
				t.Errorf("the error quotes the registry password: %v", err)
			}
			if got != test.WantDigest {
				t.Errorf("Digest() = %q, want %q", got, test.WantDigest)
			}
		})
	}
}

// TestResolverReportsAnUnreachableRegistry pins that a registry that does not answer leaves the run
// bound to its tag with ErrResolve rather than failing the submission some other way, and that a
// reference already pinned is never sent anywhere.
func TestResolverReportsAnUnreachableRegistry(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	host := strings.TrimPrefix(srv.URL, "https://")
	client := srv.Client()
	srv.Close()
	r := NewResolver(client, 0)
	if _, err := r.Digest(context.Background(), host+"/team/runner:2", Credentials{}); !errors.Is(err, ErrResolve) {
		t.Errorf("Digest() of an unreachable registry error = %v, want ErrResolve", err)
	}
	got, err := r.Digest(context.Background(), host+"/team/runner:2@"+testDigest, Credentials{})
	if err != nil || got != testDigest {
		t.Errorf("Digest() of a pinned reference = %q, %v, want its digest without a request", got, err)
	}
}

// hangingRegistry is a registry that accepts each request and never answers it, counting them.
func hangingRegistry(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var asked atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		asked.Add(1)
		<-req.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

// TestADeadRegistryIsAskedAgainOnlyAfterAMinute pins the negative cache. A registry that does not
// answer within the timeout is left alone for DefaultDownFor, so the lookups in that minute fail at
// once without a request, and the first lookup after it asks again.
func TestADeadRegistryIsAskedAgainOnlyAfterAMinute(t *testing.T) {
	t.Parallel()
	srv, asked := hangingRegistry(t)
	ref := strings.TrimPrefix(srv.URL, "https://") + "/team/runner:2"
	clock := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := NewResolver(srv.Client(), 100*time.Millisecond)
	r.now = func() time.Time { return clock }
	steps := []struct {
		// Name labels the step.
		Name string
		// Advance moves the clock before the lookup.
		Advance time.Duration
		// WantAsked is how many requests the registry has seen after the lookup.
		WantAsked int64
	}{{ // Step 0: The first lookup waits out the timeout.
		Name: "first lookup", WantAsked: 1,
	}, { // Step 1: A lookup inside the minute sends nothing.
		Name: "inside the minute", Advance: 59 * time.Second, WantAsked: 1,
	}, { // Step 2: Once the minute has passed, the registry is asked again.
		Name: "after the minute", Advance: 2 * time.Second, WantAsked: 2,
	}}
	for stepNum, step := range steps {
		clock = clock.Add(step.Advance)
		if _, err := r.Digest(context.Background(), ref, Credentials{}); !errors.Is(err, ErrResolve) {
			t.Errorf("step %d %s: Digest() error = %v, want ErrResolve", stepNum, step.Name, err)
		}
		if got := asked.Load(); got != step.WantAsked {
			t.Errorf("step %d %s: the registry was asked %d times, want %d", stepNum, step.Name, got,
				step.WantAsked)
		}
	}
}

// TestALookupEndsWithTheRequest pins the lookup to the context it is given. When the request ends,
// the lookup ends with it, well before the resolver's own timeout, and wraps the request's error. A
// registry cut off that way was never given its timeout, so it does not count as down and the next
// lookup asks it again. A request that ended before the lookup sends nothing at all.
func TestALookupEndsWithTheRequest(t *testing.T) {
	t.Parallel()
	srv, asked := hangingRegistry(t)
	ref := strings.TrimPrefix(srv.URL, "https://") + "/team/runner:2"
	r := NewResolver(srv.Client(), 3*time.Second)
	for attempt := int64(1); attempt <= 2; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := r.Digest(ctx, ref, Credentials{})
		cancel()
		if !errors.Is(err, ErrResolve) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("attempt %d: Digest() error = %v, want ErrResolve wrapping the request's deadline",
				attempt, err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("attempt %d: the lookup took %s, past the request's end", attempt, elapsed)
		}
		if got := asked.Load(); got != attempt {
			t.Errorf("attempt %d: the registry was asked %d times, want %d", attempt, got, attempt)
		}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Digest(ended, ref, Credentials{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Digest() with an ended request error = %v, want context.Canceled", err)
	}
	if got := asked.Load(); got != 2 {
		t.Errorf("a request that had already ended reached the registry: asked %d times, want 2", got)
	}
}
