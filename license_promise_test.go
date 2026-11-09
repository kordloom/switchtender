package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fixedChangeDate matches a Change Date written as a calendar date rather than as a span measured
// from each version's release.
var fixedChangeDate = regexp.MustCompile(`Change Date:\s*\d{4}-\d{2}-\d{2}`)

// TestTheLicenseSaysWhatTheCommitmentSays holds the license text to the promise published beside it.
//
// The conversion to Apache 2.0 is one of seven commitments the terms call contractual, and its
// wording is deliberate: the escape hatch is in the license text rather than in anyone's goodwill.
// That sentence is only true while the text agrees with it.
//
// It stopped being true once and nothing noticed. The Change Date was a fixed calendar date, about
// four years out, and was changed to a two-year span measured from each version's release. The
// Business Source License applies separately to each version, so the new wording governs only the
// versions that carry it, and 95 already-published versions kept the fixed date while three pages
// went on saying every release converts after two years. A reader disproved it with one command on a
// public repository. The remedy was a supplemental grant, which is what LICENSE-GRANT.md is, but the
// reason it went unseen for 95 releases is that a claim in prose and a parameter in a file had
// nothing holding them together.
//
// This is that. A fixed date in LICENSE means the promise and the text have parted company again.
func TestTheLicenseSaysWhatTheCommitmentSays(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatalf("read LICENSE: %v", err)
	}
	text := string(raw)

	if loc := fixedChangeDate.FindString(text); loc != "" {
		t.Errorf("LICENSE carries a fixed Change Date (%q), but the terms, the pricing page and the "+
			"continuity doc all say every release converts two years after it ships, in the license "+
			"text rather than in our goodwill. Either restore the relative wording, or change every "+
			"page that makes the promise and grant the versions already published under it.", loc)
	}
	if !strings.Contains(text, "Two years after the date each version of the Licensed Work") {
		t.Error("LICENSE no longer states the two-year Change Date in the relative form the " +
			"published commitment describes")
	}
	if !strings.Contains(text, "Apache License, Version 2.0") {
		t.Error("LICENSE no longer names Apache 2.0 as the Change License, which every page " +
			"describing the conversion names")
	}
}

// TestTheSupplementalGrantCoversTheVersionsThatShippedWithout it holds the grant in place.
//
// The grant is what makes the published commitment true for the versions released before the license
// text carried it. Deleting it, or narrowing the range it names, silently returns those versions to
// a 2030 conversion while the pages keep promising two years.
func TestTheSupplementalGrantCoversTheVersionsThatShippedWithoutIt(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("LICENSE-GRANT.md")
	if err != nil {
		t.Fatalf("read LICENSE-GRANT.md, which is what makes the two-year commitment true for the "+
			"versions published before LICENSE said it: %v", err)
	}
	text := string(raw)

	for _, want := range []string{
		"v1.72.0",     // the boundary the grant is written around
		"irrevocable", // a revocable grant is worth little to somebody relying on it
		"Apache License, Version 2.0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the supplemental grant no longer says %q, so it may no longer do what the "+
				"published commitment depends on it doing", want)
		}
	}
}

// TestLicensingPromiseSaysWherePaidCodeLives holds LICENSING.md to the open-core mechanism. The
// Community build is the public repository, the paid code sits in a private module, and a page that
// goes back to saying paid features ship in the public binary promises a lock that is not there.
func TestLicensingPromiseSaysWherePaidCodeLives(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("LICENSING.md")
	if err != nil {
		t.Fatalf("read LICENSING.md: %v", err)
	}
	text := whitespaceRun.ReplaceAllString(string(raw), " ")

	for _, want := range []string{
		"The Community build is this repository.",
		"Paid features live in a private module, and the paid build that carries them is a separate download",
		"mounted at internal/paid/impl as a git submodule that public clones do not fetch",
		"A Community build refuses each paid feature by naming the tier it needs.",
		"The paid engine is not there to patch a key out of.",
		"Team is what lets you write the rule.",
		"PostgreSQL, including active-active high availability on it, is Community.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("LICENSING.md no longer says %q", want)
		}
	}
	if strings.Contains(strings.ToLower(text), "paid features ship in the same binary") {
		t.Error("LICENSING.md says paid features ship in the same binary, but the paid code is in a " +
			"private module and the paid build is a separate download")
	}
}

// conversionClaim matches a sentence that says every or each release or version converts.
var conversionClaim = regexp.MustCompile(`(?i)\b(every|each)\b.*\b(release|version)s?\b.*\bconvert`)

// blockClose matches the closing tags that end a sentence in HTML.
var blockClose = regexp.MustCompile(`(?i)</(p|li|h[1-6]|td|th|div|dd|dt|summary|blockquote)>`)

// anyTag matches any remaining HTML tag, which sits inside a sentence.
var anyTag = regexp.MustCompile(`<[^>]*>`)

// sentenceEnd matches a sentence terminator followed by space, or a block boundary marker.
var sentenceEnd = regexp.MustCompile(`[.!?]["']?\s|\x00`)

// sentences splits flattened page text into sentences. Closing block tags end a sentence, other
// tags become spaces, and a terminator inside a number such as 2.0 does not split.
func sentences(flat string) []string {
	flat = blockClose.ReplaceAllString(flat, "\x00")
	flat = anyTag.ReplaceAllString(flat, " ")
	flat = whitespaceRun.ReplaceAllString(flat, " ")
	return sentenceEnd.Split(flat, -1)
}

// unqualifiedConversions returns each sentence that claims every release converts without
// saying "public core".
func unqualifiedConversions(flat string) []string {
	var bad []string
	for _, s := range sentences(flat) {
		if conversionClaim.MatchString(s) && !strings.Contains(strings.ToLower(s), "public core") {
			bad = append(bad, strings.TrimSpace(s))
		}
	}
	return bad
}

// TestLicensingPromiseConversionMatcher tests the matcher against literal strings, without touching
// a page.
func TestLicensingPromiseConversionMatcher(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In        string
		WantCount int
	}{{ // Test 0: An unqualified every-release claim fails.
		In: "Every release converts to Apache 2.0.", WantCount: 1,
	}, { // Test 1: The public core qualifier passes.
		In: "Each release of the public core also converts to Apache 2.0.", WantCount: 0,
	}, { // Test 2: A published-release qualifier passes.
		In: "Note that every published release of the public core converts.", WantCount: 0,
	}, { // Test 3: A tag inside the sentence does not hide the match.
		In: "<p>Every <strong>version</strong> converts after two years.</p>", WantCount: 1,
	}, { // Test 4: A qualifier in a neighboring sentence does not count.
		In: "The public core is open. Every release converts.", WantCount: 1,
	}, { // Test 5: A wrapped sentence is still one sentence.
		In: "<p>Every\n   release\n converts to Apache 2.0.</p>", WantCount: 1,
	}, { // Test 6: A sentence with no conversion claim passes.
		In: "Every release is signed.", WantCount: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			flat := whitespaceRun.ReplaceAllString(test.In, " ")
			if got := len(unqualifiedConversions(flat)); got != test.WantCount {
				t.Errorf("got %d unqualified sentences, want %d", got, test.WantCount)
			}
		})
	}
}

// TestLicensingPromiseConversionClaimsNamePublicCore fails on any every-release conversion sentence
// that does not say "public core", because paid code in the private module does not convert.
func TestLicensingPromiseConversionClaimsNamePublicCore(t *testing.T) {
	t.Parallel()
	var files []string
	for _, pattern := range []string{"site/*.html", "docs/*.md"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	files = append(files, "README.md", "LICENSING.md")
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			for _, s := range unqualifiedConversions(readFlat(t, file)) {
				t.Errorf("%s: conversion claim without \"public core\": %q", file, s)
			}
		})
	}
}
