package audit

import (
	"encoding/json"
	"fmt"
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
	// A fractional number, an exponent, and an integer above 2^53 make jcs.Serialize fail, which
	// once made the digest fall back to the raw body and leak the secret beside them.
	f.Add([]byte(`{"password":"hunter2","timeout":1.5}`))
	f.Add([]byte(`{"secret":"s","rate":1e3}`))
	f.Add([]byte(`{"token":"t","big":9007199254740993}`))
	f.Add([]byte(`{"ansible_become_password":"x","host":"web"}`))
	f.Add([]byte(`{"content":"ansible_password=hunter2\nweb ansible_host=10.0.0.1"}`))
	f.Add([]byte(`{"repo_url":"https://user:tok@github.com/o/r.git"}`))
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

// TestContentDigestNoSecretLeak pins the specific leaks the harden sweep found: a secret must not
// influence the digest even when the body carries a JCS-unserializable number, when the secret sits
// under a stem-matching key, when it is embedded in the free-text inventory content, or when it is a
// credential inside a URL. Each pair differs only in the secret; the digests must match.
func TestContentDigestNoSecretLeak(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		A, B string
	}{{ // A float makes jcs.Serialize fail; the old code then digested the raw body with the secret.
		Name: "float beside password",
		A:    `{"password":"left","timeout":1.5}`,
		B:    `{"password":"right","timeout":1.5}`,
	}, { // An integer past 2^53 also fails jcs.Serialize.
		Name: "big int beside secret",
		A:    `{"secret":"aaa","n":9007199254740993}`,
		B:    `{"secret":"bbb","n":9007199254740993}`,
	}, { // A stem-matching key, not an exact one, must still redact.
		Name: "substring secret key",
		A:    `{"ansible_become_password":"aaa","host":"web"}`,
		B:    `{"ansible_become_password":"bbb","host":"web"}`,
	}, { // The inventory content is free text that can embed a connection secret.
		Name: "embedded inventory secret",
		A:    `{"content":"web ansible_password=aaa"}`,
		B:    `{"content":"web ansible_password=bbb"}`,
	}, { // A credential inside a URL must not survive into the digest.
		Name: "url userinfo",
		A:    `{"repo_url":"https://user:aaa@github.com/o/r.git"}`,
		B:    `{"repo_url":"https://user:bbb@github.com/o/r.git"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			a, b := ContentDigestOf([]byte(test.A)), ContentDigestOf([]byte(test.B))
			if a != b {
				t.Errorf("digests differ though only the secret changed: %q vs %q; the secret leaked", a, b)
			}
			if a == "" || !strings.HasPrefix(a, "sha256:") {
				t.Errorf("digest = %q, want a sha256 digest", a)
			}
		})
	}
}
