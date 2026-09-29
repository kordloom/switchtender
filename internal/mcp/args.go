package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kordloom/switchtender/internal/run"
)

// object builds a JSON Schema object for a tool's arguments. A tool taking nothing still declares an
// object, since clients expect a schema of that shape.
func object(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// prop builds one JSON Schema property.
func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

// decode reads a tool's arguments into out, treating absent arguments as an empty object so a tool
// with only optional inputs can be called with none.
//
// An argument the tool does not define is refused, not ignored. A model reaching for a control it
// half-remembers writes the name from the tool it knows: check_mode rather than dry_run, extra_vars
// rather than answers, host rather than limit. Dropping those quietly meant the run executed with the
// control unset, which for a preview flag means the change happened for real, and the tool answered
// with a success the model then reported as a preview. Refusing puts the mistake in the one exchange
// where it can still be corrected.
func decode(args json.RawMessage, out any) error {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w%s", err, argHint(err.Error()))
	}
	return nil
}

// deliberatelyUnsupported maps an argument a model is likely to reach for onto what to use instead.
// These are not typos: they are controls another tool really has, and this one withholds on purpose, so
// the refusal says why and where to go rather than only that the name is unknown.
var deliberatelyUnsupported = map[string]string{
	"extra_vars": "extra vars override everything a template and its inventory set, so an agent " +
		"cannot supply them: use answers to fill the survey fields the operator declared, or ask an " +
		"operator to add a survey field for the value you need",
	"vars":       "use answers to fill the survey fields the operator declared",
	"check_mode": "use dry_run for the tool's no-change mode",
	"check":      "use dry_run for the tool's no-change mode",
	"hosts":      "use limit to narrow which hosts a run touches",
	"host":       "use limit to narrow which hosts a run touches",
	"playbook": "a playbook is not chosen here: propose a template from list_templates, or ask an " +
		"operator to enable ad-hoc proposals",
	"command": "a command is not chosen here: propose a template from list_templates, or ask an " +
		"operator to enable ad-hoc proposals",
}

// argHint returns the guidance for a rejected argument, or empty when there is none to give. It reads
// the field name back out of the decoder's own message, which is the only place it appears.
func argHint(msg string) string {
	const marker = `unknown field "`
	at := strings.Index(msg, marker)
	if at == -1 {
		return ""
	}
	rest := msg[at+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end == -1 {
		return ""
	}
	if hint, ok := deliberatelyUnsupported[rest[:end]]; ok {
		return ": " + hint
	}
	return ""
}

// everyHostPatterns was four literal spellings of "no narrowing at all", which an agent walks around
// by writing a fifth: "all:all", "all,all", "*:*" and "all:!nogroup" all reach every host and none of
// them were in the list. The reading now comes from run.WholeInventoryLimit, the same function the
// risk grade uses, so a pattern that widens the run is refused here and graded wide there rather than
// the two disagreeing about what "all" means.

// checkLimit refuses a host pattern that widens what a template may touch rather than narrowing it.
//
// The launch endpoint takes a caller's limit as a replacement for the template's, which is right for a
// person who chose the template and is wrong for an agent working from a menu: a template pinned to one
// canary host could be aimed at an entire inventory by passing a limit, under the same template name
// the audit trail records. Passing "all" was worse than widening, because the risk grade the approval
// policies key on is computed partly from how wide a run reaches, so the widest possible run also
// graded itself down and could fall under the threshold that would otherwise have held it.
//
// Narrowing is left alone. An agent asking to touch one host out of many is the useful case, and it is
// the direction that cannot cause harm the template did not already permit.
func checkLimit(ctx context.Context, c *Client, templateID, limit string) error {
	limit = strings.TrimSpace(limit)
	if limit == "" {
		return nil
	}
	if run.WholeInventoryLimit(limit) {
		return fmt.Errorf("limit %q means every host, which widens the run rather than narrowing it: "+
			"name the hosts this run should touch, or leave limit out to use the template's own target",
			limit)
	}
	var tpl struct {
		Limit string `json:"limit"`
	}
	if err := c.do(ctx, "GET", "/v1/templates/"+escapeID(templateID), nil, &tpl); err != nil {
		return err
	}
	if pinned := strings.TrimSpace(tpl.Limit); pinned != "" && pinned != limit {
		return fmt.Errorf("this template pins its target to %q, so limit cannot be changed: launch it "+
			"as defined, or ask an operator for a template that targets %q", pinned, limit)
	}
	return nil
}

// idArg reads one required string argument by name.
//
// An argument the tool does not define is refused here, not left to decode. decode asks the JSON
// decoder to disallow unknown fields, which does nothing when the destination is a map: every key
// fits a map, so the read tools accepted any argument at all and dropped it. A model asking
// get_run_log for the last hundred lines was handed the whole log and a success, which is the same
// silent drop the refusal rule exists to prevent, so the keys are checked against the one this tool
// defines instead.
func idArg(args json.RawMessage, name string) (string, error) {
	var in map[string]any
	if err := decode(args, &in); err != nil {
		return "", err
	}
	unknown := make([]string, 0, len(in))
	for key := range in {
		if key != name {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		// Sorted so the same arguments always draw the same refusal, whatever order the map ranged
		// in. The message is shaped like the decoder's own so argHint can read the field back out
		// and add the guidance for a control this server withholds on purpose.
		sort.Strings(unknown)
		msg := fmt.Sprintf("unknown field %q", unknown[0])
		return "", fmt.Errorf("invalid arguments: %s%s", msg, argHint(msg))
	}
	value, _ := in[name].(string)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

// escapeID renders an identifier as one URL path segment.
//
// The value reaches here from a model, which means it is untrusted text however plausible it looks. A
// bare id concatenated into a path lets "../../v1/users" or a query string walk to a different
// endpoint than the tool intends, turning a read tool into a request the caller never authorized.
// PathEscape confines it to a single segment.
//
// PathEscape alone is not enough for a value made only of dots, since neither "." nor ".." holds a
// character it escapes. A run id of ".." therefore survived as a real dot segment: the request line
// read /v1/runs/../logs, any server that cleans paths, which net/http's own mux does, resolves that
// to /v1/logs and redirects there, and the client follows carrying the operator's bearer token.
// That is the walk to another endpoint this function exists to prevent, so the dots are
// percent-encoded, which leaves a path cleaner nothing to act on and still decodes back to the id
// the model named.
func escapeID(id string) string {
	escaped := url.PathEscape(id)
	if escaped != "" && strings.Trim(escaped, ".") == "" {
		return strings.ReplaceAll(escaped, ".", "%2E")
	}
	return escaped
}

// listQuery builds the query string for a run listing, omitting the parts the caller left out. A
// non-positive limit is dropped so the server's own default applies rather than a zero page.
//
// The page parameter is named limit because that is the name the runs endpoint reads. Sending any
// other name is not a rejected request, it is an ignored one: the server falls back to its own
// default page and answers 200, so an agent that asked for ten runs quietly received two hundred and
// had no way to tell.
func listQuery(query string, limit int) string {
	values := url.Values{}
	if q := strings.TrimSpace(query); q != "" {
		values.Set("q", q)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	return values.Encode()
}

// clip shortens s to at most max bytes, so a model's free text cannot write an unbounded label.
//
// The bound is spent in bytes, which is what keeps a model from writing a megabyte of label
// whatever alphabet it uses, but the cut lands on a character boundary. The label rides into the
// hash-chained audit trail as the agent's stated reason, and a reason written in any non-ASCII
// alphabet cut through the middle of a character was recorded as invalid UTF-8: a record no reader
// can render and no auditor can quote back. Dropping the trailing partial character costs at most
// three bytes of a reason already past its bound.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut
}

// render turns an API reply into the indented JSON the model reads. Indented rather than compact
// because the reader is a language model, for which structure is easier to follow than density.
func render(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode reply: %w", err)
	}
	return string(data), nil
}
