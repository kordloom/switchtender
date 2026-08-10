package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzContentDigestOf holds the digest to its contract on arbitrary bodies: it never panics, it is
// deterministic, an empty body carries no digest, and above all a secret value never influences the
// digest, since the digest is served in exports and a digest a secret can influence is an offline
// brute-force target.
func FuzzContentDigestOf(f *testing.F) {
	f.Add([]byte(`{"name":"web","password":"hunter2"}`))
	f.Add([]byte(`{"vars":{"token":"t"},"list":[{"secret":"s"},{"ok":1}]}`))
	f.Add([]byte(`{"fields":{"user":"u","password":"p"}}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"PASSWORD":"case"}`))
	f.Add([]byte(`{"nested":{"deep":{"private_key":"k"}}}`))
	f.Add([]byte{0xff, 0xfe, 0x00})
	f.Fuzz(func(t *testing.T, body []byte) {
		got := ContentDigestOf(body)
		if len(body) == 0 {
			if got != "" {
				t.Fatalf("empty body digest = %q, want absent", got)
			}
			return
		}
		if !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
			t.Fatalf("digest %q is not sha256: plus 64 hex characters", got)
		}
		if again := ContentDigestOf(body); again != got {
			t.Fatalf("digest is not deterministic: %q then %q", got, again)
		}

		// The redaction invariant: when the body is a JSON object, two bodies that differ only in
		// the value under a secret key must digest identically, because the value was replaced
		// before the digest was taken. If they ever differ, the secret leaked into the digest.
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
			return
		}
		one, two := map[string]any{}, map[string]any{}
		for k, v := range parsed {
			one[k], two[k] = v, v
		}
		one["password"], two["password"] = "left-secret", "right-secret"
		oneRaw, err1 := json.Marshal(one)
		twoRaw, err2 := json.Marshal(two)
		if err1 != nil || err2 != nil {
			return
		}
		if len(oneRaw) > MaxCanonicalDigestBytes || len(twoRaw) > MaxCanonicalDigestBytes {
			return
		}
		if d1, d2 := ContentDigestOf(oneRaw), ContentDigestOf(twoRaw); d1 != d2 {
			t.Fatalf("digests differ on bodies equal except for a password value: %q vs %q; "+
				"the secret influenced the digest", d1, d2)
		}
	})
}
