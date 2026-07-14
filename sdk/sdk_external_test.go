// Package sdk_test authors a plugin against the public SDK surface. The registration half of each
// test uses only sdk exports, so this file compiling proves the SDK is sufficient to write a
// plugin; the observation half reaches into the internal packages a real plugin never imports.
package sdk_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/dcadolph/railwarden/internal/ai"
	"github.com/dcadolph/railwarden/internal/roundhouse"
	"github.com/dcadolph/railwarden/internal/run"
	"github.com/dcadolph/railwarden/internal/secretsource"
	"github.com/dcadolph/railwarden/sdk"
)

// TestRegisterAIProvider registers a model backend through the SDK and confirms the core builds a
// provider from it, with the model, URL, and key settings passed through to the factory. It does
// not call t.Parallel: it writes the provider registry.
func TestRegisterAIProvider(t *testing.T) {
	var gotModel, gotURL, gotKey string
	factory := sdk.AIProviderFactory(func(model, url, apiKey string) (sdk.AIProvider, error) {
		gotModel, gotURL, gotKey = model, url, apiKey
		return sdk.AIProviderFunc(func(_ context.Context, system, user string) (string, error) {
			return "sdkext: " + system + " " + user, nil
		}), nil
	})
	sdk.RegisterAIProvider("sdkext-ai", factory)

	if names := sdk.AIProviderNames(); !slices.Contains(names, "sdkext-ai") {
		t.Errorf("AIProviderNames() = %v, want it to include sdkext-ai", names)
	}

	provider, err := ai.New("sdkext-ai", "test-model", "http://localhost:0", "test-key")
	if err != nil {
		t.Fatalf("ai.New(sdkext-ai) error: %v", err)
	}
	if gotModel != "test-model" || gotURL != "http://localhost:0" || gotKey != "test-key" {
		t.Errorf("factory got (%q, %q, %q), want the settings passed through", gotModel, gotURL, gotKey)
	}
	reply, err := provider.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if want := "sdkext: sys user"; reply != want {
		t.Errorf("Complete = %q, want %q", reply, want)
	}
}

// TestRegisterTool registers an execution tool through the SDK and confirms the run layer accepts
// its name and the tool router dispatches a spec naming it to the registered runner. It does not
// call t.Parallel: it writes the tool registries.
func TestRegisterTool(t *testing.T) {
	sdk.RegisterTool("sdkext-echo", sdk.ToolRunnerFunc(
		func(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
			_, _ = fmt.Fprintf(out, "sdkext ran: %s\n", spec.Command)
			return sdk.ToolResult{ExitCode: 7}, nil
		}))

	if !run.ValidTool("sdkext-echo") {
		t.Error("ValidTool(sdkext-echo) = false, want the registered tool accepted")
	}

	var out bytes.Buffer
	res, err := roundhouse.NewAnsibleRunner().Run(
		context.Background(), roundhouse.Spec{Tool: "sdkext-echo", Command: "ping"}, &out)
	if err != nil {
		t.Fatalf("router Run error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7 from the registered runner", res.ExitCode)
	}
	if !strings.Contains(out.String(), "sdkext ran: ping") {
		t.Errorf("output = %q, want the registered runner's output", out.String())
	}
}

// TestRegisterNotifier registers a notification channel through the SDK and confirms the registry
// recorded it: a second registration under the same name panics. Delivery of terminal runs to a
// registered channel is covered by the dispatch package's own tests. It does not call t.Parallel:
// it writes the notifier registry.
func TestRegisterNotifier(t *testing.T) {
	notifier := sdk.NotifierFunc(func(context.Context, *sdk.Run) error { return nil })
	sdk.RegisterNotifier("sdkext-notify", notifier)

	defer func() {
		if recover() == nil {
			t.Error("duplicate RegisterNotifier did not panic, want the first registration recorded")
		}
	}()
	sdk.RegisterNotifier("sdkext-notify", notifier)
}

// TestRegisterSecretSource registers a secrets engine through the SDK and confirms the core
// resolves a value through it. It does not call t.Parallel: it writes the resolver registry.
func TestRegisterSecretSource(t *testing.T) {
	resolver := sdk.SecretResolver(func(_ context.Context, config string) (string, error) {
		return "static:" + config, nil
	})
	sdk.RegisterSecretSource("sdkext-static", resolver)

	got, err := secretsource.Resolve(context.Background(), "sdkext-static", "cfg")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if want := "static:cfg"; got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

// TestRegisterDynamicSecretSource registers a dynamic secrets engine through the SDK and confirms
// the core mints a value through it and the returned lease names the engine and revokes the
// secret. It does not call t.Parallel: it writes the dynamic engine registry.
func TestRegisterDynamicSecretSource(t *testing.T) {
	revoked := false
	minter := sdk.SecretMinter(func(_ context.Context, config string) (string, *sdk.SecretLease, error) {
		lease := sdk.NewSecretLease("sdkext-dyn", func(context.Context) error {
			revoked = true
			return nil
		})
		return "minted:" + config, lease, nil
	})
	sdk.RegisterDynamicSecretSource("sdkext-dyn", minter)

	value, lease, err := secretsource.ResolveLeased(context.Background(), "sdkext-dyn", "role")
	if err != nil {
		t.Fatalf("ResolveLeased error: %v", err)
	}
	if want := "minted:role"; value != want {
		t.Errorf("value = %q, want %q", value, want)
	}
	if lease.Kind() != "sdkext-dyn" {
		t.Errorf("lease kind = %q, want sdkext-dyn", lease.Kind())
	}
	if err := lease.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke error: %v", err)
	}
	if !revoked {
		t.Error("Revoke did not call the engine's revoke func")
	}
}
