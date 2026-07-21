// Command helloplugin is the extension binary the extplugin tests build and load. It provides
// every seam once, with observable behavior: the tool echoes its command and takes its exit code
// from the run's extra vars, and the notifier records each delivery to the file named by the
// EXTTEST_NOTIFY_FILE environment variable.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/kordloom/switchtender/sdk"
	"github.com/kordloom/switchtender/sdk/plugin"
)

// main serves the test extension.
func main() {
	plugin.Serve(&plugin.Extension{
		Tools: map[string]sdk.ToolRunner{
			"exttest-hello": sdk.ToolRunnerFunc(runHello),
		},
		Notifiers: map[string]sdk.Notifier{
			"exttest-notify": sdk.NotifierFunc(notify),
		},
		AIProviders: map[string]sdk.AIProviderFactory{
			"exttest-ai": newAI,
		},
		SecretSources: map[string]sdk.SecretResolver{
			"exttest-static": resolve,
		},
		DynamicSecretSources: map[string]sdk.SecretMinter{
			"exttest-dyn": mint,
		},
	})
}

// runHello echoes the run's command, notes a dry run, and exits with the "exit" extra var.
func runHello(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
	_, _ = fmt.Fprintf(out, "plugin says: %s\n", spec.Command)
	if spec.DryRun {
		_, _ = fmt.Fprintln(out, "dry run")
	}
	exit := 0
	if v, ok := spec.ExtraVars["exit"].(float64); ok {
		exit = int(v)
	}
	return sdk.ToolResult{ExitCode: exit}, nil
}

// notify records the delivered run's id and extra var count to the EXTTEST_NOTIFY_FILE path.
func notify(_ context.Context, r *sdk.Run) error {
	path := os.Getenv("EXTTEST_NOTIFY_FILE")
	if path == "" {
		return nil
	}
	return os.WriteFile(path, fmt.Appendf(nil, "%s|%d", r.ID, len(r.ExtraVars)), 0o600)
}

// newAI builds a provider whose reply proves the settings and prompt crossed the wire.
func newAI(model, _, _ string) (sdk.AIProvider, error) {
	return sdk.AIProviderFunc(func(_ context.Context, system, user string) (string, error) {
		return "ai:" + model + ":" + system + ":" + user, nil
	}), nil
}

// resolve returns a value derived from the config so the round trip is observable.
func resolve(_ context.Context, config string) (string, error) {
	return "static:" + config, nil
}

// mint returns a value derived from the config and a revocable lease.
func mint(_ context.Context, config string) (string, *sdk.SecretLease, error) {
	lease := sdk.NewSecretLease("exttest-dyn", func(context.Context) error { return nil })
	return "minted:" + config, lease, nil
}
