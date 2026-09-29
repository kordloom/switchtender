package cmd

import (
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// restoreLicense puts this process on Community for the duration of the test and puts back whatever
// it found afterward.
//
// The four tests that prove a Community refusal used to observe license.Current and skip when it
// was not nil. That reads as caution and behaves as a hole: the license is a process global, two
// other tests in this package set it, and a skip reports as a pass, so a refusal on the paid
// boundary could quietly stop being checked because of something another test did. Nothing enforced
// the ordering that makes it safe today.
//
// A test that needs Community can have Community. The caller must not be parallel, which none of
// them are, because the value being swapped is shared by the whole process.
func restoreLicense(t *testing.T) {
	t.Helper()
	previous := license.Current()
	t.Cleanup(func() { license.Set(previous) })
	license.Set(nil)
}

// TestRestoreLicenseForcesCommunityWhateverTheProcessCarries proves the helper against the
// condition the old skip stood down for.
//
// With a license installed, the four refusal tests used to skip and report a pass. The helper has
// to make Community true rather than wait for it, and has to put back what it found, or it would
// break whichever test set the license in the first place.
func TestRestoreLicenseForcesCommunityWhateverTheProcessCarries(t *testing.T) {
	installed := &license.License{Claims: license.Claims{Tier: "team"}}
	license.Set(installed)
	t.Cleanup(func() { license.Set(nil) })

	t.Run("inside", func(t *testing.T) {
		restoreLicense(t)
		if got := license.Current(); got != nil {
			t.Errorf("license.Current() = %v, want nil: the helper did not force Community, so a "+
				"refusal test would have stood down instead of checking the gate", got)
		}
	})

	if got := license.Current(); got != installed {
		t.Errorf("license.Current() = %v, want the license this test installed: the helper did not "+
			"put back what it found", got)
	}
}
