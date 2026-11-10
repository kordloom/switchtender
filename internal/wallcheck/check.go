package wallcheck

import (
	"cmp"
	"fmt"
	"slices"
)

// Finding is one place in the public tree that matches the paid list. It never carries content.
type Finding struct {
	// Path is the public file's slash path relative to the checked root.
	Path string
	// Line is the first matching line.
	Line int
	// Label is the matched kind, with the declaration kind for units.
	Label string
	// Shingles is the number of distinct shingle matches in the file, or zero for other kinds.
	Shingles int
}

// String formats the finding as a path, line, and kind.
func (f Finding) String() string {
	if f.Shingles > 0 {
		return fmt.Sprintf("%s:%d: matches paid code (%d shingles)", f.Path, f.Line, f.Shingles)
	}
	return fmt.Sprintf("%s:%d: matches paid code (%s)", f.Path, f.Line, f.Label)
}

// CheckResult is what Check found.
type CheckResult struct {
	// Findings are the matches, sorted by path and line.
	Findings []Finding
	// Unparsed lists Go files that failed to parse and were checked by file and shingle hashes only.
	Unparsed []string
}

// Check scans the public tree at root and reports every declaration or file whose hash is on the
// list, and every file with at least the configured number of distinct shingle matches. A
// declaration that matches exactly is reported once, not again for its renamed hash.
func Check(root string, list *List, cfg Config) (CheckResult, error) {
	res, err := scan(root, cfg.Exclude, cfg)
	if err != nil {
		return CheckResult{}, err
	}
	var findings []Finding
	reported := make(map[string]bool)
	shingles := make(map[string]*shingleHits)
	for _, fp := range res.fps {
		if !list.Has(fp) {
			continue
		}
		if fp.Kind == KindShingle {
			s, ok := shingles[fp.Path]
			if !ok {
				s = &shingleHits{hashes: make(map[string]bool), first: fp.Line}
				shingles[fp.Path] = s
			}
			s.hashes[fp.Hash] = true
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", fp.Path, fp.Line, fp.Decl)
		if reported[key] {
			continue
		}
		reported[key] = true
		findings = append(findings, Finding{Path: fp.Path, Line: fp.Line, Label: label(fp)})
	}
	for path, s := range shingles {
		if len(s.hashes) >= cfg.ShingleHits {
			findings = append(findings, Finding{
				Path: path, Line: s.first, Label: string(KindShingle), Shingles: len(s.hashes),
			})
		}
	}
	slices.SortFunc(findings, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Label, b.Label))
	})
	return CheckResult{Findings: findings, Unparsed: res.unparsed}, nil
}

// shingleHits collects the shingle matches in one public file.
type shingleHits struct {
	// hashes is the set of distinct matched shingle hashes.
	hashes map[string]bool
	// first is the line of the earliest match.
	first int
}
