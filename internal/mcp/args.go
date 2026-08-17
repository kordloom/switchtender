package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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

// everyHostPatterns are the Ansible spellings of "no narrowing at all".
var everyHostPatterns = map[string]bool{"all": true, "*": true, "all:*": true, "*:all": true}

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
	if everyHostPatterns[strings.ToLower(limit)] {
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
func idArg(args json.RawMessage, name string) (string, error) {
	var in map[string]any
	if err := decode(args, &in); err != nil {
		return "", err
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
func escapeID(id string) string {
	return url.PathEscape(id)
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
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
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
