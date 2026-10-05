package ui

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/kordloom/switchtender/internal/schedule"
)

// skipBadgeLine finds the schedules list's copy of the skip badge threshold.
var skipBadgeLine = regexp.MustCompile(`(?m)^const SKIP_BADGE_FIRES = (\d+);$`)

// TestSkipBadgeThresholdMatchesTheServer pins the schedules list's badge to the server's threshold.
// The doctor warns at schedule.SkipBadgeFires and the list draws its badge at the number written in
// the script, so the two must be the same number or the list and the doctor disagree about which
// schedules have stopped reaching hosts.
func TestSkipBadgeThresholdMatchesTheServer(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("assets/js/18-host-page.js")
	if err != nil {
		t.Fatalf("read the schedules script: %v", err)
	}
	m := skipBadgeLine.FindSubmatch(src)
	if m == nil {
		t.Fatal("the schedules script no longer declares SKIP_BADGE_FIRES, so nothing ties its " +
			"badge to the server's threshold")
	}
	got, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("SKIP_BADGE_FIRES is not a number: %v", err)
	}
	if got != schedule.SkipBadgeFires {
		t.Errorf("SKIP_BADGE_FIRES = %d in the script, schedule.SkipBadgeFires = %d on the server",
			got, schedule.SkipBadgeFires)
	}
}
