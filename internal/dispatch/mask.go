package dispatch

import (
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/util"
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
	// mu guards strs, byFirst, and shortest.
	mu sync.RWMutex
	// strs holds the values to redact, longest first, so a longer secret wins over a shorter one
	// starting in the same place.
	strs []string
	// byFirst indexes strs by the first byte of each value, keeping that longest-first order.
	// Redaction asks what begins at a position rather than searching the text once per secret, so
	// this is what makes that a lookup per byte instead of a pass per secret.
	byFirst [256][]int
	// shortest is the byte length of the shortest secret, zero when none are set. Text shorter than
	// it cannot contain any secret, so it is returned untouched.
	shortest int
}

// matchAt returns the length of the longest secret that begins in text at i, or zero when none
// does. It is generic over the two shapes the masker is asked about, a byte chunk of a run's output
// and a string field of an event, so one matcher serves both rather than a second copy of this loop
// or a byte copy of every string.
//
// byFirst keeps strs order, which is longest first, so the first value that matches is the longest
// one that can.
func matchAt[T ~string | ~[]byte](m *masker, text T, i int) int {
	for _, k := range m.byFirst[text[i]] {
		sec := m.strs[k]
		// Shorter values follow, so a value that does not fit is skipped rather than ending the
		// search.
		if len(sec) > len(text)-i {
			continue
		}
		j := 1
		for ; j < len(sec); j++ {
			if text[i+j] != sec[j] {
				break
			}
		}
		if j == len(sec) {
			return len(sec)
		}
	}
	return 0
}

// beginsWith reports whether the secret sec starts with prefix.
func beginsWith(sec string, prefix []byte) bool {
	if len(prefix) > len(sec) {
		return false
	}
	for i := range prefix {
		if sec[i] != prefix[i] {
			return false
		}
	}
	return true
}

// maskBytes is the mask token as bytes, converted once rather than per replacement.
var maskBytes = []byte(maskToken)

// set replaces the masker's secret values, expanding a multi-line secret into its lines as well so
// output that streams a secret one line at a time is still redacted, and into its encoded forms so
// output that prints it as hex, base64, or an escaped string is too. A value shorter than
// minMaskLen or blank is dropped so masking cannot swallow unrelated output.
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
		// A tool that re-encodes a secret prints none of the text above, so the encodings it prints
		// the secret in are masked as well.
		if s := strings.TrimRight(v, "\r\n"); utf8.RuneCountInString(strings.TrimSpace(s)) >= minMaskLen {
			for _, form := range encodedForms(s) {
				add(form)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })

	var byFirst [256][]int
	for i, v := range out {
		byFirst[v[0]] = append(byFirst[v[0]], i)
	}
	shortest := 0
	if len(out) > 0 {
		// out is sorted longest first, so the tail is the shortest.
		shortest = len(out[len(out)-1])
	}

	m.mu.Lock()
	m.strs = out
	m.byFirst = byFirst
	m.shortest = shortest
	m.mu.Unlock()
}

// redact returns p with every known secret replaced by the mask token, allocating only when a
// redaction is made.
//
// One left-to-right pass, taking the longest value that begins at each position and resuming after
// it. A pass per secret, which this was, reads its own output: hiding "CRET" in "CRET000" writes
// "***000", and a second secret of "*000" then matches text the mask token created and was never in
// the stream. It also left the result dependent on the order equal-length values happened to sort
// into. One pass never rescans what it wrote, so what comes out depends on the secret set and the
// text and on nothing else.
func (m *masker) redact(p []byte) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.shortest == 0 || len(p) < m.shortest {
		return p
	}
	first := -1
	for i := range p {
		if matchAt(m, p, i) > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return p
	}
	out := make([]byte, 0, len(p))
	out = append(out, p[:first]...)
	for i := first; i < len(p); {
		if n := matchAt(m, p, i); n > 0 {
			out = append(out, maskBytes...)
			i += n
			continue
		}
		out = append(out, p[i])
		i++
	}
	return out
}

// longest returns the length in bytes of the longest secret, or zero when none are set. The stream
// masker uses it to decide how much of a chunk it must hold back.
func (m *masker) longest() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.strs) == 0 {
		return 0
	}
	// strs is sorted longest first, so the head is the longest.
	return len(m.strs[0])
}

// partialTail returns the length of the longest suffix of buf that is a proper prefix of some secret,
// and so could be the leading part of a secret a later chunk completes. It is at most the longest
// secret minus one byte, since a whole secret occurrence is redacted rather than withheld. A suffix
// that begins no secret returns zero, so ordinary output is not held back. The stream masker withholds
// exactly this suffix when it drains a quiet run, releasing everything before it.
func (m *masker) partialTail(buf []byte) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.strs) == 0 || len(buf) == 0 {
		return 0
	}
	// The longest possible partial is one byte short of the longest secret, and never more than the
	// buffer itself. strs is sorted longest first, so the head bounds it.
	from := max(len(buf)-(len(m.strs[0])-1), 0)
	// Earliest start first, so the longest candidate suffix is the one that answers.
	for at := from; at < len(buf); at++ {
		for _, k := range m.byFirst[buf[at]] {
			sec := m.strs[k]
			if len(buf)-at < len(sec) && beginsWith(sec, buf[at:]) {
				return len(buf) - at
			}
		}
	}
	return 0
}

// releasePoint returns the point at or before cut where the stream can be split without cutting a
// redaction in half.
//
// Releasing the head of a value that is about to be masked puts bytes in the log that redaction can
// no longer see the whole of, so the point moves off any match it lands inside, back to where that
// match begins.
//
// It walks the buffer exactly as redact does: left to right, taking the longest value that begins at
// each position and resuming after it. Walking rather than searching for an occurrence anywhere is
// the whole of it, because those are not the same set. A secret whose own prefix is also its own
// suffix occurs at every position of a long enough run of one character, so "does an occurrence
// straddle this point" answers yes at every point in such a buffer and nothing is ever releasable.
// Redaction never replaces those overlapping occurrences; it takes one and resumes after it. Asking
// about the matches it will actually make leaves a release point after every one of them.
func (m *masker) releasePoint(buf []byte, cut int) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.strs) == 0 {
		return cut
	}
	for i := 0; i < cut; {
		n := matchAt(m, buf, i)
		if n == 0 {
			i++
			continue
		}
		if i+n > cut {
			return i
		}
		i += n
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
	// Release everything except the last keep bytes, then move the release point off any match it
	// lands inside, so the whole of it is redacted together.
	cut := s.mask.releasePoint(buf, len(buf)-keep)
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
	// Release everything before the risky suffix and keep that suffix. The release point moves off
	// any match it lands inside, exactly as next does: a secret's own trailing bytes can be a
	// partial prefix of another secret (SECRET ends in ET, a prefix of ETHER), so the cut from
	// partialTail alone can land inside a match, releasing its head unredacted and carrying its tail
	// forward, where the two halves emit as contiguous plaintext. A fresh slice backs the new tail so
	// it cannot alias the emitted bytes, which redact may return pointing into the old tail.
	cut := s.mask.releasePoint(s.tail, len(s.tail)-w)
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
	first := -1
	for i := 0; i < len(s); i++ {
		if matchAt(m, s, i) > 0 {
			first = i
			break
		}
	}
	if first < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:first])
	for i := first; i < len(s); {
		if n := matchAt(m, s, i); n > 0 {
			b.WriteString(maskToken)
			i += n
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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

// runOwnSecrets collects the secret values a run carries in its own request, so the log masker holds
// back the same values the signed receipt redacts.
//
// The masker learned about secrets from two places, the credentials a run resolves and the inventory
// it targets, and neither sees a value the operator passed straight to the run. An extra var named
// db_password, a survey field collecting a token, or a bash command with an inline password are all
// ordinary parts of a request, and the evidence layer already treats them as secret: the spec record
// is redacted with this same key classifier and assignment scan before it is committed to the chain.
// The log was not, so a playbook that echoed the variable, a run at -vvv, or a task failing and
// printing its module arguments wrote the plaintext into the stored log, streamed it to every live
// viewer, and carried it into every export, while the run's own receipt showed the value redacted.
// The record and the log now read the same value the same way.
func runOwnSecrets(vars map[string]any, command string) []string {
	var out []string
	var walk func(key string, value any)
	walk = func(key string, value any) {
		switch v := value.(type) {
		case map[string]any:
			for k, child := range v {
				walk(k, child)
			}
		case []any:
			for _, child := range v {
				walk(key, child)
			}
		case string:
			// A value under a secret-sounding name is the secret itself.
			if util.SecretKey(key) {
				out = append(out, v)
				return
			}
			// A value under an ordinary name can still carry one inside it: a command line holds
			// whatever that command line holds, which is the same reading the inventory redactor and
			// the receipt already apply to free text.
			_, found := util.RedactAssignments(v, "")
			for _, a := range found {
				out = append(out, a.Value)
			}
		}
	}
	for k, v := range vars {
		walk(k, v)
	}
	// A script's own text is free text for the same reason, and a non-Ansible run carries its secret
	// there rather than in a variable.
	if command != "" {
		_, found := util.RedactAssignments(command, "")
		for _, a := range found {
			out = append(out, a.Value)
		}
	}
	return out
}
