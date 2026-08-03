package dispatch

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/event"
)

// maskToken replaces a redacted secret value in run output.
const maskToken = "***"

// minMaskLen is the shortest secret value the masker redacts. A shorter value is skipped so a one or
// two character secret does not black out unrelated output.
const minMaskLen = 4

// masker redacts known secret values from a run's log and events. Its secret set is populated once
// the run's credentials resolve, and it is safe for concurrent use by the log sink and the event
// tailer.
type masker struct {
	// mu guards secrets, strs, and shortest.
	mu sync.RWMutex
	// secrets holds the values to redact, longest first so a longer secret is masked before a
	// shorter substring of it.
	secrets [][]byte
	// strs holds the same values in the same order as strings. Keeping both representations means
	// masking a string field costs no conversion per secret per call.
	strs []string
	// shortest is the byte length of the shortest secret, zero when none are set. Text shorter than
	// it cannot contain any secret, so it is returned untouched.
	shortest int
}

// maskBytes is the mask token as bytes, converted once rather than per replacement.
var maskBytes = []byte(maskToken)

// set replaces the masker's secret values, expanding a multi-line secret into its lines as well so
// output that streams a secret one line at a time is still redacted. A value shorter than minMaskLen
// or blank is dropped so masking cannot swallow unrelated output.
func (m *masker) set(values []string) {
	seen := make(map[string]struct{})
	var out []string
	add := func(s string) {
		s = strings.TrimRight(s, "\r\n")
		if utf8.RuneCountInString(strings.TrimSpace(s)) < minMaskLen {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, v := range values {
		add(v)
		for _, line := range strings.Split(v, "\n") {
			add(line)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })

	secrets := make([][]byte, len(out))
	for i, s := range out {
		secrets[i] = []byte(s)
	}
	shortest := 0
	if len(out) > 0 {
		// out is sorted longest first, so the tail is the shortest.
		shortest = len(out[len(out)-1])
	}

	m.mu.Lock()
	m.secrets = secrets
	m.strs = out
	m.shortest = shortest
	m.mu.Unlock()
}

// redact returns p with every known secret replaced by the mask token. It returns p unchanged when
// no secrets are set, allocating a new slice only when a redaction is made.
func (m *masker) redact(p []byte) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shortest == 0 || len(p) < m.shortest {
		return p
	}
	out := p
	for _, s := range m.secrets {
		if len(s) > len(out) {
			continue
		}
		if bytes.Contains(out, s) {
			out = bytes.ReplaceAll(out, s, maskBytes)
		}
	}
	return out
}

// longest returns the length in bytes of the longest secret, or zero when none are set. The stream
// masker uses it to decide how much of a chunk it must hold back.
func (m *masker) longest() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.secrets) == 0 {
		return 0
	}
	// secrets is sorted longest first, so the head is the longest.
	return len(m.secrets[0])
}

// partialTail returns the length of the longest suffix of buf that is a proper prefix of some secret,
// and so could be the leading part of a secret a later chunk completes. It is at most the longest
// secret minus one byte, since a whole secret occurrence is redacted rather than withheld. A suffix
// that begins no secret returns zero, so ordinary output is not held back. The stream masker withholds
// exactly this suffix when it drains a quiet run, releasing everything before it.
func (m *masker) partialTail(buf []byte) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.secrets) == 0 || len(buf) == 0 {
		return 0
	}
	// The longest possible partial is one byte short of the longest secret, and never more than the
	// buffer itself. secrets is sorted longest first, so the head bounds it.
	maxLen := min(len(m.secrets[0])-1, len(buf))
	// Try the longest candidate suffix first; the first that begins a secret is the answer.
	for w := maxLen; w > 0; w-- {
		suffix := buf[len(buf)-w:]
		for _, sec := range m.secrets {
			if len(suffix) < len(sec) && bytes.HasPrefix(sec, suffix) {
				return w
			}
		}
	}
	return 0
}

// wholeSecretCut returns a release point at or after cut that no secret occurrence straddles.
//
// Splitting an occurrence would leave half of it in the released bytes, where redaction can no
// longer see the whole value, and half in the withheld tail. Extending the point to the end of any
// straddling occurrence keeps every match intact on the side that gets redacted.
func (m *masker) wholeSecretCut(buf []byte, cut int) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.secrets) == 0 {
		return cut
	}
	// A secret can only reach past the point from within the preceding longest-1 bytes, and pushing
	// the point forward can expose another straddle, so this repeats until it settles.
	for moved := true; moved; {
		moved = false
		for _, sec := range m.secrets {
			from := max(cut-len(sec)+1, 0)
			for at := from; at < cut; at++ {
				if at+len(sec) > len(buf) {
					break
				}
				if bytes.Equal(buf[at:at+len(sec)], sec) && at+len(sec) > cut {
					cut = at + len(sec)
					moved = true
					break
				}
			}
			if moved {
				break
			}
		}
	}
	return cut
}

// streamMasker redacts a byte stream whose chunk boundaries fall wherever the operating system
// happened to split the pipe. Redacting each chunk on its own misses a secret straddling two of
// them: neither half contains the whole value, so neither is replaced and the plaintext reaches the
// stored log and every live viewer.
//
// It holds back the last few bytes of each chunk, enough that a secret ending in the next one is
// reassembled before either half is emitted, and releases them once the stream ends. Output is
// therefore delayed by at most the length of the longest secret, and never reordered or duplicated.
// It is used by a single stream and is not safe for concurrent use.
type streamMasker struct {
	// mask holds the secret values, shared with the event tailer.
	mask *masker
	// tail is the withheld end of the stream so far, still unredacted and not yet emitted.
	tail []byte
}

// next redacts what it can of chunk and returns the bytes that are safe to emit now. Whatever it
// withholds is carried into the following call, so a secret split across the boundary is caught.
func (s *streamMasker) next(chunk []byte) []byte {
	if s.mask == nil {
		return chunk
	}
	keep := s.mask.longest() - 1
	if keep < 0 {
		keep = 0
	}
	buf := chunk
	if len(s.tail) > 0 {
		buf = make([]byte, 0, len(s.tail)+len(chunk))
		buf = append(buf, s.tail...)
		buf = append(buf, chunk...)
	}
	if keep == 0 || len(buf) <= keep {
		if keep == 0 {
			s.tail = nil
			return s.mask.redact(buf)
		}
		// Nothing can be released yet: every byte so far could still begin a secret the next chunk
		// finishes. The bytes are kept as they arrived, not redacted, because redacting them now
		// could replace a short secret with the mask token and destroy the bytes a longer secret
		// overlapping it needs once the rest of it lands.
		s.tail = append(s.tail[:0], buf...)
		return nil
	}
	// Release everything except the last keep bytes, then push the release point past any secret
	// that would otherwise be cut in half by it, so the whole occurrence is redacted together.
	cut := s.mask.wholeSecretCut(buf, len(buf)-keep)
	s.tail = append(s.tail[:0], buf[cut:]...)
	return s.mask.redact(buf[:cut])
}

// drain releases the part of the withheld tail that is safe to emit while the stream is still open,
// redacted. It keeps back only the longest suffix of the tail that is a proper prefix of some secret,
// which could still be the leading half of a secret a later chunk completes; everything before that
// suffix cannot begin a secret that continues past it and is released. A tail that is not a partial
// secret, as a slow run's ordinary output is, is released whole, so the log advances instead of
// sitting blank. What it keeps is carried in the tail exactly as next would carry it, so a following
// chunk still reassembles a straddling secret, and flush releases that remainder once the stream ends.
func (s *streamMasker) drain() []byte {
	if s.mask == nil {
		out := s.tail
		s.tail = nil
		return out
	}
	if len(s.tail) == 0 {
		return nil
	}
	w := s.mask.partialTail(s.tail)
	if w >= len(s.tail) {
		// Every byte could still be the start of a secret, so none of it is safe to release yet.
		return nil
	}
	// Release everything before the risky suffix and keep that suffix. A fresh slice backs the new
	// tail so it cannot alias the emitted bytes, which redact may return pointing into the old tail.
	cut := len(s.tail) - w
	out := s.mask.redact(s.tail[:cut])
	s.tail = append([]byte(nil), s.tail[cut:]...)
	return out
}

// flush releases the withheld end of the stream, redacted. It is called once the stream is finished,
// when nothing further can arrive to complete a secret.
func (s *streamMasker) flush() []byte {
	if len(s.tail) == 0 {
		return nil
	}
	out := s.tail
	s.tail = nil
	if s.mask == nil {
		return out
	}
	return s.mask.redact(out)
}

// redactString returns s with every known secret replaced by the mask token.
func (m *masker) redactString(s string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.redactStringLocked(s)
}

// redactStringLocked is redactString for a caller already holding the read lock, so masking a whole
// event takes the lock once rather than once per field. Text shorter than the shortest secret, and
// any secret longer than what remains of the text, are skipped without scanning.
func (m *masker) redactStringLocked(s string) string {
	if m.shortest == 0 || len(s) < m.shortest {
		return s
	}
	for _, sec := range m.strs {
		if len(sec) > len(s) {
			continue
		}
		s = strings.ReplaceAll(s, sec, maskToken)
	}
	return s
}

// redactEvent masks the free-text fields of an event in place, covering a task's captured output,
// message, diff, any string values a play published with set_stats however deeply nested, and the
// play, task, and host name fields, since a secret embedded in a task name or a dynamic host name
// would otherwise reach storage unredacted. Masking is deterministic per value, so a host name that
// contains a secret substring redacts identically on every event and the host matrix still groups.
// It takes the read lock once for the whole event rather than once per field.
func (m *masker) redactEvent(e *event.Event) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shortest == 0 {
		return
	}
	e.Play = m.redactStringLocked(e.Play)
	e.Task = m.redactStringLocked(e.Task)
	e.Host = m.redactStringLocked(e.Host)
	e.Message = m.redactStringLocked(e.Message)
	e.Stdout = m.redactStringLocked(e.Stdout)
	e.Stderr = m.redactStringLocked(e.Stderr)
	e.Diff = m.redactStringLocked(e.Diff)
	for k, v := range e.Outputs {
		e.Outputs[k] = m.redactValueLocked(v)
	}
}

// redactValueLocked returns v with every string it contains redacted, walking nested maps and slices
// so a secret published under a nested set_stats key is still masked. The caller holds the read lock.
func (m *masker) redactValueLocked(v any) any {
	switch t := v.(type) {
	case string:
		return m.redactStringLocked(t)
	case map[string]any:
		for k, val := range t {
			t[k] = m.redactValueLocked(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = m.redactValueLocked(val)
		}
		return t
	default:
		return v
	}
}
