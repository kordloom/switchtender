package witnessclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/safedial"
	"github.com/kordloom/switchtender/internal/util"
	"github.com/kordloom/switchtender/witness"
)

const (
	// EnrollmentFile names the file an install keeps its enrollment in. It lives beside the license
	// file, so a backup and a restore carry both or neither.
	EnrollmentFile = "witness-enrollment.json"
	// KeysPath is where a witness publishes its signed key list.
	KeysPath = "/.well-known/loomseal.json"
	// enrollPath is where a witness takes enrollments.
	enrollPath = "/v1/enroll"
	// requestTimeout bounds one exchange with the witness.
	requestTimeout = 30 * time.Second
	// maxAnswer bounds how much of a witness answer is read. An honest one is a few kilobytes.
	maxAnswer = 1 << 20
)

// GuardedClient returns the client every request to a witness goes through when the caller supplies
// none. The witness address is operator configuration the server follows on its own network, so it
// dials through the same guard the timestamp authority's client does, and it follows no redirect: a
// redirect would carry the enrollment's license file, or a signed push, to an address nobody
// configured.
func GuardedClient() *http.Client {
	return safedial.Client(requestTimeout)
}

// PathFor returns where the enrollment for the license file at licensePath lives: the same
// directory.
func PathFor(licensePath string) string {
	return filepath.Join(filepath.Dir(licensePath), EnrollmentFile)
}

// Enrollment is what an install keeps of its enrollment with a push witness: where the witness is,
// the keys it accepts countersignatures from, and the producer key and label the witness bound. It
// holds no secret, since every push is authenticated by the producer's signature, so there is no
// token to leak or rotate.
type Enrollment struct {
	// Witness is the witness's base URL.
	Witness string `json:"witness"`
	// Keys is the witness's signed key list. A countersignature is accepted only from a key it lists
	// as the witness's word at the time the countersignature names.
	Keys witness.WitnessKeys `json:"keys"`
	// InstallID is the install's label, the id its producer key was born to.
	InstallID string `json:"install_id"`
	// KeyID is the producer key's sha256 fingerprint, the key the witness keeps the memory under.
	KeyID string `json:"key_id"`
	// PublicKey is the producer's raw ed25519 public key in standard base64.
	PublicKey string `json:"public_key"`
	// EnrolledAt is when the witness first enrolled the install, as the witness signed it.
	EnrolledAt string `json:"enrolled_at"`
	// PushPath is the path on the witness this install posts its beats to.
	PushPath string `json:"push_path"`
}

// pushPath returns the path a witness takes the pushes of the producer key keyID at.
func pushPath(keyID string) string { return "/v1/installs/" + keyID + "/beats" }

// NormalizeURL returns the witness base URL raw names, without a trailing slash. It must be http or
// https with a host and nothing after the path, since every request appends its own path to it.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: the witness URL does not parse: %w", ErrEnrollment, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%w: the witness URL %q is not an http or https URL with a host",
			ErrEnrollment, raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: the witness URL %q carries a user, query, or fragment, and a "+
			"witness base URL has none", ErrEnrollment, raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// RequireSecureURL refuses a witness base URL that sends the install's requests in the clear: plain
// http is accepted only when its host is a loopback address, 127.0.0.0/8, ::1, or localhost, which
// never leaves the machine. Over plain http a network position can answer for the witness, and the
// enrollment carries the license file. base is a URL NormalizeURL accepted.
func RequireSecureURL(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: the witness URL does not parse: %w", ErrEnrollment, err)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%w: the witness URL %s is plain http to a host that is not a loopback "+
		"address, so anyone on the network path could answer for the witness and read what the "+
		"install sends it. Use the witness's https URL", ErrEnrollment, base)
}

// Enroll asks the witness at base to enroll the install id signs for, carrying the license file
// licenseFile for the witness's enrollment policy, and returns the enrollment the witness's signed
// answer makes.
//
// An identity whose install id was not born to its key is refused before any request is sent: it
// is a rotated identity, and the witness binds an id to the key it was born to. The answer must
// verify under the key list it carries, and it must bind this install's producer key and label and
// name this key's push path. Which witness key to trust is the caller's to settle, by comparing
// Keys.KeyID with the published id or with a pin.
func Enroll(ctx context.Context, client *http.Client, base string, id identity.Identity,
	licenseFile []byte, now time.Time) (*Enrollment, error) {
	if !witness.BornTo(id.InstallID, id.Public()) {
		return nil, fmt.Errorf("%w: this install's id %s was born to a different key than the one it "+
			"signs with, which is a rotated identity. The witness binds an id to the key it was born "+
			"to. Restore that key, or keep producer-key.json and enroll once succession support "+
			"ships", ErrEnrollment, id.InstallID)
	}
	base, err := NormalizeURL(base)
	if err != nil {
		return nil, err
	}
	if err := RequireSecureURL(base); err != nil {
		return nil, err
	}
	req := witness.EnrollRequest{InstallID: id.InstallID, License: json.RawMessage(licenseFile),
		At: now.UTC().Format(time.RFC3339)}
	if len(licenseFile) == 0 {
		req.License = nil
	}
	if err := witness.SignEnroll(&req, id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEnrollment, err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("%w: encode the enrollment: %w", ErrEnrollment, err)
	}
	code, raw, err := exchange(ctx, client, http.MethodPost, base+enrollPath, body)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, answerError("the enrollment", code, raw)
	}
	var resp witness.EnrollResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: the witness's enrollment answer is not one: %w", ErrEnrollment,
			err)
	}
	if _, err := witness.VerifyEnrollResponse(&resp); err != nil {
		return nil, fmt.Errorf("%w: the witness's enrollment answer: %w", ErrEnrollment, err)
	}
	e := &Enrollment{Witness: base, Keys: resp.Witness, InstallID: resp.InstallID,
		KeyID: resp.KeyID, PublicKey: resp.PublicKey, EnrolledAt: resp.EnrolledAt,
		PushPath: resp.PushPath}
	if err := e.Bind(id); err != nil {
		return nil, fmt.Errorf("the witness answered for another install: %w", err)
	}
	if err := e.check(); err != nil {
		return nil, err
	}
	return e, nil
}

// Bind checks that the enrollment is for the producer identity id: its key id, its key, and its
// label. A client never pushes under an enrollment that is not its own identity's, since the
// witness would refuse every push signed by another key, and a key changed under the enrollment is
// a rotated identity.
func (e *Enrollment) Bind(id identity.Identity) error {
	switch {
	case e.KeyID != id.KeyID():
		return fmt.Errorf("%w: the enrollment is for producer key %s, and this install signs with %s",
			ErrEnrollment, e.KeyID, id.KeyID())
	case e.PublicKey != id.PublicKeyBase64():
		return fmt.Errorf("%w: the enrollment's producer key is not the one this install signs with",
			ErrEnrollment)
	case e.InstallID != id.InstallID:
		return fmt.Errorf("%w: the enrollment is for install %s, and this install is %s",
			ErrEnrollment, e.InstallID, id.InstallID)
	}
	return nil
}

// Pin checks that the witness key the enrollment names as current is keyID.
func (e *Enrollment) Pin(keyID string) error {
	if e.Keys.KeyID != keyID {
		return fmt.Errorf("%w: the witness signs with key %s, not the pinned %s, so nothing was "+
			"written", ErrWitnessKey, e.Keys.KeyID, keyID)
	}
	return nil
}

// Follows checks that the enrollment may replace held, the one already on disk: the same witness,
// the same producer key and label, and a key list held's keys accept through VerifyKeyList. A list
// no held key signed is never accepted in place of the held one, so a replaced or forged witness
// cannot slip its key in through a second enrollment. A nil held is a first enrollment.
func (e *Enrollment) Follows(held *Enrollment) error {
	if held == nil {
		return nil
	}
	if e.Witness != held.Witness {
		return fmt.Errorf("%w: this install is enrolled with the witness at %s, not %s. Remove %s to "+
			"enroll with another witness", ErrEnrollment, held.Witness, e.Witness, EnrollmentFile)
	}
	if e.KeyID != held.KeyID || e.InstallID != held.InstallID {
		return fmt.Errorf("%w: the enrollment on disk is for install %s under key %s, and this one "+
			"is for install %s under key %s. Remove %s to enroll this identity", ErrEnrollment,
			held.InstallID, held.KeyID, e.InstallID, e.KeyID, EnrollmentFile)
	}
	if err := witness.VerifyKeyList(held.Keys, e.Keys); err != nil {
		return fmt.Errorf("%w: the witness answered with a key list the held one does not accept, so "+
			"nothing was written: %w", ErrWitnessKey, err)
	}
	return nil
}

// check verifies the enrollment's own form: a witness URL, a key list that verifies, a producer key
// id that is the digest of its key, a label the key was born to, and the push path of that key.
func (e *Enrollment) check() error {
	if base, err := NormalizeURL(e.Witness); err != nil || base != e.Witness {
		return fmt.Errorf("%w: the enrollment names no witness URL in form", ErrEnrollment)
	}
	if _, err := witness.VerifyWitnessKeys(&e.Keys); err != nil {
		return fmt.Errorf("%w: the enrollment's witness key list: %w", ErrWitnessKey, err)
	}
	raw, err := base64.StdEncoding.DecodeString(e.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: the enrollment's producer key is not a base64 ed25519 key",
			ErrEnrollment)
	}
	pub := ed25519.PublicKey(raw)
	if seal.KeyID(pub) != e.KeyID {
		return fmt.Errorf("%w: the enrollment's producer key id is not the digest of its key",
			ErrEnrollment)
	}
	if !witness.BornTo(e.InstallID, pub) {
		return fmt.Errorf("%w: the enrollment's install %s was not born to its key", ErrEnrollment,
			e.InstallID)
	}
	if e.PushPath != pushPath(e.KeyID) {
		return fmt.Errorf("%w: the enrollment's push path %q is not %s", ErrEnrollment, e.PushPath,
			pushPath(e.KeyID))
	}
	if e.EnrolledAt == "" {
		return fmt.Errorf("%w: the enrollment says no time it was made", ErrEnrollment)
	}
	return nil
}

// Load reads the enrollment at path and checks its form. A missing file is fs.ErrNotExist, wrapped,
// so a caller can tell an install that never enrolled from a file that is broken.
func Load(path string) (*Enrollment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrEnrollment, path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e Enrollment
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("%w: %s is not an enrollment: %w", ErrEnrollment, path, err)
	}
	if err := e.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &e, nil
}

// LoadIfPresent reads the enrollment at path, or returns nil when there is none.
func LoadIfPresent(path string) (*Enrollment, error) {
	e, err := Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return e, err
}

// Save writes the enrollment to path, replacing any there in one rename, so a reader never sees
// half of one.
func (e *Enrollment) Save(path string) error {
	if err := e.check(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encode: %w", ErrEnrollment, err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+EnrollmentFile+".*")
	if err != nil {
		return fmt.Errorf("%w: write %s: %w", ErrEnrollment, path, err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("%w: write %s: %w", ErrEnrollment, path, werr)
	}
	return nil
}

// FetchKeys returns the key list the witness at base publishes, once it verifies on its own.
// Whether to accept it in place of a held list is VerifyKeyList's question, not this one's.
func FetchKeys(ctx context.Context, client *http.Client, base string) (*witness.WitnessKeys, error) {
	base, err := NormalizeURL(base)
	if err != nil {
		return nil, err
	}
	code, raw, err := exchange(ctx, client, http.MethodGet, base+KeysPath, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, answerError("the key list", code, raw)
	}
	var keys witness.WitnessKeys
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("%w: the witness's key list is not one: %w", ErrWitnessKey, err)
	}
	if _, err := witness.VerifyWitnessKeys(&keys); err != nil {
		return nil, fmt.Errorf("%w: the witness's key list: %w", ErrWitnessKey, err)
	}
	return &keys, nil
}

// RefreshKeys fetches the key list the witness of the enrollment at path publishes and returns the
// enrollment with that list in place of the held one, when witness.VerifyKeyList accepts it: the
// key the held list names as current signed it, and it keeps every key the held list names. It
// reports whether the list changed and writes nothing, so the caller records the change before it
// saves the result. A list no held key signed is refused, so a rotation is authenticated by the
// chain of keys and never by whoever answers at the witness's address. A non-empty pin must be the
// new list's current key too.
func RefreshKeys(ctx context.Context, client *http.Client, path, pin string) (*Enrollment, bool, error) {
	held, err := Load(path)
	if err != nil {
		return nil, false, err
	}
	keys, err := FetchKeys(ctx, client, held.Witness)
	if err != nil {
		return nil, false, err
	}
	if pin != "" && keys.KeyID != pin {
		return nil, false, fmt.Errorf("%w: the witness publishes %s as its current key, not the "+
			"pinned %s, so nothing was written", ErrWitnessKey, keys.KeyID, pin)
	}
	if err := witness.VerifyKeyList(held.Keys, *keys); err != nil {
		return nil, false, fmt.Errorf("%w: the witness publishes a key list the held one does not "+
			"accept, so it was not kept: %w", ErrWitnessKey, err)
	}
	same, err := sameKeys(held.Keys, *keys)
	if err != nil || same {
		return held, false, err
	}
	next := *held
	next.Keys = *keys
	return &next, true, nil
}

// Same reports whether e and o are the same enrollment, member for member.
func (e *Enrollment) Same(o *Enrollment) bool {
	if o == nil {
		return false
	}
	x, xerr := json.Marshal(e)
	y, yerr := json.Marshal(o)
	return xerr == nil && yerr == nil && bytes.Equal(x, y)
}

// sameKeys reports whether two key lists are the same document.
func sameKeys(a, b witness.WitnessKeys) (bool, error) {
	x, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("%w: encode: %w", ErrWitnessKey, err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("%w: encode: %w", ErrWitnessKey, err)
	}
	return bytes.Equal(x, y), nil
}

// exchange sends one request to a witness and returns the status and the answer, read up to
// maxAnswer bytes. A request that gets no answer is ErrUnreachable, and so is a 5xx: the witness
// could not answer, which is never a refusal. A nil client is GuardedClient.
func exchange(ctx context.Context, client *http.Client, method, target string,
	body []byte) (int, []byte, error) {
	if client == nil {
		client = GuardedClient()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build the request: %w", ErrRejected, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswer))
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read the answer: %w", ErrUnreachable, err)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return resp.StatusCode, raw, fmt.Errorf("%w: the witness answered %d: %s", ErrUnreachable,
			resp.StatusCode, answerText(raw))
	}
	return resp.StatusCode, raw, nil
}

// answerError names a witness answer that was not the one asked for. A 402 is ErrLapsed.
func answerError(what string, code int, raw []byte) error {
	kind := ErrRejected
	if code == http.StatusPaymentRequired {
		kind = ErrLapsed
	}
	return fmt.Errorf("%w: the witness answered %s with %d: %s", kind, what, code, answerText(raw))
}

// answerText returns the error a witness answer carries, or a short note when it carries none.
func answerText(raw []byte) string {
	var body struct {
		// Error is the witness's reason.
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return util.Clip(body.Error, 512)
	}
	return "no reason given"
}
