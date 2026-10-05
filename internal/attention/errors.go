package attention

import "errors"

var (
	// ErrConfig is returned for a thresholds file that cannot be read or states something this
	// package cannot act on.
	ErrConfig = errors.New("attention thresholds")
	// ErrStore is returned when the store behind the dashboard cannot be read.
	ErrStore = errors.New("attention store")
)
