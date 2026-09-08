package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
)

// testSealerOnce derives the test encryption key once. Key derivation is argon2id and deliberately
// slow, so every case sharing one sealer keeps the suite fast without weakening what is being
// tested: the same key seals and opens, which is exactly the deployment case.
var testSealerOnce = sync.OnceValue(func() Sealer { return credential.NewSealer("pass", "salt") })

// testGzip returns the gzip form of body, which is what the seal wraps inside a backup file.
func testGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// testFile builds a backup file whose sealed contents are exactly inner, so a test can craft a file
// that decrypts cleanly and is still garbage underneath.
func testFile(t *testing.T, createdAt time.Time, inner []byte) []byte {
	t.Helper()
	sealed, err := testSealerOnce().Seal(string(inner))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	out, err := json.Marshal(envelope{
		Format: Format, Version: Version, CreatedAt: createdAt, Sealed: sealed,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return out
}

// testObjectCount returns how many objects of every backed-up kind a set of stores holds, so a
// refused restore can be shown to have written nothing at all.
func testObjectCount(t *testing.T, ctx context.Context, s Stores) int {
	t.Helper()
	total := 0
	add := func(n int, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		total += n
	}
	creds, err := s.Credentials.List(ctx)
	add(len(creds), err)
	projects, err := s.Projects.List(ctx)
	add(len(projects), err)
	templates, err := s.Templates.List(ctx)
	add(len(templates), err)
	invs, err := s.Inventories.List(ctx)
	add(len(invs), err)
	srcs, err := s.InventorySources.List(ctx)
	add(len(srcs), err)
	scheds, err := s.Schedules.List(ctx)
	add(len(scheds), err)
	trigs, err := s.Triggers.List(ctx)
	add(len(trigs), err)
	users, err := s.Users.List(ctx)
	add(len(users), err)
	toks, err := s.Tokens.List(ctx)
	add(len(toks), err)
	teams, err := s.Teams.List(ctx)
	add(len(teams), err)
	orgs, err := s.Orgs.List(ctx)
	add(len(orgs), err)
	grants, err := s.Grants.List(ctx)
	add(len(grants), err)
	pols, err := s.Policies.List(ctx)
	add(len(pols), err)
	types, err := s.CredentialTypes.List(ctx)
	add(len(types), err)
	return total
}

// TestReadRefusesACorruptArchive walks every way a backup file can be broken and proves each one is
// refused with nothing written.
//
// This is the property the whole package rests on. A restore is an upsert into a live control plane
// with no transaction around it, and it is run by somebody whose install is already gone. A file
// that half-applies, or that applies content the seal did not cover, is worse than one that will not
// open at all: the operator is told the recovery worked and goes on believing the accounts, tokens,
// and approval gates in front of them are the ones they backed up.
//
//nolint:funlen // Test function.
func TestReadRefusesACorruptArchive(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	good, err := json.Marshal(payload{CreatedAt: stamp})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	truncatedGzip := testGzip(t, good)[:20]

	tests := []struct {
		Name        string
		Body        []byte
		Want        error
		WantMessage string
	}{{ // Test 0: An empty file is not a backup.
		Name: "empty file", Body: nil, Want: ErrFormat,
	}, { // Test 1: A file that is not JSON at all.
		Name: "not json", Body: []byte("this is a text file"), Want: ErrFormat,
	}, { // Test 2: JSON that stops partway through, as a truncated copy would.
		Name: "truncated json", Body: []byte(`{"format":"switchtender-backup","ver`),
		Want: ErrFormat,
	}, { // Test 3: An empty JSON object names no format.
		Name: "no format", Body: []byte(`{}`), Want: ErrFormat, WantMessage: "wrong file format",
	}, { // Test 4: Another product's export.
		Name: "foreign format", Body: []byte(`{"format":"other-tool","version":2}`),
		Want: ErrFormat, WantMessage: "wrong file format",
	}, { // Test 5: The format name is matched exactly, not case-insensitively.
		Name: "format in the wrong case", Body: []byte(`{"format":"SwitchTender-Backup","version":2}`),
		Want: ErrFormat, WantMessage: "wrong file format",
	}, { // Test 6: Version 1 carried an unauthenticated header and is refused rather than read.
		Name: "version 1", Body: []byte(`{"format":"switchtender-backup","version":1,"sealed":"x"}`),
		Want: ErrFormat, WantMessage: "unsupported version 1",
	}, { // Test 7: A version this build has never seen.
		Name: "future version", Body: []byte(`{"format":"switchtender-backup","version":3,"sealed":"x"}`),
		Want: ErrFormat, WantMessage: "unsupported version 3",
	}, { // Test 8: A missing version field reads as zero and is refused.
		Name: "no version", Body: []byte(`{"format":"switchtender-backup"}`),
		Want: ErrFormat, WantMessage: "unsupported version 0",
	}, { // Test 9: The right header with no sealed payload behind it.
		Name: "empty seal", Body: []byte(`{"format":"switchtender-backup","version":2,"sealed":""}`),
		Want: ErrOpen,
	}, { // Test 10: A sealed field holding something that was never sealed.
		Name: "seal is not ciphertext",
		Body: []byte(`{"format":"switchtender-backup","version":2,"sealed":"hello"}`),
		Want: ErrOpen,
	}, { // Test 11: A seal that opens but holds something that is not gzip.
		Name: "seal holds plain text", Body: testFile(t, stamp, []byte("not compressed at all")),
		WantMessage: "decompress",
	}, { // Test 12: A gzip stream that stops partway through.
		Name: "gzip cut short", Body: testFile(t, stamp, truncatedGzip), WantMessage: "decompress",
	}, { // Test 13: Valid gzip holding something that is not the payload.
		Name: "gzip holds junk", Body: testFile(t, stamp, testGzip(t, []byte("{{{not json"))),
		WantMessage: "decode payload",
	}, { // Test 14: Valid gzip holding JSON of the wrong shape.
		Name: "payload is an array", Body: testFile(t, stamp, testGzip(t, []byte(`[1,2,3]`))),
		WantMessage: "decode payload",
	}, { // Test 15: The header time is compared exactly, so even a one second edit is caught.
		Name: "header off by one second",
		Body: testFile(t, stamp.Add(time.Second), testGzip(t, good)),
		Want: ErrFormat, WantMessage: "header was changed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dst := freshStores()
			sum, err := Read(ctx, dst, testSealerOnce(), bytes.NewReader(test.Body))
			if err == nil {
				t.Fatalf("%s: a corrupt backup restored cleanly, reporting %+v", test.Name, sum)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("%s: error = %v, want %v", test.Name, err, test.Want)
			}
			if test.WantMessage != "" && !strings.Contains(err.Error(), test.WantMessage) {
				t.Errorf("%s: error = %v, want it to mention %q", test.Name, err, test.WantMessage)
			}
			if got := testObjectCount(t, ctx, dst); got != 0 {
				t.Errorf("%s: a refused restore wrote %d objects, want none", test.Name, got)
			}
		})
	}
}

// TestReadRefusesTrailingGarbageQuietly pins what happens when a backup file has extra bytes after
// the envelope, which is what a partially overwritten or concatenated file looks like.
//
// The decoder reads one JSON value and stops, so the extra bytes are ignored rather than refused.
// That is worth stating out loud: the seal covers the payload, so appended content cannot change
// what is restored, and a file with a second envelope glued on restores only the first.
func TestReadRefusesTrailingGarbageQuietly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stamp := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	inner, err := json.Marshal(payload{CreatedAt: stamp})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	file := testFile(t, stamp, testGzip(t, inner))
	file = append(file, []byte("\nGARBAGE APPENDED BY SOMETHING ELSE\n")...)

	dst := freshStores()
	sum, err := Read(ctx, dst, testSealerOnce(), bytes.NewReader(file))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !sum.CreatedAt.Equal(stamp) {
		t.Errorf("Summary.CreatedAt = %s, want the sealed snapshot time %s", sum.CreatedAt, stamp)
	}
	if got := testObjectCount(t, ctx, dst); got != 0 {
		t.Errorf("the appended bytes contributed %d objects, want none", got)
	}
}

// TestReadAndWriteRefuseANilSealer proves the nil interface is treated as no key rather than
// dereferenced. A caller that has not wired encryption gets the refusal, not a panic in the middle
// of a disaster recovery.
func TestReadAndWriteRefuseANilSealer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := Write(ctx, freshStores(), nil, &bytes.Buffer{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Write() with a nil sealer error = %v, want ErrDisabled", err)
	}
	if _, err := Read(ctx, freshStores(), nil, strings.NewReader("{}")); !errors.Is(err, ErrDisabled) {
		t.Errorf("Read() with a nil sealer error = %v, want ErrDisabled", err)
	}
}

// TestReadChecksTheKeyBeforeTheStores proves the encryption key is required before the file is even
// looked at, so a deployment without a key never reports a format complaint that would send an
// operator hunting for a bad file when the real problem is a missing key.
func TestReadChecksTheKeyBeforeTheStores(t *testing.T) {
	t.Parallel()
	off := credential.NewSealer("", "")
	_, err := Read(context.Background(), freshStores(), off, strings.NewReader("total nonsense"))
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("Read() of junk without a key error = %v, want ErrDisabled", err)
	}
	if errors.Is(err, ErrFormat) {
		t.Error("a missing key was reported as a bad file")
	}
}
