package importer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// utf16Of encodes text as UTF-16 in the given byte order, behind the byte order mark for it.
func utf16Of(text string, order binary.ByteOrder) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2+2*len(units))
	order.PutUint16(out, 0xFEFF)
	for i, u := range units {
		order.PutUint16(out[2+2*i:], u)
	}
	return out
}

// TestEveryReaderReadsWhatThePageReads pins that an export is read the same whatever encoding it
// was saved in. The browser assessment decodes a dropped file through the platform's text decoder,
// which honors a byte order mark, so an export saved as UTF-8 with a mark, or as UTF-16 the way
// Windows PowerShell writes by default, assessed cleanly on the page. The command, the import, and
// the API handed the same bytes to a parser that stops at the mark, so the tool that moves an
// estate refused the file the page had just assessed.
func TestEveryReaderReadsWhatThePageReads(t *testing.T) {
	t.Parallel()
	fixture := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, err)
		}
		return string(b)
	}
	readers := []struct {
		// Name is the format.
		Name string
		// Read is the reader under test.
		Read func([]byte, time.Time) (*Plan, error)
		// Text is an export it reads.
		Text string
	}{
		{Name: "awx", Read: FromAWX, Text: fixture("testdata/awx-awxkit-export.json")},
		{Name: "semaphore", Read: FromSemaphore, Text: fixture("testdata/semaphore-export.json")},
		{Name: "chef", Read: FromChef, Text: chefExport},
		{Name: "puppet", Read: FromPuppet,
			Text: `[{"certname":"a.prod","catalog_environment":"production"}]`},
		{Name: "rundeck", Read: FromRundeck(""), Text: fixture("testdata/rundeck-quoted-scalars.yaml")},
		{Name: "cron", Read: FromCron("", false), Text: "0 3 * * * /usr/local/bin/backup.sh\n"},
	}
	encodings := []struct {
		// Name is the encoding.
		Name string
		// Encode saves text in it.
		Encode func(string) []byte
	}{
		{Name: "utf-8 with a mark", Encode: func(s string) []byte {
			return append([]byte("\xef\xbb\xbf"), s...)
		}},
		{Name: "utf-16le", Encode: func(s string) []byte { return utf16Of(s, binary.LittleEndian) }},
		{Name: "utf-16be", Encode: func(s string) []byte { return utf16Of(s, binary.BigEndian) }},
	}

	testNum := 0
	for _, reader := range readers {
		for _, enc := range encodings {
			t.Run(fmt.Sprintf("test %d %s %s", testNum, reader.Name, enc.Name), func(t *testing.T) {
				t.Parallel()
				now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
				want, err := reader.Read([]byte(reader.Text), now)
				if err != nil {
					t.Fatalf("the plain export was refused: %v", err)
				}
				got, err := reader.Read(enc.Encode(reader.Text), now)
				if err != nil {
					t.Fatalf("the %s export was refused: %v", enc.Name, err)
				}
				if diff := cmp.Diff(want.Assess(), got.Assess(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("the %s export reads differently (-plain +encoded):\n%s", enc.Name, diff)
				}
			})
			testNum++
		}
	}
}

// TestTextOfRefusesAMarkedExportThatIsNotWhole pins the refusals. An export marked as UTF-16 whose
// bytes cannot all be read as UTF-16 was cut short or is not text, and reading it anyway would
// replace what it could not decode with a placeholder in the middle of a hostname or a command.
func TestTextOfRefusesAMarkedExportThatIsNotWhole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what is wrong with the input, or that nothing is.
		Name string
		// In is the input.
		In []byte
		// WantResult is the text expected, when the input is read.
		WantResult string
		// Want is the error expected.
		Want error
	}{{ // Test 0: Unmarked input passes through untouched, archives included.
		Name: "unmarked", In: []byte("PK\x03\x04archive"), WantResult: "PK\x03\x04archive",
	}, { // Test 1: A UTF-8 mark is removed.
		Name: "utf-8 mark", In: []byte("\xef\xbb\xbf{}"), WantResult: "{}",
	}, { // Test 2: UTF-16 with a pair of surrogates decodes to the one character they spell.
		Name: "surrogate pair", In: utf16Of(`{"a":"😀"}`, binary.LittleEndian),
		WantResult: `{"a":"😀"}`,
	}, { // Test 3: An odd number of bytes after a UTF-16 mark cannot be UTF-16.
		Name: "odd length", In: []byte("\xff\xfe{\x00}"), Want: ErrNotText,
	}, { // Test 4: A lone surrogate is not a character.
		Name: "lone surrogate", In: []byte("\xff\xfe\x00\xd8{\x00"), Want: ErrNotText,
	}, { // Test 5: A low surrogate cannot open a pair.
		Name: "low surrogate first", In: []byte("\xff\xfe\x00\xdc\x00\xd8"), Want: ErrNotText,
	}, { // Test 6: A high surrogate that ends the file has no partner.
		Name: "high surrogate at the end", In: []byte("\xff\xfe{\x00\x00\xd8"), Want: ErrNotText,
	}, { // Test 7: Big-endian with a pair decodes the same character.
		Name: "big-endian pair", In: utf16Of(`😀`, binary.BigEndian), WantResult: `😀`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := textOf(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("textOf() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, string(got)); diff != "" {
				t.Errorf("textOf() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// FuzzTextOf holds textOf to what every reader relies on. Whatever it is handed, it either refuses
// with ErrNotText or returns text, and text it transcoded from UTF-16 is valid UTF-8. Any valid
// text it returns, saved back as UTF-16 in either byte order, reads back as the same text.
func FuzzTextOf(f *testing.F) {
	for _, seed := range [][]byte{
		nil, []byte("{}"), []byte("\xef\xbb\xbf{}"), utf16Of(`{"a":"😀"}`, binary.LittleEndian),
		utf16Of("x", binary.BigEndian), []byte("\xff\xfe\x00\xd8"), []byte("\xff\xfe{"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := textOf(data)
		if err != nil {
			if !errors.Is(err, ErrNotText) {
				t.Fatalf("textOf() error = %v, want ErrNotText or none", err)
			}
			return
		}
		wide := bytes.HasPrefix(data, []byte("\xff\xfe")) || bytes.HasPrefix(data, []byte("\xfe\xff"))
		if wide && !utf8.Valid(got) {
			t.Fatalf("textOf(%q) transcoded to invalid UTF-8 %q", data, got)
		}
		if !utf8.Valid(got) {
			return
		}
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			again, err := textOf(utf16Of(string(got), order))
			if err != nil || !bytes.Equal(again, got) {
				t.Fatalf("%q saved as UTF-16 read back as %q, %v", got, again, err)
			}
		}
	})
}
