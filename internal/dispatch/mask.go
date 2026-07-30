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
	// Redact the whole buffer before deciding what to release, so a secret lying across the release
	// point is replaced rather than being split between the emitted part and the withheld part.
	red := s.mask.redact(buf)
	if keep == 0 {
		s.tail = nil
		return red
	}
	if len(red) <= keep {
		// Nothing can be released yet: every byte so far could still begin a secret the next chunk
		// finishes. A copy is taken because the caller owns chunk and may reuse it.
		s.tail = append(s.tail[:0], red...)
		return nil
	}
	// What is withheld is the redacted tail. A secret only partly arrived does not match yet, so its
	// prefix is carried forward intact and matches once the rest lands.
	cut := len(red) - keep
	s.tail = append(s.tail[:0], red[cut:]...)
	return red[:cut]
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
