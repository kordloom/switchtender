// Command modeplugin is the extension binary the loader's refusal tests build and load. The seams it
// declares are chosen by SWITCHTENDER_PLUGIN_MODE, so one binary can present each Describe response
// the host must refuse: a name that collides with a built-in, a blank name, and a secret kind
// declared twice. Every mode also declares a notifier under a name nothing else claims, so a test can
// prove the plugin was skipped whole rather than partly wired.
//
// It writes its process id to SWITCHTENDER_PLUGIN_PIDFILE before serving, so a test can prove a
// refused plugin's process is killed rather than left running against the host's environment.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/kordloom/switchtender/sdk"
	"github.com/kordloom/switchtender/sdk/plugin"
)

// main records the process id and serves the extension the mode asks for.
func main() {
	if path := os.Getenv("SWITCHTENDER_PLUGIN_PIDFILE"); path != "" {
		_ = os.WriteFile(path, fmt.Appendf(nil, "%d", os.Getpid()), 0o600)
	}
	plugin.Serve(extension(os.Getenv("SWITCHTENDER_PLUGIN_MODE")))
}

// run is the tool body every declared tool shares. No test reaches it, since every mode is refused.
func run(_ context.Context, _ sdk.ToolSpec, _ io.Writer) (sdk.ToolResult, error) {
	return sdk.ToolResult{}, nil
}

// notify is the notifier body every mode declares. No test reaches it either.
func notify(context.Context, *sdk.Run) error { return nil }

// extension builds the extension for one mode. An unknown mode declares only its notifier, which the
// host would accept, so a test that misspells a mode fails loudly rather than passing by accident.
func extension(mode string) *plugin.Extension {
	ext := &plugin.Extension{
		Notifiers: map[string]sdk.Notifier{
			"modeplugin-" + mode: sdk.NotifierFunc(notify),
		},
	}
	switch mode {
	case "collide-tool":
		// bash is a built-in execution tool, so registering it would panic the shared registry.
		ext.Tools = map[string]sdk.ToolRunner{"bash": sdk.ToolRunnerFunc(run)}
	case "blank-tool":
		// A name of only spaces passes the SDK's own empty check and must still be refused by the host.
		ext.Tools = map[string]sdk.ToolRunner{"   ": sdk.ToolRunnerFunc(run)}
	case "collide-ai":
		// The AI registry keys on the lowercased name, so this collides with the built-in openai.
		ext.AIProviders = map[string]sdk.AIProviderFactory{
			"OpenAI": func(string, string, string) (sdk.AIProvider, error) { return nil, nil },
		}
	case "dup-secret":
		// Resolvers and minters share one namespace, so declaring a kind in both is a collision.
		ext.SecretSources = map[string]sdk.SecretResolver{
			"modeplugin-kind": func(context.Context, string) (string, error) { return "", nil },
		}
		ext.DynamicSecretSources = map[string]sdk.SecretMinter{
			"modeplugin-kind": func(context.Context, string) (string, *sdk.SecretLease, error) {
				return "", nil, nil
			},
		}
	}
	return ext
}
