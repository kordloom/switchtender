package extplugin

import "errors"

// ErrLoad is returned when the plugins directory cannot be read.
var ErrLoad = errors.New("plugin load failed")

// ErrProtocol is returned when a plugin breaks the wire protocol, such as ending a tool stream
// without a result.
var ErrProtocol = errors.New("plugin protocol error")
