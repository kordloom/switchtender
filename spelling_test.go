package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// britishSpellings are the forms this project does not use, with the form it does. American spelling
// is the rule everywhere: code, comments, identifiers, error messages, documentation, and every page
// the site serves.
//
// Protocol and platform identifiers are not prose and are not listed. "notifications/cancelled" is a
// method name in the Model Context Protocol and "!cancelled()" is GitHub Actions syntax, so both are
// spelled the way the system that owns them spells them.
var britishSpellings = map[string]string{
	"licence":    "license",
	"defence":    "defense",
	"behaviour":  "behavior",
	"colour":     "color",
	"centre":     "center",
	"favour":     "favor",
	"honour":     "honor",
	"neighbour":  "neighbor",
	"analyse":    "analyze",
	"organise":   "organize",
	"recognise":  "recognize",
	"realise":    "realize",
	"apologise":  "apologize",
	"prioritise": "prioritize",
	"optimise":   "optimize",
	"initialise": "initialize",
	"normalise":  "normalize",
	"serialise":  "serialize",
	"travelling": "traveling",
	"modelled":   "modeled",
	"whilst":     "while",
	"amongst":    "among",
	"cancelled":  "canceled",
	"fulfil":     "fulfill",
	"judgement":  "judgment",
}

// spellingExtensions are the file types this walks. The linter's misspell check already covers Go
// source and is pinned to the US locale, so the gap this closes is everything else: the YAML a
// deployment reads, the compose file, the documentation, and the HTML the site serves. Three
// instances of one word had reached those, including a page a prospect reads, because nothing looked.
var spellingExtensions = map[string]bool{
	".md": true, ".html": true, ".yaml": true, ".yml": true, ".txt": true, ".json": true,
}

// spellingSkipDirs are paths whose contents this project does not author or cannot spell for.
var spellingSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "vendor": true,
	// Generated from docs/*.md by sitegen, so a finding here is a finding in the source it came from
	// and fixing it in both places would be fixing it in the wrong one.
	"docs": true,
}

// platformIdentifiers are the exact strings where a British spelling belongs because the system that
// owns the name spells it that way. A line containing one is exempt for that word alone.
var platformIdentifiers = []string{
	"notifications/cancelled",
	"cancelled()",
	"!cancelled",
	"steps.cancelled",
}

// TestEveryFileSpellsItTheAmericanWay holds the whole repository to one spelling, not just its Go.
//
// The linter enforces this over Go source and stops there. Everything else went unchecked, and it
// showed: the Helm values file, the compose file and a public page on the site all carried the
// British spelling of the one word this product says most often. A rule that only half a repository
// is held to is a preference.
func TestEveryFileSpellsItTheAmericanWay(t *testing.T) {
	t.Parallel()
	// Word boundaries on both sides, so "license" inside "licenses" is not matched by "licence" and a
	// longer word that happens to contain a listed form is not a finding.
	patterns := map[string]*regexp.Regexp{}
	for wrong, right := range britishSpellings {
		if right == "" {
			continue
		}
		patterns[wrong] = regexp.MustCompile(`(?i)\b` + wrong + `\b`)
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if spellingSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !spellingExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		for lineNum, line := range strings.Split(string(raw), "\n") {
			for wrong, pattern := range patterns {
				if !pattern.MatchString(line) {
					continue
				}
				if exemptLine(line) {
					continue
				}
				t.Errorf("%s:%d spells it %q; this project spells it %q everywhere, not only in Go:\n  %s",
					rel, lineNum+1, wrong, britishSpellings[wrong], strings.TrimSpace(line))
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
}

// exemptLine reports whether a line carries a platform identifier that owns its own spelling.
func exemptLine(line string) bool {
	for _, id := range platformIdentifiers {
		if strings.Contains(line, id) {
			return true
		}
	}
	return false
}
