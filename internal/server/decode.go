package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
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

// strictDecode decodes one JSON value from body into dst with unknown fields refused. It is the
// single place the decoder is configured, so no call site can forget the setting.
func strictDecode(body io.Reader, dst any) error {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// decodeErrorMessage renders a decode failure for the caller. An unknown field is named, because a
// caller who misspelled a control needs to know which word was wrong; anything else is the generic
// bad body message.
func decodeErrorMessage(err error) string {
	if name, ok := strings.CutPrefix(err.Error(), unknownFieldPrefix); ok {
		return "unknown field " + util.Clip(name, unknownFieldNameCap) + " in the request body"
	}
	return badBodyMessage
}
