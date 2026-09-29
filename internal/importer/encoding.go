package importer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// textOf returns an export as UTF-8 text, removing a byte order mark and transcoding UTF-16.
//
// The browser assessment reads a dropped file through the platform's text decoder, which honors a
// byte order mark, so an export saved as UTF-8 with a mark, or as UTF-16 the way Windows PowerShell
// writes by default, assessed cleanly on the page. Every other caller handed the same bytes to a
// parser that stops at the mark, so the command that moves an estate refused the file the page had
// just assessed. Every reader starts here, so all of them read what the page reads.
//
// Only a marked encoding is changed. Unmarked input passes through untouched, which covers ordinary
// UTF-8 and the archives some readers take, since no archive begins with a byte order mark.
func textOf(data []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(data, []byte("\xef\xbb\xbf")):
		return data[3:], nil
	case bytes.HasPrefix(data, []byte("\xff\xfe")):
		return fromUTF16(data[2:], binary.LittleEndian)
	case bytes.HasPrefix(data, []byte("\xfe\xff")):
		return fromUTF16(data[2:], binary.BigEndian)
	}
	return data, nil
}

// fromUTF16 transcodes UTF-16 in the given byte order to UTF-8, in one pass. Input that is not
// whole UTF-16 is refused rather than read with a placeholder where a character could not be
// decoded, since that placeholder would land in the middle of a hostname or a command.
func fromUTF16(body []byte, order binary.ByteOrder) ([]byte, error) {
	if len(body)%2 != 0 {
		return nil, fmt.Errorf("%w: it is marked as UTF-16 and holds an odd number of bytes",
			ErrNotText)
	}
	out := make([]byte, 0, len(body)/2)
	for i := 0; i < len(body); i += 2 {
		r := rune(order.Uint16(body[i:]))
		if utf16.IsSurrogate(r) {
			// A surrogate is half of a character, and only a high one followed by a low one makes a
			// whole character. DecodeRune answers anything else with U+FFFD, which no pair decodes
			// to, so that answer is the refusal.
			pair := utf8.RuneError
			if i+3 < len(body) {
				pair = utf16.DecodeRune(r, rune(order.Uint16(body[i+2:])))
			}
			if pair == utf8.RuneError {
				return nil, fmt.Errorf("%w: it is marked as UTF-16 and holds a surrogate with no "+
					"partner", ErrNotText)
			}
			r = pair
			i += 2
		}
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}
