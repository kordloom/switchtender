// Command bareplugin is a binary that speaks SwitchTender's plugin handshake but serves none of the
// Extension protocol behind it. It stands for any third-party binary dropped into the plugins
// directory that answers the handshake and then cannot answer what it provides, and the loader has to
// skip it rather than register it or fail the server's whole startup.
package main

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/kordloom/switchtender/sdk/plugin"
)

// bare serves an empty gRPC server, so every Extension call comes back unimplemented.
type bare struct {
	goplugin.Plugin
}

// GRPCServer registers nothing, which is the whole point of this binary.
func (bare) GRPCServer(*goplugin.GRPCBroker, *grpc.Server) error { return nil }

// GRPCClient is never called here, since the host builds its own client.
func (bare) GRPCClient(context.Context, *goplugin.GRPCBroker, *grpc.ClientConn) (any, error) {
	return nil, nil
}

// main completes the handshake and serves nothing.
func main() {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: plugin.Handshake,
		Plugins:         goplugin.PluginSet{plugin.Key: bare{}},
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}
