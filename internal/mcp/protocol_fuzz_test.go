package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// FuzzServe drives the stdio loop with arbitrary bytes. The reader sits on the other side of a
// pipe from a model, so the properties are survival ones: no input panics the server, the loop
// always terminates at end of input, and every reply it does write is itself one well-formed JSON
// object per line, since a mangled reply would wedge the client the same way a mangled request
// must not wedge this server.
func FuzzServe(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	f.Add([]byte("not json\n\n{\"id\":4}\n"))
	f.Add([]byte(`{"id":[],"method":5}` + "\n"))
	f.Add([]byte{0x00, 0xff, '\n'})
	f.Fuzz(func(t *testing.T, input []byte) {
		echo := Tool{
			Name: "echo", Description: "Echo.",
			InputSchema: map[string]any{"type": "object"},
			Run: func(_ context.Context, args json.RawMessage) (string, error) {
				if len(args) > 1<<10 {
					return "", errors.New("too big")
				}
				return string(args), nil
			},
		}
		var out bytes.Buffer
		srv := NewServer("fuzz", "0.0.0", []Tool{echo})
		if err := srv.Serve(context.Background(), bytes.NewReader(input), &out); err != nil &&
			!strings.Contains(err.Error(), "token too long") {
			t.Fatalf("Serve() error = %v on plain byte input", err)
		}
		for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var reply map[string]any
			if err := json.Unmarshal(line, &reply); err != nil {
				t.Fatalf("server wrote a non-JSON line %q: %v", line, err)
			}
			if reply["jsonrpc"] != "2.0" {
				t.Fatalf("reply %q does not declare jsonrpc 2.0", line)
			}
		}
	})
}
