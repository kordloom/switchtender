package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/util"
)

// unknownFieldPrefix is what encoding/json puts in front of the offending name when a decoder
// configured with DisallowUnknownFields refuses a body. The rest of the message is fixed and the
// name is already JSON quoted, so the field can be lifted straight out and reported to the caller.
const unknownFieldPrefix = "json: unknown field "

// unknownFieldNameCap bounds how much of a rejected field name is echoed back, so a caller cannot
// turn a megabyte of key into a megabyte of error body.
const unknownFieldNameCap = 80

// badBodyMessage is the answer for a body that is not decodable at all: truncated, not an object,
// or the wrong type in a field. It says nothing about the parser's internals on purpose.
const badBodyMessage = "invalid request body"

// decodeStrict decodes one JSON value from body into dst, refuses any field dst does not declare,
// and writes the error response itself. It reports whether the body was accepted, so a handler
// returns the moment it is false.
//
// Strict is the rule for every body whose shape this server defines. encoding/json drops an unknown
// field by default, which turns a misspelled safety control into a silent success: a caller asking
// for a dry run, a host limit, or a hold for approval, and misspelling any of the three, is
// answered 202 and gets a live run with none of them. Nothing in the response distinguishes that
// from the run they asked for, so refusing the request is the only answer that tells them.
//
// A body whose shape belongs to somebody else goes through decodeForeign instead.
func decodeStrict(w http.ResponseWriter, log *zap.Logger, body io.Reader, dst any) bool {
	if err := strictDecode(body, dst); err != nil {
		respondError(w, log, http.StatusBadRequest, decodeErrorMessage(err))
		return false
	}
	return true
}

// decodeStrictOptional is decodeStrict for the handlers whose body may be absent, where nothing
// sent means no overrides rather than a malformed request. An empty body leaves dst untouched; a
// body that is present is held to the same strict rule.
func decodeStrictOptional(w http.ResponseWriter, log *zap.Logger, body io.Reader, dst any) bool {
	if body == nil {
		return true
	}
	err := strictDecode(body, dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	respondError(w, log, http.StatusBadRequest, decodeErrorMessage(err))
	return false
}

// decodeForeign decodes JSON whose shape this server does not define, and so keeps encoding/json's
// default of ignoring what it does not recognize.
//
// This is the named exception to the strict rule, and the only one. A git host's webhook delivery,
// a vendor export, and a model's reply are all written by somebody else and gain fields without
// asking us. Refusing them on an unknown key would break a push the moment GitHub added a field.
// Every waiver of strictness goes through this function so an audit can find them by name.
func decodeForeign(data []byte, dst any) error {
	return json.Unmarshal(data, dst)
}

// strictDecode decodes one JSON value from body into dst with unknown fields refused, and refuses a
// value whose strings hold a NUL character. It is the single place the decoder is configured, so no
// call site can forget either rule.
//
// JSON spells a NUL as the escape \u0000, which is legal in any string, and encoding/json decodes
// it into the byte itself. SQLite stores that byte and PostgreSQL refuses it with SQLSTATE 22021,
// so a NUL in an inventory's, a template's, or a team's name was created on one backend and
// answered 500 on the other. No field this server reads can hold one, so the body is refused and
// the field named.
func strictDecode(body io.Reader, dst any) error {
	var raw json.RawMessage
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return err
	}
	if bytes.Contains(raw, []byte(`\u0000`)) {
		if err := findNUL(raw); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// nulFieldError reports a request body whose strings hold a NUL character.
type nulFieldError struct {
	// path is where the string sits, such as steps[2].name, empty for the body's own value.
	path string
	// inName is true when the NUL is in a field's name rather than its value, and then path names
	// the object holding that field.
	inName bool
}

// Error describes where the NUL sits.
func (e *nulFieldError) Error() string {
	where := strconv.Quote(util.Clip(e.path, unknownFieldNameCap))
	switch {
	case e.inName && e.path == "":
		return "a field name in the request body holds a NUL character"
	case e.inName:
		return "a field name in " + where + " in the request body holds a NUL character"
	case e.path == "":
		return "the request body holds a NUL character"
	default:
		return "the field " + where + " in the request body holds a NUL character"
	}
}

// bodyFrame is one open object or array while findNUL walks a body.
type bodyFrame struct {
	// object is true for an object and false for an array.
	object bool
	// key is the name of the object's field being read.
	key string
	// wantKey is true when the object's next string is a field name.
	wantKey bool
	// index is the position of the array's element being read.
	index int
}

// findNUL walks the JSON document data and returns a nulFieldError for the first string, field name
// or value, that holds a NUL character, or nil when none does. data is already known to be valid
// JSON, so the walk only tracks where it is.
func findNUL(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []bodyFrame
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		top := len(stack) - 1
		if name, ok := tok.(string); ok && top >= 0 && stack[top].object && stack[top].wantKey {
			if strings.ContainsRune(name, 0) {
				return &nulFieldError{path: bodyPath(stack[:top]), inName: true}
			}
			stack[top].key, stack[top].wantKey = name, false
			continue
		}
		if top >= 0 && !stack[top].object && tok != json.Delim(']') {
			stack[top].index++
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, bodyFrame{object: true, wantKey: true})
			case '[':
				stack = append(stack, bodyFrame{index: -1})
			default:
				stack = stack[:top]
				valueRead(stack)
			}
		case string:
			if strings.ContainsRune(t, 0) {
				return &nulFieldError{path: bodyPath(stack)}
			}
			valueRead(stack)
		default:
			valueRead(stack)
		}
	}
}

// valueRead records that the innermost object's current field has its whole value, so the next
// string in it is a name.
func valueRead(stack []bodyFrame) {
	if n := len(stack); n > 0 && stack[n-1].object {
		stack[n-1].wantKey = true
	}
}

// bodyPath renders the position stack describes, as steps[2].name.
func bodyPath(stack []bodyFrame) string {
	var b strings.Builder
	for _, f := range stack {
		if !f.object {
			b.WriteString("[" + strconv.Itoa(f.index) + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(f.key)
	}
	return b.String()
}

// decodeErrorMessage renders a decode failure for the caller. An unknown field is named, because a
// caller who misspelled a control needs to know which word was wrong, and so is a field holding a
// NUL character. Anything else is the generic bad body message.
func decodeErrorMessage(err error) string {
	if name, ok := strings.CutPrefix(err.Error(), unknownFieldPrefix); ok {
		return "unknown field " + util.Clip(name, unknownFieldNameCap) + " in the request body"
	}
	var nul *nulFieldError
	if errors.As(err, &nul) {
		return nul.Error()
	}
	return badBodyMessage
}
