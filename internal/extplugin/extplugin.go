// Package extplugin loads out-of-process extensions at startup. Each executable in the plugins
// directory is launched as a go-plugin subprocess speaking the Extension gRPC protocol over a
// local socket with mutual TLS. The loader asks each plugin what it provides and registers every
// seam through the same SDK entry points a compiled-in extension uses, so the rest of the server
// cannot tell the difference.
package extplugin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zapio"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
	"github.com/kordloom/switchtender/sdk/plugin"
)

// describeTimeout bounds the first call to a freshly launched plugin.
const describeTimeout = 10 * time.Second

// Load launches every plugin binary in dir and registers everything each one provides. It returns
// a close func that shuts the plugin processes down; call it after the dispatcher has drained so
// in-flight runs keep their tools. A binary that fails to launch or describe itself is logged and
// skipped, so one broken plugin does not take the server down. A name that collides with a
// built-in or another plugin panics, the registries' contract for a configuration error caught at
// startup. An empty dir loads nothing.
func Load(dir string, log *zap.Logger) (func(), error) {
	if dir == "" {
		return func() {}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrLoad, dir, err)
	}

	var clients []*goplugin.Client
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&0o111 == 0 {
			log.Debug("extplugin: skipping non-executable entry", zap.String("file", entry.Name()))
			continue
		}
		path := filepath.Join(dir, entry.Name())
		client, err := load(path, log)
		if err != nil {
			log.Warn("extplugin: skipping plugin: "+err.Error(), zap.String("file", entry.Name()))
			continue
		}
		clients = append(clients, client)
	}

	return func() {
		for _, c := range clients {
			c.Kill()
		}
	}, nil
}

// pluginEnvPrefix namespaces the environment an operator may deliberately pass to plugins. A
// variable outside the pass-through list below reaches a plugin only under this prefix, so
// configuring one is an explicit act rather than a consequence of what the server happens to hold.
const pluginEnvPrefix = "SWITCHTENDER_PLUGIN_"

// pluginPassThrough names the environment every process needs to function, none of which says
// anything about this install. Everything else is withheld.
var pluginPassThrough = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
	"LANG": true, "LC_ALL": true, "SYSTEMROOT": true, "USERPROFILE": true,
}

// pluginEnv builds the environment a plugin subprocess runs with: the few variables any process
// needs, plus anything the operator namespaced for plugins. It is an allowlist because the deny
// list is unbounded and grows every time this server learns to read another secret from the
// environment, and a missed entry hands that secret to every plugin on the machine.
func pluginEnv() []string {
	out := make([]string, 0, 8)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if pluginPassThrough[strings.ToUpper(name)] || strings.HasPrefix(name, pluginEnvPrefix) {
			out = append(out, kv)
		}
	}
	return out
}

// load launches one plugin binary, asks what it provides, and registers each seam. It returns the
// running client so the caller can shut it down, or an error when the binary cannot be launched
// or described.
func load(path string, log *zap.Logger) (*goplugin.Client, error) {
	cmd := exec.Command(path)
	cmd.Env = pluginEnv()
	client := goplugin.NewClient(&goplugin.ClientConfig{
		HandshakeConfig: plugin.Handshake,
		Plugins:         plugin.Set(nil),
		Cmd:             cmd,
		// The plugin gets the environment this loader hands it and nothing else. Without this the
		// library appends the server's whole environment, which carries the deployment encryption key
		// and salt, the worker token, and every configured provider secret: a drop-in binary could
		// read the key that seals every stored credential without asking for anything.
		SkipHostEnv:      true,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		Logger:           newHCLog(log),
	})

	proto, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("launch: %w", err)
	}
	raw, err := proto.Dispense(plugin.Key)
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("dispense: %w", err)
	}
	ext, ok := raw.(extproto.ExtensionClient)
	if !ok {
		client.Kill()
		return nil, fmt.Errorf("%w: unexpected client type %T", ErrProtocol, raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), describeTimeout)
	defer cancel()
	desc, err := ext.Describe(ctx, &extproto.DescribeRequest{})
	if err != nil {
		client.Kill()
		return nil, fmt.Errorf("describe: %w", err)
	}

	register(ext, desc)
	log.Info("extplugin: loaded plugin",
		zap.String("file", filepath.Base(path)),
		zap.Strings("tools", desc.GetTools()),
		zap.Strings("notifiers", desc.GetNotifiers()),
		zap.Strings("ai_providers", desc.GetAiProviders()),
		zap.Strings("secret_sources", desc.GetSecretSources()),
		zap.Strings("dynamic_secret_sources", desc.GetDynamicSecretSources()))
	return client, nil
}

// register wires everything a plugin declared into the process registries, through the same SDK
// entry points a compiled-in extension uses.
func register(ext extproto.ExtensionClient, desc *extproto.DescribeResponse) {
	for _, tool := range desc.GetTools() {
		sdk.RegisterTool(tool, &toolRunner{client: ext, tool: tool})
	}
	for _, channel := range desc.GetNotifiers() {
		sdk.RegisterNotifier(channel, &notifier{client: ext, channel: channel})
	}
	for _, name := range desc.GetAiProviders() {
		sdk.RegisterAIProvider(name, aiFactory(ext, name))
	}
	for _, kind := range desc.GetSecretSources() {
		sdk.RegisterSecretSource(kind, secretResolver(ext, kind))
	}
	for _, kind := range desc.GetDynamicSecretSources() {
		sdk.RegisterDynamicSecretSource(kind, secretMinter(ext, kind))
	}
}

// newHCLog adapts the zap logger to the hclog interface go-plugin logs through, which also
// carries the plugin process's own stderr.
func newHCLog(log *zap.Logger) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{
		Name:        "extplugin",
		Level:       hclog.Info,
		Output:      &zapio.Writer{Log: log.Named("extplugin"), Level: zapcore.DebugLevel},
		DisableTime: true,
	})
}
