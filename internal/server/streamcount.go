package server

import (
	"net/http"
	"sync"
)

// Stream limits. A live stream costs a goroutine, a subscription, and a store poll every second for as
// long as the run lasts, so the number open at once is a direct multiplier on what one caller can make
// the server do. The per-caller limit is what bounds one account; the total is what keeps many accounts
// from adding up to the same thing. Both sit far above what the interface opens, which is one stream per
// run page a person has in front of them.
const (
	// maxStreamsPerActor is how many live streams one caller may hold open.
	maxStreamsPerActor = 32
	// maxStreamsTotal is how many live streams the server holds open across every caller.
	maxStreamsTotal = 512
)

// streamCount tracks the live streams, in total and per caller, so the endpoint can refuse a caller who
// is holding too many open rather than growing without bound.
type streamCount struct {
	// mu guards total and byActor.
	mu sync.Mutex
	// total is how many streams are open across every caller.
	total int
	// byActor is how many each caller holds, keyed by whatever identifies them.
	byActor map[string]int
}

// admit reserves a stream for actor and returns the release to call when it closes, reporting false when
// either limit is already reached. The release is safe to call once; calling it is the caller's job and
// a deferred call is how every path, including a dropped connection, gives the slot back.
func (s *streamCount) admit(actor string) (release func(), admitted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byActor == nil {
		s.byActor = make(map[string]int)
	}
	if s.total >= maxStreamsTotal || s.byActor[actor] >= maxStreamsPerActor {
		return func() {}, false
	}
	s.total++
	s.byActor[actor]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.total--
			if s.byActor[actor] <= 1 {
				delete(s.byActor, actor)
				return
			}
			s.byActor[actor]--
		})
	}, true
}

// actorKeyFor identifies the caller a stream is counted against: the authenticated user when there is
// one, and the remote address otherwise, so an install serving without authentication is still bounded
// per client rather than only in total.
func actorKeyFor(r *http.Request) string {
	if actor, ok := actorFrom(r.Context()); ok {
		if actor.UserID != "" {
			return "user:" + actor.UserID
		}
		if actor.Name != "" {
			return "name:" + actor.Name
		}
	}
	return "addr:" + r.RemoteAddr
}
