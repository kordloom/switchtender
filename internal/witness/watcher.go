package witness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// Watcher watches one server's beat feed, keeping its signed memory in one state file. The single
// witness command and the hosted witness service both check through it, so there is exactly one
// definition of what a watch does: load the pinned checkpoint, fetch the feed, hold the two
// against each other, and save what was seen.
type Watcher struct {
	// server is the watched base URL, normalized.
	server string
	// state is the checkpoint path.
	state string
	// id signs the checkpoint and pins its signer.
	id audit.Identity
	// client fetches the feed.
	client *http.Client
}

// NewWatcher returns a watcher for one server. A nil client gets a 30 second timeout, since a
// witness that can hang forever on one fetch is a witness that stops watching.
func NewWatcher(server, statePath string, id audit.Identity, client *http.Client) *Watcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Watcher{server: NormalizeServer(server), state: statePath, id: id, client: client}
}

// Server returns the watched base URL, normalized.
func (w *Watcher) Server() string { return w.server }

// CheckOnce runs one watch cycle and returns the findings it raised. The checkpoint's signer is
// pinned to this witness's key, so a replaced state file is refused rather than believed.
func (w *Watcher) CheckOnce(ctx context.Context) ([]Finding, error) {
	prev, err := Load(w.state, w.id.PublicKeyHex())
	if err != nil {
		return nil, err
	}
	beats, err := w.fetchBeats(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch beats: %w", err)
	}
	next, findings, err := Check(prev, w.server, beats, time.Now())
	if err != nil {
		return nil, err
	}
	if err := Save(w.state, next, w.id); err != nil {
		return findings, err
	}
	return findings, nil
}

// Checkpoint returns the watcher's current signed memory, nil before the first successful watch.
func (w *Watcher) Checkpoint() (*Checkpoint, error) {
	return Load(w.state, w.id.PublicKeyHex())
}

// fetchBeats reads the unauthenticated beat feed. The limit asked for is the number the witness
// can remember: asking for more would serve beats it forgets, and a forgotten beat's rewrite is
// re-adopted rather than reported.
func (w *Watcher) fetchBeats(ctx context.Context) ([]Beat, error) {
	feed := fmt.Sprintf("%s/v1/audit/beats?limit=%d", w.server, FeedLimit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed, nil)
	if err != nil {
		return nil, err
	}
	res, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the feed answered %s", res.Status)
	}
	var beats []Beat
	if err := json.NewDecoder(res.Body).Decode(&beats); err != nil {
		return nil, fmt.Errorf("parse the feed: %w", err)
	}
	return beats, nil
}

// BlindEdge turns a stream of check errors into findings on the edges of an outage: one
// witness_blind when checking starts failing, one witness_seeing when it recovers. Alerting on
// every failed poll trains the reader to mute the channel the real finding arrives on.
type BlindEdge struct {
	// blind is whether the last check failed.
	blind bool
}

// Observe folds one check result and returns a finding when the state changed, nil otherwise.
func (b *BlindEdge) Observe(err error) *Finding {
	if err != nil {
		if b.blind {
			return nil
		}
		b.blind = true
		return &Finding{Kind: "witness_blind",
			Detail: "the witness could not check this server: " + err.Error()}
	}
	if !b.blind {
		return nil
	}
	b.blind = false
	return &Finding{Kind: "witness_seeing", Detail: "the witness can check this server again"}
}

// Blind reports whether the last observed check failed.
func (b *BlindEdge) Blind() bool { return b.blind }

// Delta suppresses findings already raised by the previous poll, so a condition that persists is
// reported when it appears and again when it changes, not once per interval. A chain truncation is
// one event; sixty polls against it are not sixty events, and a findings total that counted them
// as such would overstate what the witness saw. Only successful polls should be folded: a failed
// poll observed nothing, and clearing the memory on one would re-report a standing condition
// after every blip.
type Delta struct {
	// prev keys the previous poll's findings by kind and detail.
	prev map[string]struct{}
}

// Fresh returns the findings the previous poll did not raise, and remembers this poll's set.
func (d *Delta) Fresh(findings []Finding) []Finding {
	next := make(map[string]struct{}, len(findings))
	var fresh []Finding
	for _, f := range findings {
		key := f.Kind + "\n" + f.Detail
		next[key] = struct{}{}
		if _, seen := d.prev[key]; !seen {
			fresh = append(fresh, f)
		}
	}
	d.prev = next
	return fresh
}

// StateFileName is the checkpoint file for a server: its host for a human reading the directory,
// and a hash of the full normalized URL so two servers can never share a file, whatever their
// spelling. One state file per server is a checked invariant, because a checkpoint held against
// another server's feed invents findings and overwrites the memory that would catch a real
// rewrite.
func StateFileName(server string) string {
	normalized := NormalizeServer(server)
	sum := sha256.Sum256([]byte(normalized))
	host := normalized
	if u, err := url.Parse(normalized); err == nil && u.Host != "" {
		host = u.Host
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String() + "-" + hex.EncodeToString(sum[:4]) + ".json"
}

// ServerKey is how a watched server is addressed in the hosted witness API: its state file name
// without the extension, deterministic from the URL alone.
func ServerKey(server string) string {
	return strings.TrimSuffix(StateFileName(server), ".json")
}
