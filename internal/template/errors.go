package template

import "errors"

// ErrAWXConflict is returned when an AWX job template id is already bound to a different AWX
// object, so binding it again would hand a boot script's callback to a template it never meant.
var ErrAWXConflict = errors.New("awx job template id is bound to a different awx object")
