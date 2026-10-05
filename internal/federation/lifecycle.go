package federation

import (
	"fmt"
	"time"
)

// Rotation timing. Both are fixed rather than settings: they are the promise a cloud administrator
// plans around, that a normal rotation never presents a token signed by a key the cloud could not
// have fetched a day earlier, and never drops a key a token it signed may still need.
const (
	// RotationDelay is how long a normal rotation publishes the new key before the key signs. The key
	// it replaces keeps signing until then.
	RotationDelay = 24 * time.Hour
	// RetiredKeyGrace is how long a key stays in the public key set after it stops signing. It is far
	// longer than MaxTokenTTL, so every token the key signed expires while its key is still published.
	RetiredKeyGrace = 24 * time.Hour
)

// Key states, as the key listing names them.
const (
	// StatePending is a key that is published and not yet signing, the day a normal rotation gives
	// relying parties to fetch it.
	StatePending = "pending"
	// StateSigning is a key that has started signing and not stopped.
	StateSigning = "signing"
	// StateRetired is a key that no longer signs and is still published, so the tokens it signed
	// keep verifying until they expire.
	StateRetired = "retired"
	// StateRemoved is a key that has left the public key set. Its private half is erased and only
	// its public half and its times stay, as the record of when it was trusted.
	StateRemoved = "removed"
)

// removed reports whether k has left the public key set by now. A key whose removal is set and
// whose private half is erased is removed whatever now says. The erasure happens only once the
// removal has come by the clock the key store judges by, or at once in an emergency rotation, so a
// process whose own clock trails that moment must not keep publishing the key until it catches up:
// a cloud that fetched the key set from it then would accept a token forged with the removed key.
func removed(k *Key, now time.Time) bool {
	if k.RemovedAt == nil {
		return false
	}
	return k.Sealed == "" || !now.Before(*k.RemovedAt)
}

// published reports whether k is in the public key set at now: from its creation until its removal.
func published(k *Key, now time.Time) bool {
	return !removed(k, now)
}

// activated reports whether k has started signing by now.
func activated(k *Key, now time.Time) bool {
	return k.ActivatedAt != nil && !now.Before(*k.ActivatedAt)
}

// retired reports whether k has stopped signing by now.
func retired(k *Key, now time.Time) bool {
	return k.RetiredAt != nil && !now.Before(*k.RetiredAt)
}

// pending reports whether k is published and due to start signing later.
func pending(k *Key, now time.Time) bool {
	return !removed(k, now) && k.ActivatedAt != nil && now.Before(*k.ActivatedAt)
}

// signs reports whether k may sign at now: it has started, has not stopped, has not been removed,
// and still holds its private half.
func signs(k *Key, now time.Time) bool {
	return k.Sealed != "" && activated(k, now) && !retired(k, now) && !removed(k, now)
}

// keyState names k's state at now.
func keyState(k *Key, now time.Time) string {
	switch {
	case removed(k, now):
		return StateRemoved
	case retired(k, now):
		return StateRetired
	case activated(k, now):
		return StateSigning
	default:
		return StatePending
	}
}

// signer returns the key that signs new tokens at now, or nil when no key can.
//
// It is the most recently activated key that may sign. Settling leaves at most one such key, and
// when a store still holds two, from two first keys generated at once by an earlier build, the
// later one signs and settling retires the other.
//
// When none may sign, it falls back to the key due to sign next that still holds its private half.
// That happens only on a process judging by a clock that trails the one that ran an emergency
// rotation, which a key store with a clock of its own rules out: the old key is already erased
// while the new key's activation is still, by this clock, a moment away. Signing with the new key
// early is safe, since it was published as it was created, and generating a key of this process's
// own would publish a key no relying party had fetched.
func signer(keys []*Key, now time.Time) *Key {
	var best *Key
	for _, k := range keys {
		if signs(k, now) && (best == nil || activatedLater(k, best)) {
			best = k
		}
	}
	if best != nil {
		return best
	}
	return nextSigner(keys, now, true)
}

// nextSigner returns the pending key due to start signing first, holding its private half when
// sealed is set, or nil when no key is pending.
func nextSigner(keys []*Key, now time.Time, sealed bool) *Key {
	var best *Key
	for _, k := range keys {
		if !pending(k, now) || (sealed && k.Sealed == "") {
			continue
		}
		if best == nil || activatedLater(best, k) {
			best = k
		}
	}
	return best
}

// activatedLater reports whether a activates after b, breaking a tie by creation time and then by
// id, so every process picks the same key from the same keys.
func activatedLater(a, b *Key) bool {
	if c := a.ActivatedAt.Compare(*b.ActivatedAt); c != 0 {
		return c > 0
	}
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c > 0
	}
	return a.ID > b.ID
}

// refusePending returns ErrRotationPending, naming the key a normal rotation published and has not
// yet started signing with and when it starts, or nil when no rotation is under way.
func refusePending(keys []*Key, now time.Time) error {
	p := nextSigner(keys, now, false)
	if p == nil {
		return nil
	}
	return fmt.Errorf("%w: key %s was published at %s and starts signing at %s. Wait for it, or "+
		"use an emergency rotation if a key may be compromised", ErrRotationPending, p.ID,
		p.CreatedAt.UTC().Format(time.RFC3339), p.ActivatedAt.UTC().Format(time.RFC3339))
}

// settle brings keys to the state their schedule says they are in at now, changing them in place,
// and returns the keys it changed for the caller to save inside the change it read them in.
//
// It erases the private half of every key whose removal has come, so a key that is no longer
// trusted cannot sign again whatever a later bug says, while its public half and its four times
// stay as the record of when it was trusted. And it gives every key that signs, other than the one
// that signs new tokens, a retirement: the signing key retires when a pending rotation's key starts
// signing, and any other key that signs retires now. No key is then left signing, published, and
// holding its private half with no schedule to retire, remove, and erase it, which is what a
// rotation interrupted between its two writes, two rotations at once, or two first keys generated
// at once left behind before rotations ran as one change.
func settle(keys []*Key, now time.Time) []*Key {
	current := signer(keys, now)
	next := nextSigner(keys, now, false)
	var changed []*Key
	for _, k := range keys {
		touched := false
		if k.Sealed != "" && removed(k, now) {
			k.Sealed = ""
			touched = true
		}
		switch {
		case !signs(k, now):
		case current != nil && k.ID == current.ID:
			if next != nil {
				touched = retireBy(k, *next.ActivatedAt) || touched
			}
		default:
			touched = retireBy(k, now) || touched
		}
		if touched {
			changed = append(changed, k)
		}
	}
	return changed
}

// retireBy schedules k to retire no later than at and to leave the published set no later than
// RetiredKeyGrace after that, keeping any earlier time already set, and reports whether either
// time changed. A schedule only ever moves earlier, so a removal another change set stands.
func retireBy(k *Key, at time.Time) bool {
	changed := false
	if k.RetiredAt == nil || k.RetiredAt.After(at) {
		k.RetiredAt = cloneTime(&at)
		changed = true
	}
	remove := at.Add(RetiredKeyGrace)
	if k.RemovedAt == nil || k.RemovedAt.After(remove) {
		k.RemovedAt = &remove
		changed = true
	}
	return changed
}

// KeyInfo is the public description of one signing key, for the key listing. It carries the key's
// id and times and nothing of the key itself.
type KeyInfo struct {
	// ID is the key id, the kid a token names.
	ID string `json:"id"`
	// Algorithm is the JWS algorithm, RS256.
	Algorithm string `json:"algorithm"`
	// State is pending, signing, retired, or removed.
	State string `json:"state"`
	// CreatedAt is when the key was generated and first published.
	CreatedAt time.Time `json:"created_at"`
	// ActivatedAt is when the key starts or started signing, absent for a key that never signs.
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	// RetiredAt is when the key stops or stopped signing, absent while no rotation has replaced it.
	RetiredAt *time.Time `json:"retired_at,omitempty"`
	// RemovedAt is when the key leaves or left the public key set, absent while none is scheduled.
	RemovedAt *time.Time `json:"removed_at,omitempty"`
	// Signing reports that this is the key new tokens are signed with.
	Signing bool `json:"signing"`
	// Published reports that this key is in the public key set.
	Published bool `json:"published"`
}

// infoOf describes k at now, given the key that signs.
func infoOf(k *Key, now time.Time, signing *Key) KeyInfo {
	return KeyInfo{
		ID: k.ID, Algorithm: k.Algorithm, State: keyState(k, now), CreatedAt: k.CreatedAt,
		ActivatedAt: cloneTime(k.ActivatedAt), RetiredAt: cloneTime(k.RetiredAt),
		RemovedAt: cloneTime(k.RemovedAt), Signing: signing != nil && k.ID == signing.ID,
		Published: published(k, now),
	}
}
