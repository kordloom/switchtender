package util_test

import (
	"encoding/json"
	"testing"

	"github.com/kordloom/switchtender/internal/util"
)

// TestRedactDeepReachesEveryShapeASecretCanTake pins the scrubber against the shapes that walked
// past its predecessor.
//
// The old scrubber asserted a string and skipped anything else, so a secret one level down, a
// secret in an array, and a secret-named key holding a number all survived into every run read a
// viewer can make. The key names a secret whatever its value's type, and a value can hide one at
// any depth.
func TestRedactDeepReachesEveryShapeASecretCanTake(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"nested":      map[string]any{"password": "hunter2"},
		"db_password": []any{"hunter2"},
		"pin":         json.Number("1234"),
		"deep":        map[string]any{"a": []any{map[string]any{"api_key": "sk-live-xyz"}}},
		"assignment":  "psql postgres://u:hunter2@db",
		"innocent":    "nothing to see",
	}
	out, changed := util.RedactDeep(in, "[redacted]")
	if !changed {
		t.Fatal("RedactDeep reported no change over a map full of secrets")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The one assertion that matters: no shape of the secret survives anywhere in the output.
	if bytes := string(encoded); contains(bytes, "hunter2") || contains(bytes, "sk-live-xyz") {
		t.Errorf("a secret survived redaction: %s", bytes)
	}

	// The input is never touched, so a scrubber cannot edit the record it is protecting.
	if got := in["nested"].(map[string]any)["password"]; got != "hunter2" {
		t.Errorf("the source map was mutated: nested password = %v", got)
	}

	// A map with nothing to hide reports no change, so the caller can skip the copy.
	if _, hit := util.RedactDeep(map[string]any{"a": "b", "n": 1}, "[redacted]"); hit {
		t.Error("RedactDeep reported a change over a map holding no secret")
	}
}

// contains is a tiny helper so the assertion above reads as one line.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
