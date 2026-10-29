package paid

import "github.com/kordloom/switchtender/internal/license"

// Requires returns the refusal a Community stub gives for f: license.ErrRequiresTier wrapped around
// the unlicensed line, the same refusal license.Allow gives for a feature the build does not carry.
func Requires(f license.Feature) error {
	return license.Absent(f)
}
