// Package receipt builds a signed, offline-verifiable receipt for one run: the chain segment from
// the request that created it through the entry that recorded what it did, with the run's outcome,
// its approval decisions, and its redacted spec disclosed beside the digests the chain committed.
//
// It exists as a package rather than as command code because the receipt is the product's headline
// evidence artifact and every way of asking for one has to produce the same bytes. The command was
// the only way to get one, so the run a person was looking at in the browser could not be turned
// into the artifact the whole claim rests on without shell access to the server.
package receipt

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// Options are the shapes a receipt can take.
type Options struct {
	// Sparse discloses only this run's own entries, each proved to belong to the whole chain by an
	// audit path, so nothing about the entries around them travels. A sparse receipt cannot carry
	// the outcome disclosure, because its leaf hashes are computed over the chain's own entries.
	Sparse bool
	// From, when above zero, proves with a consistency proof that the log only appended since that
	// size. It applies to a sparse receipt.
	From int64
}

// Result is a built receipt and what a caller needs to say about it.
type Result struct {
	// Signed is the receipt's bytes, ready to write or serve.
	Signed []byte
	// Claims is how many chain entries the receipt carries.
	Claims int
	// Anchors is how many external anchors cover it.
	Anchors int
	// KeyID is the producer fingerprint a verifier pins.
	KeyID string
	// Notes are things the caller should tell the operator, such as an outcome that could not be
	// rebuilt. They are not failures.
	Notes []string
	// UnanchoredSparse reports that a sparse receipt carries no tree anchor, so the root it proves
	// membership in rests on this install's word alone. It is a typed flag rather than a note
	// because each caller words the remedy for its own audience.
	UnanchoredSparse bool
}

// Build assembles and signs the receipt for one run.
//
// It fails rather than producing a weaker artifact when the run cannot be receipted at all: a run
// with no creation entry cannot be placed on the chain, a run with no committed outcome has nothing
// to attest, and a chain that does not satisfy an anchor recorded over it must not be published as
// one that does.
func Build(ctx context.Context, runs run.Store, audits audit.Store, id audit.Identity,
	version, runID string, opts Options) (*Result, error) {
	r, err := runs.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("read run %s: %w", runID, err)
	}
	if r.AuditReceipt == "" {
		return nil, fmt.Errorf("run %s has no creation receipt, so its start cannot be placed on "+
			"the chain", runID)
	}
	creationSeq, _, err := parseReceipt(r.AuditReceipt)
	if err != nil {
		return nil, fmt.Errorf("run %s carries an unreadable creation receipt: %w", runID, err)
	}

	entries, err := audits.Chain(ctx)
	if err != nil {
		return nil, fmt.Errorf("read audit chain: %w", err)
	}
	outcomePrefix := "/runs/" + runID + "/outcome/"
	var outcomeSeq int64
	var outcomeEntry *audit.Entry
	for _, e := range entries {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, outcomePrefix) {
			outcomeSeq = e.Seq
			outcomeEntry = e
		}
	}
	if outcomeSeq == 0 {
		return nil, fmt.Errorf("run %s has no committed outcome yet, so there is nothing to "+
			"receipt. A run is receiptable once it has finished", runID)
	}
	if outcomeSeq < creationSeq {
		return nil, fmt.Errorf("run %s: its outcome is recorded before its creation, which is a "+
			"broken chain", runID)
	}

	subject := audit.BundleSubject{Type: "run", ID: runID}
	res := &Result{KeyID: id.KeyID()}
	var doc *audit.Bundle
	if opts.Sparse {
		doc, err = sparseBundle(entries, runID, creationSeq, outcomeSeq, id, version, subject, opts.From)
	} else {
		doc, err = rangeBundle(entries, creationSeq, outcomeSeq, id, version, subject)
	}
	if err != nil {
		return nil, err
	}
	res.Claims = len(doc.Claims)

	if !opts.Sparse {
		if body, berr := outcome.Body(ctx, runs, r); berr == nil {
			var bodyObj any
			if json.Unmarshal(body, &bodyObj) == nil {
				for i := range doc.Claims {
					if doc.Claims[i].Chain.Seq == outcomeSeq {
						doc.Claims[i].Payload["outcome_body"] = bodyObj
						doc.Claims[i].Payload["outcome_nonce"] = outcomeEntry.Nonce
						discloseSpec(&doc.Claims[i], r)
					}
				}
			}
		} else {
			res.Notes = append(res.Notes, "the outcome could not be rebuilt, so the receipt proves "+
				"the chain but does not show what the run did: "+berr.Error())
		}
		discloseDecisions(doc, entries, r)
	}

	if anchors, ok := audits.(audit.AnchorStore); ok {
		recorded, aerr := anchors.Anchors(ctx, 0)
		if aerr != nil {
			return nil, fmt.Errorf("read anchors: %w", aerr)
		}
		if reachedAll, results := audit.CheckAnchors(entries, recorded, id.InstallID); !reachedAll {
			var problems []string
			for _, r := range results {
				if !r.Reached {
					problems = append(problems, r.Anchor.ID+": "+r.Problem)
				}
			}
			return nil, fmt.Errorf("this chain does not satisfy every anchor recorded over it, so a "+
				"receipt drawn from it must not be published as one that does: %s",
				strings.Join(problems, "; "))
		}
		res.Anchors = doc.AttachAnchors(recorded)
		res.UnanchoredSparse = opts.Sparse && res.Anchors == 0
	}

	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		return nil, err
	}
	res.Signed = append(signed, '\n')
	return res, nil
}

// rangeBundle builds the contiguous receipt: the chain segment from the request that created the run
// through the entry that recorded what it did. It can disclose the run's outcome, because its claims
// are not leaves of a tree, but on a busy install the segment also carries the entries that happened
// to be recorded between them.
func rangeBundle(entries []*audit.Entry, creationSeq, outcomeSeq int64, id audit.Identity,
	version string, subject audit.BundleSubject) (*audit.Bundle, error) {
	segment := make([]*audit.Entry, 0, outcomeSeq-creationSeq+1)
	for _, e := range entries {
		if e.Seq >= creationSeq && e.Seq <= outcomeSeq {
			segment = append(segment, e)
		}
	}
	doc, err := audit.BuildBundle(segment, id, version, time.Now())
	if err != nil {
		return nil, err
	}
	doc.Subject = subject
	return doc, nil
}

// sparseBundle builds the tree receipt: only this run's own entries, each proved to belong to the
// whole chain by an audit path. Nothing about the entries around them travels, which is what lets a
// receipt be handed to an outside auditor on an install that runs other people's work.
func sparseBundle(entries []*audit.Entry, runID string, creationSeq, outcomeSeq int64,
	id audit.Identity, version string, subject audit.BundleSubject, from int64) (*audit.Bundle, error) {
	// The run's own entries are its creation, whose receipt it carries, and every later entry whose
	// path names it: an approval, a rejection, a cancellation, and its outcome.
	disclose := map[int64]bool{creationSeq: true, outcomeSeq: true}
	for _, e := range entries {
		if e.Seq > creationSeq && strings.Contains(e.Path, runID) {
			disclose[e.Seq] = true
		}
	}
	doc, err := audit.BuildTreeBundle(entries, disclose, id, version, subject, time.Now())
	if err != nil {
		return nil, err
	}
	if from > 0 {
		if err := doc.AttachConsistency(entries, from, id); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// discloseSpec attaches the run's redacted spec to the outcome claim, so a verifier can read what
// the digests commit to and recompute them. The spec is redacted before it leaves the outcome
// package, so this never hands out bytes the redaction did not pass over.
func discloseSpec(claim *audit.BundleClaim, r *run.Run) {
	spec, err := outcome.Spec(r)
	if err != nil {
		return
	}
	var specObj any
	if json.Unmarshal(spec, &specObj) == nil {
		claim.Payload["spec_body"] = specObj
	}
}

// discloseDecisions attaches each approval decision's body and nonce to its claim, the same way the
// outcome is disclosed, so a verifier can show who approved exactly what and prove the chain
// committed it. The bodies are rebuilt from the run, so a row that changed since the decision
// produces a body the committed digest refuses, which is the tamper this disclosure exists to
// surface.
func discloseDecisions(doc *audit.Bundle, entries []*audit.Entry, r *run.Run) {
	prefix := "/runs/" + r.ID + "/decision/"
	for _, e := range entries {
		if e.Method != audit.MethodDecision || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		body, _, err := outcome.DecisionBody(r, strings.TrimPrefix(e.Path, prefix))
		if err != nil {
			continue
		}
		var bodyObj any
		if json.Unmarshal(body, &bodyObj) != nil {
			continue
		}
		for i := range doc.Claims {
			if doc.Claims[i].Chain.Seq == e.Seq {
				doc.Claims[i].Payload["decision_body"] = bodyObj
				doc.Claims[i].Payload["decision_nonce"] = e.Nonce
			}
		}
	}
}

// parseReceipt splits a seq:link audit receipt into its parts.
func parseReceipt(v string) (int64, string, error) {
	seqPart, link, found := strings.Cut(v, ":")
	if !found {
		return 0, "", fmt.Errorf("a receipt looks like seq:link, for example 41:9f2c")
	}
	seq, err := strconv.ParseInt(seqPart, 10, 64)
	if err != nil || seq < 1 {
		return 0, "", fmt.Errorf("receipt sequence %q is not a chain position", seqPart)
	}
	if link == "" {
		return 0, "", fmt.Errorf("receipt is missing its link")
	}
	return seq, link, nil
}
