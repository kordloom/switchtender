package dispatch

import (
	"bytes"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/dcadolph/yardmaster/internal/event"
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
	// mu guards secrets.
	mu sync.RWMutex
	// secrets holds the values to redact, longest first so a longer secret is masked before a
	// shorter substring of it.
	secrets [][]byte
}

// set replaces the masker's secret values, expanding a multi-line secret into its lines as well so
// output that streams a secret one line at a time is still redacted. A value shorter than minMaskLen
// or blank is dropped so masking cannot swallow unrelated output.
func (m *masker) set(values []string) {
	seen := make(map[string]struct{})
	var out [][]byte
	add := func(s string) {
		s = strings.TrimRight(s, "\r\n")
		if utf8.RuneCountInString(strings.TrimSpace(s)) < minMaskLen {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, []byte(s))
	}
	for _, v := range values {
		add(v)
		for _, line := range strings.Split(v, "\n") {
			add(line)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })

	m.mu.Lock()
	m.secrets = out
	m.mu.Unlock()
}

// redact returns p with every known secret replaced by the mask token. It returns p unchanged when
// no secrets are set, allocating a new slice only when a redaction is made.
func (m *masker) redact(p []byte) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.secrets) == 0 {
		return p
	}
	out := p
	for _, s := range m.secrets {
		if bytes.Contains(out, s) {
			out = bytes.ReplaceAll(out, s, []byte(maskToken))
		}
	}
	return out
}

// redactString returns s with every known secret replaced by the mask token.
func (m *masker) redactString(s string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.secrets) == 0 || s == "" {
		return s
	}
	for _, sec := range m.secrets {
		s = strings.ReplaceAll(s, string(sec), maskToken)
	}
	return s
}

// redactEvent masks the free-text fields of an event in place, covering a task's captured output,
// message, diff, and any string values a play published with set_stats.
func (m *masker) redactEvent(e *event.Event) {
	m.mu.RLock()
	empty := len(m.secrets) == 0
	m.mu.RUnlock()
	if empty {
		return
	}
	e.Message = m.redactString(e.Message)
	e.Stdout = m.redactString(e.Stdout)
	e.Stderr = m.redactString(e.Stderr)
	e.Diff = m.redactString(e.Diff)
	for k, v := range e.Outputs {
		if sv, ok := v.(string); ok {
			e.Outputs[k] = m.redactString(sv)
		}
	}
}
