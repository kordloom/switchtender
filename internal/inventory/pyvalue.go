package inventory

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// The native engine builds values with the types Python ends up holding after Ansible reads a
// document, and converts them to the JSON shapes ansible-inventory --list prints only at the end.
// Keeping the Python shape until then matters where Python and JSON disagree: an int and a float
// that print alike, a mapping key that is a number, and a string Ansible will not template.

// unsafeString is a string Ansible marks as not to be templated, written with the !unsafe YAML tag
// or as {"__ansible_unsafe": ...} in JSON. ansible-inventory prints it in that JSON form.
type unsafeString string

// pyNumber is a Python int or float, carried as the text Python writes for it so a large integer
// stays exact and a float keeps the digits Python's repr chooses.
type pyNumber struct {
	// text is the number as Python's repr writes it.
	text string
	// float reports a Python float rather than an int.
	float bool
}

// zero reports whether the number is zero, which is false to Python.
func (n pyNumber) zero() bool {
	if n.float {
		f, err := strconv.ParseFloat(n.text, 64)
		return err == nil && f == 0
	}
	return n.text == "0"
}

// maxIntDigits is the longest integer Python converts from text by default. A longer literal is
// refused by Python 3.11 and later, and is no inventory value anyway.
const maxIntDigits = 4300

// pyInt returns the Python int a decimal string with an optional sign stands for, or false when
// it is not one or is longer than Python converts.
func pyInt(sign int, digits string, base int) (pyNumber, bool) {
	if digits == "" || len(digits) > maxIntDigits {
		return pyNumber{}, false
	}
	v, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return pyNumber{}, false
	}
	if sign < 0 {
		v.Neg(v)
	}
	return pyNumber{text: v.String()}, true
}

// pyIntFromBig returns the Python int v stands for.
func pyIntFromBig(v *big.Int) pyNumber {
	return pyNumber{text: v.String()}
}

// pyFloat returns the Python float f, or false when it is infinite or not a number, which JSON
// cannot carry and an inventory never needs.
func pyFloat(f float64) (pyNumber, bool) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return pyNumber{}, false
	}
	return pyNumber{text: pyFloatRepr(f), float: true}, true
}

// pyFloatRepr writes f the way Python's repr writes a float: the shortest digits that read back as
// the same value, in positional notation when the decimal point falls within sixteen places of the
// first digit and four places after the point, and in exponent notation otherwise, always with a
// decimal point or an exponent so the text reads back as a float rather than an int.
func pyFloatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	// The shortest round-trip digits, as d.ddddde±XX.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mantissa, ".", "", 1)
	decpt := exp + 1
	if decpt > -4 && decpt <= 16 {
		switch {
		case decpt <= 0:
			return sign + "0." + strings.Repeat("0", -decpt) + digits
		case decpt >= len(digits):
			return sign + digits + strings.Repeat("0", decpt-len(digits)) + ".0"
		default:
			return sign + digits[:decpt] + "." + digits[decpt:]
		}
	}
	out := digits[:1]
	if len(digits) > 1 {
		out += "." + digits[1:]
	}
	esign := "+"
	if exp < 0 {
		esign = "-"
		exp = -exp
	}
	return sign + out + "e" + esign + leftPad(strconv.Itoa(exp), 2)
}

// leftPad pads s with leading zeros to at least width characters.
func leftPad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat("0", width-len(s)) + s
}

// mapEntry is one key and value of an orderedMap.
type mapEntry struct {
	// key is the entry's key: a string, or an int as pyNumber.
	key any
	// value is the entry's value.
	value any
}

// orderedMap is a Python dict: keys keep the position of their first insertion and the value of
// their last, so a document that repeats a key reads the way Ansible reads it.
type orderedMap struct {
	// entries are the keys and values in insertion order.
	entries []mapEntry
	// index maps a key's identity to its position in entries.
	index map[string]int
}

// newOrderedMap returns an empty orderedMap.
func newOrderedMap() *orderedMap {
	return &orderedMap{index: map[string]int{}}
}

// keyIdentity returns the identity two equal keys share, or false for a key the engine does not
// carry: anything but a string or an int.
func keyIdentity(k any) (string, bool) {
	switch t := k.(type) {
	case string:
		return "s" + t, true
	case pyNumber:
		if !t.float {
			return "i" + t.text, true
		}
	}
	return "", false
}

// Set stores value under key, keeping the key's first position. It reports false for a key the
// engine does not carry.
func (m *orderedMap) Set(key, value any) bool {
	id, ok := keyIdentity(key)
	if !ok {
		return false
	}
	if i, ok := m.index[id]; ok {
		m.entries[i].value = value
		return true
	}
	m.index[id] = len(m.entries)
	m.entries = append(m.entries, mapEntry{key: key, value: value})
	return true
}

// Get returns the value under a string key.
func (m *orderedMap) Get(key string) (any, bool) {
	i, ok := m.index["s"+key]
	if !ok {
		return nil, false
	}
	return m.entries[i].value, true
}

// Len returns how many keys the map holds.
func (m *orderedMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// listingValue converts a value the engine built into the shape ansible-inventory --list prints and
// ParseListing reads back: strings, json.Number for every number, booleans, nil, slices, and maps
// with string keys, an unsafe string as its {"__ansible_unsafe": ...} object. It fails on a mapping
// whose keys are not all strings or not all ints, which Python's JSON encoders do not print the
// same way across Ansible releases.
func listingValue(v any) (any, error) {
	switch t := v.(type) {
	case nil, bool, string:
		return t, nil
	case unsafeString:
		return map[string]any{"__ansible_unsafe": string(t)}, nil
	case pyNumber:
		return json.Number(t.text), nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			c, err := listingValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case *orderedMap:
		out := make(map[string]any, t.Len())
		ints, strs := 0, 0
		for _, e := range t.entries {
			var key string
			switch k := e.key.(type) {
			case string:
				key = k
				strs++
			case pyNumber:
				key = k.text
				ints++
			}
			c, err := listingValue(e.value)
			if err != nil {
				return nil, err
			}
			out[key] = c
		}
		if ints > 0 && strs > 0 {
			return nil, needsAnsible("a mapping mixes number and string keys, which Ansible " +
				"releases print differently")
		}
		return out, nil
	}
	return nil, needsAnsible("a value of a kind the native engine does not carry")
}
