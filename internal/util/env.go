package util

import (
	"os"
	"strings"
)

// configEnvPrefix is the prefix every SwitchTender configuration variable carries.
const configEnvPrefix = "SWITCHTENDER_"

// RunEnviron returns the process environment with SwitchTender's own configuration removed, for
// handing to a child process that runs somebody's playbook, script, or plan.
//
// The server reads its master encryption key, its salt, its worker token, and every provider secret
// from the environment. Passing that environment through to a run handed all of it to whatever the
// run executes: an operator with permission to submit a one-line shell run could print the master
// key, and with the key and salt they could decrypt every stored credential and any backup. The run
// is the thing the key protects, so the key does not belong in it.
//
// Everything a run legitimately reads from us is set per run rather than inherited, so removing the
// whole prefix is complete rather than a list of names to remember. A secret added to the
// configuration later is stripped by having the prefix, which is the property a denylist lacks.
// Variables outside the prefix are left alone, because a playbook is often meant to inherit the
// cloud and proxy settings of the host it runs on.
func RunEnviron() []string {
	return FilterConfigEnv(os.Environ())
}

// FilterConfigEnv returns env without SwitchTender's own configuration variables.
func FilterConfigEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, configEnvPrefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
