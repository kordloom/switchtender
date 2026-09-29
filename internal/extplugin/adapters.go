package extplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
)

// seam couples a plugin's gRPC stub to a report on whether the process behind it is still running.
type seam struct {
	// client speaks to the plugin process.
	client extproto.ExtensionClient
	// exited reports whether the plugin process has stopped.
	exited func() bool
}

// fail wraps an error from a call through this seam, naming the extension that broke.
//
// Registration is one way: a plugin that dies keeps every name it registered, because the SDK
// registries have no unregister and the server holds the entry until it restarts. Every later call
// then fails deep in the transport, with a message like "the client connection is closing" that
// names neither the plugin nor the reason. An operator reading that in a run log cannot tell a dead
// plugin from a network fault or a bug in their playbook, so say which it is.
func (s seam) fail(kind, name string, err error) error {
	if s.exited != nil && s.exited() {
		return fmt.Errorf("%w: %s %q is still registered but its process is gone, so it stays broken "+
			"until the server restarts: %w", ErrPluginGone, kind, name, err)
	}
	return fmt.Errorf("plugin %s %s: %w", kind, name, err)
}

// toolRunner proxies one registered tool to its plugin process, streaming output back into the
// run's log as it arrives.
type toolRunner struct {
	// seam reaches the plugin process this tool lives in.
	seam
	// tool is the declared tool name this runner serves.
	tool string
}

// Run executes the spec on the plugin and relays its output stream, returning the exit code the
// plugin ends the stream with.
func (t *toolRunner) Run(ctx context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
	var extraVars []byte
	if len(spec.ExtraVars) > 0 {
		var err error
		if extraVars, err = json.Marshal(spec.ExtraVars); err != nil {
			return sdk.ToolResult{ExitCode: -1}, fmt.Errorf("encode extra vars: %w", err)
		}
	}
	stream, err := t.client.RunTool(ctx, &extproto.RunToolRequest{
		Tool:          t.tool,
		Command:       spec.Command,
		DryRun:        spec.DryRun,
		ExtraVarsJson: extraVars,
		Env:           spec.Env,
		Dir:           spec.Dir,
	})
	if err != nil {
		return sdk.ToolResult{ExitCode: -1}, t.fail("tool", t.tool, err)
	}
	for {
		reply, err := stream.Recv()
		if err == io.EOF {
			return sdk.ToolResult{ExitCode: -1},
				fmt.Errorf("%w: tool %s stream ended without a result", ErrProtocol, t.tool)
		}
		if err != nil {
			return sdk.ToolResult{ExitCode: -1}, t.fail("tool", t.tool, err)
		}
		switch r := reply.GetReply().(type) {
		case *extproto.RunToolReply_Output:
			if _, err := out.Write(r.Output); err != nil {
				return sdk.ToolResult{ExitCode: -1}, fmt.Errorf("write tool output: %w", err)
			}
		case *extproto.RunToolReply_Result:
			return sdk.ToolResult{ExitCode: int(r.Result.GetExitCode())}, nil
		}
	}
}

// notifier proxies one registered notification channel to its plugin process.
type notifier struct {
	// seam reaches the plugin process this notifier lives in.
	seam
	// channel is the declared notifier name this notifier serves.
	channel string
}

// Notify delivers the redacted terminal run to the plugin as its v1 API JSON shape.
func (n *notifier) Notify(ctx context.Context, r *sdk.Run) error {
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode run: %w", err)
	}
	if _, err := n.client.Notify(ctx, &extproto.NotifyRequest{
		Channel: n.channel, RunJson: body,
	}); err != nil {
		return n.fail("notifier", n.channel, err)
	}
	return nil
}

// aiFactory returns the provider factory for one declared AI provider. The provider it builds
// carries the host's settings on every completion call, so the plugin holds no configuration.
func aiFactory(s seam, name string) sdk.AIProviderFactory {
	return func(model, url, apiKey string) (sdk.AIProvider, error) {
		return sdk.AIProviderFunc(func(ctx context.Context, system, user string) (string, error) {
			resp, err := s.client.Complete(ctx, &extproto.CompleteRequest{
				Provider: name, Model: model, Url: url, ApiKey: apiKey,
				System: system, User: user,
			})
			if err != nil {
				return "", s.fail("ai provider", name, err)
			}
			return resp.GetText(), nil
		}), nil
	}
}

// secretResolver returns the resolver for one declared static secret source.
func secretResolver(s seam, kind string) sdk.SecretResolver {
	return func(ctx context.Context, config string) (string, error) {
		resp, err := s.client.ResolveSecret(ctx, &extproto.ResolveSecretRequest{
			Kind: kind, Config: config,
		})
		if err != nil {
			return "", s.fail("secret source", kind, err)
		}
		return resp.GetValue(), nil
	}
}

// secretMinter returns the minter for one declared dynamic secret source. The lease it returns
// revokes the minted secret through the plugin by the lease id the plugin assigned.
func secretMinter(s seam, kind string) sdk.SecretMinter {
	return func(ctx context.Context, config string) (string, *sdk.SecretLease, error) {
		resp, err := s.client.MintSecret(ctx, &extproto.MintSecretRequest{
			Kind: kind, Config: config,
		})
		if err != nil {
			return "", nil, s.fail("dynamic secret source", kind, err)
		}
		leaseID := resp.GetLeaseId()
		if leaseID == "" {
			return resp.GetValue(), sdk.NewSecretLease(kind, nil), nil
		}
		revoke := func(ctx context.Context) error {
			if _, err := s.client.RevokeLease(ctx, &extproto.RevokeLeaseRequest{LeaseId: leaseID}); err != nil {
				return s.fail("dynamic secret source lease revoke", kind, err)
			}
			return nil
		}
		return resp.GetValue(), sdk.NewSecretLease(kind, revoke), nil
	}
}
