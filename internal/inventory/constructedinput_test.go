package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// secretValue is written inside every refused value, so a test can assert no message quotes it.
const secretValue = "SENTINELVALUE"

// TestConstructedInputRefusesTemplateText pins what an input of a constructed inventory may hold.
// Template text anywhere is refused, a name or key may hold no brace, control character, or decoder
// key, and a refusal says where the problem is without quoting the value.
func TestConstructedInputRefusesTemplateText(t *testing.T) {
	t.Parallel()
	lookup := `"{{ lookup('pipe', '` + secretValue + `') }}"`
	tests := []struct {
		In      string
		Want    error
		WantMsg string
	}{{ // Test 0: A clean INI input passes.
		In: "web01 a=1\n[web]\nweb01\n[web:vars]\nb=two\n",
	}, { // Test 1: A clean YAML input with nested data passes.
		In: "all:\n  hosts:\n    h1:\n      a:\n        b: [1, two, {c: three}]\n",
	}, { // Test 2: A clean JSON input passes.
		In: `{"web": {"hosts": {"h1": {"port": 22}}, "vars": {"role": "web"}}}`,
	}, { // Test 3: Template text in a host name.
		In:   "all:\n  hosts:\n    'web{{ x }}':\n",
		Want: ErrConstructedInput, WantMsg: "a host name holds template text",
	}, { // Test 4: Template text in a group name.
		In:   "'g{% if x %}':\n  hosts:\n    h1:\n",
		Want: ErrConstructedInput, WantMsg: "a group name holds template text",
	}, { // Test 5: Template text in a child group name.
		In:   "web:\n  hosts:\n    h1:\n  children:\n    'c{# x #}':\n",
		Want: ErrConstructedInput, WantMsg: "a group name holds template text",
	}, { // Test 6: Template text in a variable name.
		In:   "all:\n  hosts:\n    h1:\n      '{{ x }}': 1\n",
		Want: ErrConstructedInput, WantMsg: `a variable name of host "h1" holds template text`,
	}, { // Test 7: Template text in a nested key.
		In:   "all:\n  hosts:\n    h1:\n      a:\n        '{{ k }}': 1\n",
		Want: ErrConstructedInput, WantMsg: `a key under a in the variables of host "h1"`,
	}, { // Test 8: Template text in a value deep in a list.
		In: "all:\n  hosts:\n    h1:\n      a:\n        b:\n          - x\n" +
			"          - " + lookup + "\n",
		Want: ErrConstructedInput, WantMsg: `variable a.b[1] of host "h1" holds template text`,
	}, { // Test 9: Template text in the variables of all.
		In:   "all:\n  vars:\n    v: " + lookup + "\n  hosts:\n    h1:\n",
		Want: ErrConstructedInput, WantMsg: `variable v of group "all"`,
	}, { // Test 10: Template text in a group with no host.
		In:   "all:\n  hosts:\n    h1:\nempty:\n  vars:\n    v: " + lookup + "\n",
		Want: ErrConstructedInput, WantMsg: `variable v of group "empty"`,
	}, { // Test 11: Template text in a reserved name the listing leaves out.
		In:   "all:\n  hosts:\n    h1:\n      ansible_playbook_python: " + lookup + "\n",
		Want: ErrConstructedInput, WantMsg: `variable ansible_playbook_python of host "h1"`,
	}, { // Test 12: A value marked as not to be templated gets no exemption.
		In:   "all:\n  hosts:\n    h1:\n      v: !unsafe " + lookup + "\n",
		Want: ErrConstructedInput, WantMsg: `variable v of host "h1" holds template text`,
	}, { // Test 13: The header that changes the delimiters counts as template text.
		In:   "all:\n  hosts:\n    h1:\n      v: '#jinja2:variable_start_string:\"[[\"'\n",
		Want: ErrConstructedInput, WantMsg: `variable v of host "h1" holds template text`,
	}, { // Test 14: A lone brace in a host name.
		In:   "all:\n  hosts:\n    'web{x':\n",
		Want: ErrConstructedInput, WantMsg: "a host name holds a brace",
	}, { // Test 15: A control character in a variable name.
		In:   "all:\n  hosts:\n    h1:\n      \"a\\u0007b\": 1\n",
		Want: ErrConstructedInput, WantMsg: "holds a control character",
	}, { // Test 16: The vault decoder key nested in a value.
		In:   "all:\n  hosts:\n    h1:\n      v:\n        __ansible_vault: x\n",
		Want: ErrConstructedInput, WantMsg: "is a key Ansible reads as a tagged value",
	}, { // Test 17: The unsafe decoder key as a variable name.
		In:   "all:\n  vars:\n    __ansible_unsafe: x\n  hosts:\n    h1:\n",
		Want: ErrConstructedInput, WantMsg: "is a key Ansible reads as a tagged value",
	}, { // Test 18: An input of exactly {} is a host named {} and is refused in plain words.
		In:   "{}",
		Want: ErrConstructedInput, WantMsg: "one host named {}",
	}, { // Test 19: A {} that is not the whole input is still refused as a host name.
		In:   "[g]\n{}\n",
		Want: ErrConstructedInput, WantMsg: "a host name holds a brace",
	}, { // Test 20: An input the native engine declines is refused with its reason.
		In:   "all:\n  hosts:\n    h1:\n      v: !vault |\n        $ANSIBLE_VAULT;1.1;AES256\n",
		Want: ErrConstructedInput, WantMsg: "this one cannot be",
	}, { // Test 21: A whole-file vault is refused.
		In:   "$ANSIBLE_VAULT;1.1;AES256\n6231\n",
		Want: ErrConstructedInput, WantMsg: "encrypted with Ansible Vault",
	}, { // Test 22: An input the native engine finds invalid is refused.
		In:   "[web\nh1\n",
		Want: ErrConstructedInput, WantMsg: "this one cannot be",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out, err := ConstructedInput(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ConstructedInput() error = %v, want %v", err, test.Want)
			}
			if err == nil {
				if len(out) == 0 {
					t.Fatal("ConstructedInput() returned no document")
				}
				return
			}
			if errors.Is(err, ErrNeedsAnsible) {
				t.Errorf("ConstructedInput() error = %v, which matches ErrNeedsAnsible", err)
			}
			if !strings.Contains(err.Error(), test.WantMsg) {
				t.Errorf("ConstructedInput() error = %q, want it to hold %q", err, test.WantMsg)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("ConstructedInput() error = %q, which quotes the value", err)
			}
		})
	}
}

// TestConstructedInputMarksEveryString pins the rendering's shape: every string is an unsafe
// object, numbers and booleans keep their type, and a group keeps its own variables.
func TestConstructedInputMarksEveryString(t *testing.T) {
	t.Parallel()
	out, err := ConstructedInput("[web]\nh1 n=5 f=1.5 b=True s=text\n[web:vars]\nrole=web\n")
	if err != nil {
		t.Fatalf("ConstructedInput() error = %v", err)
	}
	for _, want := range []string{
		`"s":{"__ansible_unsafe":"text"}`, `"n":5`, `"f":1.5`, `"b":true`,
		`"web":{"hosts":{"h1":`, `"vars":{"role":{"__ansible_unsafe":"web"}}`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("ConstructedInput() = %s, want it to hold %s", out, want)
		}
	}
}

// TestWholeFileVaultIsMatchedAtTheFirstByte pins that the vault header declines a file only at its
// first byte, as Ansible matches it.
func TestWholeFileVaultIsMatchedAtTheFirstByte(t *testing.T) {
	t.Parallel()
	const reason = "it is a whole file encrypted with Ansible Vault"
	_, err := ResolveNative("$ANSIBLE_VAULT;1.1;AES256\n6231\n")
	if got, _ := NeedsAnsibleReason(err); got != reason {
		t.Errorf("ResolveNative() error = %v, want the vault reason", err)
	}
	_, err = ResolveNative(" $ANSIBLE_VAULT;1.1;AES256\n6231\n")
	if got, _ := NeedsAnsibleReason(err); got == reason {
		t.Errorf("ResolveNative() error = %v, want no vault reason after a space", err)
	}
}

// TestConstructedInputRoundTrip reads every corpus document the native engine reads, renders it as
// a constructed inventory's input, reads the rendering back, and compares the two: the same hosts,
// the same groups, and the same variables of the same types once marker-free strings are unwrapped.
func TestConstructedInputRoundTrip(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(conformanceDir)
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, "needs-ansible-") || strings.HasPrefix(name, "invalid-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(conformanceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		want, err := ResolveNative(string(raw))
		if err != nil {
			t.Fatalf("%s: ResolveNative() error = %v", name, err)
		}
		rendered, err := ConstructedInput(string(raw))
		if errors.Is(err, ErrConstructedInput) {
			// A document holding braces or template text is refused, which the scan test covers.
			continue
		}
		if err != nil {
			t.Fatalf("%s: ConstructedInput() error = %v", name, err)
		}
		got, err := ResolveNative(string(rendered))
		if err != nil {
			t.Fatalf("%s: ResolveNative(rendering) error = %v\n%s", name, err, rendered)
		}
		want.UnwrapUnsafe()
		got.UnwrapUnsafe()
		if diff := cmp.Diff(want.HostGroups(), got.HostGroups(), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s: groups mismatch (-want +got):\n%s", name, diff)
		}
		for _, h := range want.Hosts() {
			if diff := cmp.Diff(want.Vars(h), got.Vars(h), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: host %s variables mismatch (-want +got):\n%s", name, h, diff)
			}
		}
		if diff := cmp.Diff(want.Hosts(), got.Hosts(), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s: hosts mismatch (-want +got):\n%s", name, diff)
		}
		compared++
	}
	if compared < 30 {
		t.Errorf("compared %d corpus documents, want at least 30", compared)
	}
}

// TestUnwrapUnsafeKeepsTemplateText pins the way back: a marker-free unsafe object becomes its
// string at every depth, and one that holds template text keeps its object form.
func TestUnwrapUnsafeKeepsTemplateText(t *testing.T) {
	t.Parallel()
	wrap := func(s string) map[string]any { return map[string]any{unsafeKey: s} }
	l := NewHostListing([]string{"h1"}, map[string]map[string]any{"h1": {
		"plain":  wrap("text"),
		"nested": map[string]any{"list": []any{wrap("a"), wrap("{{ x }}")}},
		"kept":   wrap("{% if x %}"),
		"other":  map[string]any{unsafeKey: "a", "b": wrap("c")},
	}})
	l.UnwrapUnsafe()
	want := map[string]any{
		"plain":  "text",
		"nested": map[string]any{"list": []any{"a", wrap("{{ x }}")}},
		"kept":   wrap("{% if x %}"),
		"other":  map[string]any{unsafeKey: "a", "b": "c"},
	}
	if diff := cmp.Diff(want, l.Vars("h1")); diff != "" {
		t.Errorf("UnwrapUnsafe() mismatch (-want +got):\n%s", diff)
	}
}
