// The hosted witness: one process watching many servers from outside them, remembering what each
// feed served in a signed checkpoint, persisting every finding, and answering auditors over HTTP
// with countersigned attestations. Independence is the product: a witness the watched operator
// runs proves little, so this is built to run where they have no hand, by another team, another
// company, or a service.
package witness

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/util"
)

// findingsFile is the append-only record of everything the service has witnessed, one JSON object
// per line, kept beside the checkpoints so the archive travels as one directory.
const findingsFile = "findings.jsonl"

// findingsTail bounds how many findings an API response carries. The file keeps everything.
const findingsTail = 200

// maxDetailLen bounds a recorded finding's detail, and maxRecordLine is the longest record line
// the readers accept. The write side clips below what the read side allows, so no finding this
// service records can ever brick its own restart or its findings endpoint.
const (
	maxDetailLen  = 2048
	maxRecordLine = 1 << 20
)

// RecordedFinding is one finding as persisted and served: what the witness saw, where, and when.
type RecordedFinding struct {
	// At is when the finding was raised.
	At time.Time `json:"at"`
	// Server is the watched server the finding is about.
	Server string `json:"server"`
	// Kind and Detail are the finding itself.
	Kind string `json:"kind"`
	// Detail says what was expected and what was seen.
	Detail string `json:"detail"`
}

// ServerState is one watched server's status as the API reports it.
type ServerState struct {
	// Server is the watched base URL, normalized.
	Server string `json:"server"`
	// Key addresses the server in the API paths.
	Key string `json:"key"`
	// LastCheck is when the service last completed a check, successful or not. Zero before the
	// first one.
	LastCheck time.Time `json:"last_check"`
	// Blind is whether the last check failed, meaning the witness currently cannot see this feed.
	Blind bool `json:"blind"`
	// LastError is why the witness is blind, empty when it can see.
	LastError string `json:"last_error,omitempty"`
	// FindingsTotal counts every chain finding ever recorded for this server.
	FindingsTotal int64 `json:"findings_total"`
	// CheckpointAt, LastBeat, LastSeq, and LastHead summarize the signed memory, zero before the
	// first successful watch.
	CheckpointAt time.Time `json:"checkpoint_at,omitempty"`
	LastBeat     int64     `json:"last_beat,omitempty"`
	LastSeq      int64     `json:"last_seq,omitempty"`
	LastHead     string    `json:"last_head,omitempty"`
}

// serverStatus is the mutable runtime state behind a ServerState.
type serverStatus struct {
	// lastCheck, lastErr, and findings back the reported fields.
	lastCheck time.Time
	lastErr   string
	findings  int64
	// edge fires blind findings on outage edges rather than every poll.
	edge BlindEdge
	// delta keeps a persisting condition from being recorded once per poll.
	delta Delta
	// checkpoint is the newest signed memory this process holds for the server, loaded from disk
	// at start and refreshed by every successful poll. Attestations and listings read it here, so
	// a signed statement never pairs one poll's memory with another poll's totals.
	checkpoint *Checkpoint
}

// Service watches a set of servers and serves what it has witnessed.
type Service struct {
	// id signs every checkpoint and attestation the service produces.
	id audit.Identity
	// dir holds the checkpoints and the findings record.
	dir string
	// interval is how often every server is checked.
	interval time.Duration
	// log records service activity.
	log *zap.Logger
	// notify, when set, receives every finding, including the blind edges, so alerting rides the
	// operator's channel.
	notify func(server string, f Finding)
	// readToken, when set, is the bearer token every /witness route requires. It gates the whole
	// read surface, most importantly the two global routes that would otherwise let any anonymous
	// caller enumerate every watched server and read cross-server findings. Empty leaves the API
	// open, which is correct for a single-tenant or local run and refused for a public one by the
	// serve command. It does not weaken offline verification: a relying party checks an attestation
	// it already holds against the pinned key with no call to this service, so the token gates
	// delivery, not trust.
	readToken string
	// clock reads the time, replaced in tests.
	clock func() time.Time
	// watchers holds one watcher per server, keyed by ServerKey.
	watchers map[string]*Watcher
	// order lists keys in the order servers were given, so the API answers stably.
	order []string
	// mu guards status and the findings file.
	mu sync.Mutex
	// status holds runtime state per key.
	status map[string]*serverStatus
	// findings is the open append handle, nil until Start.
	findings *os.File
	// ctx and cancel stop the loop; wg waits for it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// ServiceOption configures a Service.
type ServiceOption func(*Service)

// WithServiceNotify delivers every finding to fn, blind edges included.
func WithServiceNotify(fn func(server string, f Finding)) ServiceOption {
	return func(s *Service) { s.notify = fn }
}

// WithServiceClock replaces the clock, so a test controls the timestamps findings carry.
func WithServiceClock(now func() time.Time) ServiceOption {
	return func(s *Service) { s.clock = now }
}

// WithServiceReadToken requires the given bearer token on every /witness route. An empty token
// leaves the API open.
func WithServiceReadToken(token string) ServiceOption {
	return func(s *Service) { s.readToken = token }
}

// NewService returns a hosted witness over the given servers. It refuses a duplicate server,
// however spelled, because two watchers over one state file take turns overwriting the memory
// that catches a rewrite. It panics on an interval under ten seconds, a programming error: the
// point of the pin is memory over time, not a busy loop.
func NewService(id audit.Identity, dir string, interval time.Duration, servers []string,
	log *zap.Logger, client *http.Client, opts ...ServiceOption) (*Service, error) {
	if dir == "" {
		panic("witness: state directory required")
	}
	if interval < 10*time.Second {
		panic("witness: interval must be at least ten seconds")
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("nothing to watch; give at least one server")
	}
	if log == nil {
		log = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		id: id, dir: dir, interval: interval, log: log, clock: time.Now,
		watchers: map[string]*Watcher{}, status: map[string]*serverStatus{},
		ctx: ctx, cancel: cancel,
	}
	for _, opt := range opts {
		opt(s)
	}
	for _, raw := range servers {
		key := ServerKey(raw)
		if _, dup := s.watchers[key]; dup {
			cancel()
			return nil, fmt.Errorf("%s is watched twice; one state file per server", NormalizeServer(raw))
		}
		s.watchers[key] = NewWatcher(raw, filepath.Join(dir, StateFileName(raw)), id, client)
		s.status[key] = &serverStatus{}
		s.order = append(s.order, key)
	}
	return s, nil
}

// Start validates the archive and launches the watch loop. The directory and the findings record
// are checked here rather than at the first finding, because a finding is the one moment the
// service exists for and the worst moment to learn the disk was never writable. Prior findings
// are recounted from the record, so a restart does not reset the totals an attestation carries.
func (s *Service) Start() error {
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return fmt.Errorf("witness state directory: %w", err)
	}
	if err := s.recount(); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, findingsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("witness findings record: %w", err)
	}
	s.findings = f
	// Prior memory is loaded before the first sweep, so a witness restarted into an outage still
	// attests what it holds on disk rather than claiming it witnessed nothing.
	for key, w := range s.watchers {
		c, err := w.Checkpoint()
		if err != nil {
			return fmt.Errorf("state for %s: %w", w.Server(), err)
		}
		s.status[key].checkpoint = c
	}
	// The first sweep runs before Start returns, so the API never answers from an empty memory
	// and a caller observing after Start sees a settled state rather than racing the loop.
	s.CheckAll(s.ctx)
	s.wg.Go(func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.CheckAll(s.ctx)
			}
		}
	})
	return nil
}

// recount restores per-server finding totals from the record, so a restarted service attests the
// same history it attested before rather than a total that quietly reset to zero.
func (s *Service) recount() error {
	f, err := os.Open(filepath.Join(s.dir, findingsFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("witness findings record: %w", err)
	}
	defer func() { _ = f.Close() }()
	byServer := map[string]int64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLine)
	for sc.Scan() {
		var rec RecordedFinding
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			byServer[rec.Server]++
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("witness findings record: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, w := range s.watchers {
		s.status[key].findings = byServer[w.Server()]
	}
	return nil
}

// sweepWorkers bounds how many servers are checked at once. A sweep that visited servers one at
// a time would stall the whole watch behind each dark target's timeout, delaying both the API and
// the detection this service exists for.
const sweepWorkers = 8

// CheckAll runs one watch cycle over every server and returns when the sweep is done.
func (s *Service) CheckAll(ctx context.Context) {
	sem := make(chan struct{}, sweepWorkers)
	var wg sync.WaitGroup
	for _, key := range s.order {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.checkOne(ctx, k)
		}(key)
	}
	wg.Wait()
}

// checkOne watches one server and folds the outcome into its status and the record.
func (s *Service) checkOne(ctx context.Context, key string) {
	w := s.watchers[key]
	c, findings, err := w.CheckOnce(ctx)
	now := s.clock()

	s.mu.Lock()
	st := s.status[key]
	st.lastCheck = now
	if c != nil {
		st.checkpoint = c
	}
	if err != nil {
		st.lastErr = err.Error()
	} else {
		st.lastErr = ""
	}
	if err == nil || len(findings) > 0 {
		// A poll that produced findings observed the chain, even when saving its checkpoint then
		// failed, so it is folded: a full disk must not turn one standing truncation into a
		// recorded finding per poll. A restart starts with an empty memory here, so a condition
		// that outlives the process is re-attested once by the process that still sees it.
		findings = st.delta.Fresh(findings)
	}
	var recorded []RecordedFinding
	for _, f := range findings {
		// Clipped here too, so the notified copy matches what the record and the API serve.
		rec := RecordedFinding{At: now, Server: w.Server(), Kind: f.Kind,
			Detail: util.Clip(f.Detail, maxDetailLen)}
		if werr := s.record(rec); werr != nil {
			// The finding still counts and still alerts; only its durability failed, and that
			// failure is loud. Dropping the count instead would make the attestation understate
			// what this witness saw.
			s.log.Error("witness: record finding: " + werr.Error())
		}
		st.findings++
		recorded = append(recorded, rec)
	}
	edge := st.edge.Observe(err)
	s.mu.Unlock()

	if err != nil {
		s.log.Error("witness: check "+w.Server()+": "+err.Error(), zap.String("server", w.Server()))
	}
	for _, rec := range recorded {
		s.log.Warn("witness: FINDING "+rec.Kind+": "+rec.Detail, zap.String("server", rec.Server))
		if s.notify != nil {
			s.notify(rec.Server, Finding{Kind: rec.Kind, Detail: rec.Detail})
		}
	}
	if edge != nil && s.notify != nil {
		s.notify(w.Server(), *edge)
	}
}

// record appends one finding to the record, clipping the detail below what the readers accept so
// no write can brick the restart that recounts it. Callers hold mu.
func (s *Service) record(rec RecordedFinding) error {
	if s.findings == nil {
		return fmt.Errorf("findings record is not open")
	}
	rec.Detail = util.Clip(rec.Detail, maxDetailLen)
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = s.findings.Write(append(line, '\n'))
	return err
}

// Close stops the loop, waits for it, and closes the record.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findings != nil {
		_ = s.findings.Close()
		s.findings = nil
	}
}

// states returns the current ServerState list, stable in the order servers were given.
func (s *Service) states() []ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServerState, 0, len(s.order))
	for _, key := range s.order {
		w, st := s.watchers[key], s.status[key]
		state := ServerState{
			Server: w.Server(), Key: key, LastCheck: st.lastCheck,
			Blind: st.edge.Blind(), LastError: st.lastErr, FindingsTotal: st.findings,
		}
		if c := st.checkpoint; c != nil {
			state.CheckpointAt = c.ObservedAt
			state.LastBeat, state.LastSeq, state.LastHead = c.LastBeat, c.LastSeq, c.LastHead
		}
		out = append(out, state)
	}
	return out
}

// tail returns the newest recorded findings, all servers or one, newest last.
func (s *Service) tail(server string) ([]RecordedFinding, error) {
	f, err := os.Open(filepath.Join(s.dir, findingsFile))
	if os.IsNotExist(err) {
		return []RecordedFinding{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []RecordedFinding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLine)
	for sc.Scan() {
		var rec RecordedFinding
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if server != "" && rec.Server != server {
			continue
		}
		out = append(out, rec)
		if len(out) > findingsTail {
			out = out[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []RecordedFinding{}
	}
	return out, nil
}

// Attest mints a signed statement of what this witness currently holds for a server: the memory,
// how much of it, every finding it has ever recorded, and whether it can see the feed right now.
// It returns nil when nothing has been witnessed yet, since an attestation of nothing is noise
// wearing a signature.
func (s *Service) Attest(key string) (*Attestation, error) {
	w, ok := s.watchers[key]
	if !ok {
		return nil, fmt.Errorf("no such watched server")
	}
	s.mu.Lock()
	st := s.status[key]
	c := st.checkpoint
	if c == nil {
		s.mu.Unlock()
		return nil, nil
	}
	a := &Attestation{
		Server: w.Server(), MintedAt: s.clock().UTC(), CheckpointAt: c.ObservedAt.UTC(),
		LastBeat: c.LastBeat, LastSeq: c.LastSeq, LastHead: c.LastHead,
		BeatsRemembered: len(c.Recent), FindingsTotal: st.findings, Blind: st.edge.Blind(),
	}
	s.mu.Unlock()
	if err := SignAttestation(a, s.id); err != nil {
		return nil, err
	}
	return a, nil
}

// Handler serves the witness API. Every route is a read: the service has no mutating surface,
// because what a witness says must come only from what it saw.
//
// When a read token is configured, every /witness route requires it as a bearer token, so a public
// deployment does not hand an anonymous caller the list of watched servers or the cross-server
// findings feed. The /healthz route stays open so a load balancer can probe it. The token is
// compared in constant time.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /witness/servers", func(w http.ResponseWriter, _ *http.Request) {
		s.respond(w, map[string]any{
			"witness": map[string]string{
				"public_key": s.id.PublicKeyHex(), "key_id": s.id.KeyID(),
			},
			"servers": s.states(),
		})
	})
	mux.HandleFunc("GET /witness/servers/{key}/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		watcher, ok := s.watchers[r.PathValue("key")]
		if !ok {
			s.fail(w, http.StatusNotFound, "no such watched server")
			return
		}
		c, err := watcher.Checkpoint()
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "checkpoint could not be read")
			s.log.Error("witness: read checkpoint: " + err.Error())
			return
		}
		if c == nil {
			s.fail(w, http.StatusNotFound, "nothing witnessed yet")
			return
		}
		s.respond(w, c)
	})
	mux.HandleFunc("GET /witness/servers/{key}/attestation", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if _, ok := s.watchers[key]; !ok {
			s.fail(w, http.StatusNotFound, "no such watched server")
			return
		}
		a, err := s.Attest(key)
		if err != nil {
			// The detail goes to the log, not to an anonymous caller on a public API.
			s.fail(w, http.StatusInternalServerError, "attestation could not be minted")
			s.log.Error("witness: attest: " + err.Error())
			return
		}
		if a == nil {
			s.fail(w, http.StatusNotFound, "nothing witnessed yet")
			return
		}
		s.respond(w, a)
	})
	mux.HandleFunc("GET /witness/servers/{key}/findings", func(w http.ResponseWriter, r *http.Request) {
		watcher, ok := s.watchers[r.PathValue("key")]
		if !ok {
			s.fail(w, http.StatusNotFound, "no such watched server")
			return
		}
		s.serveFindings(w, watcher.Server())
	})
	mux.HandleFunc("GET /witness/findings", func(w http.ResponseWriter, _ *http.Request) {
		s.serveFindings(w, "")
	})
	return s.gate(mux)
}

// gate wraps the mux so every /witness route requires the read token when one is configured, while
// /healthz and anything else stays open. With no token the handler is returned unchanged, which is
// the open single-tenant and local mode.
func (s *Service) gate(next http.Handler) http.Handler {
	if s.readToken == "" {
		return next
	}
	want := []byte("Bearer " + s.readToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/witness/") {
			got := r.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				s.fail(w, http.StatusUnauthorized, "a bearer token is required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// serveFindings answers with the newest recorded findings, filtered to one server when set.
func (s *Service) serveFindings(w http.ResponseWriter, server string) {
	recs, err := s.tail(server)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "findings record could not be read")
		s.log.Error("witness: read findings: " + err.Error())
		return
	}
	s.respond(w, map[string]any{"findings": recs, "count": len(recs)})
}

// respond writes v as JSON.
func (s *Service) respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.log.Error("witness: write response: " + err.Error())
	}
}

// fail writes a JSON error.
func (s *Service) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "{%q:%q}\n", "error", msg)
}

// Attestation is a countersigned statement of what an independent witness holds for one server.
// A relying party verifies it against the witness key they have pinned, offline, with
// "switchtender witness verify-attestation". It is the artifact the hosted witness exists to
// produce: the watched operator cannot mint one, cannot alter one, and cannot answer one that
// disagrees with the chain they serve.
type Attestation struct {
	// Server is the watched base URL, normalized.
	Server string `json:"server"`
	// MintedAt is when this statement was signed.
	MintedAt time.Time `json:"minted_at"`
	// CheckpointAt is when the memory it reports was last updated.
	CheckpointAt time.Time `json:"checkpoint_at"`
	// LastBeat, LastSeq, and LastHead are the newest beat this witness holds for the server.
	LastBeat int64  `json:"last_beat"`
	LastSeq  int64  `json:"last_seq"`
	LastHead string `json:"last_head"`
	// BeatsRemembered is how many beats the memory holds.
	BeatsRemembered int `json:"beats_remembered"`
	// FindingsTotal counts every chain finding this witness has ever recorded for the server.
	FindingsTotal int64 `json:"findings_total"`
	// Blind is whether the witness could see the feed at minting. A true here with an old
	// CheckpointAt is itself a signal: the server went dark on its witness.
	Blind bool `json:"blind"`
	// PublicKey is the hex key that signed this statement.
	PublicKey string `json:"public_key,omitempty"`
	// Sig is the hex signature over the statement with the signature fields cleared.
	Sig string `json:"sig,omitempty"`
}

// attestationContent is the canonical bytes an attestation signature covers.
func attestationContent(a *Attestation) ([]byte, error) {
	cp := *a
	cp.PublicKey, cp.Sig = "", ""
	return json.Marshal(&cp)
}

// SignAttestation stamps the statement with the witness identity.
func SignAttestation(a *Attestation, id audit.Identity) error {
	content, err := attestationContent(a)
	if err != nil {
		return fmt.Errorf("sign attestation: %w", err)
	}
	a.PublicKey, a.Sig = signBytes(id, content)
	return nil
}

// VerifyAttestation checks the statement's signature and returns the signer's hex key, which the
// relying party compares against the witness key they have pinned. Sort the pin out of band: a
// signature only proves the document is internally consistent, which a forger with their own key
// satisfies trivially.
func VerifyAttestation(a *Attestation) (publicKey string, err error) {
	content, err := attestationContent(a)
	if err != nil {
		return "", fmt.Errorf("verify attestation: %w", err)
	}
	if err := verifyBytes("attestation", a.PublicKey, a.Sig, content); err != nil {
		return "", err
	}
	return a.PublicKey, nil
}
