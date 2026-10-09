package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// decodeLenient decodes an export into v the way the strict decoder does, except that a list entry
// which does not decode on its own is left out with a warning instead of failing the whole document.
//
// One malformed asset used to stop an import outright: a single job template carrying its forks as a
// word refused an export of hundreds. The entry is dropped whole, never imported with the field that
// would not read set to zero, because a zeroed limit or a missing credential changes what the asset
// does, and a skipped asset is visible where a quietly altered one is not.
//
// Containers are types whose values hold lists of assets themselves, such as a Semaphore project or
// an AWX inventory and the related block awxkit nests its hosts under. An entry of one of those
// that does not decode has its own lists cleaned the same way, rather than every asset in it being
// dropped for one bad template, and a container held in a field of another is cleaned in place, so
// one host whose name is a number costs that host and not the inventory and every template on it.
func decodeLenient(data []byte, v any, containers ...reflect.Type) ([]lenientSkip, error) {
	err := decodeNumbers(data, v)
	var typeErr *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &typeErr) {
		return nil, err
	}
	target := reflect.ValueOf(v).Elem()
	cleaned, skipped, ok := cleanObject(data, target.Type(), "", nil, containers)
	if !ok || len(skipped) == 0 {
		return nil, err
	}
	target.Set(reflect.Zero(target.Type()))
	if err := decodeNumbers(cleaned, v); err != nil {
		return nil, err
	}
	return skipped, nil
}

// lenientSkip is one list entry the lenient decoder left out.
type lenientSkip struct {
	// Text is the sentence the report carries for the entry.
	Text string
	// Name is the entry's name when it has one as text, and empty otherwise.
	Name string
	// Path locates the list the entry was in, from the document down: the key of each field on the
	// way, with the position of the container entry the path passes through in a list.
	Path []lenientStep
}

// lenientStep is one field on the path from a document to a list the lenient decoder cleaned.
type lenientStep struct {
	// Key is the field's JSON key.
	Key string
	// Index is the position, among the entries kept, of the container entry the path passes
	// through in this field's list, or -1 for a field that is not a list or is the list itself.
	Index int
}

// stepKeys returns the keys of the steps joined with dots, such as related.hosts.
func stepKeys(steps []lenientStep) string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Key
	}
	return strings.Join(out, ".")
}

// decodeNumbers decodes one JSON value into v, keeping numbers as json.Number so a large integer in
// a variable survives verbatim.
func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// cleanObject returns the JSON object data with every list field of t stripped of the entries that do
// not decode into the list's element type, a record of each entry removed, and whether data was an
// object of that shape at all. path is where data sits in the document.
func cleanObject(data []byte, t reflect.Type, where string, path []lenientStep,
	containers []reflect.Type) ([]byte, []lenientSkip, bool) {
	t = elemType(t)
	var obj map[string]json.RawMessage
	if t.Kind() != reflect.Struct || json.Unmarshal(data, &obj) != nil || obj == nil {
		return data, nil, false
	}
	var skipped []lenientSkip
	for i := range t.NumField() {
		field := t.Field(i)
		key := jsonName(field)
		raw, present := obj[key]
		if key == "" || !present {
			continue
		}
		if field.Type.Kind() != reflect.Slice {
			// A container held in a field, such as the related block an inventory's hosts sit
			// under, is cleaned where it is. Only a value that lost an entry is written back, so a
			// field that decodes as it stands is left exactly as the export wrote it.
			if slices.Contains(containers, elemType(field.Type)) {
				inner, why, ok := cleanObject(raw, field.Type, where+key+".",
					stepInto(path, key, -1), containers)
				if ok && len(why) > 0 {
					obj[key] = inner
					skipped = append(skipped, why...)
				}
			}
			continue
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			continue
		}
		elem := field.Type.Elem()
		kept := make([]json.RawMessage, 0, len(items))
		for idx, item := range items {
			err := decodeNumbers(item, reflect.New(elem).Interface())
			if err == nil {
				kept = append(kept, item)
				continue
			}
			label := where + key + " entry " + assetLabel(item, idx)
			if slices.Contains(containers, elem) {
				inner, why, ok := cleanObject(item, elem, label+": ", stepInto(path, key, len(kept)),
					containers)
				if ok && len(why) > 0 && decodeNumbers(inner, reflect.New(elem).Interface()) == nil {
					kept = append(kept, inner)
					skipped = append(skipped, why...)
					continue
				}
			}
			skipped = append(skipped, lenientSkip{
				Text: fmt.Sprintf("%s was skipped because %s, and the rest of the export imports "+
					"without it", label, decodeProblem(err)),
				Name: entryName(item), Path: stepInto(path, key, -1),
			})
		}
		cleaned, err := json.Marshal(kept)
		if err != nil {
			return data, nil, false
		}
		obj[key] = cleaned
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data, nil, false
	}
	return out, skipped, true
}

// stepInto returns a copy of path with one more step, so sibling paths never share a backing array.
func stepInto(path []lenientStep, key string, index int) []lenientStep {
	return append(slices.Clone(path), lenientStep{Key: key, Index: index})
}

// elemType returns the type behind any pointers, which is how a container is named whether a field
// holds it directly or through a pointer.
func elemType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// jsonName returns the key a struct field decodes from, or empty for a field JSON never fills.
func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	if name, _, _ := strings.Cut(tag, ","); name != "" {
		return name
	}
	return f.Name
}

// assetLabel names a list entry for a warning: its name when it has one, its position otherwise.
func assetLabel(item json.RawMessage, idx int) string {
	if name := entryName(item); name != "" {
		return strconv.Quote(name)
	}
	return "#" + strconv.Itoa(idx+1)
}

// entryName returns a list entry's name when the entry carries one as text, and empty otherwise.
func entryName(item json.RawMessage) string {
	var named struct {
		// Name is the entry's own name, when it has one.
		Name any `json:"name"`
	}
	if json.Unmarshal(item, &named) == nil {
		if s, ok := named.Name.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// decodeProblem says why an entry did not decode in terms of the export rather than of this
// program's types.
func decodeProblem(err error) string {
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		return oneLine(err.Error())
	}
	field := typeErr.Field
	if field == "" {
		field = "a value"
	} else {
		field = "its " + field + " field"
	}
	return fmt.Sprintf("%s is a %s where %s belongs", field, typeErr.Value, kindWord(typeErr.Type))
}

// kindWord names what a Go type holds in the words an export's author uses.
func kindWord(t reflect.Type) string {
	if t == nil {
		return "something else"
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "text"
	case reflect.Slice, reflect.Array:
		return "a list"
	default:
		return "an object"
	}
}

// looseInt is a whole-number field an export may carry as a number or as a string holding one. AWX
// writes numbers, but an export converted by another tool or edited by hand quotes them, and one
// quoted forks value failed the whole document.
type looseInt int

// UnmarshalJSON reads a number or a string holding one, and null or an empty string as zero. Anything
// else is a type error, which leaves the asset holding it out rather than importing it with a zero.
func (n *looseInt) UnmarshalJSON(b []byte) error {
	text := strings.TrimSpace(string(b))
	if text == "null" {
		return nil
	}
	kind := "number " + text
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		text, kind = strings.TrimSpace(s), "string"
		if text == "" {
			*n = 0
			return nil
		}
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		return &json.UnmarshalTypeError{Value: kind, Type: reflect.TypeFor[int]()}
	}
	*n = looseInt(v)
	return nil
}
