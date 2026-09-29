package dispatch

import (
	"bytes"
	"strings"
	"testing"
)

// fuzzMaxData bounds the stream a fuzz case may build, and fuzzMaxSecrets the number of values it
// may hide. Both exist so a case spends its time on boundaries rather than on volume: the masker
// scans every secret against every release point, so an unbounded case is a slow case rather than
// an interesting one.
const (
	fuzzMaxData    = 4096
	fuzzMaxSecrets = 32
)

// FuzzStreamMaskerHoldsAcrossAnyChunking asserts the two properties the stream masker exists for,
// against splits nobody chose.
//
// Every hand-written case beside this one names a boundary a person thought of, and each was
// written after the leak it describes had already shipped. Where a run's output is actually cut is
// decided by the operating system: a pipe splits where it fills, and a secret reaching the stored
// log because a write landed in a place no test named is a plaintext credential inside a
// tamper-evident chain that is kept for years and handed to an auditor.
//
// Two properties, and the second is what makes the first worth asserting:
//
// Nothing leaks. No value the masker was told to hide survives in what it emitted.
//
// Chunking decides nothing. The emitted bytes equal what redacting the whole stream in one pass
// produces. A masker that emitted nothing at all would satisfy the leak property and fail this one,
// and so would one that released a secret's first half and withheld its second. This is the claim
// the withheld tail is for, stated as an equality rather than as an absence.
func FuzzStreamMaskerHoldsAcrossAnyChunking(f *testing.F) {
	// The seeds are the cases the hand-written tests cover, so the fuzzer starts from the known
	// boundaries and mutates outward rather than from nothing.
	// The leak this found, kept as a seed because a seed runs under plain go test and the corpus
	// entry the fuzzer wrote does not survive a clean checkout. A secret whose own prefix is also its
	// own suffix, against a run of its character: the release point was moved forward past the match
	// it landed inside, which ate the withheld remainder, and the next secret to start near the new
	// point ran off the end of what had arrived. Its head went to the log as plaintext and its tail
	// went to the following chunk, where the two emitted as one unbroken credential.
	f.Add("0000", "00000000", []byte{48, 67}, uint32(0xFFFFFFFF))
	f.Add("hunter2-swordfish", "before hunter2-swordfish after", []byte{8}, uint32(0))
	f.Add("hunter2-swordfish", "before hunter2-swordfish after", []byte{1}, uint32(0xFFFFFFFF))
	f.Add("SECRET\x00ETHER", "xxSECRETyyETHERzz", []byte{3, 1, 5}, uint32(0x55555555))
	f.Add("a-secret-value", "line one\nline two\nline three\n", []byte{5}, uint32(0))
	f.Add("-----BEGIN KEY-----\nabcdefgh\n-----END KEY-----", "log\n-----BEGIN KEY-----\nabcdefgh\n-----END KEY-----\ndone",
		[]byte{7, 2}, uint32(0x0F0F0F0F))

	f.Fuzz(func(t *testing.T, secretBlob, data string, chop []byte, drainBits uint32) {
		if len(data) > fuzzMaxData {
			return
		}
		secrets := strings.Split(secretBlob, "\x00")
		if len(secrets) > fuzzMaxSecrets {
			return
		}
		m := &masker{}
		m.set(secrets)

		// The oracle: the same values redacted from the same bytes, with no boundary in the way.
		want := m.redact([]byte(data))

		sm := &streamMasker{mask: m}
		var got []byte
		for i, n := 0, 0; i < len(data); n++ {
			size := 1
			if len(chop) > 0 {
				size = int(chop[n%len(chop)])%16 + 1
			}
			end := min(i+size, len(data))
			got = append(got, sm.next([]byte(data[i:end]))...)
			i = end
			// A drain is what a live viewer's poll does to a run that has gone quiet, and it is the
			// part of this machinery that most recently got a release point wrong, so the fuzzer
			// decides where they fall rather than the test.
			if drainBits>>(n%32)&1 == 1 {
				got = append(got, sm.drain()...)
			}
		}
		got = append(got, sm.flush()...)

		if !bytes.Equal(got, want) {
			t.Fatalf("chunking changed the output.\nstreamed = %q\nwhole    = %q\nsecrets  = %q\nchop = %v drains = %#x",
				got, want, m.strs, chop, drainBits)
		}
		// m.strs is what set kept after dropping the blank and the too-short, which is the set the
		// masker actually promises about. Asserting against the raw input would fail on a value the
		// masker openly declines to hide.
		//
		// A value built out of mask token characters is excluded, and that is a property of the
		// alphabet rather than a gap in the masker. A secret of "****" is spelled by any two masks
		// that land side by side, and "*000" by a mask followed by an ordinary "000" that was never
		// secret, so no masker emitting this token can promise its output never spells one. Closing
		// it would mean redacting the output again until it stops matching, which is the pass-per-
		// secret behavior removed from redact: the whole buffer would see the collision and a
		// chunked stream could not, which trades this coincidence for the boundary leak the equality
		// above exists to catch. What the masker does promise is that nothing the run emitted
		// survives, and that is what is asserted here.
		for _, sec := range m.strs {
			if strings.ContainsAny(sec, maskToken) {
				continue
			}
			if bytes.Contains(got, []byte(sec)) {
				t.Fatalf("secret %q survived the stream: %q", sec, got)
			}
		}
	})
}
