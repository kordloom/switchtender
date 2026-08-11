package mcp

import (
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
func decode(args json.RawMessage, out any) error {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if err := json.Unmarshal(args, out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
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
