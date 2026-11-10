package wallcheck

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strings"
)

// listHeader opens every written list. It is fixed text, so regenerating an unchanged tree
// reproduces the file byte for byte.
const listHeader = `# switchtender-wall-v1: fingerprints of the private paid module.
# One hash and kind per line. Public CI fails when a public file matches one.
# Regenerate with wallcheck gen, never by hand.
`

// List is a set of paid fingerprints keyed by hash.
type List struct {
	// entries maps each hash to its kind.
	entries map[string]Kind
}

// NewList returns an empty list.
func NewList() *List {
	return &List{entries: make(map[string]Kind)}
}

// Add puts a fingerprint's hash in the list.
func (l *List) Add(fp Fingerprint) {
	l.entries[fp.Hash] = fp.Kind
}

// Has reports whether the list holds the fingerprint's hash with the same kind.
func (l *List) Has(fp Fingerprint) bool {
	k, ok := l.entries[fp.Hash]
	return ok && k == fp.Kind
}

// Len returns the number of hashes in the list.
func (l *List) Len() int {
	return len(l.entries)
}

// WriteTo writes the header and the sorted entries to w.
func (l *List) WriteTo(w io.Writer) (int64, error) {
	lines := make([]string, 0, len(l.entries))
	for h, k := range l.entries {
		lines = append(lines, h+" "+string(k)+"\n")
	}
	slices.Sort(lines)
	n, err := io.WriteString(w, listHeader+strings.Join(lines, ""))
	return int64(n), err
}

// ReadList parses a list written by WriteTo. Blank lines and lines starting with # are ignored.
func ReadList(r io.Reader) (*List, error) {
	l := NewList()
	sc := bufio.NewScanner(r)
	for num := 1; sc.Scan(); num++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !isHash(fields[0]) || !isKind(Kind(fields[1])) {
			return nil, fmt.Errorf("%w: line %d: want a %d character hex hash and a kind",
				ErrList, num, hashLen)
		}
		l.entries[fields[0]] = Kind(fields[1])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: read: %w", ErrList, err)
	}
	return l, nil
}

// isHash reports whether s is a lowercase hex hash of the kept length.
func isHash(s string) bool {
	if len(s) != hashLen {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// isKind reports whether k is a known kind.
func isKind(k Kind) bool {
	return slices.Contains([]Kind{KindExact, KindAlpha, KindFile, KindShingle}, k)
}
