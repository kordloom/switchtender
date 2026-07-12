package cmd

import "runtime/debug"

// Version is the Yardmaster build version, overridden via ldflags on a release build.
var Version = "0.0.0-dev"

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
