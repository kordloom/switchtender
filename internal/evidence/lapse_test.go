package evidence

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// TestALapsedRegisterStopsWritingPacks covers the half of the license rule a startup gate cannot
// enforce.
//
// The period change register is a paid feature, and serve refuses to start one for an install that
// never bought it. That gate runs once, and the emitter is a goroutine that outlives it, so a term
// that lapsed while the process ran kept writing paid artifacts every cadence for as long as the
// process lived. The startup path even logs, on a lapse, that no further packs are written, so the
// install was contradicting its own stated behavior.
//
// The clock is read on every call everywhere else a license is consulted, and it is read on every
// tick here for the same reason. Packs already written stay on disk and keep verifying offline,
// because a lapse takes nothing that was already produced.
func TestALapsedRegisterStopsWritingPacks(t *testing.T) {
	runs, audits, base := seedPeriod(t)
	dir := t.TempDir()

	// The emitter's own loop reads the clock from its goroutine while this test drives emitDue from
	// another, so the clock is guarded rather than raced.
	var mu sync.Mutex
	clock := base
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	set := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = base.Add(d)
	}

	packs := func() int {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir() error = %v", err)
		}
		n := 0
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".tmp") {
				n++
			}
		}
		return n
	}

	e := NewEmitter(runs, audits, "", dir, time.Hour, nil, WithClock(now))
	defer e.Close()
	if err := e.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Licensed: a cadence passes and a pack lands, which is the behavior being paid for.
	set(90 * time.Minute)
	e.emitDue()
	before := packs()
	if before < 1 {
		t.Fatalf("a licensed register wrote %d packs after a full cadence, want at least 1", before)
	}

	// The term runs out while the process keeps running. license.Set is process-wide, so this test
	// does not run in parallel with the rest of the package.
	held := license.Current()
	license.Set(&license.License{Claims: license.Claims{
		V: 1, ID: "lic_lapsed", Org: "Example", Tier: license.TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2026-02-01T00:00:00Z",
	}})
	t.Cleanup(func() { license.Set(held) })

	set(10 * time.Hour)
	e.emitDue()
	if after := packs(); after != before {
		t.Errorf("the register wrote %d packs after its license lapsed, was %d: a paid artifact "+
			"kept being produced for an install whose term had run out, which is the opposite of "+
			"what the startup path reports on a lapse", after, before)
	}

	// What was already written is untouched: a lapse takes nothing that was produced.
	if n := packs(); n < before {
		t.Errorf("packs already written were removed on a lapse: %d, want at least %d", n, before)
	}
}
