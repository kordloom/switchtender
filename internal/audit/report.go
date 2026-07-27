package audit

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"
)

// reportTemplateSource is the self-contained HTML evidence report, rendered offline from an export.
//
//go:embed report.html
var reportTemplateSource string

// reportTemplate is the parsed evidence-report template.
var reportTemplate = template.Must(template.New("report").Parse(reportTemplateSource))

// actorStat is one actor and how many entries they account for.
type actorStat struct {
	// Actor is the acting principal.
	Actor string
	// Count is how many chain entries the actor produced.
	Count int
}

// entryRow is one audit entry as shown in the report table.
type entryRow struct {
	// Seq is the entry's position in the chain.
	Seq int64
	// At is the entry time, formatted for display.
	At string
	// Actor is the acting principal.
	Actor string
	// Action is the method and path the entry recorded.
	Action string
}

// reportView is the data rendered into the HTML evidence report.
type reportView struct {
	// Status is the machine verdict: verified, unsigned, signature-invalid, or broken.
	Status string
	// StatusText is the human verdict shown in the banner.
	StatusText string
	// ChainIntact reports whether the hash chain recomputed and ended at the head.
	ChainIntact bool
	// Count is the number of entries.
	Count int
	// HeadHash is the full head hash.
	HeadHash string
	// HeadShort is the head hash truncated for display.
	HeadShort string
	// PublicKey is the hex signing key an auditor pins, empty when unsigned.
	PublicKey string
	// Algo names the signature algorithm, empty when unsigned.
	Algo string
	// SignedAt is when the export was signed, empty when unsigned.
	SignedAt string
	// Period is the time span the entries cover.
	Period string
	// Actors is the per-actor breakdown, most active first.
	Actors []actorStat
	// ActorCount is the number of distinct actors.
	ActorCount int
	// Mutations is the number of state-changing entries.
	Mutations int
	// Reads is the number of read-only entries.
	Reads int
	// GeneratedAt is when the report was rendered.
	GeneratedAt string
	// Entries is the full chain, oldest first.
	Entries []entryRow
}

// Report verifies exp and renders a self-contained HTML evidence report of the audit chain, stating
// whether the chain is intact and, when signed, whether the signature is valid against the embedded
// key. It renders a report for a broken or unsigned export too, since the report's purpose is to state
// the verification result plainly; it returns an error only when the template cannot execute. The
// caller passes now so the render time is explicit and tests stay deterministic.
func Report(exp *Export, now time.Time) ([]byte, error) {
	view := buildReportView(exp, now)
	var buf bytes.Buffer
	if err := reportTemplate.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("audit report: render: %w", err)
	}
	return buf.Bytes(), nil
}

// buildReportView verifies the export and derives the report's verdict and summary statistics.
func buildReportView(exp *Export, now time.Time) reportView {
	signed, verr := VerifyExport(exp)
	// A signature error still means the hash chain itself checked out; only a chain or head failure
	// makes the export untrustworthy as a record.
	chainBroken := verr != nil && !errors.Is(verr, ErrBadSignature)

	v := reportView{
		ChainIntact: !chainBroken,
		Count:       exp.Count,
		HeadHash:    exp.HeadHash,
		HeadShort:   shortHash(exp.HeadHash),
		PublicKey:   exp.PublicKey,
		Algo:        exp.Algo,
		SignedAt:    exp.SignedAt,
		GeneratedAt: now.UTC().Format(time.RFC3339),
	}
	switch {
	case chainBroken:
		v.Status = "broken"
		v.StatusText = "Audit chain broken. This export does not verify and must not be trusted."
	case exp.Signature == "":
		v.Status = "unsigned"
		v.StatusText = "Chain intact. The export is unsigned, so integrity is proven but attribution is not."
	case signed:
		v.Status = "verified"
		v.StatusText = "Chain intact and signature verified against the embedded public key."
	default:
		v.Status = "signature-invalid"
		v.StatusText = "Chain intact, but the signature does not verify. Attribution cannot be trusted."
	}

	if n := len(exp.Entries); n > 0 {
		v.Period = fmtTime(exp.Entries[0].At) + " to " + fmtTime(exp.Entries[n-1].At)
	} else {
		v.Period = "no entries"
	}

	actors := map[string]int{}
	for _, e := range exp.Entries {
		actors[e.Actor]++
		if isMutation(e.Method) {
			v.Mutations++
		} else {
			v.Reads++
		}
		v.Entries = append(v.Entries, entryRow{
			Seq: e.Seq, At: fmtTime(e.At), Actor: e.Actor,
			Action: strings.TrimSpace(e.Method + " " + e.Path),
		})
	}
	for actor, count := range actors {
		v.Actors = append(v.Actors, actorStat{Actor: actor, Count: count})
	}
	sort.Slice(v.Actors, func(i, j int) bool {
		if v.Actors[i].Count != v.Actors[j].Count {
			return v.Actors[i].Count > v.Actors[j].Count
		}
		return v.Actors[i].Actor < v.Actors[j].Actor
	})
	v.ActorCount = len(v.Actors)
	return v
}

// isMutation reports whether an HTTP method changes state, so the report can separate changes from
// reads.
func isMutation(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	default:
		return false
	}
}

// fmtTime formats an entry time for the report in UTC.
func fmtTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// shortHash truncates a hex digest for display, matching the audit CLI's short form.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
