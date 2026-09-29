package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/util"
)

// RedactedValue stands in for a secret value removed from inventory content.
const RedactedValue = "***"

// Secrets returns the values of secret-looking variables in inventory content, so a host list that
// carries an ansible_password or an API token does not leak it into a run's log or events. A quoted
// value is unwrapped so a masker holds the bare secret and matches it literally in output;
// capturing the surrounding quotes would mask a string the output never contains.
func Secrets(content string) []string {
	_, secrets := rewrite(content)
	return secrets
}

// Redact replaces the value of every secret-looking variable in content, keeping the variable names
// and the host layout so the inventory still reads as one.
//
// An inventory is served to anyone who may read it, and a host list routinely carries an
// ansible_password or a become password inline. Those are credentials, and the rest of the API does
// not hand a credential's material to a reader, so this one should not either. The names stay
// because knowing that a host sets ansible_password is not the secret; its value is.
func Redact(content string) string {
	redacted, _ := rewrite(content)
	return redacted
}

// rewrite returns content with every secret value replaced and the values it replaced, so the copy
// the API serves and the list the run-log masker holds come from one pass and cannot disagree.
//
// Inventory content arrives in three encodings and the value of a variable is only reliably
// found by parsing. A dynamic inventory is stored as the JSON document the dump conversion
// writes, and a user may paste the same from ansible-inventory --list; a static inventory is
// usually YAML; only the INI form has no parser. So a JSON or YAML document is walked structurally
// and classified by key name, and text no parser accepts falls back to matching assignments line by
// line.
func rewrite(content string) (string, []string) {
	if out, secrets, ok := rewriteJSON(content); ok {
		return out, secrets
	}
	if out, secrets, ok := rewriteYAML(content); ok {
		return out, secrets
	}
	var secrets []string
	return scanText(content, &secrets), secrets
}

// rewriteJSON walks content as a JSON document, replacing the value of every secret-bearing key and
// collecting what it replaced. It reports false when content is not a JSON object or array, leaving
// the caller to try the other encodings. Numbers keep their literal form so re-encoding does
// not turn a port into 2.2e+01, and HTML escaping stays off so a host variable holding an
// ampersand is served as the operator wrote it.
func rewriteJSON(content string) (string, []string, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return "", nil, false
	}
	if !json.Valid([]byte(trimmed)) {
		return "", nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", nil, false
	}
	var secrets []string
	doc = redactValue(doc, &secrets)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return "", nil, false
	}
	return buf.String(), secrets, true
}

// redactValue walks a decoded JSON value, replacing the value of every secret-bearing key with the
// marker and scanning ordinary string leaves for an embedded assignment. It returns the value so a
// scrubbed leaf propagates to its parent.
func redactValue(value any, secrets *[]string) any {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if util.SecretKey(key) {
				collectValue(key, child, secrets)
				v[key] = RedactedValue
				continue
			}
			v[key] = redactValue(child, secrets)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = redactValue(child, secrets)
		}
		return v
	case string:
		return scanText(v, secrets)
	default:
		return value
	}
}

// collectValue appends every scalar under a secret-bearing key to secrets, so the run-log masker
// masks a password whether it was stored as a string, a number, or a nested bag of fields. A
// boolean or a null is left out: masking every "true" in a run's output would black out unrelated
// lines. The key travels with the value so a nested bag is judged by the field it sits under.
func collectValue(key string, value any, secrets *[]string) {
	switch v := value.(type) {
	case string:
		if !pathReference(key, v) {
			addSecret(v, secrets)
		}
	case json.Number:
		addSecret(v.String(), secrets)
	case map[string]any:
		for childKey, child := range v {
			collectValue(childKey, child, secrets)
		}
	case []any:
		for _, child := range v {
			collectValue(key, child, secrets)
		}
	}
}

// pathReference reports whether a key names a file location and holds one, in which case the value
// is where the secret lives rather than the secret. The redactor still removes it, because a reader
// has no business knowing which key file a host uses, but the masker must not hold it: a path like
// /home/deploy/.ssh/id_rsa appears throughout ordinary run output and masking it would black out
// lines that carry nothing sensitive. Key material pasted inline instead of referenced is spread
// over several lines, so it is still collected.
func pathReference(key, value string) bool {
	k := strings.ToLower(key)
	if !strings.HasSuffix(k, "_file") && !strings.HasSuffix(k, "_path") {
		return false
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	return strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") ||
		strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") ||
		windowsPath.MatchString(value)
}

// windowsPath matches a value that begins with a Windows drive letter, so a key file referenced on
// a Windows host is recognized as a location too.
var windowsPath = regexp.MustCompile(`^[a-zA-Z]:[\\/]`)

// rewriteYAML walks content as a YAML document, replacing the value of every secret-bearing key and
// collecting what it replaced. It reports false when content is not a document whose root is a
// mapping or a sequence, which is how an INI inventory (a plain multi-line scalar to YAML) and any
// malformed content reach the textual fallback. Every document in a multi-document stream is
// rewritten, so a stream is never truncated to its first document.
func rewriteYAML(content string) (string, []string, bool) {
	dec := yaml.NewDecoder(strings.NewReader(content))
	var docs []*yaml.Node
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, false
		}
		docs = append(docs, &node)
	}
	if !anyStructured(docs) {
		return "", nil, false
	}
	var secrets []string
	for _, doc := range docs {
		redactNode(doc, &secrets)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return "", nil, false
		}
	}
	if err := enc.Close(); err != nil {
		return "", nil, false
	}
	return buf.String(), secrets, true
}

// anyStructured reports whether any decoded document has a mapping or sequence at its root, the
// shape an inventory the YAML plugin accepts always has.
func anyStructured(docs []*yaml.Node) bool {
	for _, doc := range docs {
		for _, root := range doc.Content {
			if root.Kind == yaml.MappingNode || root.Kind == yaml.SequenceNode {
				return true
			}
		}
	}
	return false
}

// redactNode walks a YAML node tree, replacing the value node of every secret-bearing key and
// scanning ordinary scalars for an embedded assignment. Nodes are rewritten in place rather than
// re-marshaled from a decoded map so comments, quoting, and key order survive into what the reader
// is served.
func redactNode(node *yaml.Node, secrets *[]string) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			redactNode(child, secrets)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if util.SecretKey(key.Value) {
				collectNode(key.Value, value, secrets)
				blankNode(value)
				continue
			}
			redactNode(value, secrets)
		}
	case yaml.ScalarNode:
		node.Value = scanText(node.Value, secrets)
	case yaml.AliasNode:
	}
}

// blankNode turns a node into the redaction marker, whatever it held. A whole mapping under a
// secret-bearing key goes too, since a bag of fields is as much a credential as a lone string. The
// anchor stays so an alias elsewhere in the document still resolves, to the marker.
func blankNode(node *yaml.Node) {
	anchor := node.Anchor
	*node = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: RedactedValue, Anchor: anchor}
}

// collectNode appends every scalar under a secret-bearing key to secrets. Booleans, nulls, and file
// references are left out for the same reasons collectValue leaves them out.
func collectNode(key string, node *yaml.Node, secrets *[]string) {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!bool" || node.Tag == "!!null" || pathReference(key, node.Value) {
			return
		}
		addSecret(node.Value, secrets)
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			collectNode(node.Content[i].Value, node.Content[i+1], secrets)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, child := range node.Content {
			collectNode(key, child, secrets)
		}
	case yaml.AliasNode:
	}
}

// scanText replaces the value of every secret-looking assignment in a run of plain text, appending
// each replaced value to secrets when it is not nil. It backs the INI form, which has no parser,
// and also runs over the string leaves of parsed content, where a variable such as a command line
// can carry an assignment of its own.
//
// A non-secret name does not consume the rest of the match: scanning resumes at the start of its
// value, so a secret written after an ordinary assignment on the same line is still found.
// The scan itself lives in util, because a run's variables carry these same assignments inside
// ordinary string values and the audit redactor needs the identical reading of them. What stays here is
// which of the values found belong in the masker's list: a key file's location is redacted from the
// content like anything else, and is not a secret to hunt for in a run's output.
func scanText(text string, secrets *[]string) string {
	out, found := util.RedactAssignments(text, RedactedValue)
	for _, a := range found {
		if !pathReference(a.Name, a.Value) {
			addSecret(a.Value, secrets)
		}
	}
	return out
}

// addSecret appends value to secrets unless it is empty or already there, keeping the masker's list
// free of duplicates from a variable repeated across hosts.
func addSecret(value string, secrets *[]string) {
	if value == "" || secrets == nil {
		return
	}
	for _, have := range *secrets {
		if have == value {
			return
		}
	}
	*secrets = append(*secrets, value)
}
