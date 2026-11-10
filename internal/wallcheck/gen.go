package wallcheck

import (
	"fmt"
	"strings"
)

// GenReport counts what Gen did with each paid fingerprint.
type GenReport struct {
	// Kept is the number of fingerprints written to the list, before duplicates collapse.
	Kept int
	// Dropped is the number of test or short units already in the public tree, left off the list.
	Dropped int
	// Pruned is the number of shingles that match public boilerplate, left off the list.
	Pruned int
}

// Gen fingerprints the paid tree and returns the list the public tree is checked against. A paid
// fingerprint the public tree already has is left off the list when it is a shingle, comes from a
// test file, or is a declaration under the present floor, since none of those can tell a copy from
// common code. Any other match means paid code is already public, and Gen fails naming every such
// paid file and the public file it matches.
func Gen(paidRoot, publicRoot string, cfg Config) (*List, GenReport, error) {
	paid, err := scan(paidRoot, nil, cfg)
	if err != nil {
		return nil, GenReport{}, err
	}
	if len(paid.unparsed) > 0 {
		return nil, GenReport{}, fmt.Errorf("%w: %s", ErrParse, strings.Join(paid.unparsed, ", "))
	}
	public, err := scan(publicRoot, cfg.Exclude, cfg)
	if err != nil {
		return nil, GenReport{}, err
	}
	seen := make(map[string]Fingerprint, len(public.fps))
	for _, fp := range public.fps {
		if _, ok := seen[fp.Hash]; !ok {
			seen[fp.Hash] = fp
		}
	}
	list := NewList()
	var rep GenReport
	var present []string
	for _, fp := range paid.fps {
		pub, hit := seen[fp.Hash]
		switch {
		case !hit:
			list.Add(fp)
			rep.Kept++
		case fp.Kind == KindShingle:
			rep.Pruned++
		case fp.Test || (fp.Kind != KindFile && fp.Tokens < cfg.PresentFloor):
			rep.Dropped++
		default:
			present = append(present, fmt.Sprintf("%s:%d %s matches %s:%d",
				fp.Path, fp.Line, label(fp), pub.Path, pub.Line))
		}
	}
	if len(present) > 0 {
		return nil, rep, fmt.Errorf("%w: %s", ErrPresent, strings.Join(present, ", "))
	}
	return list, rep, nil
}

// label describes a fingerprint's kind for messages, without any of its content.
func label(fp Fingerprint) string {
	if fp.Decl == "" {
		return string(fp.Kind)
	}
	return string(fp.Kind) + " " + fp.Decl
}
