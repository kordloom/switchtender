package main

import (
	"os"
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
