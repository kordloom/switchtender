package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// proposeArgs is the argument shape propose_run decodes into, mirrored here so the decoder can be
// exercised without standing a server behind it.
type proposeArgs struct {
	// TemplateID is the template to launch.
	TemplateID string `json:"template_id"`
	// Answers are the survey answers.
	Answers map[string]any `json:"answers,omitempty"`
	// Limit narrows the launch to a host pattern.
	Limit string `json:"limit,omitempty"`
	// DryRun asks for the tool's no-change mode.
	DryRun *bool `json:"dry_run,omitempty"`
	// Reason is why the run is being proposed.
	Reason string `json:"reason,omitempty"`
}

// TestDecodeRefusesAnArgumentTheToolDoesNotDefine pins the rule that a half-remembered control is a
// refusal rather than a silent drop.
//
// This is the difference between a preview and a change. A model that writes check_mode instead of
// dry_run had the argument dropped, the run executed for real, and the tool answered with a success
// the model reported as a no-change preview. The refusal has to name the argument, and where the
// withheld control has a supported replacement it has to say which one.
//
//nolint:funlen // Test function.
func TestDecodeRefusesAnArgumentTheToolDoesNotDefine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Args is the raw arguments object.
		Args string
		// WantErr is whether the decode must be refused.
		WantErr bool
		// WantMentions are substrings the refusal has to carry.
		WantMentions []string
		// WantResult is the decoded value when the decode is allowed.
		WantResult proposeArgs
	}{
		{ // Test 0: Absent arguments decode to the zero value, so an all-optional tool is callable.
			Name: "absent", Args: "",
		},
		{ // Test 1: A JSON null is absent too, which is what several clients send for no arguments.
			Name: "null", Args: "null",
		},
		{ // Test 2: Whitespace is absent as well rather than a parse failure.
			Name: "whitespace", Args: "   \n\t ",
		},
		{ // Test 3: An empty object decodes to the zero value.
			Name: "empty object", Args: "{}",
		},
		{ // Test 4: The ordinary case decodes every declared field.
			Name: "declared fields",
			Args: `{"template_id":"tpl_1","limit":"web01","reason":"disk","answers":{"env":"prod"}}`,
			WantResult: proposeArgs{TemplateID: "tpl_1", Limit: "web01", Reason: "disk",
				Answers: map[string]any{"env": "prod"}},
		},
		{ // Test 5: Extra vars override a template at Ansible's highest precedence, so they are
			// refused with the supported channel named.
			Name: "extra_vars", Args: `{"template_id":"t","extra_vars":{"env":"prod"}}`,
			WantErr: true, WantMentions: []string{"extra_vars", "survey", "answers"},
		},
		{ // Test 6: The shorter spelling of the same control is refused the same way.
			Name: "vars", Args: `{"template_id":"t","vars":{"env":"prod"}}`,
			WantErr: true, WantMentions: []string{"vars", "answers"},
		},
		{ // Test 7: The Ansible name for the preview flag points at dry_run.
			Name: "check_mode", Args: `{"template_id":"t","check_mode":true}`,
			WantErr: true, WantMentions: []string{"check_mode", "dry_run"},
		},
		{ // Test 8: And its short form.
			Name: "check", Args: `{"template_id":"t","check":true}`,
			WantErr: true, WantMentions: []string{"check", "dry_run"},
		},
		{ // Test 9: Naming hosts directly points at limit.
			Name: "hosts", Args: `{"template_id":"t","hosts":"web*"}`,
			WantErr: true, WantMentions: []string{"hosts", "limit"},
		},
		{ // Test 10: The singular spelling too.
			Name: "host", Args: `{"template_id":"t","host":"web01"}`,
			WantErr: true, WantMentions: []string{"host", "limit"},
		},
		{ // Test 11: Choosing the playbook here would author the work rather than launch a template,
			// which is the surface the ad-hoc tool is deliberately kept behind a flag.
			Name: "playbook", Args: `{"template_id":"t","playbook":"site.yml"}`,
			WantErr: true, WantMentions: []string{"playbook", "list_templates", "ad-hoc"},
		},
		{ // Test 12: Same for a command.
			Name: "command", Args: `{"template_id":"t","command":"rm -rf /"}`,
			WantErr: true, WantMentions: []string{"command", "list_templates"},
		},
		{ // Test 13: An argument with no guidance is still refused, just without a pointer.
			Name: "unmapped unknown", Args: `{"template_id":"t","wibble":1}`,
			WantErr: true, WantMentions: []string{"wibble"},
		},
		{ // Test 14: A declared field of the wrong type is refused rather than coerced.
			Name: "wrong type", Args: `{"template_id":123}`,
			WantErr: true, WantMentions: []string{"invalid arguments"},
		},
		{ // Test 15: Arguments that are not an object at all are refused.
			Name: "array", Args: `["tpl_1"]`, WantErr: true, WantMentions: []string{"invalid arguments"},
		},
		{ // Test 16: Truncated JSON is refused, not partly applied.
			Name: "truncated", Args: `{"template_id":"t"`, WantErr: true,
			WantMentions: []string{"invalid arguments"},
		},
		{ // Test 17: A declared field written in another case is still that field, which is the
			// encoding/json rule. Tolerating it errs toward understanding the model rather than
			// refusing a control it did supply, which is the safe direction for this decoder.
			Name: "case differs", Args: `{"Template_ID":"t"}`,
			WantResult: proposeArgs{TemplateID: "t"},
		},
		{ // Test 18: A duplicate key takes the last value rather than being refused, so a model that
			// repeated itself does not get a control it did not intend from the earlier copy.
			Name: "duplicate key", Args: `{"limit":"all","limit":"web01"}`,
			WantResult: proposeArgs{Limit: "web01"},
		},
		{ // Test 19: An explicit dry_run of false is carried as a set pointer, not lost as a zero
			// value, so asking for a real run is distinguishable from not asking at all.
			Name: "explicit false dry run", Args: `{"template_id":"t","dry_run":false}`,
			WantResult: proposeArgs{TemplateID: "t", DryRun: new(bool)},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got proposeArgs
			err := decode(json.RawMessage(test.Args), &got)
			if (err != nil) != test.WantErr {
				t.Fatalf("decode(%s) error = %v, want error: %v", test.Name, err, test.WantErr)
			}
			for _, want := range test.WantMentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal for %s = %v, want it to mention %q", test.Name, err, want)
				}
			}
			if test.WantErr {
				return
			}
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("decoded value (-want +got):\n%s", diff)
			}
		})
	}
}

// TestArgHintReadsTheFieldNameOutOfTheDecoderMessage pins the parser that turns a decoder message
// into guidance. It reads the only place the rejected field name appears, so a message it cannot
// parse must yield no hint rather than a fragment of the message.
func TestArgHintReadsTheFieldNameOutOfTheDecoderMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Message is the decoder message to read.
		Message string
		// WantHint is the guidance, empty when there is none to give.
		WantHint string
	}{
		{ // Test 0: No unknown-field marker means nothing to say.
			Message: "json: cannot unmarshal number into Go value of type string",
		},
		{ // Test 1: An empty message yields nothing.
			Message: "",
		},
		{ // Test 2: A marker with no closing quote is not parsed into a hint.
			Message: `json: unknown field "check_mode`,
		},
		{ // Test 3: A field with no guidance yields nothing rather than a bare restatement.
			Message: `json: unknown field "wibble"`,
		},
		{ // Test 4: A withheld control yields the reason and the replacement.
			Message:  `json: unknown field "check_mode"`,
			WantHint: ": use dry_run for the tool's no-change mode",
		},
		{ // Test 5: An empty field name is not in the table, so no hint.
			Message: `json: unknown field ""`,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := argHint(test.Message); got != test.WantHint {
				t.Errorf("argHint(%q) = %q, want %q", test.Message, got, test.WantHint)
			}
		})
	}
}

// TestIDArgRefusesAnythingThatIsNotANonEmptyString pins the required-identifier check. The value
// arrives from a model, so a missing, blank, or wrongly typed id has to be a refusal: reading it as
// empty would send a request at the collection endpoint instead of the one the tool names.
func TestIDArgRefusesAnythingThatIsNotANonEmptyString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Args is the raw arguments object.
		Args string
		// WantID is the identifier read out, empty when the call must be refused.
		WantID string
	}{
		{Name: "absent arguments", Args: ""},                               // Test 0: Nothing at all is a refusal.
		{Name: "null arguments", Args: "null"},                             // Test 1: A JSON null is a refusal.
		{Name: "empty object", Args: "{}"},                                 // Test 2: An object without the key.
		{Name: "empty string", Args: `{"run_id":""}`},                      // Test 3: A blank id is not an id.
		{Name: "whitespace", Args: `{"run_id":"  \t "}`},                   // Test 4: Nor is whitespace.
		{Name: "number", Args: `{"run_id":42}`},                            // Test 5: A number is not coerced.
		{Name: "boolean", Args: `{"run_id":true}`},                         // Test 6: Nor is a boolean.
		{Name: "null value", Args: `{"run_id":null}`},                      // Test 7: Nor an explicit null.
		{Name: "array", Args: `{"run_id":["a"]}`},                          // Test 8: Nor a one-element array.
		{Name: "object", Args: `{"run_id":{"id":"a"}}`},                    // Test 9: Nor a nested object.
		{Name: "wrong key", Args: `{"id":"run_1"}`},                        // Test 10: A different key is not the id.
		{Name: "malformed", Args: `{"run_id":`},                            // Test 11: Broken JSON is a refusal.
		{Name: "ok", Args: `{"run_id":"run_1"}`, WantID: "run_1"},          // Test 12: The ordinary case.
		{Name: "unicode", Args: `{"run_id":"run_ünï"}`, WantID: "run_ünï"}, // Test 13: Unicode passes.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := idArg(json.RawMessage(test.Args), "run_id")
			if test.WantID == "" {
				if err == nil {
					t.Fatalf("idArg(%s) = %q, want a refusal", test.Name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("idArg(%s) error = %v", test.Name, err)
			}
			if got != test.WantID {
				t.Errorf("idArg(%s) = %q, want %q", test.Name, got, test.WantID)
			}
		})
	}
}

// TestEscapeIDConfinesAnIdentifierToOnePathSegment pins the escaping around an untrusted id. The
// value reaches here from a model however plausible it looks, and a bare id concatenated into a
// path lets it walk to an endpoint the tool never named, carrying the operator's bearer token.
func TestEscapeIDConfinesAnIdentifierToOnePathSegment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// ID is the identifier a model supplied.
		ID string
	}{
		{Name: "traversal", ID: "../../v1/users"},         // Test 0: A climb to the account list.
		{Name: "leading slash", ID: "/v1/users"},          // Test 1: An absolute path.
		{Name: "query string", ID: "run_1?admin=true"},    // Test 2: A smuggled parameter.
		{Name: "fragment", ID: "run_1#frag"},              // Test 3: A smuggled fragment.
		{Name: "encoded slash", ID: "run_1%2f..%2fusers"}, // Test 4: A pre-encoded separator.
		{Name: "space", ID: "run 1"},                      // Test 5: Whitespace in an id.
		{Name: "newline", ID: "run_1\nGET /v1/users"},     // Test 6: A request-splitting attempt.
		{Name: "unicode", ID: "run_ünï"},                  // Test 7: Non-ASCII text.
		{Name: "semicolon", ID: "run_1;jsessionid=x"},     // Test 8: A path parameter.
		{Name: "empty", ID: ""},                           // Test 9: Nothing at all.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := escapeID(test.ID)
			for _, banned := range []string{"/", "?", "#", "\n", " "} {
				if strings.Contains(got, banned) {
					t.Errorf("escapeID(%q) = %q, which still carries %q and can leave the segment",
						test.ID, got, banned)
				}
			}
			// The escaping has to be reversible, or a legitimate id no longer names its own run.
			back, err := url.PathUnescape(got)
			if err != nil {
				t.Fatalf("escapeID(%q) = %q, which is not a valid escaping: %v", test.ID, got, err)
			}
			if back != test.ID {
				t.Errorf("escapeID(%q) round-tripped to %q", test.ID, back)
			}
		})
	}
}

// TestARunIDOfDotDotClimbsOutOfTheRunsEndpoint demonstrates a defect. escapeID leaves a bare ".."
// alone, since PathEscape only escapes the separator, so the request line is /v1/runs/../logs. Any
// HTTP server that cleans paths, which net/http's own mux does, resolves that to /v1/logs and
// redirects there, and the client follows the redirect carrying the operator's bearer token. That is
// exactly the walk to another endpoint escapeID exists to prevent.
func TestARunIDOfDotDotClimbsOutOfTheRunsEndpoint(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("served"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	tool := getRunLogTool(t, testClient(t, ts))
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"run_id":".."}`)); err != nil {
		t.Fatalf("get_run_log error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range paths {
		if !strings.HasPrefix(path, "/v1/runs/") {
			t.Errorf("the request reached %q, outside the runs endpoint the tool named", path)
		}
	}
}

// TestListQueryBuildsOnlyTheNamesTheRunsEndpointReads pins the page and search parameters. A
// misnamed parameter is not a rejected request but an ignored one: the server serves its own default
// page and answers 200, so an agent that asked for ten runs receives two hundred and cannot tell.
func TestListQueryBuildsOnlyTheNamesTheRunsEndpointReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Query is the fielded search the agent supplied.
		Query string
		// Limit is the page size the agent asked for.
		Limit int
		// WantValues is the exact parameter set the query string must encode.
		WantValues url.Values
	}{
		{ // Test 0: Nothing asked for sends no parameters, leaving the server's defaults in force.
			Name: "empty", WantValues: url.Values{},
		},
		{ // Test 1: A whitespace-only search is not a search.
			Name: "blank query", Query: "  \t\n ", WantValues: url.Values{},
		},
		{ // Test 2: A search is trimmed before it travels.
			Name: "trimmed", Query: "  status:failed  ",
			WantValues: url.Values{"q": {"status:failed"}},
		},
		{ // Test 3: Zero is not a page size, so the server's default applies.
			Name: "zero limit", Limit: 0, WantValues: url.Values{},
		},
		{ // Test 4: Nor is a negative one, which would otherwise ask for a zero page.
			Name: "negative limit", Limit: -5, WantValues: url.Values{},
		},
		{ // Test 5: One is the smallest real page.
			Name: "one", Limit: 1, WantValues: url.Values{"limit": {"1"}},
		},
		{ // Test 6: A very large page is passed through for the server to bound, not silently capped
			// here where the agent would never learn of it.
			Name: "huge", Limit: 1 << 30, WantValues: url.Values{"limit": {"1073741824"}},
		},
		{ // Test 7: A search carrying reserved characters is encoded rather than smuggled.
			Name: "reserved characters", Query: "label:env=prod&limit=999",
			WantValues: url.Values{"q": {"label:env=prod&limit=999"}},
		},
		{ // Test 8: Unicode survives the round trip.
			Name: "unicode", Query: "host:web-ünï", WantValues: url.Values{"q": {"host:web-ünï"}},
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := url.ParseQuery(listQuery(test.Query, test.Limit))
			if err != nil {
				t.Fatalf("listQuery produced an unparsable query: %v", err)
			}
			if diff := cmp.Diff(test.WantValues, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("query parameters (-want +got):\n%s", diff)
			}
			if got.Has("page_size") || got.Has("per_page") {
				t.Errorf("the query carries a page parameter the runs endpoint does not read: %v", got)
			}
		})
	}
}

// TestClipBoundsAModelsFreeText pins the bound on the label a model writes. The reason rides into
// the audit trail as a label, so an unbounded one is a model writing unbounded data into the record.
func TestClipBoundsAModelsFreeText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// In is the text to bound.
		In string
		// Max is the byte bound.
		Max int
		// WantResult is the bounded text.
		WantResult string
	}{
		{Name: "empty", In: "", Max: 5, WantResult: ""},                  // Test 0: Nothing stays nothing.
		{Name: "under", In: "abc", Max: 5, WantResult: "abc"},            // Test 1: Short text is untouched.
		{Name: "exact", In: "abcde", Max: 5, WantResult: "abcde"},        // Test 2: The bound itself fits.
		{Name: "over by one", In: "abcdef", Max: 5, WantResult: "abcde"}, // Test 3: One past is cut.
		{Name: "zero bound", In: "abc", Max: 0, WantResult: ""},          // Test 4: A zero bound keeps none.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := clip(test.In, test.Max); got != test.WantResult {
				t.Errorf("clip(%q, %d) = %q, want %q", test.In, test.Max, got, test.WantResult)
			}
		})
	}

	// The bound is spent in bytes, which is what matters for the record: a model writing a megabyte
	// of prose cannot write a megabyte of label whatever alphabet it uses. A bound spent in runes
	// instead would let these thousand three-byte characters through at six hundred bytes.
	//
	// The bound is a ceiling rather than an exact length, because the cut also lands on a character
	// boundary and 200 is not a multiple of this character's width. Nothing valid can be both, so
	// this asserts what the bound means: never over, and not so far under that the reason is lost.
	long := strings.Repeat("な", 1000)
	if got := len(clip(long, 200)); got > 200 || got < 198 {
		t.Errorf("clip bounded a multi-byte reason at %d bytes, want at most 200", got)
	}
}

// TestClipDoesNotCutAMultiByteCharacterInHalf demonstrates a defect. The bound is spent in bytes
// with no regard for rune boundaries, so a reason written in any non-ASCII alphabet is cut mid
// character and the label recorded in the audit trail is not valid UTF-8.
func TestClipDoesNotCutAMultiByteCharacterInHalf(t *testing.T) {
	t.Parallel()
	got := clip(strings.Repeat("な", 1000), 200)
	if !utf8.ValidString(got) {
		t.Errorf("clip produced invalid UTF-8: %q", got)
	}
}

// TestReadToolsIgnoreAnArgumentTheyDoNotDefine demonstrates a defect. decode refuses an unknown
// argument, which is what stops a half-remembered control from being silently dropped, but the
// read tools decode into a map rather than a struct and DisallowUnknownFields has no effect on a
// map. A model asking get_run_log for the last hundred lines receives the whole log and a success,
// which is the same failure mode the refusal rule exists to prevent.
func TestReadToolsIgnoreAnArgumentTheyDoNotDefine(t *testing.T) {
	t.Parallel()
	if _, err := idArg(json.RawMessage(`{"run_id":"run_1","tail":100}`), "run_id"); err == nil {
		t.Error("a read tool accepted an argument it does not define, so the control was dropped")
	}
}

// TestCheckLimitRefusesEveryPatternThatDoesNotNarrow pins the guard on the launch limit, reading
// what a pattern selects rather than which literal string it is.
//
// The launch endpoint takes a caller's limit as a replacement for the template's, so an agent
// handed a template pinned to one canary host could aim it at an entire inventory. Passing a
// whole-inventory pattern is worse than widening: the risk grade the approval policies key on is
// computed partly from how wide a run reaches, so the widest possible run graded itself down and
// could fall under the threshold that would have held it.
func TestCheckLimitRefusesEveryPatternThatDoesNotNarrow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Limit is the pattern the agent proposed.
		Limit string
		// WantRefused is whether the guard must refuse before any request is made.
		WantRefused bool
	}{
		{Name: "the word all", Limit: "all", WantRefused: true},              // Test 0.
		{Name: "uppercase", Limit: "ALL", WantRefused: true},                 // Test 1.
		{Name: "mixed case", Limit: "AlL", WantRefused: true},                // Test 2.
		{Name: "padded", Limit: "  all  ", WantRefused: true},                // Test 3.
		{Name: "star", Limit: "*", WantRefused: true},                        // Test 4.
		{Name: "colon pair", Limit: "all:*", WantRefused: true},              // Test 5.
		{Name: "reversed pair", Limit: "*:all", WantRefused: true},           // Test 6.
		{Name: "repeated", Limit: "all:all", WantRefused: true},              // Test 7.
		{Name: "comma separated", Limit: "all,all", WantRefused: true},       // Test 8.
		{Name: "star pair", Limit: "*:*", WantRefused: true},                 // Test 9.
		{Name: "all less a group", Limit: "all:!nogroup", WantRefused: true}, // Test 10.
		{Name: "only an exclusion", Limit: "!web", WantRefused: true},        // Test 11: Starts from all.
		{Name: "only an intersection", Limit: "&web", WantRefused: true},     // Test 12: Starts from all.
		{Name: "separators only", Limit: ":,:", WantRefused: true},           // Test 13: Selects nothing
		// explicitly, so it still starts from everything.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// A nil client proves the guard refuses before it reaches the API: a case that got past
			// the pattern check would panic on the template read rather than pass quietly.
			err := checkLimit(context.Background(), nil, "tpl_1", test.Limit)
			if (err != nil) != test.WantRefused {
				t.Fatalf("checkLimit(%q) error = %v, want refused: %v", test.Limit, err, test.WantRefused)
			}
			if err != nil && !strings.Contains(err.Error(), "every host") {
				t.Errorf("refusal for %q = %v, want it to say the pattern reaches every host",
					test.Limit, err)
			}
		})
	}
}

// TestCheckLimitFailsClosedWhenTheTemplateCannotBeRead pins the direction the guard errs in. It has
// to read the template to learn whether a target is pinned, and a read it could not make is not
// permission to launch: a store fault or an authorization refusal must stop the proposal, not wave
// it through with the agent's own limit.
func TestCheckLimitFailsClosedWhenTheTemplateCannotBeRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Status is what the template read answers.
		Status int
		// Body is the reply body.
		Body string
	}{
		{Name: "forbidden", Status: http.StatusForbidden, Body: `{"error":"not authorized"}`}, // Test 0.
		{Name: "not found", Status: http.StatusNotFound, Body: `{"error":"no template"}`},     // Test 1.
		{Name: "server fault", Status: http.StatusInternalServerError, Body: `oops`},          // Test 2.
		{Name: "unreadable body", Status: http.StatusOK, Body: `not json`},                    // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.Status)
				_, _ = w.Write([]byte(test.Body))
			}))
			defer ts.Close()
			c, err := NewClient(ts.URL, "st_test_token", 5*time.Second)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			if err := checkLimit(context.Background(), c, "tpl_1", "web01"); err == nil {
				t.Errorf("checkLimit allowed a launch over a template read that %s", test.Name)
			}
		})
	}
}

// TestCheckLimitAllowsOnlyANarrowingPattern pins the direction that is left alone. An agent asking
// to touch one host out of many is the useful case and the one direction that cannot cause harm the
// template did not already permit, so it must not be caught by the guard.
func TestCheckLimitAllowsOnlyANarrowingPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Pinned is the template's own target, empty when it pins none.
		Pinned string
		// Limit is what the agent proposed.
		Limit string
		// WantErr is whether the proposal must be refused.
		WantErr bool
	}{
		{Name: "no limit at all", Pinned: "canary-1", Limit: ""},                // Test 0: Left alone.
		{Name: "unpinned narrowing", Pinned: "", Limit: "web01"},                // Test 1: Allowed.
		{Name: "unpinned group", Pinned: "", Limit: "web:db"},                   // Test 2: Allowed.
		{Name: "unpinned pattern", Pinned: "", Limit: "web-*"},                  // Test 3: Allowed.
		{Name: "same as pinned", Pinned: "canary-1", Limit: "canary-1"},         // Test 4: Allowed.
		{Name: "pinned target padded", Pinned: " canary-1 ", Limit: "canary-1"}, // Test 5: Allowed.
		{Name: "different from pinned", Pinned: "canary-1", Limit: "web01", // Test 6: Refused.
			WantErr: true},
		{Name: "widens a pinned target", Pinned: "canary-1", Limit: "canary-1:web", // Test 7: Refused.
			WantErr: true},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var reads int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads++
				_, _ = fmt.Fprintf(w, `{"id":"tpl_1","limit":%q}`, test.Pinned)
			}))
			defer ts.Close()
			c, err := NewClient(ts.URL, "st_test_token", 5*time.Second)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			err = checkLimit(context.Background(), c, "tpl_1", test.Limit)
			if (err != nil) != test.WantErr {
				t.Fatalf("checkLimit(pinned=%q, limit=%q) error = %v, want error: %v",
					test.Pinned, test.Limit, err, test.WantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "pins") {
				t.Errorf("refusal = %v, want it to name the template's pinned target", err)
			}
			// An empty limit is nothing to check, so the template is never read for it.
			if test.Limit == "" && reads != 0 {
				t.Errorf("the template was read %d time(s) for a launch that named no limit", reads)
			}
		})
	}
}

// TestRenderIndentsTheReplyForAModel pins that an API reply reaches a model as structured text. The
// reader is a language model, for which structure is easier to follow than density.
func TestRenderIndentsTheReplyForAModel(t *testing.T) {
	t.Parallel()
	got, err := render(map[string]any{"id": "run_1", "status": "pending_approval"})
	if err != nil {
		t.Fatalf("render() error = %v", err)
	}
	if !strings.Contains(got, "\n  \"id\"") {
		t.Errorf("render() = %q, want indented JSON", got)
	}
	if _, err := render(make(chan int)); err == nil {
		t.Error("render() encoded a value JSON cannot carry")
	}
}
