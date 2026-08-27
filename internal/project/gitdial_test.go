package project

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

// TestGitCloneRefusesLoopbackAtTheDial pins that the dial-time guard is installed on the transport
// go-git uses, not only on the text of the URL.
//
// ValidateRepoURL reads the host as written, which settles nothing for a name: an attacker needs no
// rebinding trick, only a DNS record for a name they own pointing at 127.0.0.1, and the name passes
// every text check there is. The clone then reaches whatever answers on the loopback interface from
// the server's own network position. The refusal has to happen once the address is resolved, and this
// proves it does: the URL here is an ordinary https URL, and the dial is what stops it.
func TestGitCloneRefusesLoopbackAtTheDial(t *testing.T) {
	// Not parallel: it asserts on process-wide transport registration.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A git-looking answer, so a clone that got through would fail on content rather than dialing.
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n0000"))
	}))
	defer srv.Close()

	// httptest serves http; the guard is installed for https, so drive the same loopback address
	// through the scheme the guard covers. The dial must fail before TLS is even attempted.
	url := strings.Replace(srv.URL, "http://", "https://", 1) + "/repo.git"
	_, err := git.PlainClone(t.TempDir(), false, &git.CloneOptions{URL: url})
	if err == nil {
		t.Fatal("a clone of a loopback address succeeded, so the dial is unguarded")
	}
	if !strings.Contains(err.Error(), "this server itself") {
		t.Errorf("the clone failed for some other reason than the dial guard: %v", err)
	}
}
