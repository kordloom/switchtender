package witnessclient

import (
	"slices"
	"time"
)

// queueSpan is how long a witness outage the queue rides out: one day of beats at the cadence. Past
// it the oldest push is dropped, which costs the witness's view of that beat and never the next
// proof, because every push is proved at the moment it is sent from the size the witness last
// acknowledged.
const queueSpan = 24 * time.Hour

// QueueCap returns how many pushes the queue holds at the given beat cadence: one day of beats, and
// at least one.
func QueueCap(cadence time.Duration) int {
	if cadence <= 0 {
		return 1
	}
	return max(1, int(queueSpan/cadence))
}

// pending is one beat waiting to be pushed: its coordinate and the tree root at its size, fixed
// when the beat was appended so a retry pushes exactly what the chain held then. Its tree size is
// its sequence, since a chain numbers its entries from one without gaps.
type pending struct {
	// Beat is the span beat's number.
	Beat int64
	// At is when the beat was appended, in the form the beat feed serves.
	At string
	// Seq is the beat entry's position in the chain, and the tree size the push claims.
	Seq int64
	// Head is the beat entry's chain link.
	Head string
	// Root is the hex tree root over the first Seq entries.
	Root string
}

// queue holds the pushes not yet answered, oldest first, bounded so an outage costs memory in
// proportion to a day of beats and no more.
type queue struct {
	// items are the waiting pushes in the order the beats were appended.
	items []pending
	// capacity is how many items the queue holds before it drops the oldest.
	capacity int
	// dropped counts every push dropped to make room.
	dropped int64
}

// newQueue returns an empty queue holding at most capacity pushes, and at least one.
func newQueue(capacity int) *queue {
	return &queue{capacity: max(1, capacity)}
}

// add appends p behind every waiting push. The queue may hold one past its capacity until trim,
// so the beat that finds the witness back is sent with every push waiting before it.
func (q *queue) add(p pending) {
	q.items = append(q.items, p)
}

// trim drops the oldest waiting pushes until the queue is within its capacity, counting each, and
// returns them oldest first.
func (q *queue) trim() []pending {
	over := len(q.items) - q.capacity
	if over <= 0 {
		return nil
	}
	dropped := slices.Clone(q.items[:over])
	q.items = slices.Delete(q.items, 0, over)
	q.dropped += int64(over)
	return dropped
}

// front returns the oldest waiting push.
func (q *queue) front() (pending, bool) {
	if len(q.items) == 0 {
		return pending{}, false
	}
	return q.items[0], true
}

// pop removes the oldest waiting push.
func (q *queue) pop() {
	if len(q.items) > 0 {
		q.items = q.items[1:]
	}
}

// clear removes every waiting push without counting any as dropped, for a client that will push
// nothing more.
func (q *queue) clear() { q.items = nil }

// size returns how many pushes are waiting.
func (q *queue) size() int { return len(q.items) }
