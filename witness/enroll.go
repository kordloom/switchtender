package witness

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
)

// An enrolled install's memory lives in its own directory under installsDir, named for the producer
// key that signs its pushes. The record is the key, never the install id: only the holder of a key
// can sign for it, so no install can occupy, push for, or block another install's memory, and the
// install id is a label the key was born to.
const (
	// installsDir is the directory under the state directory that holds one directory per install.
	installsDir = "installs"
	// enrollmentFile holds what the witness keeps of an enrollment, and nothing else.
	enrollmentFile = "enrollment.json"
	// installCheckpointFile is the install's signed memory, the tree the next push must extend.
	installCheckpointFile = "checkpoint.json"
	// headsFile is the install's head log, one countersigned coordinate per line, oldest first.
	headsFile = "heads.jsonl"
	// RestoreMarker is the file a restore leaves in the state directory. The service turns it into
	// one memory_restored note per install when it starts, then removes it, so the statement's
	// history shows that the witness's own memory rewound.
	RestoreMarker = "memory-restored"
	// LapseGrace is how long after a license term ends the witness keeps accepting pushes, so a
	// renewal that lands late costs no coverage. Past it a push is refused as lapsed.
	LapseGrace = 30 * 24 * time.Hour
	// DefaultMaxInstalls is how many installs one license may enroll unless the operator says
	// otherwise. The counted unit is a producer signing key.
	DefaultMaxInstalls = 10
	// StatementVersion tags the witness's signed statement about one install.
	StatementVersion = "witness-statement/1"
	// maxPushBody bounds a push or an enrollment body. An honest push is well under a kilobyte.
	maxPushBody = 64 << 10
)

// A recorded finding about an install is either a refusal or a note. A refusal is the witness's
// testimony that a push signed with the install's key contradicted what it remembers, and verify
// --witness requires there to be none. A note is about the witness's own coverage: where its memory
// starts, beats it never received, a rewind of its own memory, or an install gone quiet. It changes
// nothing about what was countersigned and never fails a verify.
const (
	// ClassRefusal marks a finding the witness refused a push over.
	ClassRefusal = "refusal"
	// ClassNote marks a finding about the witness's coverage.
	ClassNote = "note"
)

// noteKinds are the finding kinds that are notes. Every other kind is a refusal, including a kind
// this build has never heard of, so a reader routing on the class can never read an accusation as
// coverage information.
var noteKinds = []string{"first_root", "missing_beat", "memory_restored", "silent_install"}

// pushClass returns the class a finding of the given kind is recorded under.
func pushClass(kind string) string {
	if slices.Contains(noteKinds, kind) {
		return ClassNote
	}
	return ClassRefusal
}

// keyIDForm matches a producer key id as a bundle prints it.
var keyIDForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// recordName returns the directory name of the record for a producer key id, the key id without its
// sha256: prefix, and false when the value is not a key id at all.
func recordName(keyID string) (string, bool) {
	if !keyIDForm.MatchString(keyID) {
		return "", false
	}
	return strings.TrimPrefix(keyID, "sha256:"), true
}

// Enrollment is what the witness keeps of one enrolled install: the producer key the record is
// keyed on, the label that key was born to, and the license term that entitles it. Nothing else
// from the request is kept, so no organization, URL, contact, or address reaches the state
// directory.
type Enrollment struct {
	// KeyID is the producer key's sha256 fingerprint, the name of the record.
	KeyID string `json:"key_id"`
	// PublicKey is the producer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// InstallID is the install's label, an id the key was born to.
	InstallID string `json:"install_id"`
	// LicenseID names the license that entitles the install, empty under a policy with no license.
	LicenseID string `json:"license_id"`
	// Expires is when the license term ends, RFC 3339 in UTC, empty when there is no term.
	Expires string `json:"expires"`
	// EnrolledAt is when the witness first enrolled the install, RFC 3339 in UTC.
	EnrolledAt string `json:"enrolled_at"`
}

// Entitlement is what an enrollment policy grants an install.
type Entitlement struct {
	// LicenseID names the license that entitles the install. Installs are capped per license id,
	// and an empty one, from a policy with no license, is capped by nothing.
	LicenseID string
	// Expires is when the license term ends. The zero time means no term.
	Expires time.Time
}

// EnrollmentPolicy decides whether a verified enrollment request is entitled to a memory, and under
// which license. A policy that refuses returns an error saying why, and the witness writes nothing.
// It is injected rather than built in, so this package stays free of the product's license code and
// a company running its own push witness can choose its own rule.
type EnrollmentPolicy func(r EnrollRequest) (Entitlement, error)

// OpenEnrollment entitles every request whose signature and label verify, with no license and no
// term. It is the policy for a push witness on a private network, where who may reach the witness
// at all is the control.
func OpenEnrollment(EnrollRequest) (Entitlement, error) {
	return Entitlement{}, nil
}

// WitnessKeyOf returns the published entry for a witness identity, valid from validFrom.
func WitnessKeyOf(id identity.Identity, validFrom time.Time) WitnessKey {
	return WitnessKey{KeyID: id.KeyID(), PublicKey: id.PublicKeyBase64(),
		ValidFrom: stamp(validFrom)}
}

// headLine is one line of an install's head log: a coordinate the witness countersigned and when it
// first did.
type headLine struct {
	// At is when the witness adopted the coordinate, by its own clock.
	At time.Time `json:"at"`
	// Beat is the beat number pushed.
	Beat int64 `json:"beat"`
	// Seq is the chain position of the beat.
	Seq int64 `json:"seq"`
	// Head is the chain link at Seq.
	Head string `json:"head"`
	// Size is the tree size countersigned.
	Size int64 `json:"size"`
	// Root is the tree root at Size.
	Root string `json:"root"`
}

// sameCoordinate reports whether two lines name the same countersigned coordinate, whenever each
// was written.
func (h headLine) sameCoordinate(o headLine) bool {
	return h.Beat == o.Beat && h.Seq == o.Seq && h.Head == o.Head && h.Size == o.Size &&
		h.Root == o.Root
}

// install is the runtime state of one enrolled install. Everything but name, dir, and pub is
// guarded by mu, which also serializes the install's pushes, so two pushes never race over one
// memory. An update to rec or expires holds the service's installsMu as well, which is what lets
// the cap count read rec under installsMu alone.
type install struct {
	// mu guards the fields below it and serializes pushes.
	mu sync.Mutex
	// name is the record's directory name, the key id's hex.
	name string
	// dir is the record's directory.
	dir string
	// pub is the enrolled producer key every push must be signed by.
	pub ed25519.PublicKey
	// rec is the enrollment as written.
	rec Enrollment
	// expires is rec.Expires parsed, zero when there is no term.
	expires time.Time
	// checkpoint is the newest signed memory, nil before the first adopted push. Statements read it
	// here, so one never pairs one push's memory with another push's totals.
	checkpoint *Checkpoint
	// delta keeps a refusal that stands across pushes on one rewritten chain from being recorded
	// once per push.
	delta Delta
	// refusals and notes count every recorded finding of each class.
	refusals, notes int64
	// lastSeen is when the install last pushed something the witness held against its memory: a
	// newly adopted or a refused push. Retries and errors do not count.
	lastSeen time.Time
	// memorySince is when the witness adopted its first coordinate for this install, zero before.
	memorySince time.Time
	// lastHead is the newest line of the head log.
	lastHead headLine
	// silent is whether silent_install was recorded for the silence that has not ended yet.
	silent bool
	// gone is whether the sweep deleted the record, set under installsMu as well. A caller that
	// found the install before the deletion then answers as if it were never enrolled, rather than
	// reading a directory that is gone or that a fresh enrollment of the same key now holds.
	gone bool
}

// path returns the path of a file in the install's record.
func (in *install) path(name string) string { return filepath.Join(in.dir, name) }

// record appends findings to the install's own record in one write, so a disk error leaves none or
// all of them, and syncs it, because a refusal the witness answered with must still be on record
// after a crash.
func (in *install) record(recs []RecordedFinding) error {
	if len(recs) == 0 {
		return nil
	}
	var buf []byte
	for _, rec := range recs {
		rec.Detail = clip(rec.Detail, maxDetailLen)
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	return appendSynced(in.path(findingsFile), buf)
}

// count adds recorded findings to the install's totals by class.
func (in *install) count(recs []RecordedFinding) {
	for _, rec := range recs {
		if rec.Class == ClassNote {
			in.notes++
		} else {
			in.refusals++
		}
	}
}

// appendHead appends one coordinate to the head log and syncs it.
func (in *install) appendHead(line headLine) error {
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	return appendSynced(in.path(headsFile), append(raw, '\n'))
}

// appendSynced appends data to the file at path, creating it owner-only, and syncs it to disk.
func appendSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeEnrollment writes the enrollment record atomically, so a crash never leaves one half
// written.
func writeEnrollment(dir string, rec Enrollment) error {
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, enrollmentFile+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, enrollmentFile))
}

// stamp writes t as RFC 3339 in UTC with its fraction, or empty for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// laterTerm reports whether term a ends after term b, where the zero time is no term at all and so
// ends after every term.
func laterTerm(a, b time.Time) bool {
	switch {
	case b.IsZero():
		return false
	case a.IsZero():
		return true
	}
	return a.After(b)
}

// lookup returns the install whose producer key id is keyID, or nil.
func (s *Service) lookup(keyID string) *install {
	name, ok := recordName(keyID)
	if !ok {
		return nil
	}
	s.installsMu.RLock()
	defer s.installsMu.RUnlock()
	return s.installs[name]
}

// installList returns every enrolled install in a stable order.
func (s *Service) installList() []*install {
	s.installsMu.RLock()
	defer s.installsMu.RUnlock()
	out := make([]*install, 0, len(s.installs))
	for _, in := range s.installs {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// atCap reports whether licenseID already holds as many records as one license may, not counting
// the record named except. A record with no license is capped by nothing. Callers hold installsMu.
func (s *Service) atCap(licenseID, except string) bool {
	if licenseID == "" {
		return false
	}
	held := 0
	for name, in := range s.installs {
		if name != except && in.rec.LicenseID == licenseID {
			held++
		}
	}
	return held >= s.maxInstalls
}

// Enroll admits an install to the witness's memory, or answers exactly what it answered before.
//
// The order is the design's: the request's own signature and its born-to label first, then the
// enrollment policy, then idempotency by producer key, then the cap per license, then the write. A
// request refused at any step writes nothing. The same key enrolling again under the same label
// gets the same signed answer, with a newer license term recorded when it brings one. That holds
// for a lapsed record still in its read-only year too: a renewal reopens it with its memory, so
// nothing the witness still holds is forgotten, and only a key whose record the sweep deleted
// enrolls into a fresh memory. The same key under another label is refused, because one key keeps
// one label and one memory.
func (s *Service) Enroll(r EnrollRequest) (*EnrollResponse, error) {
	if !s.enrolling {
		return nil, fmt.Errorf("%w: this witness accepts no enrollments", ErrClosed)
	}
	if err := VerifyEnroll(&r); err != nil {
		return nil, err
	}
	ent, err := s.policy(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEntitlement, err)
	}
	now := s.clock()
	if !ent.Expires.IsZero() && now.After(ent.Expires) {
		return nil, fmt.Errorf("%w: the license term ended %s", ErrLapsed, stamp(ent.Expires))
	}
	pub, err := decodeKey(r.PublicKey)
	if err != nil {
		return nil, err
	}
	keyID := seal.KeyID(pub)
	name, _ := recordName(keyID)
	budget := ent.LicenseID
	if budget == "" {
		budget = keyID
	}
	if ok, _ := s.enrollLimit.allow(budget, now); !ok {
		return nil, fmt.Errorf("%w: too many enrollments for this license, try again later",
			ErrLimited)
	}

	s.installsMu.Lock()
	defer s.installsMu.Unlock()
	if in, ok := s.installs[name]; ok {
		in.mu.Lock()
		defer in.mu.Unlock()
		if in.rec.InstallID != r.InstallID {
			return nil, fmt.Errorf("%w: key %s is enrolled as %s, so it cannot enroll as %s", ErrLabel,
				keyID, in.rec.InstallID, clip(r.InstallID, 64))
		}
		if laterTerm(ent.Expires, in.expires) {
			if ent.LicenseID != in.rec.LicenseID && s.atCap(ent.LicenseID, name) {
				return nil, s.capError()
			}
			rec := in.rec
			rec.LicenseID, rec.Expires = ent.LicenseID, stamp(ent.Expires)
			if err := writeEnrollment(in.dir, rec); err != nil {
				return nil, fmt.Errorf("%w: enrollment record: %w", ErrWrite, err)
			}
			in.rec, in.expires = rec, ent.Expires
		}
		return s.enrolled(in.rec)
	}
	if s.atCap(ent.LicenseID, "") {
		return nil, s.capError()
	}
	rec := Enrollment{KeyID: keyID, PublicKey: r.PublicKey, InstallID: r.InstallID,
		LicenseID: ent.LicenseID, Expires: stamp(ent.Expires), EnrolledAt: stamp(now)}
	dir := filepath.Join(s.dir, installsDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("%w: install directory: %w", ErrWrite, err)
	}
	if err := writeEnrollment(dir, rec); err != nil {
		// The directory was made for this record alone, so a failed write leaves nothing behind
		// that a restart would read as a record.
		_ = os.Remove(dir)
		return nil, fmt.Errorf("%w: enrollment record: %w", ErrWrite, err)
	}
	s.installs[name] = &install{name: name, dir: dir, pub: pub, rec: rec, expires: ent.Expires,
		lastSeen: now}
	return s.enrolled(rec)
}

// capError names the cap a refused enrollment ran into.
func (s *Service) capError() error {
	return fmt.Errorf("%w: this license has enrolled %d %s, as many as one license may", ErrCap,
		s.maxInstalls, plural(s.maxInstalls, "install", "installs"))
}

// enrolled returns the witness's signed answer for an enrollment record. Every member comes from
// the record and the witness's configuration, never from the request or the clock, so asking again
// gets the same bytes.
func (s *Service) enrolled(rec Enrollment) (*EnrollResponse, error) {
	keys, err := s.keyDocument()
	if err != nil {
		return nil, err
	}
	resp := &EnrollResponse{Witness: *keys, InstallID: rec.InstallID, KeyID: rec.KeyID,
		PublicKey: rec.PublicKey, EnrolledAt: rec.EnrolledAt, MaxInstalls: s.maxInstalls,
		PushPath: "/v1/installs/" + rec.KeyID + "/beats"}
	if err := SignEnrollResponse(resp, s.id); err != nil {
		return nil, err
	}
	return resp, nil
}

// keyDocument returns the witness's signed key list, or nil when it publishes none.
func (s *Service) keyDocument() (*WitnessKeys, error) {
	if s.keys == nil {
		return nil, fmt.Errorf("%w: this witness publishes no key list", ErrClosed)
	}
	doc := &WitnessKeys{Keys: slices.Clone(s.keys)}
	if err := SignWitnessKeys(doc, s.id, heldPrivate(s.endorsers)...); err != nil {
		return nil, err
	}
	return doc, nil
}

// PushAnswer is the witness's answer to a push. A countersigned push carries both attestations and
// any notes the push raised. A refused one carries its findings and no attestation. A push proving
// from a base the witness does not hold, or claiming a tree behind the one it holds, carries the
// size and beat the witness holds so the install proves again from there.
type PushAnswer struct {
	// Error says why the push was not countersigned, empty when it was.
	Error string `json:"error,omitempty"`
	// InstallID is the install's label.
	InstallID string `json:"install_id"`
	// KeyID is the producer key the install's memory is kept under.
	KeyID string `json:"key_id"`
	// Beat, Seq, Head, Size, and Root are the countersigned coordinate on a 200, and the pushed one
	// otherwise.
	Beat int64  `json:"beat"`
	Seq  int64  `json:"seq"`
	Head string `json:"head"`
	Size int64  `json:"size"`
	Root string `json:"root"`
	// ClaimAttestation countersigns the link at Seq, present only when the push was countersigned.
	ClaimAttestation *Countersignature `json:"claim_attestation,omitempty"`
	// HeadAttestation countersigns the root at Size, present only when the push was countersigned.
	HeadAttestation *Countersignature `json:"head_attestation,omitempty"`
	// Findings are what the push raised, in the recorded shape with each finding's class.
	Findings []RecordedFinding `json:"findings,omitempty"`
	// HeldSize and HeldBeat are the tree size and beat the witness holds, present when the push must
	// be proved again from there. Zero is a real answer: the witness holds no tree.
	HeldSize *int64 `json:"held_size,omitempty"`
	HeldBeat *int64 `json:"held_beat,omitempty"`
}

// Accept holds one push against the memory of the install whose producer key id is keyID, and
// countersigns it exactly when the memory then holds it.
//
// A push not signed by the enrolled key, or naming another label than the one the key is enrolled
// as, writes nothing. The witness then takes the install's lock, checks the rate and the license
// term with its grace, and holds the push against the signed checkpoint. What it adopts it writes
// first, the head log line and then the checkpoint, and signs second, so a disk error answers
// ErrWrite with nothing countersigned. A refusal is recorded and answered with ErrRefused and its
// findings. A malformed, stale, or wrongly based push is an error that records nothing.
func (s *Service) Accept(keyID string, p Push) (*PushAnswer, error) {
	in := s.lookup(keyID)
	if in == nil {
		return nil, fmt.Errorf("%w: no install is enrolled under key %s", ErrNotEnrolled,
			clip(keyID, 80))
	}
	if err := VerifyPush(&p, in.pub); err != nil {
		return nil, err
	}
	in.mu.Lock()
	answer, recorded, err := s.hold(in, p)
	label, enrolledKey := in.rec.InstallID, in.rec.KeyID
	in.mu.Unlock()
	s.announce(label, enrolledKey, recorded)
	return answer, err
}

// hold is Accept's work under the install's lock. It returns the answer, the findings it recorded,
// and the error that says how the push was answered.
func (s *Service) hold(in *install, p Push) (*PushAnswer, []RecordedFinding, error) {
	if in.gone {
		return nil, nil, fmt.Errorf("%w: no install is enrolled under key %s", ErrNotEnrolled,
			in.rec.KeyID)
	}
	label := in.rec.InstallID
	if p.InstallID != label {
		return nil, nil, fmt.Errorf("%w: the push names install %q, and key %s is enrolled as %s",
			ErrMalformed, clip(p.InstallID, 48), in.rec.KeyID, label)
	}
	now := s.clock()
	if ok, wait := s.pushLimit.allow(in.name, now); !ok {
		return nil, nil, fmt.Errorf("%w: one push every %s after a burst of %d, next in %s",
			ErrLimited, pushEvery, pushBurst, wait.Round(time.Second))
	}
	if !in.expires.IsZero() && now.After(in.expires.Add(LapseGrace)) {
		return nil, nil, fmt.Errorf("%w: the license for this install ended %s and its %d days of "+
			"grace have passed, so this witness countersigns nothing more from it", ErrLapsed,
			stamp(in.expires), int(LapseGrace/(24*time.Hour)))
	}
	prev, err := loadMemory(in.path(installCheckpointFile), s.id, s.retiredMemory)
	if err != nil {
		return nil, nil, fmt.Errorf("install memory: %w", err)
	}
	next, found, err := CheckPush(prev, label, p, now)
	if errors.Is(err, ErrProofBase) || errors.Is(err, ErrStale) {
		size, beat := heldSize(prev), int64(0)
		if prev != nil {
			beat = prev.LastBeat
		}
		answer := s.pushed(in, p)
		answer.Error, answer.HeldSize, answer.HeldBeat = err.Error(), &size, &beat
		return answer, nil, err
	}
	if err != nil {
		return nil, nil, err
	}
	raised := classify(label, found, now)
	if !holdsPush(next, &p) {
		return s.refuse(in, p, found, raised, now)
	}
	if holdsPush(prev, &p) {
		// A retry of the push the memory already holds. It writes nothing and answers what the
		// first attempt answered, so a client that lost the answer is never told something new.
		// It does not count as hearing from the install: a signed push can be replayed byte for
		// byte, and a chain cut back could replay its last held push forever and never read as
		// silent.
		return s.countersign(in, prev, nil, nil)
	}
	// Memory before testimony. The head log line goes first, so a checkpoint never holds a
	// coordinate the log lacks, and a line already written by an attempt that failed later is not
	// written twice.
	line := headLine{At: now.UTC(), Beat: p.BeatNumber, Seq: p.Seq, Head: p.Head, Size: p.Size,
		Root: p.Root}
	if !line.sameCoordinate(in.lastHead) {
		if err := in.appendHead(line); err != nil {
			return nil, nil, fmt.Errorf("%w: head log: %w", ErrWrite, err)
		}
		in.lastHead = line
		if in.memorySince.IsZero() {
			in.memorySince = line.At
		}
	}
	saved := in.delta
	recorded := classify(label, in.delta.Fresh(found), now)
	if err := in.record(recorded); err != nil {
		in.delta = saved
		return nil, nil, fmt.Errorf("%w: findings record: %w", ErrWrite, err)
	}
	// Recorded notes count from here, even when the checkpoint then fails to save: they are on disk,
	// and the retry that follows finds them already seen rather than recording them twice.
	in.count(recorded)
	if err := Save(in.path(installCheckpointFile), next, s.id); err != nil {
		return nil, recorded, fmt.Errorf("%w: checkpoint: %w", ErrWrite, err)
	}
	in.checkpoint, in.lastSeen, in.silent = next, now, false
	return s.countersign(in, next, raised, recorded)
}

// refuse records a refused push's findings and answers them. Nothing is adopted and nothing is
// countersigned. When the record cannot be written the push is answered as a write error and the
// deduplication is rolled back, so the retry records what this attempt could not.
func (s *Service) refuse(in *install, p Push, found []Finding, raised []RecordedFinding,
	now time.Time) (*PushAnswer, []RecordedFinding, error) {
	saved := in.delta
	recorded := classify(in.rec.InstallID, in.delta.Fresh(found), now)
	if err := in.record(recorded); err != nil {
		in.delta = saved
		return nil, nil, fmt.Errorf("%w: findings record: %w", ErrWrite, err)
	}
	in.count(recorded)
	in.lastSeen, in.silent = now, false
	answer := s.pushed(in, p)
	answer.Error = "this witness refused the push: it contradicts what the witness remembers of " +
		"this install, so nothing was countersigned"
	answer.Findings = raised
	return answer, recorded, ErrRefused
}

// countersign answers a push the memory c holds with the two LoomSeal attestations, minted at the
// time the memory adopted it by the key that was the witness's word then, so every answer to that
// push carries the same bytes, across a key rotation too while the witness holds that key.
func (s *Service) countersign(in *install, c *Checkpoint, raised,
	recorded []RecordedFinding) (*PushAnswer, []RecordedFinding, error) {
	priv, at := s.mintingKey(c.ObservedAt)
	claim, head, err := countersignBy(c.LastHead, c.Root, c.Size, at, priv)
	if err != nil {
		return nil, recorded, err
	}
	return &PushAnswer{InstallID: in.rec.InstallID, KeyID: in.rec.KeyID, Beat: c.LastBeat,
		Seq: c.LastSeq, Head: c.LastHead, Size: c.Size, Root: c.Root, ClaimAttestation: &claim,
		HeadAttestation: &head, Findings: raised}, recorded, nil
}

// pushed returns an answer naming the install and the pushed coordinate, for a push that was not
// countersigned.
func (s *Service) pushed(in *install, p Push) *PushAnswer {
	return &PushAnswer{InstallID: in.rec.InstallID, KeyID: in.rec.KeyID, Beat: p.BeatNumber,
		Seq: p.Seq, Head: p.Head, Size: p.Size, Root: p.Root}
}

// classify turns findings into records about the install labeled label, each with its class.
func classify(label string, found []Finding, now time.Time) []RecordedFinding {
	if len(found) == 0 {
		return nil
	}
	out := make([]RecordedFinding, 0, len(found))
	for _, f := range found {
		out = append(out, RecordedFinding{At: now.UTC(), Server: label, Kind: f.Kind,
			Detail: clip(f.Detail, maxDetailLen), Class: pushClass(f.Kind)})
	}
	return out
}

// announce logs and notifies every finding recorded about an install, refusals at error and notes
// at warn. It runs outside the install's lock, because a notifier may post to a webhook.
func (s *Service) announce(label, keyID string, recs []RecordedFinding) {
	for _, rec := range recs {
		msg := "witness: install " + label + " " + strings.ToUpper(rec.Class) + " " + rec.Kind +
			": " + rec.Detail
		if rec.Class == ClassNote {
			s.log.Warn(msg)
		} else {
			s.log.Error(msg)
		}
		if s.notify != nil {
			s.notify(keyID, Finding{Kind: rec.Kind, Detail: rec.Detail, Class: rec.Class})
		}
	}
}

// Statement is the witness's signed account of what it holds for one enrolled install: the newest
// coordinate it countersigned, since when its memory holds, whether the install has gone quiet, and
// how many pushes it refused and how many notes it made. A relying party holds a bundle to it with
// verify --witness, which requires the producer key to match, the bundle to reproduce the
// coordinate, and RefusalsTotal to be zero. It is its own type rather than the pull Attestation
// because none of that statement's words about a watched feed are true of a push.
type Statement struct {
	// Switchtender is the domain tag, StatementVersion.
	Switchtender string `json:"switchtender"`
	// InstallID is the install's label, the id its producer key was born to.
	InstallID string `json:"install_id"`
	// KeyID is the producer key's sha256 fingerprint, the key the memory is kept under.
	KeyID string `json:"key_id"`
	// PublicKey is the producer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// MintedAt is when this statement was signed.
	MintedAt string `json:"minted_at"`
	// CheckpointAt is when the memory last adopted a coordinate.
	CheckpointAt string `json:"checkpoint_at"`
	// LastBeat, LastSeq, and LastHead are the newest beat the witness countersigned.
	LastBeat int64  `json:"last_beat"`
	LastSeq  int64  `json:"last_seq"`
	LastHead string `json:"last_head"`
	// Size and Root are the tree the witness holds, which every later push must extend.
	Size int64  `json:"size"`
	Root string `json:"root"`
	// MemorySince is when the witness adopted its first coordinate for this install. Nothing the
	// chain held before that was seen by this witness.
	MemorySince string `json:"memory_since"`
	// Silent is whether the install has pushed nothing for longer than StaleAfter.
	Silent bool `json:"silent"`
	// LastSeen is when the install last pushed something the witness held against its memory.
	LastSeen string `json:"last_seen"`
	// RefusalsTotal counts every push the witness refused, recorded once per standing refusal.
	RefusalsTotal int64 `json:"refusals_total"`
	// NotesTotal counts every note about the witness's coverage of this install.
	NotesTotal int64 `json:"notes_total"`
	// WitnessKeyID is the sha256 fingerprint of the witness key that signed the statement.
	WitnessKeyID string `json:"witness_key_id"`
	// WitnessPublicKey is that key, raw ed25519 in standard base64.
	WitnessPublicKey string `json:"witness_public_key"`
	// Sig is the base64 witness signature over the canonical statement without this member.
	Sig string `json:"sig,omitempty"`
}

// AttestInstall mints a signed statement of what the witness holds for the install whose producer
// key id is keyID. It returns nil before the first adopted push, since a statement of nothing is
// noise wearing a signature, and ErrNotEnrolled for a key with no record.
func (s *Service) AttestInstall(keyID string) (*Statement, error) {
	in := s.lookup(keyID)
	if in == nil {
		return nil, fmt.Errorf("%w: no install is enrolled under key %s", ErrNotEnrolled,
			clip(keyID, 80))
	}
	now := s.clock()
	in.mu.Lock()
	if in.gone {
		in.mu.Unlock()
		return nil, fmt.Errorf("%w: no install is enrolled under key %s", ErrNotEnrolled,
			in.rec.KeyID)
	}
	c := in.checkpoint
	if c == nil || c.Root == "" {
		in.mu.Unlock()
		return nil, nil
	}
	st := &Statement{InstallID: in.rec.InstallID, KeyID: in.rec.KeyID,
		PublicKey: in.rec.PublicKey, MintedAt: stamp(now), CheckpointAt: stamp(c.ObservedAt),
		LastBeat: c.LastBeat, LastSeq: c.LastSeq, LastHead: c.LastHead, Size: c.Size, Root: c.Root,
		MemorySince: stamp(in.memorySince), Silent: now.Sub(in.lastSeen) > StaleAfter,
		LastSeen: stamp(in.lastSeen), RefusalsTotal: in.refusals, NotesTotal: in.notes}
	in.mu.Unlock()
	if err := SignStatement(st, s.id); err != nil {
		return nil, err
	}
	return st, nil
}

// SignStatement stamps the statement with its domain tag and the witness key, and signs it.
func SignStatement(st *Statement, id identity.Identity) error {
	if st == nil {
		return fmt.Errorf("%w: sign statement: no statement", ErrMalformed)
	}
	st.Switchtender = StatementVersion
	st.WitnessKeyID, st.WitnessPublicKey = id.KeyID(), id.PublicKeyBase64()
	sig, err := signTagged(StatementVersion, st, id.Private())
	if err != nil {
		return fmt.Errorf("sign statement: %w", err)
	}
	st.Sig = sig
	return nil
}

// VerifyStatement checks the statement's tag, that its producer key id is the digest of its
// producer key, that its witness key id is the digest of its witness key, and that the witness key
// signed every other member. It returns the witness key id, which the relying party compares with
// the key it pinned: a signature alone proves only that the document is internally consistent.
func VerifyStatement(st *Statement) (string, error) {
	if st == nil {
		return "", fmt.Errorf("%w: verify statement: no statement", ErrMalformed)
	}
	if st.Switchtender != StatementVersion {
		return "", fmt.Errorf("%w: statement is tagged %q, not %q", ErrMalformed,
			clip(st.Switchtender, 32), StatementVersion)
	}
	producer, err := decodeKey(st.PublicKey)
	if err != nil {
		return "", fmt.Errorf("verify statement: producer: %w", err)
	}
	if seal.KeyID(producer) != st.KeyID {
		return "", fmt.Errorf("%w: statement key_id does not match its public_key", ErrMalformed)
	}
	pub, err := signerKey(st.WitnessKeyID, st.WitnessPublicKey)
	if err != nil {
		return "", fmt.Errorf("verify statement: %w", err)
	}
	if err := verifyTagged(StatementVersion, st, pub, st.Sig); err != nil {
		return "", fmt.Errorf("verify statement: %w", err)
	}
	return st.WitnessKeyID, nil
}

// SignWitnessKeys stamps the key list with its domain tag and the identity's key as the current
// one, and signs it with that identity. Each retired key given endorses the list over the same
// bytes, so a client that holds any of them accepts it, and each must be one the list names as
// retired. A list that names its keys out of form is refused rather than signed.
func SignWitnessKeys(k *WitnessKeys, id identity.Identity, retired ...ed25519.PrivateKey) error {
	if k == nil {
		return fmt.Errorf("%w: sign key list: no list", ErrMalformed)
	}
	k.Switchtender, k.KeyID, k.PublicKey = KeysVersion, id.KeyID(), id.PublicKeyBase64()
	k.Sig, k.Endorsements = "", nil
	if err := checkKeyHistory(k.Keys, k.KeyID); err != nil {
		return fmt.Errorf("sign key list: %w", err)
	}
	sig, err := signTagged(KeysVersion, k, id.Private())
	if err != nil {
		return fmt.Errorf("sign key list: %w", err)
	}
	endorsements, err := endorse(k, retired)
	if err != nil {
		return fmt.Errorf("sign key list: %w", err)
	}
	k.Sig, k.Endorsements = sig, endorsements
	return nil
}

// VerifyWitnessKeys checks the key list's tag, that its current key is listed, that its key id is
// the digest of its key, that the key signed the list, that the list names its keys in form with
// the current key as the one in use, and that every endorsement is a listed retired key's signature
// over the same bytes. It returns the current key id, which a client accepts only when it already
// holds that key or pins it. VerifyKeyList is the check for a list that replaces one already held.
func VerifyWitnessKeys(k *WitnessKeys) (string, error) {
	if k == nil {
		return "", fmt.Errorf("%w: verify key list: no list", ErrMalformed)
	}
	if k.Switchtender != KeysVersion {
		return "", fmt.Errorf("%w: key list is tagged %q, not %q", ErrMalformed,
			clip(k.Switchtender, 32), KeysVersion)
	}
	pub, err := signerKey(k.KeyID, k.PublicKey)
	if err != nil {
		return "", fmt.Errorf("verify key list: %w", err)
	}
	listed := slices.ContainsFunc(k.Keys, func(w WitnessKey) bool {
		return w.KeyID == k.KeyID && w.PublicKey == k.PublicKey
	})
	if !listed {
		return "", fmt.Errorf("%w: the key list does not list its own current key", ErrMalformed)
	}
	if err := verifyTagged(KeysVersion, bareKeyList(k), pub, k.Sig); err != nil {
		return "", fmt.Errorf("verify key list: %w", err)
	}
	if err := checkKeyHistory(k.Keys, k.KeyID); err != nil {
		return "", fmt.Errorf("verify key list: %w", err)
	}
	if err := verifyEndorsements(k); err != nil {
		return "", fmt.Errorf("verify key list: %w", err)
	}
	return k.KeyID, nil
}

// SignEnrollResponse stamps the answer with its domain tag and signs it with the witness identity,
// whose signed key list the answer must already carry.
func SignEnrollResponse(r *EnrollResponse, id identity.Identity) error {
	if r == nil {
		return fmt.Errorf("%w: sign enrollment answer: no answer", ErrMalformed)
	}
	r.Switchtender = EnrolledVersion
	sig, err := signTagged(EnrolledVersion, r, id.Private())
	if err != nil {
		return fmt.Errorf("sign enrollment answer: %w", err)
	}
	r.Sig = sig
	return nil
}

// VerifyEnrollResponse checks the answer's tag, the key list it carries, that the list's current
// key signed the answer, and that the enrolled producer key id is the digest of the enrolled key.
// It returns the witness key id, which the install compares with the id the witness publishes.
func VerifyEnrollResponse(r *EnrollResponse) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%w: verify enrollment answer: no answer", ErrMalformed)
	}
	if r.Switchtender != EnrolledVersion {
		return "", fmt.Errorf("%w: enrollment answer is tagged %q, not %q", ErrMalformed,
			clip(r.Switchtender, 32), EnrolledVersion)
	}
	keyID, err := VerifyWitnessKeys(&r.Witness)
	if err != nil {
		return "", fmt.Errorf("verify enrollment answer: %w", err)
	}
	producer, err := decodeKey(r.PublicKey)
	if err != nil {
		return "", fmt.Errorf("verify enrollment answer: producer: %w", err)
	}
	if seal.KeyID(producer) != r.KeyID {
		return "", fmt.Errorf("%w: enrollment answer key_id does not match its public_key",
			ErrMalformed)
	}
	pub, err := decodeKey(r.Witness.PublicKey)
	if err != nil {
		return "", fmt.Errorf("verify enrollment answer: %w", err)
	}
	if err := verifyTagged(EnrolledVersion, r, pub, r.Sig); err != nil {
		return "", fmt.Errorf("verify enrollment answer: %w", err)
	}
	return keyID, nil
}

// signerKey decodes a signer's base64 key and checks that keyID is its digest.
func signerKey(keyID, publicKey string) (ed25519.PublicKey, error) {
	pub, err := decodeKey(publicKey)
	if err != nil {
		return nil, err
	}
	if seal.KeyID(pub) != keyID {
		return nil, fmt.Errorf("%w: the signer's key_id does not match its public_key", ErrMalformed)
	}
	return pub, nil
}

// loadInstalls reads every enrollment record under the state directory, so a restarted witness
// holds every install's memory, totals, and silence where the last process left them. A record that
// fails its checks stops the start, as a pull memory that fails its signature does: a witness that
// skipped one would stop countersigning an install while reporting itself healthy. A deletion the
// last process began is finished here and never read as a record.
func (s *Service) loadInstalls() error {
	root := filepath.Join(s.dir, installsDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("witness installs: %w", err)
	}
	loaded := map[string]*install{}
	for _, e := range entries {
		if !e.IsDir() || s.finishRetired(root, e.Name()) {
			continue
		}
		in, err := s.loadInstall(filepath.Join(root, e.Name()), e.Name())
		if err != nil {
			return fmt.Errorf("install record %s: %w", e.Name(), err)
		}
		if in != nil {
			loaded[e.Name()] = in
		}
	}
	s.installsMu.Lock()
	defer s.installsMu.Unlock()
	s.installs = loaded
	return nil
}

// loadInstall reads one install record. It returns nil for a directory holding no enrollment, which
// is what a crash between making the directory and writing the record leaves.
func (s *Service) loadInstall(dir, name string) (*install, error) {
	raw, err := os.ReadFile(filepath.Join(dir, enrollmentFile))
	if os.IsNotExist(err) {
		s.log.Warn("witness: install directory " + name + " holds no enrollment, skipping it")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec Enrollment
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("parse enrollment: %w", err)
	}
	pub, err := decodeKey(rec.PublicKey)
	if err != nil {
		return nil, err
	}
	// The directory name pins the key: a record whose key was swapped no longer matches the name it
	// is filed under, and a label the key was not born to is refused here as at enrollment.
	if got, _ := recordName(seal.KeyID(pub)); rec.KeyID != seal.KeyID(pub) || got != name {
		return nil, fmt.Errorf("the record is filed under %s and holds key %s", name, rec.KeyID)
	}
	if !BornTo(rec.InstallID, pub) {
		return nil, fmt.Errorf("%w: %s", ErrNotBorn, rec.InstallID)
	}
	enrolledAt, err := time.Parse(time.RFC3339Nano, rec.EnrolledAt)
	if err != nil {
		return nil, fmt.Errorf("enrolled_at: %w", err)
	}
	var expires time.Time
	if rec.Expires != "" {
		if expires, err = time.Parse(time.RFC3339Nano, rec.Expires); err != nil {
			return nil, fmt.Errorf("expires: %w", err)
		}
	}
	in := &install{name: name, dir: dir, pub: pub, rec: rec, expires: expires}
	if in.checkpoint, err = loadMemory(in.path(installCheckpointFile), s.id,
		s.retiredMemory); err != nil {
		return nil, err
	}
	if c := in.checkpoint; c != nil && c.InstallID != rec.InstallID {
		return nil, fmt.Errorf("the checkpoint remembers install %s, and the record is %s",
			c.InstallID, rec.InstallID)
	}
	if err := in.readHeads(); err != nil {
		return nil, err
	}
	lastPush, lastSilent, err := in.recount()
	if err != nil {
		return nil, err
	}
	// The last push is not stored as such, so it is rebuilt from what each push leaves behind: the
	// adoption time in the checkpoint and the time of every finding a push recorded. It is never
	// later than the truth, so a restart can only make silence look older, never younger.
	in.lastSeen = enrolledAt
	if lastPush.After(in.lastSeen) {
		in.lastSeen = lastPush
	}
	if c := in.checkpoint; c != nil && c.ObservedAt.After(in.lastSeen) {
		in.lastSeen = c.ObservedAt
	}
	in.silent = !lastSilent.IsZero() && !lastSilent.Before(in.lastSeen)
	return in, nil
}

// readHeads reads the first and newest lines of the head log, which say since when the memory holds
// and what was written last.
func (in *install) readHeads() error {
	f, err := os.Open(in.path(headsFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxRecordLine)
	for sc.Scan() {
		var line headLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if in.memorySince.IsZero() {
			in.memorySince = line.At
		}
		in.lastHead = line
	}
	return sc.Err()
}

// recount restores the install's totals from its record and returns when it last pushed something
// recorded and when silence was last noted, which together decide whether the current silence was
// already noted.
func (in *install) recount() (lastPush, lastSilent time.Time, err error) {
	f, err := os.Open(in.path(findingsFile))
	if os.IsNotExist(err) {
		return time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLine)
	for sc.Scan() {
		var rec RecordedFinding
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		in.count([]RecordedFinding{rec})
		switch rec.Kind {
		case "silent_install":
			if rec.At.After(lastSilent) {
				lastSilent = rec.At
			}
		case "memory_restored":
		default:
			if rec.At.After(lastPush) {
				lastPush = rec.At
			}
		}
	}
	return lastPush, lastSilent, sc.Err()
}

// noteRestore turns the restore marker into one memory_restored note per install, naming the size
// the restored memory holds, and removes the marker once every note is on record. A marker that
// cannot be turned into notes stops the start, so the next start tries again rather than the rewind
// going unrecorded.
func (s *Service) noteRestore() error {
	marker := filepath.Join(s.dir, RestoreMarker)
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("restore marker: %w", err)
	}
	now := s.clock()
	type noted struct {
		// label and keyID name the install the note is about.
		label, keyID string
		// rec is the note as recorded.
		rec RecordedFinding
	}
	var notes []noted
	for _, in := range s.installList() {
		in.mu.Lock()
		held := "holds no tree for it"
		if c := in.checkpoint; c != nil && c.Root != "" {
			held = fmt.Sprintf("holds its tree at size %d with root %s", c.Size, c.Root)
		}
		rec := RecordedFinding{At: now.UTC(), Server: in.rec.InstallID, Kind: "memory_restored",
			Class: ClassNote, Detail: fmt.Sprintf("this witness's memory of install %s was restored "+
				"from an earlier copy and now %s, so anything it countersigned past that is no "+
				"longer in its memory, and the install's next push is told the size held and proves "+
				"again from there", in.rec.InstallID, held)}
		err := in.record([]RecordedFinding{rec})
		if err == nil {
			in.notes++
		}
		label, keyID := in.rec.InstallID, in.rec.KeyID
		in.mu.Unlock()
		if err != nil {
			return fmt.Errorf("restore marker: record for %s: %w", keyID, err)
		}
		notes = append(notes, noted{label: label, keyID: keyID, rec: rec})
	}
	if err := os.Remove(marker); err != nil {
		return fmt.Errorf("restore marker: %w", err)
	}
	for _, n := range notes {
		s.announce(n.label, n.keyID, []RecordedFinding{n.rec})
	}
	return nil
}

// sweepInstalls deletes every record whose read-only year after lapse has ended, then notes
// silence: an install that has pushed nothing the witness held against its memory for longer than
// StaleAfter raises silent_install once, and again only after it pushes and falls quiet anew.
// Silence is measured from the later of the last push and the service's start, because pushes
// cannot arrive while the witness itself is down, and a witness must not record its own outage as
// the install's silence.
func (s *Service) sweepInstalls() {
	now := s.clock()
	for _, in := range s.installList() {
		if s.retireIfDue(in, now) {
			continue
		}
		in.mu.Lock()
		since := in.lastSeen
		if since.Before(s.startedAt) {
			since = s.startedAt
		}
		var recs []RecordedFinding
		if !in.silent && now.Sub(since) > StaleAfter {
			rec := RecordedFinding{At: now.UTC(), Server: in.rec.InstallID, Kind: "silent_install",
				Class: ClassNote, Detail: fmt.Sprintf("install %s has pushed nothing since %s, %s "+
					"ago, so this witness has countersigned nothing from it since then",
					in.rec.InstallID, stamp(in.lastSeen), now.Sub(in.lastSeen).Round(time.Minute))}
			if err := in.record([]RecordedFinding{rec}); err != nil {
				s.log.Error("witness: record silence of " + in.rec.KeyID + ": " + err.Error())
			} else {
				in.silent = true
				in.notes++
				recs = append(recs, rec)
			}
		}
		label, keyID := in.rec.InstallID, in.rec.KeyID
		in.mu.Unlock()
		s.announce(label, keyID, recs)
	}
}

// installFindings returns the newest recorded findings about an install and its totals.
func (s *Service) installFindings(keyID string) (*install, []RecordedFinding, int64, int64, error) {
	in := s.lookup(keyID)
	if in == nil {
		return nil, nil, 0, 0, fmt.Errorf("%w: no install is enrolled under key %s",
			ErrNotEnrolled, clip(keyID, 80))
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.gone {
		return nil, nil, 0, 0, fmt.Errorf("%w: no install is enrolled under key %s",
			ErrNotEnrolled, in.rec.KeyID)
	}
	recs, err := tailFile(in.path(findingsFile), "")
	if err != nil {
		return nil, nil, 0, 0, err
	}
	return in, recs, in.refusals, in.notes, nil
}
