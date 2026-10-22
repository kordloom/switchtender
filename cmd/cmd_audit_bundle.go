package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/util"
)

var (
	// bundleDB is the --db value for the audit bundle command.
	bundleDB string
	// bundleOut is the --out value; empty writes to standard output.
	bundleOut string
	// bundleLimit caps how many of the newest entries the bundle carries.
	bundleLimit int
	// bundleFromSeq is the sequence of the first entry a windowed bundle carries.
	bundleFromSeq int64
	// bundleToSeq is the sequence of the last entry a windowed bundle carries, inclusive.
	bundleToSeq int64
	// bundleKeyDir overrides where the producer identity is read from or created.
	bundleKeyDir string
)

// auditBundleCmd emits the audit chain as a signed LoomSeal bundle.
var auditBundleCmd = &cobra.Command{
	Use:   "bundle",
	Short: "Emit the audit chain as a signed LoomSeal bundle anyone can verify offline.",
	Long: `Emit the audit chain as a signed LoomSeal bundle.

A bundle is a self-contained record. It carries the entries, the chain links, the public key that
signed it, and the signature, so a third party verifies it with an open verifier on a machine that
has never run SwitchTender and has no network access. Every link recomputes from the claims alone,
so nobody has to take our word for the history.

The signing key is created on first use and never leaves the install. Publish the fingerprint the
command prints so a relying party can pin it and know a bundle came from this install rather than
from someone who merely generated a key.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runAuditBundle,
}

// init registers the bundle command and its flags.
func init() {
	auditBundleCmd.Flags().StringVar(&bundleDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, to read the audit chain from.")
	auditBundleCmd.Flags().StringVar(&bundleOut, "out", "",
		"Write the bundle here instead of to standard output.")
	auditBundleCmd.Flags().IntVar(&bundleLimit, "limit", 0,
		"Carry only the newest N entries. The default carries the whole chain, up to 25,000 entries.")
	auditBundleCmd.Flags().Int64Var(&bundleFromSeq, "from-seq", 0,
		"Carry the entries from this sequence through --to-seq, at most 25,000 of them.")
	auditBundleCmd.Flags().Int64Var(&bundleToSeq, "to-seq", 0,
		"Carry the entries from --from-seq through this sequence, inclusive.")
	auditBundleCmd.Flags().StringVar(&bundleKeyDir, "key-dir", "",
		"Directory holding the producer signing key. Defaults to the database's directory.")
	auditCmd.AddCommand(auditBundleCmd)
}

// runAuditBundle reads the chain, assembles it into a bundle, signs it, and writes it out.
func runAuditBundle(cmd *cobra.Command, _ []string) error {
	if err := checkBundleWindow(bundleLimit, bundleFromSeq, bundleToSeq); err != nil {
		return err
	}
	store, err := openExisting(bundleDB)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// The identity is loaded before the chain is checked rather than after, because a tree anchor's
	// leaves bind to the install and the anchor check cannot run without it.
	dir, err := keyDir()
	if err != nil {
		return err
	}
	id, err := identityForChain(cmd.Context(), store.Audits(), bundleDB, dir)
	if err != nil {
		return err
	}
	// Every anchor is read, not only those at or below the head. Asking for anchors up to the head
	// meant a chain shortened below its anchor filtered that anchor out of its own evidence: the
	// bundle came out clean, signed, and missing exactly the thing that disproved it. The anchors
	// are what a shortened chain fails, so the chain is held against all of them and a chain that
	// cannot reach one is refused rather than published.
	recorded, err := storedAnchors(cmd.Context(), store.Audits())
	if err != nil {
		return err
	}
	// The whole chain is held, links and anchors together, before any window is cut from it, so
	// every entry that ends up signed was verified and so was the chain it was drawn from.
	//
	// The links used to be checked only by the builder, and the builder is handed the window, so a
	// break older than a --limit range was never looked at: the command wrote a signed bundle over
	// the tail of a chain it could see was broken, while GET /v1/audit/bundle refused the same
	// database. The anchors were already held against the whole chain here, so the thing checked and
	// the thing signed were two different objects. Checking the window instead would read a
	// deliberate narrowing as lost history: --limit would refuse to run at all on an anchored
	// install, reporting that entries the anchor covers were missing when they were merely outside
	// the window, and the workaround an operator would find is to delete the anchors, which is the
	// one action that makes real truncation invisible.
	//
	// The walk streams, as the endpoint's does, and only the window is held in memory. Reading the
	// whole chain to then cut a window from it held about 1.7 GB for a 100,000-entry chain.
	walk, problems, err := streamChainWalk(cmd.Context(), store.Audits(), recorded, id)
	if err != nil {
		return err
	}
	if walk.Count == 0 {
		return errors.New("the audit chain is empty, there is nothing to bundle")
	}
	if !walk.OK {
		return chainBreakRefusal("a bundle", walk)
	}
	if len(problems) > 0 {
		return anchorRefusal("a bundle", problems)
	}
	after, last, err := bundleSlice(walk.Count, walk.Highest, bundleLimit, bundleFromSeq, bundleToSeq)
	if err != nil {
		return err
	}
	entries, err := scanWindow(cmd.Context(), store.Audits(), after, last)
	if err != nil {
		return err
	}

	doc, err := audit.BuildBundle(entries, id, resolveVersion(), time.Now())
	if err != nil {
		// The walk above catches the ordinary tamper. This catches what it does not cover, such as
		// an entry recorded at nanosecond precision or a span beat that does not advance past the
		// one before it, and reports it under the code the endpoint answers with.
		if errors.Is(err, audit.ErrExport) {
			return unbundlableRefusal(err)
		}
		return err
	}
	// Anchors go on before signing, so the signature covers them.
	if len(recorded) == 0 {
		fmt.Fprintln(os.Stderr, "This chain carries no anchors, so nothing outside this install "+
			"attests to it and a verifier cannot tell whether it has lost its tail. "+
			"Run switchtender audit anchor on a schedule.")
	} else {
		if n := doc.AttachAnchors(recorded); n > 0 {
			fmt.Fprintf(os.Stderr, "Attached %d %s, so a verifier can see this chain has not "+
				"been shortened.\n", n, util.Plural(n, "anchor", "anchors"))
		}
		// How far the newest anchor trails the newest entry is stated plainly, because that gap is
		// the part of the record the producer key could still rewrite. A bundle assembled long
		// after its last anchor carries a large one, and so does a chain cut back to an old anchor,
		// which is the shape that otherwise reads cleaner than the honest bundle it replaced. The
		// gap is measured to the chain's head rather than to the window's last entry, because the
		// head is what the producer key could still rewrite whichever slice is handed over.
		warnAnchorLag(walk.Highest, recorded)
	}
	if _, err := doc.AttachStoredAttestations(cmd.Context(), store.Audits()); err != nil {
		return err
	}
	signed, err := audit.SignBundleDoc(doc, id.Private())
	if err != nil {
		return err
	}
	signed = append(signed, '\n')

	if bundleOut == "" {
		_, err = os.Stdout.Write(signed)
		return err
	}
	if err := os.WriteFile(bundleOut, signed, 0o644); err != nil {
		return fmt.Errorf("write bundle: %w", err)
	}
	// The fingerprint goes to standard error so it never contaminates a bundle written to standard
	// output, and so a reader is told the one thing they must publish for the bundle to mean anything.
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Wrote %s with %d entries.\nPublish this fingerprint so a verifier can pin it:\n  %s\n",
		bundleOut, len(entries), id.KeyID())
	return nil
}

// The reasons a signed artifact drawn from the chain is refused. Each names a state of the stored
// chain, never a fault in the command, so a script branches on the code without reading the
// message.
//
// They are the codes GET /v1/audit/bundle and GET /v1/runs/{id}/receipt answer with, word for word,
// so an operator who saw a refusal over HTTP and then reached for the command reads the same answer
// twice instead of two answers. The strings are repeated here rather than shared because the
// endpoints define them inside the server package, which the commands do not import.
const (
	// reasonChainBreak means the stored chain does not recompute: an entry was altered, reordered,
	// or removed. This is the tamper the audit trail exists to surface.
	reasonChainBreak = "chain_break"
	// reasonAnchorUnsatisfied means the chain no longer reaches an anchor recorded over it, which is
	// how a chain that hash-verifies but has lost its tail is caught.
	reasonAnchorUnsatisfied = "anchor_unsatisfied"
	// reasonChainUnbundlable means the builder refused the chain for a state the chain walk does not
	// cover, such as an entry recorded at nanosecond precision or a span beat that does not advance.
	reasonChainUnbundlable = "chain_unbundlable"
)

// chainWalk is what one walk of the stored chain found: whether every link recomputes, and where
// the walk stopped believing it if not. The fields are the coordinates GET /v1/audit/verify and
// the two signing endpoints report, so every answer names the same position in the trail.
type chainWalk struct {
	// OK reports that every entry's hash and link recomputed from genesis.
	OK bool
	// BrokeAt is the one-based position of the first entry that does not verify, zero when OK.
	BrokeAt int
	// BrokeSeq is the chain sequence number of that entry, zero when OK or when the entry carries no
	// readable sequence, which is itself a shape tampering takes.
	BrokeSeq int64
	// Count is how many entries the walk covered.
	Count int
	// Highest is the sequence of the newest entry the walk covered.
	Highest int64
}

// walkChain holds a chain read from the store against its own links and reports where it first
// fails. It walks the same slice a bundle is cut from, so the answer is about the bytes on their
// way to being signed rather than about a second read that could differ from them.
func walkChain(entries []*audit.Entry) chainWalk {
	ok, brokeAt := audit.Verify(entries)
	walk := chainWalk{OK: ok, BrokeAt: brokeAt, Count: len(entries)}
	if !ok && brokeAt >= 1 && brokeAt <= len(entries) && entries[brokeAt-1] != nil {
		walk.BrokeSeq = entries[brokeAt-1].Seq
	}
	return walk
}

// storedAnchors returns every anchor the store keeps, and none when it keeps no anchors.
//
// An install with no anchors is not a failure. It has simply never fixed a link anywhere it cannot
// rewrite, so nothing here can tell it whether its tail is intact, and saying so is more honest
// than passing it silently.
func storedAnchors(ctx context.Context, audits audit.Store) ([]*audit.Anchor, error) {
	anchors, ok := audits.(audit.AnchorStore)
	if !ok {
		return nil, nil
	}
	recorded, err := anchors.Anchors(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("read anchors: %w", err)
	}
	return recorded, nil
}

// refuseUnpublishableChain holds the chain against its own links and against every anchor recorded
// over it, and returns the refusal to fail the command with when either check fails. artifact names
// what could not be published, so the same coordinates read correctly for a bundle and for a
// receipt.
//
// A signed artifact is handed to a third party who has decided to trust nothing this install says.
// A caveat that does not travel inside the signed bytes is therefore not a caveat: the offline
// verifier reads the file and prints that nothing has been altered, because for the entries it
// carries that is true, with no way to know what the install knew when it signed. The caveat cannot
// be attached from here either, since re-marshaling the signed bytes would break the signature.
// Declining to sign is the only answer left that a relying party cannot be shown without, and the
// operator loses nothing forensic: the entries and the position are still there to read.
func refuseUnpublishableChain(entries []*audit.Entry, recorded []*audit.Anchor,
	producer audit.Identity, artifact string) error {
	if walk := walkChain(entries); !walk.OK {
		return chainBreakRefusal(artifact, walk)
	}
	if len(recorded) == 0 {
		return nil
	}
	reachedAll, results := audit.CheckAnchors(entries, recorded, producer)
	if reachedAll {
		return nil
	}
	problems := make([]string, 0, len(results))
	for _, res := range results {
		if !res.Reached {
			problems = append(problems, res.Anchor.ID+": "+res.Problem)
		}
	}
	return anchorRefusal(artifact, problems)
}

// chainBreakRefusal states where a walk of the stored chain found it broken, under the reason code
// and at the coordinates the HTTP refusal carries.
//
// The sequence is named only when the breaking entry carried a readable one, since a row blanked
// out is exactly the tamper that leaves none, and "sequence 0" would read as a fact rather than a
// gap.
func chainBreakRefusal(artifact string, walk chainWalk) error {
	where := fmt.Sprintf("at entry %d of %d", walk.BrokeAt, walk.Count)
	if walk.BrokeSeq > 0 {
		where += fmt.Sprintf(", sequence %d", walk.BrokeSeq)
	}
	return fmt.Errorf("%s: the audit chain does not verify %s, so it cannot be published as %s any "+
		"verifier would accept. An entry was altered, reordered, or removed. This is a break "+
		"detected in the stored chain, not a fault in this command. GET /v1/audit/verify reports "+
		"the same position", reasonChainBreak, where, artifact)
}

// anchorRefusal states that the chain no longer reaches an anchor recorded over it, naming each
// anchor it fails and what is wrong with it.
func anchorRefusal(artifact string, problems []string) error {
	return fmt.Errorf("%s: the chain does not satisfy every anchor recorded over it, so %s drawn "+
		"from it must not be published as one that does. This is a problem detected in the stored "+
		"chain, not a fault in this command: %s",
		reasonAnchorUnsatisfied, artifact, strings.Join(problems, "; "))
}

// unbundlableRefusal restates the builder's own refusal under the reason code the endpoints answer
// with. The sentinel stays wrapped, so a caller can still identify it with errors.Is and a reader
// sees which stage refused.
func unbundlableRefusal(err error) error {
	return fmt.Errorf("%s: %w", reasonChainUnbundlable, err)
}

// warnAnchorLag reports how far the newest anchor sits behind the newest entry.
//
// An anchor fixes history up to the position it names and no further, so everything after it is
// what a compromised or dishonest producer could still rewrite. The size of that gap is the honest
// measure of what an anchored chain is worth, and it is worth saying out loud at the moment the
// bundle is handed over rather than leaving a relying party to work it out.
func warnAnchorLag(head int64, anchors []*audit.Anchor) {
	// With no anchors there is no newest one to name. Saying that the newest anchor covers entry
	// zero described an anchor that does not exist, which is the ordinary state of an install that
	// has never run switchtender audit anchor.
	if head == 0 || len(anchors) == 0 {
		return
	}
	var through int64
	var anchoredAt time.Time
	for _, a := range anchors {
		if a.Seq > through {
			through, anchoredAt = a.Seq, a.At
		}
	}
	if through >= head {
		return
	}
	behind := head - through
	msg := fmt.Sprintf("The newest anchor covers entry %d and this chain ends at entry %d, so %d "+
		"entries are attested by nothing outside this install.", through, head, behind)
	if !anchoredAt.IsZero() {
		msg += fmt.Sprintf(" The last anchor was taken %s ago.",
			time.Since(anchoredAt).Round(time.Minute))
	}
	fmt.Fprintln(os.Stderr, msg)
}

// keyDir returns where the producer identity lives: the override when given, otherwise the install's
// identity directory, which serve derives the same way so the bundle is signed with the key serve
// publishes. For a SQLite file that is the directory beside the database, which an operator already
// backs up and protects; for a postgres DSN it is a stable per-user directory rather than a path
// built from the DSN.
func keyDir() (string, error) {
	if bundleKeyDir != "" {
		return bundleKeyDir, nil
	}
	return identityDir(bundleDB)
}

// maxBundleEntries is how many entries one bundle carries at most, the ceiling GET /v1/audit/bundle
// holds to. A bundle is one signed document over every claim in it, so it is assembled in memory,
// and the ceiling is what keeps that bounded on the smallest documented install.
const maxBundleEntries = audit.MaxAssembledClaims

// errWindowFull stops the window pass once it holds the last entry asked for.
var errWindowFull = errors.New("bundle window is full")

// checkBundleWindow refuses a window the flags could never serve, before the store is opened. A
// window names its slice by sequence because a time can repeat across entries and a sequence cannot.
func checkBundleWindow(limit int, from, to int64) error {
	if limit < 0 {
		return errors.New("--limit must be a count of the newest entries to bundle, such as --limit 1000")
	}
	if limit > maxBundleEntries {
		return fmt.Errorf("--limit must be at most %d, the number of entries one bundle assembles "+
			"at once", maxBundleEntries)
	}
	if from == 0 && to == 0 {
		return nil
	}
	if limit > 0 {
		return errors.New("pass --limit or --from-seq and --to-seq, not both")
	}
	if from < 1 || to < 1 {
		return errors.New("--from-seq and --to-seq go together and name entry sequences from 1, such " +
			"as --from-seq 1 --to-seq 10000")
	}
	if from > to {
		return fmt.Errorf("--from-seq %d is past --to-seq %d", from, to)
	}
	if to-from+1 > maxBundleEntries {
		return fmt.Errorf("--from-seq %d to --to-seq %d is %d entries, more than the %d one bundle "+
			"assembles at once", from, to, to-from+1, maxBundleEntries)
	}
	return nil
}

// bundleSlice decides which entries a bundle carries from the walked chain's size and head, as the
// sequence after which the window starts and the sequence it ends at. An unwindowed export past the
// ceiling is refused with the flags that would serve it, rather than assembled.
func bundleSlice(count int, highest int64, limit int, from, to int64) (int64, int64, error) {
	if from != 0 {
		// A window past the head is refused rather than cut short, because a bundle that silently
		// carries less than was named is a different slice from the one asked for.
		if to > highest {
			return 0, 0, fmt.Errorf("--to-seq %d is past the newest entry in the chain, sequence %d",
				to, highest)
		}
		return from - 1, to, nil
	}
	if limit == 0 {
		if count > maxBundleEntries {
			return 0, 0, fmt.Errorf("this chain holds %d entries, more than the %d one bundle "+
				"assembles at once. Pass --limit <count>, such as --limit 10000, for the newest "+
				"entries, or --from-seq and --to-seq for an older slice", count, maxBundleEntries)
		}
		limit = count
	}
	if limit > count {
		limit = count
	}
	return highest - int64(limit), highest, nil
}

// streamChainWalk holds the stored chain against its own links and against every anchor recorded
// over it in one streaming pass, and returns where the links first fail and the problem with each
// anchor the chain does not reach. Nothing is held but the scanners' state, so the walk costs the
// same memory at any chain length.
func streamChainWalk(ctx context.Context, audits audit.Store, recorded []*audit.Anchor,
	producer audit.Identity) (chainWalk, []string, error) {
	links := audit.NewChainScanner(true)
	var anchors *audit.AnchorScanner
	if len(recorded) > 0 {
		anchors = audit.NewAnchorScanner(recorded, producer)
	}
	var walk chainWalk
	err := audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		links.Feed(e)
		if anchors != nil {
			anchors.Feed(e)
		}
		if e == nil {
			return nil
		}
		walk.Highest = e.Seq
		// The scanner marks a break at the position it was fed and never moves it, so the entry
		// whose position equals the count fed so far is the one that broke the chain.
		if _, at, walked := links.Result(); at != 0 && at == walked {
			walk.BrokeSeq = e.Seq
		}
		return nil
	})
	if err != nil {
		return chainWalk{}, nil, fmt.Errorf("read audit chain: %w", err)
	}
	walk.OK, walk.BrokeAt, walk.Count = links.Result()
	if anchors == nil {
		return walk, nil, nil
	}
	reachedAll, results := anchors.Results()
	if reachedAll {
		return walk, nil, nil
	}
	problems := make([]string, 0, len(results))
	for _, res := range results {
		if !res.Reached {
			problems = append(problems, res.Anchor.ID+": "+res.Problem)
		}
	}
	return walk, problems, nil
}

// scanWindow reads the entries after the sequence after through last, oldest first. It stops at
// last, so an entry appended since the walk is never carried into a bundle unchecked.
func scanWindow(ctx context.Context, audits audit.Store, after, last int64) ([]*audit.Entry, error) {
	entries := make([]*audit.Entry, 0, last-after)
	err := audits.ChainScan(ctx, after, func(e *audit.Entry) error {
		if e != nil && e.Seq > last {
			return errWindowFull
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil && !errors.Is(err, errWindowFull) {
		return nil, fmt.Errorf("read audit chain: %w", err)
	}
	return entries, nil
}
