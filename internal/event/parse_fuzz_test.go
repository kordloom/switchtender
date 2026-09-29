package event

import (
	"bytes"
	"testing"
)

// FuzzParse feeds arbitrary bytes to the event stream parser to prove it never panics on malformed
// or hostile callback output.
func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"type":"runner_failed","host":"h","task":"t"}`))
	f.Add([]byte(`{"type":"stats","stats":{"h":{"ok":1}}}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = Parse(bytes.NewReader(data))
	})
}
