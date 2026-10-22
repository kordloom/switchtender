package scenario

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/witness"
)

// rewrittenHistory is the finding kind the witness refuses a push over when its tree does not
// extend the root the witness holds.
const rewrittenHistory = "rewritten_history"

// notesSuffix is the tail verify gives its witness OK line for a statement carrying notes.
func notesSuffix(notes int64) string {
	switch {
	case notes == 1:
		return ", 1 note"
	case notes > 1:
		return fmt.Sprintf(", %d notes", notes)
	}
	return ""
}

// wantPrinted fails the test for each of wants that out does not contain.
func wantPrinted(t *testing.T, what, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("%s does not say %q:\n%s", what, want, out)
		}
	}
}

// TestWitnessRefusesARewrittenChain is the end-to-end rewrite exercise of the hosted witness.
//
// A server on SQLite with a one second span cadence, enrolled with witness enroll, gets three
// beats countersigned, and its bundle verifies against the witness's statement. A database writer
// then rewrites the oldest entry and re-links the chain. A restart with the witness reachable is
// refused before anything is pushed and the statement is unchanged. A restart while the witness
// does not answer its statement, the one window the start check leaves, comes up, and its first
// push is refused as rewritten history. The statement then counts one refusal over the
// pre-rewrite coordinate, and verify fails the rewritten bundle against either statement.
//
//nolint:funlen // Test function.
func TestWitnessRefusesARewrittenChain(t *testing.T) {
	t.Parallel()
	w := newScenarioWitness(t)
	in := newWitnessInstall(t, w)

	s := in.start()
	var first int64
	waitFor(t, "three countersigned beats", func() bool {
		_, st := w.statement(t, in.keyID)
		if first == 0 {
			first = st.LastBeat
		}
		return first > 0 && st.LastBeat >= first+2
	})
	s.stop()

	// The clock stands still from here to the refused start, so the witness signs the same
	// statement for the same memory, and the comparison below can be byte for byte.
	w.clock.freeze()
	preRaw, pre := w.statement(t, in.keyID)
	if pre.RefusalsTotal != 0 || pre.NotesTotal != 1 || pre.Size < 1 {
		t.Fatalf("statement before the rewrite counts %d refusals and %d notes over a tree of %d, "+
			"want 0, the first_root note, and a tree", pre.RefusalsTotal, pre.NotesTotal, pre.Size)
	}
	preFile := in.keep("statement-before.json", preRaw)
	honest := in.export("honest.json")
	out, ok := in.verify(w, honest, preFile)
	if !ok {
		t.Fatalf("verify of the honest bundle failed:\n%s", out)
	}
	wantPrinted(t, "verify of the honest bundle", out, "\nVERIFIED",
		fmt.Sprintf("countersigned by %s, newest seq %d, for producer %s%s)", w.keyID,
			pre.LastSeq, in.keyID, notesSuffix(pre.NotesTotal)), "The witness refused no push.")
	if !strings.HasSuffix(strings.SplitN(out[strings.Index(out, "witness      OK"):], "\n",
		2)[0], ", 1 note)") {
		t.Errorf("the honest witness line does not end with the first_root note:\n%s", out)
	}

	// The oldest entry is under every tree the witness holds, so its rewrite changes the link
	// and the root at every witnessed position.
	was := in.rewrite(1)
	t.Logf("rewrote entry 1 (%s) under a witnessed tree of %d", was, pre.Size)

	// Step 1: the witness answers, so the start is held to its head and refused.
	refused := in.startRefused()
	wantPrinted(t, "the refused start", refused,
		binding.ErrDrift.Error()+": the audit chain does not reach the head the witness at "+w.url,
		fmt.Sprintf("entry %d with link %s and a tree of %d entries with root %s", pre.LastSeq,
			pre.LastHead, pre.Size, pre.Root),
		"Nothing was changed")
	afterRaw, _ := w.statement(t, in.keyID)
	if !bytes.Equal(preRaw, afterRaw) {
		t.Errorf("the statement changed across the refused start:\nbefore %s\nafter  %s", preRaw,
			afterRaw)
	}
	w.clock.thaw()

	// Step 2: the witness does not answer its statement, so the start goes on with a warning, and
	// the pushes still reach the witness.
	w.statementDown.Store(true)
	s = in.start()
	w.statementDown.Store(false)
	if w.statementsBlocked.Load() == 0 {
		t.Fatal("the start never asked for the statement the front held back")
	}
	wantPrinted(t, "the start over an unanswered statement", s.log(),
		"the witness did not answer with its signed statement, so the chain is not held to its "+
			"head at this start")

	// Step 3: the first push from the rewritten chain is refused and recorded.
	waitFor(t, "the witness to refuse a push from the rewritten chain", func() bool {
		return w.findings(t, in.keyID).RefusalsTotal > 0
	})
	s.stop()
	wantPrinted(t, "the server log", s.log(), "refusal "+rewrittenHistory)
	record := w.findings(t, in.keyID)
	var refusals []witness.RecordedFinding
	for _, f := range record.Findings {
		if f.Class == witness.ClassRefusal {
			refusals = append(refusals, f)
		}
	}
	if len(refusals) != 1 || refusals[0].Kind != rewrittenHistory {
		t.Errorf("the witness recorded refusals %+v, want one %s", refusals,
			rewrittenHistory)
	}
	postRaw, post := w.statement(t, in.keyID)
	if post.RefusalsTotal != 1 {
		t.Errorf("refusals_total = %d after the rewrite, want 1", post.RefusalsTotal)
	}
	if post.NotesTotal != pre.NotesTotal {
		t.Errorf("notes_total = %d after the rewrite, want %d as before it", post.NotesTotal,
			pre.NotesTotal)
	}
	if post.LastSeq != pre.LastSeq || post.LastHead != pre.LastHead || post.Size != pre.Size ||
		post.Root != pre.Root {
		t.Errorf("the statement moved to entry %d link %s tree %d root %s, want the pre-rewrite "+
			"entry %d link %s tree %d root %s", post.LastSeq, post.LastHead, post.Size, post.Root,
			pre.LastSeq, pre.LastHead, pre.Size, pre.Root)
	}
	postFile := in.keep("statement-after.json", postRaw)
	findingsRoute := "see GET /v1/installs/" + in.keyID + "/findings"

	rewritten := in.export("rewritten.json")
	out, ok = in.verify(w, rewritten, postFile)
	if ok {
		t.Errorf("verify of the rewritten bundle against the statement passed:\n%s", out)
	}
	wantPrinted(t, "verify of the rewritten bundle against the statement", out,
		"\nNOT VERIFIED: the witness statement does not hold: the witness refused 1 push from "+
			"this install, "+findingsRoute)
	out, ok = in.verify(w, rewritten, preFile)
	if ok {
		t.Errorf("verify of the rewritten bundle against the pre-rewrite statement passed:\n%s",
			out)
	}
	wantPrinted(t, "verify of the rewritten bundle against the pre-rewrite statement", out,
		"\nNOT VERIFIED", fmt.Sprintf("so the chain was rewritten at or before seq %d",
			pre.LastSeq))

	// The honest bundle is unchanged, and it now fails against the statement too: a refusal
	// fails verify for the life of the record, whichever export is held to it.
	out, ok = in.verify(w, honest, postFile)
	if ok {
		t.Errorf("verify of the honest bundle against the statement that counts the refusal "+
			"passed:\n%s", out)
	}
	wantPrinted(t, "verify of the honest bundle against the statement after the rewrite", out,
		"witness      FAILED (the witness refused 1 push from this install, "+findingsRoute+")",
		"\nNOT VERIFIED: the witness statement does not hold: the witness refused 1 push from "+
			"this install, "+findingsRoute)
	if out, ok = in.verify(w, honest, preFile); !ok || !strings.Contains(out, "\nVERIFIED") {
		t.Errorf("verify of the honest bundle against the statement it was exported under no "+
			"longer passes:\n%s", out)
	}
}

// TestWitnessOutageQueuesAndResumes is the outage pass of the hosted witness. A server enrolled
// with the witness gets a beat countersigned, then the witness stops answering pushes for five
// beats. The beats queue in order, the status route says so, and a bundle exported during the
// outage verifies naming only the newest seq the witness countersigned. Once the witness answers
// again the queue drains, every beat is countersigned with no beat missing, and the final bundle
// verifies against the statement.
//
//nolint:funlen // Test function.
func TestWitnessOutageQueuesAndResumes(t *testing.T) {
	t.Parallel()
	w := newScenarioWitness(t)
	in := newWitnessInstall(t, w)

	s := in.start()
	waitFor(t, "a countersigned beat", func() bool {
		_, st := w.statement(t, in.keyID)
		return st.LastBeat > 0
	})
	w.pushesDown.Store(true)
	waitFor(t, "five beats queued during the outage", func() bool {
		return in.status(s).Queue >= 5
	})
	during := in.status(s)
	heldRaw, held := w.statement(t, in.keyID)
	if during.LastBeat != held.LastBeat || during.LastSeq != held.LastSeq {
		t.Errorf("during the outage the status names countersigned beat %d at seq %d, and the "+
			"witness holds beat %d at seq %d", during.LastBeat, during.LastSeq, held.LastBeat,
			held.LastSeq)
	}
	if during.LastError == "" || w.pushesBlocked.Load() < 5 {
		t.Errorf("during the outage the status says last error %q after %d blocked pushes, want "+
			"the outage named after at least five", during.LastError, w.pushesBlocked.Load())
	}

	// The outage bundle carries what the witness countersigned and nothing after, and verify says
	// exactly that.
	outage := in.export("outage.json")
	out, ok := in.verify(w, outage, in.keep("statement-outage.json", heldRaw))
	if !ok {
		t.Fatalf("verify of the bundle exported during the outage failed:\n%s", out)
	}
	wantPrinted(t, "verify of the outage bundle", out, "\nVERIFIED",
		fmt.Sprintf("newest seq %d, for producer %s%s)", held.LastSeq, in.keyID,
			notesSuffix(held.NotesTotal)),
		fmt.Sprintf("this bundle reproduces seq %d, the newest entry the witness countersigned",
			held.LastSeq))

	w.pushesDown.Store(false)
	waitFor(t, "the queue to drain after the outage", func() bool {
		st := in.status(s)
		return st.Queue == 0 && st.LastBeat >= during.LastBeat+5
	})
	s.stop()
	finalRaw, final := w.statement(t, in.keyID)
	if final.RefusalsTotal != 0 || final.NotesTotal != 1 {
		t.Errorf("after the outage the statement counts %d refusals and %d notes, want 0 and "+
			"the first_root note alone, since every queued beat reached the witness",
			final.RefusalsTotal, final.NotesTotal)
	}
	if final.LastBeat < held.LastBeat+5 {
		t.Errorf("the witness holds beat %d after the outage, want at least %d", final.LastBeat,
			held.LastBeat+5)
	}
	bundle := in.export("resumed.json")
	out, ok = in.verify(w, bundle, in.keep("statement-resumed.json", finalRaw))
	if !ok {
		t.Fatalf("verify of the bundle after the outage failed:\n%s", out)
	}
	wantPrinted(t, "verify of the resumed bundle", out, "\nVERIFIED",
		fmt.Sprintf("newest seq %d, for producer %s%s)", final.LastSeq, in.keyID,
			notesSuffix(final.NotesTotal)), "The witness refused no push.")
}
