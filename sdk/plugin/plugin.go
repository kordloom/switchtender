// Package plugin turns an extension into a standalone binary SwitchTender loads at startup, no
// recompile of the server. A plugin author fills an Extension with the same interfaces the
// in-process SDK registers, then calls Serve from main. The server launches the binary from its
// plugins directory, asks what it provides, and registers every seam, so a plugged-in tool,
// notifier, AI provider, or secret engine behaves exactly like a compiled-in one.
//
//	func main() {
//		plugin.Serve(&plugin.Extension{
//			Tools: map[string]sdk.ToolRunner{
//				"hello": sdk.ToolRunnerFunc(runHello),
//			},
//		})
//	}
//
// The process speaks gRPC over a local socket with mutual TLS, supervised by the server. It
// exits when the server does.
package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
)

// Handshake pairs a plugin binary with a SwitchTender that speaks its protocol. The cookie is not a
// security measure, only a guard against launching a binary that is not a SwitchTender extension.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "SWITCHTENDER_EXTENSION",
	MagicCookieValue: "switchtender-extension-v1",
}

// Key names the one plugin every extension binary serves in the go-plugin set.
const Key = "extension"

// Extension is everything one plugin binary provides, keyed by the name each entry registers
// under. Every map may be empty, but at least one entry must exist across them.
type Extension struct {
	// Tools maps execution tool names to their runners.
	Tools map[string]sdk.ToolRunner
	// Notifiers maps notification channel names to their notifiers.
	Notifiers map[string]sdk.Notifier
	// AIProviders maps model backend names to their factories. The factory runs on each
	// completion with the host's configured model, URL, and API key, so a factory that wants to
	// reuse a client should memoize on those settings.
	AIProviders map[string]sdk.AIProviderFactory
	// SecretSources maps static secret engine kinds to their resolvers.
	SecretSources map[string]sdk.SecretResolver
	// DynamicSecretSources maps dynamic secret engine kinds to their minters.
	DynamicSecretSources map[string]sdk.SecretMinter
}

// empty reports whether the extension provides nothing.
func (e *Extension) empty() bool {
	return len(e.Tools) == 0 && len(e.Notifiers) == 0 && len(e.AIProviders) == 0 &&
		len(e.SecretSources) == 0 && len(e.DynamicSecretSources) == 0
}

// validate panics on an extension that cannot serve: nil, empty, or holding a nil entry. A panic
// here surfaces at plugin startup, where the host logs it and skips the binary.
func (e *Extension) validate() {
	if e == nil {
		panic("plugin: nil extension")
	}
	if e.empty() {
		panic("plugin: extension provides nothing")
	}
	for name, r := range e.Tools {
		if name == "" || r == nil {
			panic("plugin: empty name or nil runner in Tools")
		}
	}
	for name, n := range e.Notifiers {
		if name == "" || n == nil {
			panic("plugin: empty name or nil notifier in Notifiers")
		}
	}
	for name, f := range e.AIProviders {
		if name == "" || f == nil {
			panic("plugin: empty name or nil factory in AIProviders")
		}
	}
	for kind, r := range e.SecretSources {
		if kind == "" || r == nil {
			panic("plugin: empty kind or nil resolver in SecretSources")
		}
	}
	for kind, m := range e.DynamicSecretSources {
		if kind == "" || m == nil {
			panic("plugin: empty kind or nil minter in DynamicSecretSources")
		}
	}
}

// Serve runs the extension as a plugin process. Call it from main; it blocks until the host ends
// the process. It panics on an extension that provides nothing, which the host logs as the
// plugin's startup failure.
func Serve(ext *Extension) {
	ext.validate()
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         Set(ext),
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}

// Set builds the go-plugin plugin set both sides share: the plugin process passes its Extension,
// the host passes nil and receives the gRPC client from Dispense.
func Set(ext *Extension) goplugin.PluginSet {
	return goplugin.PluginSet{Key: &grpcPlugin{ext: ext}}
}

// grpcPlugin wires the Extension service into go-plugin: it serves the extension on the plugin
// side and hands the host a generated gRPC client.
type grpcPlugin struct {
	goplugin.Plugin
	// ext is the served extension, nil on the host side.
	ext *Extension
}

// GRPCServer registers the extension service on the plugin process's gRPC server.
func (p *grpcPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	extproto.RegisterExtensionServer(s, newServer(p.ext))
	return nil
}

// GRPCClient returns the generated client the host uses to call the plugin.
func (p *grpcPlugin) GRPCClient(_ context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return extproto.NewExtensionClient(c), nil
}
