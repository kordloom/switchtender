package witness

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/loomseal/seal"

	"github.com/kordloom/switchtender/identity"
)

// A witness keeps its keys in a key directory of their own, apart from the state directory, which
// is copied off the host. The current key is identity.WitnessFile, as it always was. A rotation
// copies that file byte for byte into retiredKeysDir and records the change in keyHistoryFile,
// which is the list the witness publishes. A retired key stays held so it can endorse every later
// list, which is what lets a client that pinned it accept the new one on its word, and so it can
// answer a retried push with the bytes it first answered. Deleting a retired key's file stops both:
// the list still names the key, and nothing it signs is endorsed again.
const (
	// keyHistoryFile lists every key the witness has signed with, oldest first. A witness that
	// never rotated has none, and its one key is valid from when its key file was written.
	keyHistoryFile = "witness-key-history.json"
	// retiredKeysDir holds the key file of each retired key the witness still holds, named for the
	// hex of its key id.
	retiredKeysDir = "witness-retired-keys"
	// nextKeyDir holds the key a rotation is making current, until the history names it and it is
	// moved into place.
	nextKeyDir = "witness-key-next"
)

// KeySignature is one key's signature over a witness key list.
type KeySignature struct {
	// KeyID is the signing key's sha256 fingerprint, a key the list names.
	KeyID string `json:"key_id"`
	// Sig is the base64 ed25519 signature over the canonical list without its sig and endorsements
	// members.
	Sig string `json:"sig"`
}

// heldKey is a retired key the witness still holds, with the period it spoke for the witness.
type heldKey struct {
	// priv is the retired private key.
	priv ed25519.PrivateKey
	// from is when the key began signing.
	from time.Time
	// until is when the key retired.
	until time.Time
}

// memoryKey is a retired key whose signature the witness accepts on its own memory, for memory
// observed before the key retired.
type memoryKey struct {
	// hex is the public key in hex, the form a checkpoint names its signer in.
	hex string
	// until is when the key retired.
	until time.Time
}

// keyHistory is the key history file's form.
type keyHistory struct {
	// Keys lists every key the witness has signed with, oldest first.
	Keys []WitnessKey `json:"keys"`
}

// storedKey is the one member of a witness key file a retired key is read from.
type storedKey struct {
	// Seed is the hex ed25519 seed.
	Seed string `json:"seed"`
}

// Keyring is a witness's signing keys as its key directory holds them: the current key, the history
// the witness publishes, and the retired keys it still holds.
type Keyring struct {
	// current signs everything the witness says from now on.
	current identity.Identity
	// history lists every key the witness has signed with, oldest first.
	history []WitnessKey
	// held are the retired keys the directory still holds, in history order.
	held []heldKey
}

// Rotation names the key a rotation retired and the key it made current.
type Rotation struct {
	// Retired is the key that stopped being the witness's word, with its retired time.
	Retired WitnessKey `json:"retired"`
	// Current is the key that signs from now on, valid from the same instant.
	Current WitnessKey `json:"current"`
}

// Current returns the identity that signs for the witness now.
func (k *Keyring) Current() identity.Identity { return k.current }

// Keys returns every key the witness has signed with, the list it publishes.
func (k *Keyring) Keys() []WitnessKey { return slices.Clone(k.history) }

// Held returns the key ids of the retired keys the directory still holds, the keys that endorse
// every list the witness publishes.
func (k *Keyring) Held() []string {
	ids := make([]string, 0, len(k.held))
	for _, h := range k.held {
		ids = append(ids, seal.KeyID(h.priv.Public().(ed25519.PublicKey)))
	}
	return ids
}

// memoryKeys returns every key the history shows retired, with its retired time.
func (k *Keyring) memoryKeys() []memoryKey {
	var keys []memoryKey
	for _, w := range k.history {
		if w.Retired == "" {
			continue
		}
		pub, err := decodeKey(w.PublicKey)
		_, until, perr := keyPeriod(w)
		if err != nil || perr != nil {
			continue
		}
		keys = append(keys, memoryKey{hex: hex.EncodeToString(pub), until: until})
	}
	return keys
}

// currentFrom returns when the current key began signing, or the zero time for a witness that never
// rotated, whose memory the current key adopted all of.
func (k *Keyring) currentFrom() time.Time {
	if len(k.history) < 2 {
		return time.Time{}
	}
	for _, w := range k.history {
		if w.Retired == "" {
			from, _, _ := keyPeriod(w)
			return from
		}
	}
	return time.Time{}
}

// LoadKeyring reads the witness's keys from keyDir, creating the current key on first use as the
// witness always has. A witness that never rotated publishes its one key, valid from when its key
// file was written, so the list reads the same across restarts and restores. After a rotation the
// history file is the list, and every retired key file still present endorses it. A rotation that
// stopped after recording the new key and before moving it into place is finished here. A history
// whose current key the directory does not hold is refused rather than answered with a new key,
// which nothing would endorse. A rotated key directory that is, holds, or lies inside stateDir is
// refused, because the state directory is copied off the host and retired keys must not travel
// with it.
func LoadKeyring(keyDir, stateDir string) (*Keyring, error) {
	hist, err := readKeyHistory(keyDir)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat(filepath.Join(keyDir, retiredKeysDir)); serr == nil {
			if err := refuseOverlap(keyDir, stateDir); err != nil {
				return nil, err
			}
		}
		return firstKeyring(keyDir)
	}
	if err != nil {
		return nil, err
	}
	if err := refuseOverlap(keyDir, stateDir); err != nil {
		return nil, err
	}
	var current WitnessKey
	for _, w := range hist {
		if w.Retired == "" {
			current = w
		}
	}
	id, err := loadCurrentKey(keyDir, current)
	if err != nil {
		return nil, err
	}
	ring := &Keyring{current: id, history: hist}
	for _, w := range hist {
		if w.Retired == "" {
			continue
		}
		priv, err := readRetiredKey(keyDir, w)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		from, until, _ := keyPeriod(w)
		ring.held = append(ring.held, heldKey{priv: priv, from: from, until: until})
	}
	return ring, nil
}

// firstKeyring returns the keyring of a witness that never rotated: its one key, created on first
// use, valid from when the key file was written.
func firstKeyring(keyDir string) (*Keyring, error) {
	id, err := identity.LoadWitnessFile(keyDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(keyDir, identity.WitnessFile))
	if err != nil {
		return nil, fmt.Errorf("witness key file: %w", err)
	}
	return &Keyring{current: id,
		history: []WitnessKey{WitnessKeyOf(id, info.ModTime().Truncate(time.Second))}}, nil
}

// loadCurrentKey returns the key the history names as current, from the key file, or from the
// rotation's staging directory when a rotation stopped after recording the key, in which case the
// key is moved into place. It never creates a key: a key that is missing is restored from the
// offline copy, not replaced.
func loadCurrentKey(keyDir string, want WitnessKey) (identity.Identity, error) {
	path := filepath.Join(keyDir, identity.WitnessFile)
	_, err := os.Stat(path)
	switch {
	case err == nil:
		id, err := identity.LoadWitnessFile(keyDir)
		if err != nil {
			return identity.Identity{}, err
		}
		if id.KeyID() == want.KeyID {
			return id, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		return identity.Identity{}, fmt.Errorf("witness key file: %w", err)
	}
	next := filepath.Join(keyDir, nextKeyDir)
	if _, err := os.Stat(filepath.Join(next, identity.WitnessFile)); err == nil {
		id, err := identity.LoadWitnessFile(next)
		if err != nil {
			return identity.Identity{}, err
		}
		if id.KeyID() == want.KeyID {
			if err := os.Rename(filepath.Join(next, identity.WitnessFile), path); err != nil {
				return identity.Identity{}, fmt.Errorf("finish the key rotation: %w", err)
			}
			_ = os.RemoveAll(next)
			return id, nil
		}
	}
	return identity.Identity{}, fmt.Errorf("the key history names %s as the witness's current key, "+
		"and %s does not hold it: restore the key directory from its offline copy rather than let "+
		"a new key be made", want.KeyID, keyDir)
}

// RotateKey retires the witness's current key at now and makes a new key current, valid from now.
// The retired key file is copied byte for byte into the key directory's retired keys, where it
// stays to endorse every later list, and the history gains the retired time and the new key.
// Writing the history is the step that commits the rotation: a crash before it leaves the old key
// current, and a crash after it is finished by the next LoadKeyring. It refuses a key directory
// that is, holds, or lies inside stateDir, and writes nothing there. Run it while the witness is
// stopped, since a running witness keeps signing with the key it loaded until it restarts.
func RotateKey(keyDir, stateDir string, now time.Time) (Rotation, error) {
	if err := refuseOverlap(keyDir, stateDir); err != nil {
		return Rotation{}, err
	}
	if _, err := os.Stat(filepath.Join(keyDir, identity.WitnessFile)); err != nil {
		return Rotation{}, fmt.Errorf("no witness key to rotate in %s: %w", keyDir, err)
	}
	ring, err := LoadKeyring(keyDir, stateDir)
	if err != nil {
		return Rotation{}, err
	}
	hist := ring.Keys()
	cur := slices.IndexFunc(hist, func(w WitnessKey) bool { return w.Retired == "" })
	from, _, err := keyPeriod(hist[cur])
	if err != nil {
		return Rotation{}, err
	}
	if !now.After(from) {
		return Rotation{}, fmt.Errorf("the clock reads %s, which is not after %s, when the current "+
			"key began signing", stamp(now), hist[cur].ValidFrom)
	}
	next := filepath.Join(keyDir, nextKeyDir)
	if err := os.RemoveAll(next); err != nil {
		return Rotation{}, fmt.Errorf("clear an unfinished rotation: %w", err)
	}
	fresh, err := identity.LoadWitnessFile(next)
	if err != nil {
		return Rotation{}, fmt.Errorf("make the new key: %w", err)
	}
	if slices.ContainsFunc(hist, func(w WitnessKey) bool { return w.KeyID == fresh.KeyID() }) {
		_ = os.RemoveAll(next)
		return Rotation{}, fmt.Errorf("the new key %s is already in the history", fresh.KeyID())
	}
	if err := retireKeyFile(keyDir, hist[cur]); err != nil {
		return Rotation{}, err
	}
	hist[cur].Retired = stamp(now)
	hist = append(hist, WitnessKeyOf(fresh, now))
	if err := writeKeyHistory(keyDir, hist); err != nil {
		return Rotation{}, err
	}
	if err := os.Rename(filepath.Join(next, identity.WitnessFile),
		filepath.Join(keyDir, identity.WitnessFile)); err != nil {
		return Rotation{}, fmt.Errorf("the rotation is recorded and the new key is not in place, "+
			"which the next start finishes: %w", err)
	}
	_ = os.RemoveAll(next)
	return Rotation{Retired: hist[cur], Current: hist[len(hist)-1]}, nil
}

// retireKeyFile copies the current key file into the retired keys, owner-only, and reads it back
// to prove the copy holds the key the history names, before the rotation commits.
func retireKeyFile(keyDir string, w WitnessKey) error {
	raw, err := os.ReadFile(filepath.Join(keyDir, identity.WitnessFile))
	if err != nil {
		return fmt.Errorf("read the current key: %w", err)
	}
	defer clear(raw)
	name, ok := recordName(w.KeyID)
	if !ok {
		return fmt.Errorf("%w: key id %s", ErrMalformed, clip(w.KeyID, 80))
	}
	dir := filepath.Join(keyDir, retiredKeysDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("retired keys directory: %w", err)
	}
	if err := writeSynced(dir, name+".json", raw); err != nil {
		return fmt.Errorf("retire key %s: %w", w.KeyID, err)
	}
	if _, err := readRetiredKey(keyDir, w); err != nil {
		return fmt.Errorf("retire key %s: %w", w.KeyID, err)
	}
	return nil
}

// readRetiredKey reads the private key of the retired key w, returning an error that wraps
// fs.ErrNotExist when the directory no longer holds it. A file another account can read is
// refused, and so is one holding any other key. The seed never appears in an error.
func readRetiredKey(keyDir string, w WitnessKey) (ed25519.PrivateKey, error) {
	name, ok := recordName(w.KeyID)
	if !ok {
		return nil, fmt.Errorf("%w: key id %s", ErrMalformed, clip(w.KeyID, 80))
	}
	path := filepath.Join(keyDir, retiredKeysDir, name+".json")
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("retired witness key %s is readable by other accounts (mode %04o): "+
			"chmod 600 %s", w.KeyID, info.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var stored storedKey
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("retired witness key %s: the file is not a witness key file", w.KeyID)
	}
	seed, err := hex.DecodeString(stored.Seed)
	stored.Seed = ""
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("retired witness key %s: the seed is not %d bytes of hex", w.KeyID,
			ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	pub, _ := priv.Public().(ed25519.PublicKey)
	if seal.KeyID(pub) != w.KeyID {
		return nil, fmt.Errorf("retired witness key file %s holds key %s, not %s", path,
			seal.KeyID(pub), w.KeyID)
	}
	return priv, nil
}

// readKeyHistory reads and checks the key history, returning an error that wraps fs.ErrNotExist
// for a witness that never rotated.
func readKeyHistory(keyDir string) ([]WitnessKey, error) {
	raw, err := os.ReadFile(filepath.Join(keyDir, keyHistoryFile))
	if err != nil {
		return nil, err
	}
	var doc keyHistory
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%w: key history: %w", ErrMalformed, err)
	}
	cur := slices.IndexFunc(doc.Keys, func(w WitnessKey) bool { return w.Retired == "" })
	if cur < 0 {
		return nil, fmt.Errorf("%w: key history names no key in use", ErrMalformed)
	}
	if err := checkKeyHistory(doc.Keys, doc.Keys[cur].KeyID); err != nil {
		return nil, fmt.Errorf("key history: %w", err)
	}
	return doc.Keys, nil
}

// writeKeyHistory writes the key history atomically and owner-only.
func writeKeyHistory(keyDir string, keys []WitnessKey) error {
	raw, err := json.MarshalIndent(keyHistory{Keys: keys}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the key history: %w", err)
	}
	if err := writeSynced(keyDir, keyHistoryFile, append(raw, '\n')); err != nil {
		return fmt.Errorf("write the key history: %w", err)
	}
	return nil
}

// writeSynced writes data to name in dir through an owner-only scratch file that is synced to disk
// and renamed into place, so a crash leaves either the old file or the whole new one.
func writeSynced(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	scratch := tmp.Name()
	defer func() { _ = os.Remove(scratch) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(scratch, filepath.Join(dir, name))
}

// refuseOverlap refuses a key directory that is, holds, or lies inside the state directory.
func refuseOverlap(keyDir, stateDir string) error {
	if strings.TrimSpace(keyDir) == "" {
		return fmt.Errorf("no witness key directory given")
	}
	if strings.TrimSpace(stateDir) == "" {
		return fmt.Errorf("no witness state directory given to keep the keys apart from")
	}
	overlap, err := dirsOverlap(keyDir, stateDir)
	if err != nil {
		return fmt.Errorf("compare the key and state directories: %w", err)
	}
	if overlap {
		return fmt.Errorf("the key directory %s is, holds, or lies inside the state directory %s, "+
			"which is copied off the host: keep the witness keys in a key directory of their own",
			keyDir, stateDir)
	}
	return nil
}

// dirsOverlap reports whether a and b name the same directory or one lies inside the other. Both
// are made absolute with symbolic links resolved as far as each path exists, and the parts that
// exist are also compared by file identity, so a second spelling of one directory is caught.
func dirsOverlap(a, b string) (bool, error) {
	ra, err := resolvePath(a)
	if err != nil {
		return false, err
	}
	rb, err := resolvePath(b)
	if err != nil {
		return false, err
	}
	return within(ra, rb) || within(rb, ra) || sameFileWithin(ra, rb) || sameFileWithin(rb, ra),
		nil
}

// resolvePath returns p made absolute, with symbolic links resolved in the longest leading part of
// it that exists.
func resolvePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest), nil
		}
		if filepath.Dir(dir) == dir {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

// within reports whether path is dir or lies inside it, by name.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sameFileWithin reports whether path, or a directory path lies inside, is dir on disk.
func sameFileWithin(path, dir string) bool {
	target, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, target) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// keyPeriod parses the period a listed key spoke for the witness: from its valid_from, until its
// retired time, which is zero while the key is in use.
func keyPeriod(w WitnessKey) (from, until time.Time, err error) {
	if from, err = time.Parse(time.RFC3339Nano, w.ValidFrom); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: key %s valid_from %q is not a time",
			ErrMalformed, clip(w.KeyID, 80), clip(w.ValidFrom, 40))
	}
	if w.Retired == "" {
		return from, time.Time{}, nil
	}
	if until, err = time.Parse(time.RFC3339Nano, w.Retired); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: key %s retired %q is not a time",
			ErrMalformed, clip(w.KeyID, 80), clip(w.Retired, 40))
	}
	if until.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: key %s retired before it was valid",
			ErrMalformed, clip(w.KeyID, 80))
	}
	return from, until, nil
}

// checkKeyHistory checks the keys a list names: each key id is the digest of its key, no key is
// listed twice, each key's period is in form, and exactly one key is in use, current.
func checkKeyHistory(keys []WitnessKey, current string) error {
	if len(keys) == 0 {
		return fmt.Errorf("%w: the key list names no key", ErrMalformed)
	}
	seen := map[string]bool{}
	inUse := 0
	for _, w := range keys {
		if _, err := signerKey(w.KeyID, w.PublicKey); err != nil {
			return fmt.Errorf("key %s: %w", clip(w.KeyID, 80), err)
		}
		if seen[w.KeyID] {
			return fmt.Errorf("%w: the key list names key %s twice", ErrMalformed, w.KeyID)
		}
		seen[w.KeyID] = true
		if _, _, err := keyPeriod(w); err != nil {
			return err
		}
		if w.Retired != "" {
			continue
		}
		inUse++
		if w.KeyID != current {
			return fmt.Errorf("%w: the key list shows key %s in use, and its current key is %s",
				ErrMalformed, w.KeyID, clip(current, 80))
		}
	}
	if inUse != 1 {
		return fmt.Errorf("%w: the key list shows %d keys in use, want its current key alone",
			ErrMalformed, inUse)
	}
	return nil
}

// bareKeyList returns a copy of k without its endorsements, the bytes its signature and every
// endorsement cover once taggedContent drops sig.
func bareKeyList(k *WitnessKeys) *WitnessKeys {
	bare := *k
	bare.Endorsements = nil
	return &bare
}

// endorse signs the key list k with each retired key, over the bytes its current key signs. Each
// key must be one the list names as retired, and none may endorse twice.
func endorse(k *WitnessKeys, retired []ed25519.PrivateKey) ([]KeySignature, error) {
	bare := bareKeyList(k)
	var out []KeySignature
	for _, priv := range retired {
		if len(priv) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("%w: an endorsing key is not an ed25519 key", ErrMalformed)
		}
		pub, _ := priv.Public().(ed25519.PublicKey)
		keyID := seal.KeyID(pub)
		listed := slices.ContainsFunc(k.Keys, func(w WitnessKey) bool {
			return w.KeyID == keyID && w.PublicKey == base64.StdEncoding.EncodeToString(pub) &&
				w.Retired != ""
		})
		if !listed {
			return nil, fmt.Errorf("%w: key %s endorses a list that does not show it retired",
				ErrMalformed, keyID)
		}
		if slices.ContainsFunc(out, func(e KeySignature) bool { return e.KeyID == keyID }) {
			return nil, fmt.Errorf("%w: key %s endorses the list twice", ErrMalformed, keyID)
		}
		sig, err := signTagged(KeysVersion, bare, priv)
		if err != nil {
			return nil, err
		}
		out = append(out, KeySignature{KeyID: keyID, Sig: sig})
	}
	return out, nil
}

// verifyEndorsements checks that every endorsement on k is by a key the list shows retired, that
// no key endorses twice, and that each signature holds over the bytes the current key signed.
func verifyEndorsements(k *WitnessKeys) error {
	bare := bareKeyList(k)
	seen := map[string]bool{}
	for _, e := range k.Endorsements {
		i := slices.IndexFunc(k.Keys, func(w WitnessKey) bool { return w.KeyID == e.KeyID })
		if i < 0 {
			return fmt.Errorf("%w: an endorsement by key %s, which the list does not name",
				ErrMalformed, clip(e.KeyID, 80))
		}
		if k.Keys[i].Retired == "" {
			return fmt.Errorf("%w: an endorsement by key %s, which the list does not show retired",
				ErrMalformed, e.KeyID)
		}
		if seen[e.KeyID] {
			return fmt.Errorf("%w: key %s endorses the list twice", ErrMalformed, e.KeyID)
		}
		seen[e.KeyID] = true
		pub, err := signerKey(k.Keys[i].KeyID, k.Keys[i].PublicKey)
		if err != nil {
			return err
		}
		if err := verifyTagged(KeysVersion, bare, pub, e.Sig); err != nil {
			return fmt.Errorf("endorsement by %s: %w", e.KeyID, err)
		}
	}
	return nil
}

// VerifyKeyList decides whether a client holding the key list prev accepts next in its place. Both
// must verify on their own. The key prev names as current must have signed next, as next's current
// key or by an endorsement: a key prev already shows retired does not count, since it had stopped
// being the witness's word before the client accepted prev. And next must keep every key prev
// lists, with the same public key and valid_from, and every retired time prev states, so a rotation
// adds to the history and never rewrites it.
func VerifyKeyList(prev, next WitnessKeys) error {
	if _, err := VerifyWitnessKeys(&prev); err != nil {
		return fmt.Errorf("the held key list: %w", err)
	}
	if _, err := VerifyWitnessKeys(&next); err != nil {
		return fmt.Errorf("the new key list: %w", err)
	}
	signed := next.KeyID == prev.KeyID && next.PublicKey == prev.PublicKey
	if !signed {
		signed = slices.ContainsFunc(next.Endorsements, func(e KeySignature) bool {
			return e.KeyID == prev.KeyID
		})
	}
	if !signed {
		return fmt.Errorf("%w: the new key list is not signed by %s, the key the held list names "+
			"as current", ErrSignature, prev.KeyID)
	}
	for _, old := range prev.Keys {
		i := slices.IndexFunc(next.Keys, func(w WitnessKey) bool { return w.KeyID == old.KeyID })
		if i < 0 {
			return fmt.Errorf("%w: the new key list drops key %s", ErrMalformed, old.KeyID)
		}
		kept := next.Keys[i]
		if kept.PublicKey != old.PublicKey || kept.ValidFrom != old.ValidFrom {
			return fmt.Errorf("%w: the new key list changes when key %s began signing",
				ErrMalformed, old.KeyID)
		}
		if old.Retired != "" && kept.Retired != old.Retired {
			return fmt.Errorf("%w: the new key list moves the retired time of key %s",
				ErrMalformed, old.KeyID)
		}
	}
	return nil
}

// KeyAt returns the listed key whose id is keyID when the list says it was the witness's word at
// at: on or after its valid_from and, once retired, before its retired time. A relying party that
// verified a countersignature asks this whether its signer spoke for the witness when it signed.
func (k *WitnessKeys) KeyAt(keyID string, at time.Time) (WitnessKey, bool) {
	for _, w := range k.Keys {
		if w.KeyID != keyID {
			continue
		}
		from, until, err := keyPeriod(w)
		if err != nil || at.Before(from) || (!until.IsZero() && !at.Before(until)) {
			return WitnessKey{}, false
		}
		return w, true
	}
	return WitnessKey{}, false
}

// heldPrivate returns the private keys of held, in order.
func heldPrivate(held []heldKey) []ed25519.PrivateKey {
	keys := make([]ed25519.PrivateKey, 0, len(held))
	for _, h := range held {
		keys = append(keys, h.priv)
	}
	return keys
}

// loadMemory reads a checkpoint the witness wrote. It accepts the signature of the current key, and
// of a key the witness retired only for memory observed before that key retired: what that key
// signs afterwards is not the witness's word. The next adopted push signs the memory with the
// current key, so an old signature lasts only until then.
func loadMemory(path string, current identity.Identity, retired []memoryKey) (*Checkpoint, error) {
	pins := []string{current.PublicKeyHex()}
	for _, k := range retired {
		pins = append(pins, k.hex)
	}
	c, err := Load(path, pins...)
	if err != nil || c == nil || c.PublicKey == pins[0] {
		return c, err
	}
	for _, k := range retired {
		if c.PublicKey == k.hex && !c.ObservedAt.Before(k.until) {
			return nil, fmt.Errorf("checkpoint was signed by key %s, which retired at %s, and was "+
				"observed at %s, after that key stopped being this witness's word", keyName(k.hex),
				stamp(k.until), stamp(c.ObservedAt))
		}
	}
	return c, nil
}

// mintingKey returns the key that countersigns memory observed at observed, and the time it signs
// at. The memory's adoption time is kept whenever the key that was the witness's word then is
// still held: the current key for memory it adopted, the retired key that adopted it otherwise. So
// a retried push answers the bytes the first attempt did, across a rotation too. When that key is
// no longer held, the current key countersigns at the present time, since it cannot speak for a
// time before it began.
func (s *Service) mintingKey(observed time.Time) (ed25519.PrivateKey, time.Time) {
	if s.currentFrom.IsZero() || !observed.Before(s.currentFrom) {
		return s.id.Private(), observed
	}
	for _, h := range s.endorsers {
		if !observed.Before(h.from) && observed.Before(h.until) {
			return h.priv, observed
		}
	}
	return s.id.Private(), s.clock()
}

// countersignBy mints the claim attestation over link and the head attestation over root at size,
// both by priv at at, the pair a push answer carries. They are the attestations ClaimAttestation
// and HeadAttestation mint, for a key the witness holds without an identity file of its own.
func countersignBy(link, root string, size int64, at time.Time,
	priv ed25519.PrivateKey) (claim, head Countersignature, err error) {
	if !hashHex.MatchString(link) || !hashHex.MatchString(root) {
		return claim, head, fmt.Errorf("%w: countersign: link or root is not 64 hex", ErrMalformed)
	}
	if size < 1 {
		return claim, head, fmt.Errorf("%w: countersign: size %d is not positive", ErrMalformed,
			size)
	}
	when, err := formatSealTime(at)
	if err != nil {
		return claim, head, fmt.Errorf("countersign: %w", err)
	}
	if claim, err = mint(claimPreimage(link, RoleWitness, when), RoleWitness, when, priv); err != nil {
		return claim, head, err
	}
	head, err = mint(headPreimage(root, size, RoleWitness, when), RoleWitness, when, priv)
	return claim, head, err
}
