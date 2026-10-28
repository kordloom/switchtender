package paid

import (
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/license"
)

// ErrRequiresTier marks a refusal from a Community stub: the paid code that would do this is not
// in the binary. The wrapped text is the same line license.Allow prints for an unlicensed install,
// so a Community build and an unlicensed paid build refuse in one voice.
var ErrRequiresTier = errors.New("requires a paid tier")

// Requires returns the refusal a Community build gives for f, wrapping ErrRequiresTier around the
// unlicensed line license.Allow would print.
func Requires(f license.Feature) error {
	return fmt.Errorf("%w: %w", ErrRequiresTier, license.Unlicensed(f))
}
