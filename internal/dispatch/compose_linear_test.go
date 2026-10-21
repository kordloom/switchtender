package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/inventory"
)

// endingContext is a context whose Err reports canceled from its after+1th call on, so a test can
// end it at an exact point inside a loop that looks at it.
type endingContext struct {
	context.Context
	// after is how many calls to Err report nil before it reports canceled.
	after int32
	// calls counts the calls to Err.
	calls atomic.Int32
}

// Err reports nil for the first after calls and context.Canceled from then on.
func (c *endingContext) Err() error {
	if c.calls.Add(1) > c.after {
		return context.Canceled
	}
	return nil
}

// listedInput returns a read composition input named id over a listing of n hosts, each a direct
// member of perHost of 50 groups.
func listedInput(t *testing.T, id string, n, perHost int) *composeInput {
	t.Helper()
	members := make([][]string, 50)
	for h := range n {
		for j := range perHost {
			g := (h + j*7) % 50
			members[g] = append(members[g], fmt.Sprintf(`"h%06d"`, h))
		}
	}
	var b strings.Builder
	b.WriteString(`{"_meta": {"hostvars": {}}`)
	for g, hosts := range members {
		fmt.Fprintf(&b, `, "g%d": {"hosts": [%s]}`, g, strings.Join(hosts, ","))
	}
	b.WriteString("}")
	l, err := inventory.ParseListing([]byte(b.String()))
	if err != nil {
		t.Fatalf("ParseListing() error = %v", err)
	}
	return &composeInput{inv: &inventory.Inventory{ID: id, Name: id}, listing: l,
		engine: inventory.EngineNative}
}

// TestFilterHostsStaysLinear guards smart composition against reading each host's groups by
// scanning every group, which made a smart inventory over two 100k-host inputs take 53 s. It does
// not assert a wall-clock time. One input of 100k hosts, each in five groups, is filtered under a
// 60 s deadline: a scan per host is 5e10 comparisons, about 100 s, and stops on the deadline, while
// the linear walk finishes in well under a second.
func TestFilterHostsStaysLinear(t *testing.T) {
	t.Parallel()
	in := listedInput(t, "inv_big", 100000, 5)
	filter, err := inventory.ParseHostFilter("groups__name=g3")
	if err != nil {
		t.Fatalf("ParseHostFilter() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hosts, _, drawn, err := filterHosts(ctx, filter, []*composeInput{in}, nil)
	if err != nil {
		t.Fatalf("filterHosts() error = %v, want it to finish inside the deadline", err)
	}
	if len(hosts) != 10000 || len(drawn) != 1 {
		t.Errorf("filterHosts() = %d hosts from %d inputs, want 10000 from 1", len(hosts), len(drawn))
	}
}

// TestFilterHostsStopsWhenTheContextEnds pins that the filter loop looks at its context while it
// runs, so the compose timeout and a dropped request stop it.
func TestFilterHostsStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	in := listedInput(t, "inv_a", 3000, 1)
	filter, err := inventory.ParseHostFilter("name__startswith=h")
	if err != nil {
		t.Fatalf("ParseHostFilter() error = %v", err)
	}
	tests := []struct {
		WantCount int
		Want      error
		After     int32
	}{{ // Test 0: Ended from the first look: nothing is returned.
		After: 0, Want: context.Canceled,
	}, { // Test 1: Ended partway through the hosts.
		After: 1, Want: context.Canceled,
	}, { // Test 2: Never ended: every host is selected.
		After: 1 << 30, WantCount: 3000,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := &endingContext{Context: context.Background(), after: test.After}
			hosts, _, _, err := filterHosts(ctx, filter, []*composeInput{in}, nil)
			if !errors.Is(err, test.Want) {
				t.Fatalf("filterHosts() error = %v, want %v", err, test.Want)
			}
			if len(hosts) != test.WantCount {
				t.Errorf("filterHosts() = %d hosts, want %d", len(hosts), test.WantCount)
			}
		})
	}
}

// TestReadInputsStopsWhenTheContextEnds pins that reading the inputs natively stops once the
// context has ended rather than reading every input for nobody.
func TestReadInputsStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()
	d, _, _, _ := composeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := &composeInput{inv: &inventory.Inventory{ID: "inv_x", Name: "x"}, content: "[web]\nweb1\n"}
	if _, err := d.readInputs(ctx, []*composeInput{in}); !errors.Is(err, context.Canceled) {
		t.Errorf("readInputs() error = %v, want context.Canceled", err)
	}
	if in.listing != nil {
		t.Errorf("readInputs() read the input after the context ended")
	}
}
