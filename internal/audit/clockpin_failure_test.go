package audit

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"
)

// audAnchorSkew is the clock skew the bundle verifier allows between a timestamp authority and the
// entry it covers, restated here so the test names the tolerance it holds the chain to.
const audAnchorSkew = 5 * time.Minute

// TestChainTimeIsNotDraggedForwardByOneFastClock appends one entry from a writer whose clock runs
// thirty minutes fast, the outcome a replica with a drifting clock commits with its own time, and
// then one ordinary change from a writer whose clock is right, and anchors the head at a timestamp
// authority whose clock is right too.
//
// StampAppendTime pins every entry forward to the head's time with no upper bound. One fast clock
// anywhere in a fleet sharing a chain therefore dates every later entry from every replica in the
// future, for as long as the excursion was, and the healthy replica's entry carries a time no clock
// that wrote it ever read. The anchor the default "switchtender audit anchor" takes over that head
// is then refused by the verifier, since a genuine authority's time precedes the entry it covers by
// more than the allowed skew, and a refused anchor fails every bundle and receipt drawn from the
// chain from then on. Honest evidence from a correct clock reads NOT VERIFIED because one other
// process's clock jumped.
func TestChainTimeIsNotDraggedForwardByOneFastClock(t *testing.T) {
	// Not parallel: the bundle check isolates its signing identity through the environment.
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	ctx := context.Background()
	store := NewMemStore()

	fast := &Entry{
		ID: NewID(), At: time.Now().UTC().Add(30 * time.Minute),
		Actor: "system:dispatcher", ActorType: "system", Method: MethodRun,
		Path: "/runs/run_fast_replica/outcome/succeeded",
	}
	if err := store.Append(ctx, fast); err != nil {
		t.Fatalf("Append() from the fast replica error = %v", err)
	}

	healthy := &Entry{
		ID: NewID(), Actor: "admin-laptop", ActorType: "token", Method: "POST",
		Path: "/v1/templates",
	}
	clock := time.Now().UTC()
	if err := store.Append(ctx, healthy); err != nil {
		t.Fatalf("Append() from the healthy replica error = %v", err)
	}
	if ahead := healthy.At.Sub(clock); ahead > audAnchorSkew {
		t.Errorf("the healthy replica's entry is dated %s, %s ahead of the clock that stamped it: "+
			"one fast clock moved every later entry into the future",
			healthy.At.Format(time.RFC3339), ahead.Round(time.Second))
	}

	raw, err := hex.DecodeString(healthy.Hash)
	if err != nil {
		t.Fatalf("decode the head link: %v", err)
	}
	sum := sha256.Sum256(raw)
	genTime := time.Now().UTC().Truncate(time.Second)
	proof := base64.StdEncoding.EncodeToString(tokenOverInfo(t, tstInfoAt(t, sum[:], genTime)))

	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	id, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	doc, err := BuildBundle(chain, id, "test", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	doc.AttachAnchors([]*Anchor{{
		ID: "anc_healthy_head", Type: AnchorRFC3161, Shape: AnchorShapeLinear, Seq: healthy.Seq,
		Link: healthy.Hash, At: genTime, Ref: "https://tsa.example.com", Proof: proof,
	}})
	signed, err := SignBundleDoc(doc, id.Private())
	if err != nil {
		t.Fatalf("SignBundleDoc() error = %v", err)
	}
	rep, err := VerifyBundle(signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Errorf("an anchor a correct authority took over the healthy replica's head fails the "+
			"bundle: AnchorsOK = %t, timestamp problems %q", rep.AnchorsOK, rep.TimestampProblems)
	}
}
