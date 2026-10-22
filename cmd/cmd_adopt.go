package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

var (
	// adoptDB is the database of the install adopt seals.
	adoptDB string
	// adoptDryRun prints what adoption would seal and writes nothing.
	adoptDryRun bool
	// adoptYes seals without asking, for a script.
	adoptYes bool
	// adoptFailUnsealed records adoption trusting no row and fails the unsealed rows instead.
	adoptFailUnsealed bool
)

// serveAdoptionActor is who the chain names for the empty adoption a keyed serve records at its
// first start.
const serveAdoptionActor = "switchtender serve"

// restoreAdvice says where an install goes instead of adopting when its chain or its rows show
// keyed operation began. A backup file holds neither runs nor the audit chain, so it names a copy
// of the database.
const restoreAdvice = "Restore the database, audit chain included, from a copy taken before the " +
	"upgrade, such as a copy of the SQLite file or a PostgreSQL dump"

// failUnsealedAdvice names the way out for an install with no copy to restore, beside
// restoreAdvice, for a refusal --fail-unsealed answers.
const failUnsealedAdvice = "With no such copy, switchtender adopt --fail-unsealed records the " +
	"adoption trusting no row and fails every unfinished run that carries no seal, so a server " +
	"of this release starts and runs none of them"

const (
	// adoptPage is how many top-level runs one read of the seal scan lists.
	adoptPage = 500
	// adoptShown is how many row ids a refusal names before it counts the rest.
	adoptShown = 20
)

// adoptCmd seals, once per install, the work a release with no binding key left behind.
var adoptCmd = &cobra.Command{
	Use:   "adopt",
	Short: "Seal the work an earlier release left, once, before this release first serves.",
	Long: "Seal the held runs, queued runs, workflow steps, and decisions in flight that a release " +
		"with no approval binding key left behind, so a server of this release holds them to seals " +
		"the way it holds the work it creates. A keyed server refuses to start over such work until " +
		"this has run. It is the one trusted moment in the life of an install's seals, so it " +
		"happens once: it appends an adoption record to the audit chain under an id this install's " +
		"key fixes, and only then writes the seals.\n\n" +
		"It refuses, and writes nothing, when the audit chain does not verify, when any run already " +
		"carries a seal, or when the chain holds an entry only a keyed install writes, since each " +
		"shows keyed operation already began. It prints every row it would seal and asks before it " +
		"writes. --yes skips the question for a script, and --dry-run prints the rows and stops.\n\n" +
		"--fail-unsealed is for an install adopt refuses that has no copy to restore. It records " +
		"the adoption with no row adopted and fails every unfinished run that carries no seal, " +
		"trusting none of them. A run that carries a seal is left for the server to check when it " +
		"reaches it. It still refuses a chain that does not verify and an install already " +
		"adopted.\n\n" +
		"Before it asks, it prints the chain's entry count, its newest anchor, and the install it is " +
		"bound to. Adoption runs once per install, so being asked to run it on an install already " +
		"adopted or served by this release is itself evidence that the chain was rewritten.\n\n" +
		"Run it after every process of the earlier release has stopped and before this release's " +
		"serve first starts.",
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runAdopt,
}

// init registers the adopt command and its flags.
func init() {
	adoptCmd.Flags().StringVar(&adoptDB, "db", defaultDBPath,
		"SQLite file path, or a postgres:// DSN, of the install to adopt.")
	adoptCmd.Flags().BoolVar(&adoptDryRun, "dry-run", false,
		"Print the rows adoption would seal and write nothing.")
	adoptCmd.Flags().BoolVar(&adoptYes, "yes", false,
		"Seal without asking, for a script. The rows are still printed.")
	adoptCmd.Flags().BoolVar(&adoptFailUnsealed, "fail-unsealed", false,
		"Record the adoption trusting no row, and fail every unfinished run that carries no seal.")
	rootCmd.AddCommand(adoptCmd)
}

// runAdopt adopts the install the flags name, asking on the terminal standard input is.
func runAdopt(cmd *cobra.Command, _ []string) error {
	return adopt(cmd.Context(), adoptRequest{
		DB:           dbFromEnv(cmd, adoptDB),
		DryRun:       adoptDryRun,
		Yes:          adoptYes,
		FailUnsealed: adoptFailUnsealed,
		Terminal:     term.IsTerminal(int(os.Stdin.Fd())),
		In:           os.Stdin,
		Out:          os.Stdout,
		Prompt:       os.Stderr,
	})
}

// adoptRequest is one run of the adopt command: the install, how it was asked to run, and where it
// reads its answer and shows what it does.
type adoptRequest struct {
	// DB is the database of the install to adopt.
	DB string
	// DryRun prints the plan and writes nothing.
	DryRun bool
	// Yes seals without asking.
	Yes bool
	// FailUnsealed records the adoption trusting no row and fails the unsealed rows instead.
	FailUnsealed bool
	// Terminal reports whether In is a terminal a person answers on.
	Terminal bool
	// In is where the answer to the confirmation is read.
	In io.Reader
	// Out receives the plan and what adoption did.
	Out io.Writer
	// Prompt receives the confirmation question.
	Prompt io.Writer
	// Open opens the install's stores. Nil opens them with openExisting.
	Open func(db string) (storeBundle, error)
}

// adopt checks that the install has never run keyed, shows every row adoption would seal, asks,
// and adopts. Each refusal comes before anything is written: a chain that does not verify, a run
// that carries a seal, and a chain entry only a keyed install writes. What it shows and what it
// seals are one read of the install, held by an adoptSnapshot, so a row changed or created while
// the person reads the list is never sealed as changed.
func adopt(ctx context.Context, req adoptRequest) error {
	open := req.Open
	if open == nil {
		open = openExisting
	}
	bundle, err := open(req.DB)
	if err != nil {
		return err
	}
	defer func() { _ = bundle.Close() }()
	audits := bundle.Audits()
	id, key, err := adoptionKey(ctx, audits, req.DB)
	if err != nil {
		return err
	}
	// The record and the RELEASE entries adoption appends commit to this install, as every entry
	// serve and the command line append do.
	if binder, ok := audits.(audit.InstallBinder); ok {
		binder.BindInstall(id.InstallID)
	}
	chain, err := readAdoptChain(ctx, audits, id)
	if err != nil {
		return err
	}
	if err := refuseBrokenChain(chain); err != nil {
		return err
	}
	if req.FailUnsealed {
		return failUnsealed(ctx, req, bundle.Runs(), audits, key, chain, id.InstallID)
	}
	if err := refuseKeyedEntries(chain, key); err != nil {
		return err
	}
	if err := refuseSealedRows(ctx, bundle.Runs()); err != nil {
		return err
	}
	snap := newAdoptSnapshot(bundle.Runs(), audits, key.AdoptionID(), chain.Decisions)
	deps := dispatch.AdoptionDeps{Runs: snap.runStore(), Audits: snap.chainStore(), Key: key,
		Actor: outcome.Decider{Name: cliActor(), Type: actorTypeCLI}}
	work, err := dispatch.AdoptionPlan(ctx, deps)
	if err != nil {
		return err
	}
	if len(work.Sealed) > 0 {
		return sealedRowsRefusal(work.Sealed)
	}
	showChain(req.Out, chain, id.InstallID, adoptKeySource(req.DB))
	showAdoption(req.Out, work)
	if req.DryRun {
		_, _ = fmt.Fprintln(req.Out, "Dry run: nothing was written.")
		return nil
	}
	if err := confirmAdoption(req, fmt.Sprintf("Seal these %d rows and record the adoption on "+
		"the audit chain?", len(work.Rows))); err != nil {
		return err
	}
	res, err := dispatch.Adopt(ctx, deps)
	if res != nil && res.Earlier {
		return earlierAdoptionRefusal(res.Record)
	}
	if res != nil {
		showAdopted(req.Out, res)
	}
	return err
}

// failUnsealed records the adoption of an install trusting no row and fails every unfinished run
// that carries no seal, for an install adopt refuses because it shows keyed operation began. The
// entries and seals behind that refusal are not refused here, since nothing is trusted: a run that
// carries a seal is left for a keyed server to check. An install whose chain already records its
// adoption is refused, as adopt refuses it. What it shows and what it fails are one read of the
// install, as for adopt.
func failUnsealed(ctx context.Context, req adoptRequest, runs run.Store, audits audit.Store,
	key *binding.Key, chain *adoptChain, installID string) error {
	if i := slices.IndexFunc(chain.Keyed, func(e *audit.Entry) bool {
		return e.ID == key.AdoptionID()
	}); i >= 0 {
		return earlierAdoptionRefusal(chain.Keyed[i])
	}
	snap := newAdoptSnapshot(runs, audits, key.AdoptionID(), chain.Decisions)
	deps := dispatch.AdoptionDeps{Runs: snap.runStore(), Audits: snap.chainStore(), Key: key,
		Actor: outcome.Decider{Name: cliActor(), Type: actorTypeCLI}}
	work, err := dispatch.UnsealedPlan(ctx, deps)
	if err != nil {
		return err
	}
	showChain(req.Out, chain, installID, adoptKeySource(req.DB))
	showUnsealed(req.Out, work)
	if req.DryRun {
		_, _ = fmt.Fprintln(req.Out, "Dry run: nothing was written.")
		return nil
	}
	if err := confirmAdoption(req, fmt.Sprintf("Fail these %d runs and record the adoption, "+
		"trusting no row, on the audit chain?", len(work.Fail))); err != nil {
		return err
	}
	res, err := dispatch.FailUnsealed(ctx, deps)
	if res != nil && res.Earlier {
		return earlierAdoptionRefusal(res.Record)
	}
	if res != nil {
		showUnadopted(req.Out, res)
	}
	return err
}

// adoptionKey loads the producer identity of the install db names and derives its binding key. An
// install with no identity is refused rather than given one, since a server on it holds no key
// and checks rows without seals, so there is nothing to adopt, and a key created here could be one
// no server of the install ever holds.
func adoptionKey(ctx context.Context, audits audit.Store, db string) (audit.Identity, *binding.Key, error) {
	dir, err := identityDir(db)
	if err != nil {
		return audit.Identity{}, nil, err
	}
	if !audit.IdentityPresent(dir) {
		return audit.Identity{}, nil, fmt.Errorf("this install has no producer identity in %s and "+
			"%s is not set, so its servers hold no binding key and there is nothing to adopt. Run "+
			"adopt with the identity directory or the seed every server of this install uses. "+
			"Nothing was changed", dir, identity.KeyEnv)
	}
	id, err := loadProducerIdentity(ctx, audits, db)
	if err != nil {
		return audit.Identity{}, nil, err
	}
	key, err := bindingKeyOf(&id)
	if err != nil {
		return audit.Identity{}, nil, err
	}
	return id, key, nil
}

// adoptChain is what one walk of an install's audit chain shows adopt: whether every link and
// anchor holds, the entries only a keyed install writes, and the DECISION entries adoption may seal
// a row to.
type adoptChain struct {
	// Count is how many entries the chain holds.
	Count int
	// BrokeAt is the one-based position of the first entry whose sequence, link, or hash does not
	// check out, zero when every one does.
	BrokeAt int
	// AnchorProblems says, for each anchor the chain no longer satisfies, why.
	AnchorProblems []string
	// Keyed are the entries only a keyed install writes, RELEASE entries and adoption records, in
	// chain order.
	Keyed []*audit.Entry
	// Decisions holds every DECISION entry by id, as the walk verified it.
	Decisions map[string]*audit.Entry
	// Newest is the anchor recorded over the most entries, nil when the chain has none.
	Newest *audit.Anchor
	// Bound is the install the chain's newest entry that names one names, empty for none.
	Bound string
}

// readAdoptChain walks the whole chain once, through the scanners the server's chain verification
// uses, holding it to its links from genesis and to every anchor recorded over it, and keeps what
// adopt's checks and plan read.
func readAdoptChain(ctx context.Context, audits audit.Store, id audit.Identity) (*adoptChain, error) {
	var anchors []*audit.Anchor
	if anchored, ok := audits.(audit.AnchorStore); ok {
		var err error
		if anchors, err = anchored.Anchors(ctx, 0); err != nil {
			return nil, fmt.Errorf("read the audit anchors: %w", err)
		}
	}
	links := audit.NewChainScanner(true)
	reach := audit.NewAnchorScanner(anchors, id)
	out := &adoptChain{Decisions: map[string]*audit.Entry{}}
	for _, a := range anchors {
		if out.Newest == nil || a.Seq > out.Newest.Seq {
			out.Newest = a
		}
	}
	if reader, ok := audits.(audit.InstallReader); ok {
		var err error
		if out.Bound, err = reader.BoundInstall(ctx); err != nil {
			return nil, fmt.Errorf("read the install the audit chain is bound to: %w", err)
		}
	}
	err := audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
		links.Feed(e)
		reach.Feed(e)
		if e == nil {
			return nil
		}
		kept := *e
		switch {
		case e.Method == audit.MethodRelease || e.Path == audit.AdoptedPath:
			out.Keyed = append(out.Keyed, &kept)
		case e.Method == audit.MethodDecision:
			out.Decisions[e.ID] = &kept
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the audit chain: %w", err)
	}
	_, out.BrokeAt, out.Count = links.Result()
	if len(anchors) > 0 {
		if ok, results := reach.Results(); !ok {
			for _, r := range results {
				if !r.Reached {
					out.AnchorProblems = append(out.AnchorProblems, r.Problem)
				}
			}
		}
	}
	return out, nil
}

// refuseBrokenChain refuses a chain that does not verify. Adoption happens once because its record
// is on the chain, and a chain an entry was removed from or rewritten in cannot show that it is
// not there, so a break anywhere is a reason not to adopt.
func refuseBrokenChain(c *adoptChain) error {
	if c.BrokeAt != 0 {
		return fmt.Errorf("%w: the audit chain does not verify at entry %d of %d, so it cannot show "+
			"that adoption never ran on this install. GET /v1/audit/verify reports the same break. "+
			"%s, rather than adopt over it. Nothing was changed", binding.ErrDrift, c.BrokeAt,
			c.Count, restoreAdvice)
	}
	if len(c.AnchorProblems) > 0 {
		return fmt.Errorf("%w: the audit chain no longer satisfies an anchor recorded over it, so "+
			"entries that provably existed are gone and it cannot show that adoption never ran on "+
			"this install: %s. %s, rather than adopt over it. Nothing was changed",
			binding.ErrDrift, strings.Join(c.AnchorProblems, ", "), restoreAdvice)
	}
	return nil
}

// refuseKeyedEntries refuses a chain holding an entry only an install with a binding key writes:
// a RELEASE entry, which a keyed launch appends, or an adoption record. Either shows keyed
// operation began, and adopting again would seal whatever a writer left since as if it were the
// work the earlier release left.
func refuseKeyedEntries(c *adoptChain, key *binding.Key) error {
	if len(c.Keyed) == 0 {
		return nil
	}
	if i := slices.IndexFunc(c.Keyed, func(e *audit.Entry) bool {
		return e.ID == key.AdoptionID()
	}); i >= 0 {
		return earlierAdoptionRefusal(c.Keyed[i])
	}
	e := c.Keyed[0]
	what := "a RELEASE entry"
	if e.Path == audit.AdoptedPath {
		what = "an adoption record made under another binding key"
	}
	return fmt.Errorf("%w: the audit chain holds %s, entry %s at %s, which only an install holding "+
		"a binding key writes, so keyed operation already began on this install. %s, rather than "+
		"adopt again. %s. Nothing was changed", binding.ErrDrift, what, e.ID, e.Path, restoreAdvice,
		failUnsealedAdvice)
}

// earlierAdoptionRefusal refuses adopting an install whose chain already records its adoption,
// naming when and by whom. A nil record is one another process appended and this one could not
// read back.
func earlierAdoptionRefusal(e *audit.Entry) error {
	if e == nil {
		return fmt.Errorf("%w: another adoption was recorded on this install while this one ran, "+
			"and adoption happens once. Nothing was changed", binding.ErrDrift)
	}
	return fmt.Errorf("%w: adoption was already recorded on this install, as audit entry %s at %s "+
		"by %s, and it happens once. Nothing was changed", binding.ErrDrift, e.ID,
		e.At.UTC().Format(time.RFC3339), e.Actor)
}

// refuseSealedRows refuses an install any run of which carries a seal, finished runs included. A
// release with no binding key never writes one, so a seal shows keyed operation began, and
// adopting again would seal what a writer changed since beside what that operation sealed.
func refuseSealedRows(ctx context.Context, runs run.Store) error {
	sealed, err := sealedRows(ctx, runs)
	if err != nil {
		return err
	}
	if len(sealed) > 0 {
		return sealedRowsRefusal(sealed)
	}
	return nil
}

// sealedRowsRefusal refuses adopting an install whose runs ids carry a seal.
func sealedRowsRefusal(ids []string) error {
	return fmt.Errorf("%w: %d runs already carry a seal (%s), which only an install holding a "+
		"binding key writes, so keyed operation already began on this install. %s, rather than "+
		"adopt again. %s. Nothing was changed", binding.ErrDrift, len(ids), shownIDs(ids),
		restoreAdvice, failUnsealedAdvice)
}

// sealedRows returns, sorted, the ids of the runs that carry a seal: every top-level run, every
// shard and step of a split or a workflow, and every run not finished, which covers a child whose
// parent is gone. Only a split or a workflow has children, so a plain run's are not listed.
func sealedRows(ctx context.Context, runs run.Store) ([]string, error) {
	seen := map[string]bool{}
	var sealed []string
	check := func(r *run.Run) {
		if seen[r.ID] {
			return
		}
		seen[r.ID] = true
		if dispatch.CarriesSeal(r) {
			sealed = append(sealed, r.ID)
		}
	}
	for offset := 0; ; offset += adoptPage {
		page, err := runs.ListPage(ctx, run.ListFilter{OldestFirst: true}, adoptPage, offset)
		if err != nil {
			return nil, fmt.Errorf("list the runs: %w", err)
		}
		for _, r := range page {
			check(r)
			if r.Kind != run.KindSplit && r.Kind != run.KindPipeline {
				continue
			}
			children, err := runs.Steps(ctx, r.ID)
			if err != nil {
				return nil, fmt.Errorf("list the children of run %s: %w", r.ID, err)
			}
			for _, c := range children {
				check(c)
			}
		}
		if len(page) < adoptPage {
			break
		}
	}
	live, err := runs.NonTerminal(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the unfinished runs: %w", err)
	}
	for _, r := range live {
		check(r)
	}
	slices.Sort(sealed)
	return sealed, nil
}

// shownIDs joins up to adoptShown of ids and counts the rest.
func shownIDs(ids []string) string {
	shown := strings.Join(ids[:min(len(ids), adoptShown)], ", ")
	if more := len(ids) - adoptShown; more > 0 {
		shown += fmt.Sprintf(", and %d more", more)
	}
	return shown
}

// showChain prints what the person running adopt checks the chain against before answering: how
// many entries it holds, its newest anchor, and the install it is bound to, or for a chain bound to
// none the install this adoption binds it to and source, the key that names it. Adoption runs once
// per install, so it also says that a request to run it on an install adopted before is itself
// evidence that the chain was rewritten.
func showChain(out io.Writer, c *adoptChain, installID, source string) {
	_, _ = fmt.Fprintf(out, "The audit chain holds %d entries. ", c.Count)
	if c.Newest == nil {
		_, _ = fmt.Fprint(out, "It has no anchors. ")
	} else {
		_, _ = fmt.Fprintf(out, "Its newest anchor is a %s anchor at %d, %s. ", c.Newest.Type,
			c.Newest.Seq, c.Newest.Ref)
	}
	if c.Bound == "" {
		_, _ = fmt.Fprintf(out, "It is bound to no install yet, and this adoption binds it to "+
			"install %s, %s.\n", installID, source)
	} else {
		_, _ = fmt.Fprintf(out, "It is bound to install %s.\n", c.Bound)
	}
	_, _ = fmt.Fprintln(out, "Adoption runs once per install, at the upgrade. If this install was "+
		"adopted or served by this release before, being asked to adopt it now is itself evidence "+
		"that its chain was rewritten: compare the count and the anchor with your own records, and "+
		"restore rather than answer yes.")
	_, _ = fmt.Fprintln(out)
}

// adoptKeySource names where adopt's key for db comes from: the seed in the environment, or the
// identity file it read. An unbound chain is bound to whatever key adopt runs with, and a server
// whose key names another install refuses to start, so on a shared database whose key came from
// this account's own configuration directory it also says every server must sign with that key.
func adoptKeySource(db string) string {
	if os.Getenv(identity.KeyEnv) != "" {
		return "the key in " + identity.KeyEnv
	}
	dir, err := identityDir(db)
	if err != nil {
		return "the key this account holds"
	}
	path := filepath.Join(dir, audit.IdentityFile)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	source := "the key in " + path
	if isPostgresDSN(db) && os.Getenv(identityDirEnv) == "" {
		source += ". That file is this account's own. Every server of this install must sign with " +
			"this same key, or the first keyed start is refused. If the servers will sign with a " +
			"seed, answer no and run adopt with " + identity.KeyEnv + " set to it"
	}
	return source
}

// showAdoption prints every row work seals, with the kind of seal, who asked for the row, its spec
// digest, and the RELEASE entry appended for it, then every row it leaves and why.
func showAdoption(out io.Writer, work *dispatch.AdoptionWork) {
	if len(work.Rows) == 0 {
		_, _ = fmt.Fprintln(out, "Adoption seals no rows on this install. It records on the audit "+
			"chain that it ran, so a server of this release starts.")
	} else {
		_, _ = fmt.Fprintf(out, "Adoption seals %d rows under this install's binding key, as they "+
			"read now:\n\n", len(work.Rows))
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  RUN\tSEAL\tREQUESTED BY\tDECIDED BY\tDECISION ENTRY\t"+
			"SPEC DIGEST\tRELEASE ENTRY")
		unrecorded := 0
		for _, r := range work.Rows {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Seal, orNone(r.Actor),
				orNone(r.DecidedBy), orNone(r.DecisionEntry), r.SpecDigest, orNone(r.Release))
			if r.Unrecorded {
				unrecorded++
			}
		}
		_ = tw.Flush()
		if unrecorded > 0 {
			_, _ = fmt.Fprintf(out, "\n%d decision claims carry a decision the audit chain does not "+
				"hold yet. Adoption trusts each as its claim records it, and a server of this release "+
				"records it on the chain when it settles the claim.\n", unrecorded)
		}
	}
	if len(work.Left) > 0 {
		_, _ = fmt.Fprintf(out, "\nIt leaves %d rows unsealed, which a server of this release "+
			"refuses when it reaches them:\n\n", len(work.Left))
		for _, l := range work.Left {
			_, _ = fmt.Fprintf(out, "  %s  %s\n", l.ID, l.Reason)
		}
	}
	_, _ = fmt.Fprintln(out)
}

// orNone returns s, or a dash for an empty one, so a table column never reads as missing.
func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// confirmAdoption asks the person running adopt question about the rows shown, unless req says to
// write without asking. Standard input that is not a terminal has nobody to ask, so it is refused
// rather than read as an answer. Anything but yes cancels, having written nothing.
func confirmAdoption(req adoptRequest, question string) error {
	if req.Yes {
		return nil
	}
	if !req.Terminal {
		return fmt.Errorf("%w: adopt asks before it writes, and standard input is not a terminal. "+
			"Run it at a terminal, or pass --yes from a script that has read the rows above. "+
			"Nothing was changed", ErrUsage)
	}
	_, _ = fmt.Fprintf(req.Prompt, "%s [y/N] ", question)
	line, err := bufio.NewReader(req.In).ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("adoption canceled, nothing was changed: %w", err)
	}
	if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
		return fmt.Errorf("adoption canceled, nothing was changed")
	}
	return nil
}

// showAdopted prints what an adoption did: its record, the rows it sealed, the rows another
// process moved first, and the rows it could not write.
func showAdopted(out io.Writer, res *dispatch.Adopted) {
	if res.Record != nil {
		_, _ = fmt.Fprintf(out, "Recorded the adoption as audit entry %s.\n", res.Record.ID)
	}
	_, _ = fmt.Fprintf(out, "Sealed %d rows.\n", len(res.Sealed))
	if len(res.Moved) > 0 {
		_, _ = fmt.Fprintf(out, "%d rows were sealed, decided, or settled by another process "+
			"first, and were left as it left them: %s\n", len(res.Moved), shownIDs(res.Moved))
	}
	for _, l := range res.Left {
		_, _ = fmt.Fprintf(out, "Not sealed: %s, %s\n", l.ID, l.Reason)
	}
}

// showUnsealed prints every run --fail-unsealed fails, with the status it waits in and who asked
// for it, then every unsealed run it leaves and why, then the runs that carry a seal.
func showUnsealed(out io.Writer, work *dispatch.UnsealedWork) {
	if len(work.Fail) == 0 {
		_, _ = fmt.Fprintln(out, "Nothing here is failed. The adoption is recorded trusting no "+
			"row, so a server of this release starts.")
	} else {
		_, _ = fmt.Fprintf(out, "Adoption trusts no row. These %d unfinished runs carry no seal "+
			"and are failed, so none of them runs:\n\n", len(work.Fail))
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  RUN\tSTATUS\tREQUESTED BY")
		for _, r := range work.Fail {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.ID, r.Status, orNone(r.Actor))
		}
		_ = tw.Flush()
	}
	if len(work.Left) > 0 {
		_, _ = fmt.Fprintf(out, "\nIt leaves %d unsealed runs as they are:\n\n", len(work.Left))
		for _, l := range work.Left {
			_, _ = fmt.Fprintf(out, "  %s  %s\n", l.ID, l.Reason)
		}
	}
	if len(work.Sealed) > 0 {
		_, _ = fmt.Fprintf(out, "\n%d unfinished runs carry a seal, which a server of this release "+
			"checks when it reaches them, refusing any it did not make: %s\n", len(work.Sealed),
			shownIDs(work.Sealed))
	}
	_, _ = fmt.Fprintln(out)
}

// showUnadopted prints what --fail-unsealed did: its record, the runs it failed, the runs another
// process moved first, and the runs it could not end.
func showUnadopted(out io.Writer, res *dispatch.Unadopted) {
	if res.Record != nil {
		_, _ = fmt.Fprintf(out, "Recorded the adoption, trusting no row, as audit entry %s.\n",
			res.Record.ID)
	}
	_, _ = fmt.Fprintf(out, "Failed %d runs.\n", len(res.Failed))
	if len(res.Moved) > 0 {
		_, _ = fmt.Fprintf(out, "%d runs were moved by another process first, and were left as it "+
			"left them: %s\n", len(res.Moved), shownIDs(res.Moved))
	}
	for _, l := range res.Left {
		_, _ = fmt.Fprintf(out, "Not failed: %s, %s\n", l.ID, l.Reason)
	}
}

// adoptSnapshot holds what adoption reads from the moment its plan is shown until its record is on
// the chain, so the rows it seals are the rows the person running adopt confirmed, sealed as they
// read then. A row another process changes afterward carries a seal over what it was, which fails
// closed, and a row created afterward is not sealed at all. A chain entry a seal names comes from
// the walk that verified the chain, so no seal is made over an entry changed after that walk. The
// adoption record itself is always read from the chain, which is what tells two adoptions apart,
// and once the record is appended every read goes to the stores, as the writes that follow need.
type adoptSnapshot struct {
	// runs is the run store the first reads come from and every write goes to.
	runs run.Store
	// audits is the chain the record and the RELEASE entries are appended to.
	audits audit.Store
	// recordID is the id of the adoption record.
	recordID string
	// decisions holds the DECISION entries the chain walk verified, by id.
	decisions map[string]*audit.Entry
	// mu guards every field below.
	mu sync.Mutex
	// live reports that the record was appended, after which reads go to the stores.
	live bool
	// nonTerminal is the first answer to NonTerminal, nil until there is one.
	nonTerminal []*run.Run
	// rows holds the first answer to Get by id, a nil run for one not found.
	rows map[string]*run.Run
	// steps holds the first answer to Steps by workflow id.
	steps map[string][]*run.Run
}

// newAdoptSnapshot returns a snapshot over runs and audits, with recordID the adoption record's id
// and decisions the DECISION entries the chain walk verified.
func newAdoptSnapshot(runs run.Store, audits audit.Store, recordID string,
	decisions map[string]*audit.Entry) *adoptSnapshot {
	return &adoptSnapshot{runs: runs, audits: audits, recordID: recordID, decisions: decisions,
		rows: map[string]*run.Run{}, steps: map[string][]*run.Run{}}
}

// runStore returns the run store adoption reads and writes through the snapshot.
func (s *adoptSnapshot) runStore() run.Store { return snapshotRuns{Store: s.runs, s: s} }

// chainStore returns the chain adoption reads and appends to through the snapshot.
func (s *adoptSnapshot) chainStore() audit.Store { return snapshotChain{Store: s.audits, s: s} }

// isLive reports whether the record was appended.
func (s *adoptSnapshot) isLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// cloneRuns returns a copy of rs whose rows a reader can change without changing the snapshot's.
func cloneRuns(rs []*run.Run) []*run.Run {
	out := make([]*run.Run, len(rs))
	for i, r := range rs {
		out[i] = r.Clone()
	}
	return out
}

// snapshotRuns is a run store that answers the reads adoption plans from out of an adoptSnapshot
// and sends everything else, every write included, to the store.
type snapshotRuns struct {
	// Store is the run store.
	run.Store
	// s is the snapshot.
	s *adoptSnapshot
}

// NonTerminal returns the first answer the store gave, until the record is appended.
func (r snapshotRuns) NonTerminal(ctx context.Context) ([]*run.Run, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.live {
		return r.Store.NonTerminal(ctx)
	}
	if r.s.nonTerminal == nil {
		rows, err := r.Store.NonTerminal(ctx)
		if err != nil {
			return nil, err
		}
		r.s.nonTerminal = cloneRuns(rows)
	}
	return cloneRuns(r.s.nonTerminal), nil
}

// Get returns the first answer the store gave for id, until the record is appended.
func (r snapshotRuns) Get(ctx context.Context, id string) (*run.Run, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.live {
		return r.Store.Get(ctx, id)
	}
	held, ok := r.s.rows[id]
	if !ok {
		got, err := r.Store.Get(ctx, id)
		switch {
		case err == nil:
			held = got.Clone()
		case !errors.Is(err, run.ErrNotFound):
			return nil, err
		}
		r.s.rows[id] = held
	}
	if held == nil {
		return nil, run.ErrNotFound
	}
	return held.Clone(), nil
}

// Steps returns the first answer the store gave for parentID, until the record is appended.
func (r snapshotRuns) Steps(ctx context.Context, parentID string) ([]*run.Run, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.live {
		return r.Store.Steps(ctx, parentID)
	}
	held, ok := r.s.steps[parentID]
	if !ok {
		rows, err := r.Store.Steps(ctx, parentID)
		if err != nil {
			return nil, err
		}
		held = cloneRuns(rows)
		r.s.steps[parentID] = held
	}
	return cloneRuns(held), nil
}

// snapshotChain is a chain that answers the entry reads adoption plans from out of an adoptSnapshot
// and appends to the chain, noting the moment the adoption record is appended.
type snapshotChain struct {
	// Store is the chain.
	audit.Store
	// s is the snapshot.
	s *adoptSnapshot
}

// Entry reads the adoption record, and every entry once the record is appended, from the chain.
// Before that, a DECISION entry comes from the walk that verified the chain, and any other entry
// is not one adoption can seal a row to, so it reads as absent.
func (c snapshotChain) Entry(ctx context.Context, id string) (*audit.Entry, error) {
	if id == c.s.recordID || c.s.isLive() {
		return c.Store.Entry(ctx, id)
	}
	e, ok := c.s.decisions[id]
	if !ok {
		return nil, fmt.Errorf("audit entry %s: %w", id, audit.ErrEntryNotFound)
	}
	kept := *e
	return &kept, nil
}

// Append appends e to the chain, and when e is the adoption record, sends every later read to the
// stores.
func (c snapshotChain) Append(ctx context.Context, e *audit.Entry) error {
	if e.ID == c.s.recordID {
		c.s.mu.Lock()
		c.s.live = true
		c.s.mu.Unlock()
	}
	return c.Store.Append(ctx, e)
}
