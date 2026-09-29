package cmd

import "runtime/debug"

// Version is the SwitchTender build version, overridden via ldflags on a release build.
var Version = "0.0.0-dev"

// BuildChannel names how this binary was built when that is not a release archive, set via ldflags.
// The container image sets it to container.
var BuildChannel = ""

// buildChannelContainer is the BuildChannel of a binary compiled inside the container image.
const buildChannelContainer = "container"

// resolveVersion returns the ldflags version when a release build set one, otherwise the module
// version go install embeds, so a `go install ...@latest` build reports its real version rather
// than the development placeholder.
func resolveVersion() string {
	if Version != "0.0.0-dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return Version
}
