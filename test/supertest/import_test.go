package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheAWXFixtureCarriesWhatTheCheckLooksFor keeps the expectations and the export together.
//
// The check looks for objects by the names the export gave them, and those names are written down
// in one file while the export lives in another. Nothing connected them, which is the shape that
// made a release gate demand seven engines while installing one. An export edited to drop an object
// would leave the check looking for something that was never in the file, and it would fail against
// an install that had migrated the export perfectly.
func TestTheAWXFixtureCarriesWhatTheCheckLooksFor(t *testing.T) {
	t.Parallel()
	raw := mustManifest("awx-export.json")
	var export map[string]any
	if err := json.Unmarshal([]byte(raw), &export); err != nil {
		t.Fatalf("the embedded AWX export does not parse: %v", err)
	}
	for _, want := range awxObjects {
		t.Run(want.Member+"/"+want.Name, func(t *testing.T) {
			t.Parallel()
			// The export spells its own sections differently from the API's listings, so the name
			// is looked for in the document rather than in a section this test would have to keep
			// mapped. A name absent from the whole file cannot be the thing the import produces.
			if !strings.Contains(raw, `"`+want.Name+`"`) {
				t.Errorf("the check looks for %s %q and the export never names it, so the check "+
					"would fail against an install that migrated this export correctly",
					want.Member, want.Name)
			}
		})
	}
}
