// Package jsonutil centralizes JSON marshaling so all Railwarden output uses consistent formatting.
package jsonutil

import (
	"encoding/json"
)

// Marshal serializes v to JSON. When pretty is true the output is indented with two spaces.
func Marshal(v any, pretty bool) ([]byte, error) {
	if pretty {
		return json.MarshalIndent(v, "", "  ")
	}
	return json.Marshal(v)
}
