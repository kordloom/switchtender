package importer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/template"
)

// TestSafeININameRefusesEveryTokenizingCharacter pins the refusal list the doc comment names. Each
// of these characters changes how Ansible's ini plugin reads a host line, so a name carrying one
// must be dropped rather than written. A hole here is a whole-play redirection, not a cosmetic bug.
func TestSafeININameRefusesEveryTokenizingCharacter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantSafe bool
	}{
		{Name: "empty", In: "", WantSafe: false},                              // Test 0.
		{Name: "plain", In: "web01", WantSafe: true},                          // Test 1.
		{Name: "space", In: "web1 ansible_connection=local", WantSafe: false}, // Test 2.
		{Name: "equals", In: "web1=x", WantSafe: false},                       // Test 3.
		{Name: "hash", In: "web1#c", WantSafe: false},                         // Test 4.
		{Name: "open bracket", In: "[all", WantSafe: false},                   // Test 5.
		{Name: "close bracket", In: "all]", WantSafe: false},                  // Test 6.
		{Name: "newline", In: "web1\nweb2", WantSafe: false},                  // Test 7.
		{Name: "carriage return", In: "web1\rweb2", WantSafe: false},          // Test 8.
		{Name: "tab", In: "web1\tweb2", WantSafe: false},                      // Test 9.
		{Name: "nul", In: "web1\x00", WantSafe: false},                        // Test 10.
		{Name: "delete", In: "web1\x7f", WantSafe: false},                     // Test 11.
		{Name: "unicode letters", In: "wéb01-生产", WantSafe: true},             // Test 12.
		{Name: "dotted fqdn", In: "web1.prod.example.com", WantSafe: true},    // Test 13.
		{Name: "colon port", In: "web1:2222", WantSafe: true},                 // Test 14.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := safeININame(test.In); got != test.WantSafe {
				t.Errorf("safeININame(%q) = %v, want %v", test.In, got, test.WantSafe)
			}
		})
	}
}

// TestRenderINIValueQuotesWhatShlexWouldActOn pins that a value is written as itself only when no
// character in it would be tokenized, and refused outright when it cannot live on one line. A value
// that escapes its own quoting becomes further host variables, which is the injection this guards.
func TestRenderINIValueQuotesWhatShlexWouldActOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantResult string
		WantOK     bool
	}{
		{Name: "plain", In: "local", WantResult: "local", WantOK: true},                  // Test 0.
		{Name: "empty", In: "", WantResult: "", WantOK: true},                            // Test 1.
		{Name: "space", In: "a b", WantResult: `"a b"`, WantOK: true},                    // Test 2.
		{Name: "tab refused", In: "a\tb", WantResult: "", WantOK: false},                 // Test 3.
		{Name: "hash", In: "a#b", WantResult: `"a#b"`, WantOK: true},                     // Test 4.
		{Name: "double quote", In: `a"b`, WantResult: `"a\"b"`, WantOK: true},            // Test 5.
		{Name: "single quote", In: "a'b", WantResult: `"a'b"`, WantOK: true},             // Test 6.
		{Name: "backslash", In: `a\b`, WantResult: `"a\\b"`, WantOK: true},               // Test 7.
		{Name: "closing run", In: `x" y=z`, WantResult: `"x\" y=z"`, WantOK: true},       // Test 8.
		{Name: "newline refused", In: "a\nb", WantResult: "", WantOK: false},             // Test 9.
		{Name: "carriage refused", In: "a\rb", WantResult: "", WantOK: false},            // Test 10.
		{Name: "equals is fine", In: "k=v", WantResult: "k=v", WantOK: true},             // Test 11.
		{Name: "bracket is fine", In: "[all]", WantResult: "[all]", WantOK: true},        // Test 12.
		{Name: "unicode", In: "ünïcode", WantResult: "ünïcode", WantOK: true},            // Test 13.
		{Name: "trailing slash", In: `c:\tmp\`, WantResult: `"c:\\tmp\\"`, WantOK: true}, // Test 14.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, ok := renderINIValue(test.In)
			if ok != test.WantOK {
				t.Fatalf("renderINIValue(%q) ok = %v, want %v", test.In, ok, test.WantOK)
			}
			if got != test.WantResult {
				t.Errorf("renderINIValue(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestQuotedINIValueCannotEscapeItsOwnQuotes pins the escaping rule directly: every backslash and
// double quote inside the value is preceded by a backslash, so the quoted run cannot be closed early
// by the value itself. Counting the escapes is what proves it, since the naive version passed a
// simple space test while still letting a quote out.
func TestQuotedINIValueCannotEscapeItsOwnQuotes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         string
		WantResult string
	}{
		{In: `plain`, WantResult: `"plain"`},         // Test 0.
		{In: `a"`, WantResult: `"a\""`},              // Test 1.
		{In: `\`, WantResult: `"\\"`},                // Test 2.
		{In: `\"`, WantResult: `"\\\""`},             // Test 3.
		{In: `""`, WantResult: `"\"\""`},             // Test 4.
		{In: ``, WantResult: `""`},                   // Test 5.
		{In: `a\\"b`, WantResult: `"a\\\\\"b"`},      // Test 6.
		{In: "héllo\"", WantResult: "\"héllo\\\"\""}, // Test 7: multi-byte runes are not split.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := quoteINIValue(test.In); got != test.WantResult {
				t.Errorf("quoteINIValue(%q) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}

// TestOneLineFoldsAndClips pins that a value interpolated into a warning cannot look like several
// warnings and cannot flood the report. A report an operator reads to decide what is safe must not
// be writable by the document being reported on.
func TestOneLineFoldsAndClips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         string
		WantResult string
	}{
		{Name: "plain", In: "web01", WantResult: "web01"},         // Test 0.
		{Name: "newline folded", In: "a\nb", WantResult: `a\nb`},  // Test 1.
		{Name: "carriage folded", In: "a\rb", WantResult: `a\rb`}, // Test 2.
		{Name: "crlf folded", In: "a\r\nb", WantResult: `a\r\nb`}, // Test 3.
		{Name: "exactly 80", In: strings.Repeat("x", 80),
			WantResult: strings.Repeat("x", 80)}, // Test 4: the boundary is not clipped.
		{Name: "81 clipped", In: strings.Repeat("x", 81),
			WantResult: strings.Repeat("x", 80) + "..."}, // Test 5.
		{Name: "very long clipped", In: strings.Repeat("y", 100000),
			WantResult: strings.Repeat("y", 80) + "..."}, // Test 6.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := oneLine(test.In); got != test.WantResult {
				t.Errorf("oneLine() = %q, want %q", got, test.WantResult)
			}
		})
	}
}

// TestOneLineNeverSplitsARune pins that clipping a long multi-byte name cannot leave a partial rune
// behind, so a warning stays valid UTF-8 whatever the export named its objects.
func TestOneLineNeverSplitsARune(t *testing.T) {
	t.Parallel()
	got := oneLine(strings.Repeat("生", 200))
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("oneLine() = %q, want it clipped", got)
	}
	for i, r := range got {
		if r == '\uFFFD' {
			t.Errorf("oneLine() produced a replacement rune at byte %d: %q", i, got)
		}
	}
}

// TestMapSurveyTypeFallsBackToTextWithoutClaimingExactness pins that only the mappings that really
// are equivalent report exact. An inexact mapping reported as exact is a warning the operator never
// sees, which is how a float survey silently becomes free text.
func TestMapSurveyTypeFallsBackToTextWithoutClaimingExactness(t *testing.T) {
	t.Parallel()
	tests := []struct {
		AWXType   string
		WantType  template.FieldType
		WantExact bool
	}{
		{AWXType: "text", WantType: template.FieldText, WantExact: true},             // Test 0.
		{AWXType: "textarea", WantType: template.FieldText, WantExact: true},         // Test 1.
		{AWXType: "integer", WantType: template.FieldInt, WantExact: true},           // Test 2.
		{AWXType: "multiplechoice", WantType: template.FieldChoice, WantExact: true}, // Test 3.
		{AWXType: "multiselect", WantType: template.FieldChoice, WantExact: true},    // Test 4.
		{AWXType: "float", WantType: template.FieldText, WantExact: false},           // Test 5.
		{AWXType: "", WantType: template.FieldText, WantExact: false},                // Test 6.
		{AWXType: "something_new", WantType: template.FieldText, WantExact: false},   // Test 7.
		{AWXType: "TEXT", WantType: template.FieldText, WantExact: false},            // Test 8: case matters.
		{AWXType: "password", WantType: template.FieldText, WantExact: false},        // Test 9.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.AWXType), func(t *testing.T) {
			t.Parallel()
			gotType, gotExact := mapSurveyType(test.AWXType)
			if gotType != test.WantType || gotExact != test.WantExact {
				t.Errorf("mapSurveyType(%q) = %q, %v, want %q, %v",
					test.AWXType, gotType, gotExact, test.WantType, test.WantExact)
			}
		})
	}
}

// TestChoicesFromReadsBothShapes pins that AWX's two encodings of a choice list, a JSON array and a
// newline separated string, both survive, and that a shape neither of those yields no choices rather
// than a garbled one. A choice list that arrives wrong is a survey nobody can answer.
func TestChoicesFromReadsBothShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		In          any
		WantChoices []string
	}{
		{Name: "nil", In: nil, WantChoices: nil},            // Test 0.
		{Name: "empty list", In: []any{}, WantChoices: nil}, // Test 1.
		{Name: "string list", In: []any{"prod", "stage"},
			WantChoices: []string{"prod", "stage"}}, // Test 2.
		{Name: "mixed list", In: []any{json.Number("1"), true, nil, "x"},
			WantChoices: []string{"1", "true", "", "x"}}, // Test 3: rendered faithfully.
		{Name: "newline string", In: "prod\nstage\n",
			WantChoices: []string{"prod", "stage"}}, // Test 4: the trailing blank is dropped.
		{Name: "blank lines", In: "\n\nprod\n\n  stage  \n",
			WantChoices: []string{"prod", "stage"}}, // Test 5.
		{Name: "empty string", In: "", WantChoices: nil},                             // Test 6.
		{Name: "only blanks", In: "\n \n", WantChoices: nil},                         // Test 7.
		{Name: "number is not a list", In: json.Number("3"), WantChoices: nil},       // Test 8.
		{Name: "object is not a list", In: map[string]any{"a": 1}, WantChoices: nil}, // Test 9.
		{Name: "unicode", In: "生产\nステージ", WantChoices: []string{"生产", "ステージ"}},       // Test 10.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := choicesFrom(test.In)
			if diff := cmp.Diff(test.WantChoices, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("choicesFrom() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSettingsKeyRenamesOnlyWhereInjectionReadsADifferentName pins the translation table between an
// AWX input name and the settings key the injector consumes. A constructor-style wiring bug here is
// invisible: the credential imports, the settings look right, and the injector reads nothing.
func TestSettingsKeyRenamesOnlyWhereInjectionReadsADifferentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind    credential.Kind
		AWXKey  string
		WantKey string
	}{
		{Kind: credential.KindSSHKey, AWXKey: "username", WantKey: "user"},               // Test 0.
		{Kind: credential.KindSSHKey, AWXKey: "become_username", WantKey: "become_user"}, // Test 1.
		{Kind: credential.KindSSHKey, AWXKey: "become_method",
			WantKey: "become_method"}, // Test 2: the ssh kinds keep the method's AWX name.
		{Kind: credential.KindSSHPassword, AWXKey: "username", WantKey: "user"}, // Test 3.
		{Kind: credential.KindSSHPassword, AWXKey: "become_username",
			WantKey: "become_user"}, // Test 4.
		{Kind: credential.KindNetwork, AWXKey: "username", WantKey: "user"}, // Test 5.
		{Kind: credential.KindNetwork, AWXKey: "become_username",
			WantKey: "become_username"}, // Test 6: network does not rename the become user.
		{Kind: credential.KindBecome, AWXKey: "become_method", WantKey: "method"}, // Test 7.
		{Kind: credential.KindBecome, AWXKey: "become_username", WantKey: "user"}, // Test 8.
		{Kind: credential.KindBecome, AWXKey: "username",
			WantKey: "username"}, // Test 9: become does not claim the plain username.
		{Kind: credential.KindAWS, AWXKey: "username", WantKey: "username"}, // Test 10.
		{Kind: credential.KindAWS, AWXKey: "region", WantKey: "region"},     // Test 11.
		{Kind: credential.KindEnv, AWXKey: "host", WantKey: "host"},         // Test 12.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := settingsKey(test.Kind, test.AWXKey); got != test.WantKey {
				t.Errorf("settingsKey(%q, %q) = %q, want %q",
					test.Kind, test.AWXKey, got, test.WantKey)
			}
		})
	}
}

// TestPublicInputPairsIsAnAllowlistNotAMarkerTest pins that only the named non-secret inputs come
// back, whatever else the export carried. AWX masks its own secrets with "$encrypted$", but a custom
// credential type's secret field is not masked, so deciding by the marker alone would print it.
func TestPublicInputPairsIsAnAllowlistNotAMarkerTest(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{
		"username":       "deploy",
		"password":       "hunter2",
		"ssh_key_data":   "-----BEGIN OPENSSH PRIVATE KEY-----",
		"my_secret_leak": "s3cret",
		"token":          "ghp_realtoken",
		"become_method":  "sudo",
		"region":         "us-east-1",
		"host":           "$encrypted$",
		"domain":         "   ",
		"validate_certs": false,
	}
	got := publicInputs(inputs)
	want := []string{"become_method=sudo", "region=us-east-1", "username=deploy",
		"validate_certs=false"}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("publicInputs() mismatch (-want +got):\n%s", diff)
	}
	joined := strings.Join(got, " ")
	for _, secret := range []string{"hunter2", "OPENSSH", "s3cret", "ghp_realtoken"} {
		if strings.Contains(joined, secret) {
			t.Errorf("publicInputs() leaked %q: %q", secret, joined)
		}
	}
}

// TestPublicInputPairsOnNothing pins the empty cases return nothing rather than an empty pair, since
// the callers branch on length to decide whether to warn at all.
func TestPublicInputPairsOnNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		In        map[string]any
		WantCount int
	}{
		{Name: "nil", In: nil, WantCount: 0},                                      // Test 0.
		{Name: "empty", In: map[string]any{}, WantCount: 0},                       // Test 1.
		{Name: "only secrets", In: map[string]any{"password": "x"}, WantCount: 0}, // Test 2.
		{Name: "all masked", In: map[string]any{"username": "$encrypted$"},
			WantCount: 0}, // Test 3.
		{Name: "one public", In: map[string]any{"username": "u"}, WantCount: 1}, // Test 4.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := len(publicInputPairs(test.In)); got != test.WantCount {
				t.Errorf("publicInputPairs() = %d pairs, want %d", got, test.WantCount)
			}
		})
	}
}

// TestCredentialSettingsRefusesOneInputWithoutDiscardingTheRest pins the partial-import promise: an
// input the settings rules refuse is named and skipped, and the good ones still land. Failing the
// whole credential would leave an operator re-entering everything by hand.
func TestCredentialSettingsRefusesOneInputWithoutDiscardingTheRest(t *testing.T) {
	t.Parallel()
	inputs := map[string]any{
		"username":      "deploy",
		"become_method": strings.Repeat("s", 600),
		"region":        "   ",
		"host":          "10.0.0.1",
	}
	settings, refused := credentialSettings(credential.KindSSHKey, inputs)
	wantSettings := map[string]string{"user": "deploy", "host": "10.0.0.1"}
	if diff := cmp.Diff(wantSettings, settings, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("credentialSettings() settings mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"become_method"}, refused, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("credentialSettings() refused mismatch (-want +got):\n%s", diff)
	}
}

// TestCredentialSettingsReturnsNilWhenNothingSurvives pins that a credential whose every public
// input is refused stores no settings map at all, rather than an empty one that would read as
// "settings were imported" to the caller choosing which warning to print.
func TestCredentialSettingsReturnsNilWhenNothingSurvives(t *testing.T) {
	t.Parallel()
	settings, refused := credentialSettings(credential.KindEnv, map[string]any{
		"region": strings.Repeat("r", 600),
	})
	if settings != nil {
		t.Errorf("credentialSettings() settings = %v, want nil", settings)
	}
	if diff := cmp.Diff([]string{"region"}, refused, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("credentialSettings() refused mismatch (-want +got):\n%s", diff)
	}
}

// TestSettingsListIsStableAndSorted pins that the warning text a credential import produces does not
// change order between runs, so two imports of the same export produce the same report.
func TestSettingsListIsStableAndSorted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         map[string]string
		WantResult string
	}{
		{Name: "nil", In: nil, WantResult: ""}, // Test 0.
		{Name: "one", In: map[string]string{"user": "deploy"},
			WantResult: "user=deploy"}, // Test 1.
		{Name: "sorted", In: map[string]string{"user": "d", "become_user": "root", "host": "h"},
			WantResult: "become_user=root, host=h, user=d"}, // Test 2.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := settingsList(test.In); got != test.WantResult {
				t.Errorf("settingsList() = %q, want %q", got, test.WantResult)
			}
		})
	}
}

// TestHasInputTreatsAMaskedSecretAsSet pins the one thing hasInput must know: AWX writes a set
// secret as the literal "$encrypted$", so the field is present and unreadable. Reading it as unset
// would map a password machine credential to a key one and import the wrong kind.
func TestHasInputTreatsAMaskedSecretAsSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Inputs  map[string]any
		Key     string
		WantHas bool
	}{
		{Name: "absent", Inputs: map[string]any{}, Key: "password", WantHas: false}, // Test 0.
		{Name: "nil map", Inputs: nil, Key: "password", WantHas: false},             // Test 1.
		{Name: "masked", Inputs: map[string]any{"password": "$encrypted$"},
			Key: "password", WantHas: true}, // Test 2.
		{Name: "empty string", Inputs: map[string]any{"password": ""},
			Key: "password", WantHas: false}, // Test 3.
		{Name: "whitespace", Inputs: map[string]any{"password": "   "},
			Key: "password", WantHas: false}, // Test 4.
		{Name: "real value", Inputs: map[string]any{"ssh_key_data": "-----BEGIN"},
			Key: "ssh_key_data", WantHas: true}, // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := hasInput(test.Inputs, test.Key); got != test.WantHas {
				t.Errorf("hasInput(%v, %q) = %v, want %v",
					test.Inputs, test.Key, got, test.WantHas)
			}
		})
	}
}

// TestParseExtraVarsAcceptsBothEncodingsAndRefusesTheRest pins that an extra vars string arriving as
// YAML or JSON becomes a map, that the several ways of writing "nothing" all become nil, and that a
// document which is not a mapping is an error the caller can report rather than silently no vars.
func TestParseExtraVarsAcceptsBothEncodingsAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantVars map[string]any
		WantErr  bool
	}{
		{Name: "empty", In: "", WantVars: nil},                    // Test 0.
		{Name: "whitespace", In: "  \n\t ", WantVars: nil},        // Test 1.
		{Name: "yaml doc marker", In: "---", WantVars: nil},       // Test 2.
		{Name: "spaced doc marker", In: "  ---  ", WantVars: nil}, // Test 3.
		{Name: "empty json object", In: "{}", WantVars: nil},      // Test 4.
		{Name: "json", In: `{"env":"prod","n":2}`,
			WantVars: map[string]any{"env": "prod", "n": 2}}, // Test 5.
		{Name: "yaml", In: "env: prod\nn: 2\n",
			WantVars: map[string]any{"env": "prod", "n": 2}}, // Test 6.
		{Name: "yaml null", In: "null", WantVars: nil}, // Test 7.
		{Name: "unicode key", In: "生产: prod",
			WantVars: map[string]any{"生产": "prod"}}, // Test 8.
		{Name: "scalar refused", In: "just a string", WantErr: true}, // Test 9.
		{Name: "list refused", In: "- a\n- b", WantErr: true},        // Test 10.
		{Name: "broken yaml", In: "a: [1, 2", WantErr: true},         // Test 11.
		{Name: "tab indent", In: "a:\n\tb: 1", WantErr: true},        // Test 12.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := parseExtraVars(test.In)
			if (err != nil) != test.WantErr {
				t.Fatalf("parseExtraVars(%q) error = %v, want error %v", test.In, err, test.WantErr)
			}
			if test.WantErr {
				return
			}
			if diff := cmp.Diff(test.WantVars, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parseExtraVars(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestDecodeVarsNeverFailsAndNeverGuesses pins that host variables arriving as a JSON object, as a
// YAML string, or as something unreadable each produce the map or nothing at all. A partially
// decoded variable set is the shape that changes what a play does without saying so.
func TestDecodeVarsNeverFailsAndNeverGuesses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantVars map[string]any
	}{
		{Name: "absent", In: "", WantVars: nil},                                    // Test 0.
		{Name: "json null", In: "null", WantVars: nil},                             // Test 1.
		{Name: "json object", In: `{"a":"b"}`, WantVars: map[string]any{"a": "b"}}, // Test 2.
		{Name: "json empty object", In: `{}`, WantVars: map[string]any{}},          // Test 3.
		{Name: "yaml in a string", In: `"a: b\nc: d\n"`,
			WantVars: map[string]any{"a": "b", "c": "d"}}, // Test 4.
		{Name: "json in a string", In: `"{\"a\": \"b\"}"`,
			WantVars: map[string]any{"a": "b"}}, // Test 5.
		{Name: "empty string", In: `""`, WantVars: nil},               // Test 6.
		{Name: "scalar string", In: `"not a mapping"`, WantVars: nil}, // Test 7.
		{Name: "bare number", In: `42`, WantVars: nil},                // Test 8.
		{Name: "array", In: `["a"]`, WantVars: nil},                   // Test 9.
		{Name: "malformed", In: `{`, WantVars: nil},                   // Test 10.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			var raw json.RawMessage
			if test.In != "" {
				raw = json.RawMessage(test.In)
			}
			got := decodeVars(raw)
			if diff := cmp.Diff(test.WantVars, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("decodeVars(%s) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestDecodeVarsKeepsBigIntegersExact pins that a host variable holding an integer past 2^53 arrives
// verbatim rather than through float64. An asset tag or an account id reformatted into scientific
// notation is a host variable that no longer matches the thing it names.
func TestDecodeVarsKeepsBigIntegersExact(t *testing.T) {
	t.Parallel()
	got := decodeVars(json.RawMessage(`{"account":90071992547409931}`))
	if diff := cmp.Diff("90071992547409931", jsonScalarString(got["account"])); diff != "" {
		t.Errorf("decodeVars() big integer mismatch (-want +got):\n%s", diff)
	}
}

// TestSplitAWXTagsDropsTheBlanksACommaLeaves pins that a trailing or doubled comma does not become a
// tag. An empty tag reaches ansible-playbook as --tags "a,,b", which selects differently than the
// operator wrote.
func TestSplitAWXTagsDropsTheBlanksACommaLeaves(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		In       string
		WantTags []string
	}{
		{Name: "empty", In: "", WantTags: nil},                             // Test 0.
		{Name: "whitespace", In: "   ", WantTags: nil},                     // Test 1.
		{Name: "only commas", In: ",,,", WantTags: nil},                    // Test 2.
		{Name: "one", In: "deploy", WantTags: []string{"deploy"}},          // Test 3.
		{Name: "two", In: "a,b", WantTags: []string{"a", "b"}},             // Test 4.
		{Name: "spaced", In: " a , b ", WantTags: []string{"a", "b"}},      // Test 5.
		{Name: "trailing comma", In: "a,b,", WantTags: []string{"a", "b"}}, // Test 6.
		{Name: "doubled comma", In: "a,,b", WantTags: []string{"a", "b"}},  // Test 7.
		{Name: "unicode", In: "生产,ステージ", WantTags: []string{"生产", "ステージ"}}, // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := splitAWXTags(test.In)
			if diff := cmp.Diff(test.WantTags, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("splitAWXTags(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestProjectNameQualifiesOnlyWhenItHasTo pins that a Semaphore repository name is joined to its
// project only when the two differ, so imported project names stay distinct across projects without
// producing "infra/infra" for the ordinary single-repository case.
func TestProjectNameQualifiesOnlyWhenItHasTo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Project    string
		Repo       string
		WantResult string
	}{
		{Project: "ops", Repo: "infra", WantResult: "ops/infra"}, // Test 0.
		{Project: "ops", Repo: "", WantResult: "ops"},            // Test 1.
		{Project: "ops", Repo: "ops", WantResult: "ops"},         // Test 2.
		{Project: "", Repo: "infra", WantResult: "/infra"},       // Test 3.
		{Project: "", Repo: "", WantResult: ""},                  // Test 4.
		{Project: "生产", Repo: "基础", WantResult: "生产/基础"},         // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := projectName(test.Project, test.Repo); got != test.WantResult {
				t.Errorf("projectName(%q, %q) = %q, want %q",
					test.Project, test.Repo, got, test.WantResult)
			}
		})
	}
}

// TestMapSemaphoreKeyAndVarType pins the Semaphore type tables. A key type mapped to the wrong kind
// imports a credential that the injector will not materialize, and the operator finds out at run
// time rather than in the report.
func TestMapSemaphoreKeyAndVarType(t *testing.T) {
	t.Parallel()
	keyTests := []struct {
		KeyType   string
		WantKind  credential.Kind
		WantExact bool
	}{
		{KeyType: "ssh", WantKind: credential.KindSSHKey, WantExact: true},         // Test 0.
		{KeyType: "login_password", WantKind: credential.KindEnv, WantExact: true}, // Test 1.
		{KeyType: "none", WantKind: credential.KindEnv, WantExact: false},          // Test 2.
		{KeyType: "", WantKind: credential.KindEnv, WantExact: false},              // Test 3.
		{KeyType: "SSH", WantKind: credential.KindEnv, WantExact: false},           // Test 4: case matters.
	}
	for testNum, test := range keyTests {
		t.Run(fmt.Sprintf("test %d key %s", testNum, test.KeyType), func(t *testing.T) {
			t.Parallel()
			gotKind, gotExact := mapSemaphoreKey(test.KeyType)
			if gotKind != test.WantKind || gotExact != test.WantExact {
				t.Errorf("mapSemaphoreKey(%q) = %q, %v, want %q, %v",
					test.KeyType, gotKind, gotExact, test.WantKind, test.WantExact)
			}
		})
	}

	varTests := []struct {
		VarType  string
		WantType template.FieldType
	}{
		{VarType: "int", WantType: template.FieldInt},      // Test 0.
		{VarType: "enum", WantType: template.FieldChoice},  // Test 1.
		{VarType: "string", WantType: template.FieldText},  // Test 2.
		{VarType: "", WantType: template.FieldText},        // Test 3.
		{VarType: "unknown", WantType: template.FieldText}, // Test 4.
	}
	for testNum, test := range varTests {
		t.Run(fmt.Sprintf("test %d var %s", testNum, test.VarType), func(t *testing.T) {
			t.Parallel()
			if got := mapSemaphoreVarType(test.VarType); got != test.WantType {
				t.Errorf("mapSemaphoreVarType(%q) = %q, want %q", test.VarType, got, test.WantType)
			}
		})
	}
}

// TestDedupeStringsCollapsesOnlyAdjacentRepeats pins the helper the workflow importer uses after
// sorting, so a node reached by both a success and an always edge is depended on once. It is only
// correct on sorted input, which is how it is called.
func TestDedupeStringsCollapsesOnlyAdjacentRepeats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		In         []string
		WantResult []string
	}{
		{Name: "nil", In: nil, WantResult: nil},                                  // Test 0.
		{Name: "one", In: []string{"a"}, WantResult: []string{"a"}},              // Test 1.
		{Name: "pair repeat", In: []string{"a", "a"}, WantResult: []string{"a"}}, // Test 2.
		{Name: "sorted", In: []string{"a", "a", "b", "b", "b", "c"},
			WantResult: []string{"a", "b", "c"}}, // Test 3.
		{Name: "no repeats", In: []string{"a", "b"}, WantResult: []string{"a", "b"}}, // Test 4.
		{Name: "all same", In: []string{"x", "x", "x"}, WantResult: []string{"x"}},   // Test 5.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := dedupeStrings(append([]string(nil), test.In...))
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("dedupeStrings(%v) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestNodeKeyForIDIsTheOneSpelling pins that the key a workflow node registers and the key an edge
// resolves against come from the same function. Two spellings of the same id would leave every edge
// unresolvable and refuse workflows that were fine.
func TestNodeKeyForIDIsTheOneSpelling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In         int64
		WantResult string
	}{
		{In: 0, WantResult: "id-0"},                                     // Test 0.
		{In: 1, WantResult: "id-1"},                                     // Test 1.
		{In: -3, WantResult: "id--3"},                                   // Test 2.
		{In: 9223372036854775807, WantResult: "id-9223372036854775807"}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := nodeKeyForID(test.In); got != test.WantResult {
				t.Errorf("nodeKeyForID(%d) = %q, want %q", test.In, got, test.WantResult)
			}
		})
	}
}
