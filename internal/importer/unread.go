package importer

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// envelopeKeys are the bookkeeping fields an export API wraps every object in. They carry no
// configuration, so naming them as unread would bury the fields that matter under noise on every
// single object.
var envelopeKeys = map[string]bool{
	"id": true, "type": true, "url": true, "created": true, "modified": true,
	"natural_key": true, "summary_fields": true,
}

// unreadPaths reports the places a document carries data the importer never looks at.
//
// This exists because the dangerous import report is the clean one. Every other warning tells an
// operator about something the importer saw and decided about. Nothing told them about what it did
// not see at all: a field absent from the struct is absent from the parse, so it was dropped
// without a decision, without a warning, and the summary above counted zero objects left out and
// meant it. An operator reads that as "my estate comes across" and finds out otherwise in
// production.
//
// The set of fields that count as read is taken from the struct tags rather than written down
// beside them. A hand-kept list drifts the moment someone adds a field, and it drifts silently in
// the direction of claiming to read more than it does. Deriving it means adding a field to the
// struct is what marks it read, so the two can never disagree.
//
// Paths are reported by shape rather than by index, so a thousand templates carrying the same
// unread field produce one line instead of a thousand.
func unreadPaths(raw []byte, v any) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	found := map[string]bool{}
	walkUnread(doc, reflect.TypeOf(v), "", found)

	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// walkUnread descends a decoded document beside the type that consumed it, recording paths the type
// has no field for.
func walkUnread(doc any, t reflect.Type, prefix string, found map[string]bool) {
	if t == nil {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch value := doc.(type) {
	case map[string]any:
		// An opaque field is read deliberately: the importer kept the bytes and decided about them
		// elsewhere, so nothing under it is unread.
		if t.Kind() != reflect.Struct || t == reflect.TypeOf(json.RawMessage{}) {
			return
		}
		fields := jsonFields(t)
		for key, child := range value {
			if envelopeKeys[key] {
				continue
			}
			ft, ok := fields[key]
			if !ok {
				found[join(prefix, key)] = true
				continue
			}
			walkUnread(child, ft, join(prefix, key), found)
		}
	case []any:
		if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
			return
		}
		// Every element, not just the first: a field present on one template and absent on the rest
		// is exactly the one worth reporting.
		for _, item := range value {
			walkUnread(item, t.Elem(), prefix+"[]", found)
		}
	}
}

// jsonFields maps a struct's json tag names to the types behind them, descending into embedded
// structs the way the encoding does.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, ok := f.Tag.Lookup("json")
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && (!ok || name == "") {
			for k, v := range jsonFields(f.Type) {
				out[k] = v
			}
			continue
		}
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// join builds a dotted path, keeping the array marker attached to the field it belongs to.
func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// reportUnread warns about the fields an export carries that this importer does not read.
//
// One warning rather than one per field, because a reader who sees forty lines stops reading at the
// fifth. The list is capped for the same reason and says how many it left off, since a truncated
// list that looks complete is the failure this whole function exists to prevent.
func reportUnread(plan *Plan, raw []byte, v any) {
	paths := unreadPaths(raw, v)
	if len(paths) == 0 {
		return
	}
	const show = 12
	shown := paths
	suffix := ""
	if len(shown) > show {
		shown = shown[:show]
		suffix = ", and " + strconv.Itoa(len(paths)-show) + " more"
	}
	plan.warn("this export holds %d field%s this importer does not read, so they are not imported: %s%s",
		len(paths), plural(len(paths)), strings.Join(shown, ", "), suffix)
}
