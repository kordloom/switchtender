package witness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FuzzCheck feeds the pure check arbitrary feeds, twice in a row through the checkpoint it
// produces. The feed is the one input a hostile operator fully controls, so the property is
// blunt: no input may panic the witness, the checkpoint it hands back never remembers more than
// the cap, and folding a checkpoint back in never fails against its own server.
func FuzzCheck(f *testing.F) {
	f.Add([]byte(`[{"beat":1,"at":"2026-08-10T00:00:00Z","seq":3,"head":"aa"}]`))
	f.Add([]byte(`[{"beat":1,"seq":1,"head":"aa"},{"beat":3,"seq":9,"head":"bb"}]`))
	f.Add([]byte(`[{"beat":-5,"seq":-9,"head":""}]`))
	f.Add([]byte(`[{"beat":9007199254740993,"seq":1,"head":"aa"},{"beat":1,"seq":2,"head":"bb"}]`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var beats []Beat
		if err := json.Unmarshal(raw, &beats); err != nil {
			return
		}
		const server = "https://fuzz.example"
		now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
		next, _, err := Check(nil, server, beats, now)
		if err != nil {
			return
		}
		if next == nil {
			t.Fatal("Check() returned no checkpoint and no error")
		}
		if len(next.Recent) > FeedLimit {
			t.Fatalf("checkpoint remembers %d beats, over the %d cap", len(next.Recent), FeedLimit)
		}
		// The checkpoint a check produced must be usable for the next check of the same server.
		again, _, err := Check(next, server, beats, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("Check() refused its own checkpoint: %v", err)
		}
		if len(again.Recent) > FeedLimit {
			t.Fatalf("second checkpoint remembers %d beats, over the %d cap", len(again.Recent), FeedLimit)
		}
	})
}

// FuzzLoad opens arbitrary bytes as a checkpoint state file with a pinned signer. The state file
// is this witness's own memory, but a hostile party who can write the disk must not be able to
// crash the witness or slip an unsigned memory past the pin: any input either errors or verifies
// against the pinned key.
func FuzzLoad(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"server":"https://s","last_beat":1,"recent":{"1":{"seq":1,"head":"aa"}}}`))
	f.Add([]byte(`{"public_key":"zz","sig":"zz"}`))
	f.Add([]byte(`not json`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		const pin = "0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"
		path := filepath.Join(t.TempDir(), "state.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(path, pin)
		if err != nil {
			return
		}
		// Load succeeded: the only acceptable success on arbitrary bytes is the empty first-run
		// state, since no fuzz input carries a signature by the pinned key.
		if c != nil {
			t.Fatalf("Load() accepted arbitrary bytes as a signed checkpoint: %+v", c)
		}
	})
}
