package witness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestWatcherRefusesFeedRedirect proves the witness does not follow a redirect from the feed. The
// watched server is the party the witness exists to check, so a feed that answers with a redirect to
// another host must not steer the witness into a request against it.
func TestWatcherRefusesFeedRedirect(t *testing.T) {
	t.Parallel()
	var otherHits int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&otherHits, 1)
		_, _ = w.Write([]byte("[]"))
	}))
	defer other.Close()

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/audit/beats", http.StatusFound)
	}))
	defer feed.Close()

	w := NewWatcher(feed.URL, filepath.Join(t.TempDir(), "state.json"), testIdentity(t), nil)
	_, _, err := w.CheckOnce(context.Background())
	if err == nil {
		t.Error("CheckOnce followed the feed's redirect and succeeded; it must refuse")
	}
	if n := atomic.LoadInt32(&otherHits); n != 0 {
		t.Errorf("the witness made %d request(s) to the redirect target host; it must make none", n)
	}
}
