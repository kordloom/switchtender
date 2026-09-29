package extplugin

import "errors"

// ErrLoad is returned when the plugins directory cannot be read.
var ErrLoad = errors.New("plugin load failed")

// ErrProtocol is returned when a plugin breaks the wire protocol, such as ending a tool stream
// without a result.
var ErrProtocol = errors.New("plugin protocol error")

// ErrPluginGone is returned when a seam is called after its plugin process has stopped. Nothing
// unregisters a name, so a plugin that dies stays registered and every later call through it fails.
var ErrPluginGone = errors.New("plugin process has exited")

// ErrPluginContract is returned when a plugin's Describe response is not registrable: an empty name,
// a name that collides with a built-in or an already-loaded plugin, or a duplicate within the
// response. The plugin is skipped whole rather than partly registered.
var ErrPluginContract = errors.New("plugin describe contract violation")
