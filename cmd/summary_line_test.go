package cmd

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/backup"
)

// TestTheSummaryLineCarriesEveryCountTheSummaryDoes holds the operator-facing report to the
// Summary struct, by reflection, so the two cannot drift apart again.
//
// The report is a hand-written line and the Summary is a struct that grows with the product. They
// drifted: policies, credential types, and memberships were counted, restored, and never reported,
// so a restore that wrote them said nothing about them and its report undercounted what it did.
// Each count field gets a distinct sentinel here, and each sentinel must appear in the line; a
// field added to Summary without a place in the line fails by name at the moment it is added.
func TestTheSummaryLineCarriesEveryCountTheSummaryDoes(t *testing.T) {
	t.Parallel()
	var sum backup.Summary
	v := reflect.ValueOf(&sum).Elem()
	typ := v.Type()

	// Distinct, unmistakable sentinels: no two fields share one, and none collides with a count a
	// zero value could produce.
	sentinels := map[string]int64{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Int {
			continue
		}
		sentinel := int64(90001 + i)
		v.Field(i).SetInt(sentinel)
		sentinels[f.Name] = sentinel
	}
	if len(sentinels) < 15 {
		t.Fatalf("only %d int fields found on backup.Summary; the reflection walk is broken",
			len(sentinels))
	}

	line := summaryLine(sum)
	for name, sentinel := range sentinels {
		if !strings.Contains(line, fmt.Sprintf("%d", sentinel)) {
			t.Errorf("Summary.%s is not in the report line: an object kind that restores with no "+
				"word said. Line: %s", name, line)
		}
	}
}
