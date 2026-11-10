package wallcheck

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// skipDirs are directory names no scan descends into.
var skipDirs = []string{".git", "node_modules"}

// skipFiles are dependency manifests. They are generated, not written, and the paid build's copies
// mirror the public ones, so hashing them would only flag dependency upgrades.
var skipFiles = []string{"go.mod", "go.sum"}

// binarySniff is how many leading bytes are searched for a NUL to call a file binary.
const binarySniff = 8000

// scanResult is everything one tree scan produced.
type scanResult struct {
	// fps are the fingerprints in walk order.
	fps []Fingerprint
	// unparsed lists Go files that failed to parse, so only their file and shingle hashes exist.
	unparsed []string
}

// scan fingerprints every regular file under root, skipping the slash paths in exclude.
func scan(root string, exclude []string, cfg Config) (scanResult, error) {
	var res scanResult
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (slices.Contains(skipDirs, d.Name()) || excluded(rel, exclude)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || slices.Contains(skipFiles, d.Name()) || excluded(rel, exclude) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fps, parsed := scanFile(rel, src, cfg)
		res.fps = append(res.fps, fps...)
		if !parsed {
			res.unparsed = append(res.unparsed, rel)
		}
		return nil
	})
	if err != nil {
		return scanResult{}, fmt.Errorf("%w: %s: %w", ErrWalk, root, err)
	}
	return res, nil
}

// excluded reports whether rel is one of the exclude paths or sits under one.
func excluded(rel string, exclude []string) bool {
	for _, ex := range exclude {
		if rel == ex || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}

// isTest reports whether a file name is a Go or JavaScript test file.
func isTest(name string) bool {
	return strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.")
}

// scanFile fingerprints one file. It reports false when the file is Go source that does not parse.
func scanFile(rel string, src []byte, cfg Config) ([]Fingerprint, bool) {
	test := isTest(filepath.Base(rel))
	if bytes.IndexByte(src[:min(len(src), binarySniff)], 0) >= 0 {
		fp := Fingerprint{Hash: hash(KindFile, src), Kind: KindFile, Path: rel, Line: 1, Test: test}
		return []Fingerprint{fp}, true
	}
	var fps []Fingerprint
	lines := significant(src)
	if len(lines) >= cfg.FileFloor {
		fps = append(fps, Fingerprint{
			Hash: hash(KindFile, joinLines(lines)), Kind: KindFile,
			Path: rel, Line: lines[0].num, Test: test,
		})
	}
	for i := 0; i+cfg.ShingleLines <= len(lines); i++ {
		fps = append(fps, Fingerprint{
			Hash: hash(KindShingle, joinLines(lines[i:i+cfg.ShingleLines])),
			Kind: KindShingle, Path: rel, Line: lines[i].num, Test: test,
		})
	}
	if !strings.HasSuffix(rel, ".go") {
		return fps, true
	}
	units, ok := goUnits(rel, src, test, cfg)
	return append(fps, units...), ok
}

// span is the source range of one declaration unit.
type span struct {
	// start and end bound the hashed tokens.
	start, end token.Pos
	// line is where the declaration begins.
	line token.Pos
	// decl is func, type, or value.
	decl string
}

// goUnits fingerprints every function, method, type, and const or var block in a Go file. A
// function is hashed from its type parameters or parameters through its body, leaving out its name
// and receiver, so a paid method pasted in as a renamed function still matches. A type is hashed
// without its name.
func goUnits(rel string, src []byte, test bool, cfg Config) ([]Fingerprint, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, false
	}
	var spans []span
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			start := d.Type.Params.Pos()
			if d.Type.TypeParams != nil {
				start = d.Type.TypeParams.Pos()
			}
			spans = append(spans, span{start: start, end: d.Body.End(), line: d.Pos(), decl: "func"})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				for _, s := range d.Specs {
					ts := s.(*ast.TypeSpec)
					spans = append(spans, span{start: ts.Name.End(), end: ts.End(), line: ts.Pos(), decl: "type"})
				}
			case token.CONST, token.VAR:
				spans = append(spans, span{start: d.Pos(), end: d.End(), line: d.Pos(), decl: "value"})
			}
		}
	}
	tf := fset.File(f.Pos())
	var fps []Fingerprint
	for _, s := range spans {
		toks := lex(src[tf.Offset(s.start):tf.Offset(s.end)])
		at := Fingerprint{
			Path: rel, Line: fset.Position(s.line).Line, Decl: s.decl, Tokens: len(toks), Test: test,
		}
		if len(toks) >= cfg.ExactFloor {
			at.Hash, at.Kind = hash(KindExact, exactBody(toks)), KindExact
			fps = append(fps, at)
		}
		if len(toks) >= cfg.AlphaFloor {
			at.Hash, at.Kind = hash(KindAlpha, alphaBody(toks)), KindAlpha
			fps = append(fps, at)
		}
	}
	return fps, true
}
