// Package extplugin loads out-of-process extensions at startup. Each executable in the plugins
// directory is launched as a go-plugin subprocess speaking the Extension gRPC protocol over a
// local socket with mutual TLS. The loader asks each plugin what it provides and registers every
// seam through the same SDK entry points a compiled-in extension uses, so the rest of the server
// cannot tell the difference.
package extplugin

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
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
	// The Describe response is untrusted input. Registering a bad name panics the shared registry,
	// and that panic unwinds through the Load loop past every plugin left to load, so one bad plugin
	// takes down the rest. Validate the whole response first and skip this plugin whole on a failure,
	// which also means a partly wired plugin never exists: register runs only once nothing in the
	// response can panic.
	if err := validate(desc); err != nil {
		client.Kill()
		return nil, err
	}

	register(seam{client: ext, exited: client.Exited}, desc)
	log.Info("extplugin: loaded plugin",
		zap.String("file", filepath.Base(path)),
		zap.Strings("tools", desc.GetTools()),
		zap.Strings("notifiers", desc.GetNotifiers()),
		zap.Strings("ai_providers", desc.GetAiProviders()),
		zap.Strings("secret_sources", desc.GetSecretSources()),
		zap.Strings("dynamic_secret_sources", desc.GetDynamicSecretSources()))
	return client, nil
}

// validate checks a plugin's Describe response is registrable before any of it is registered. It
// mirrors what each registry rejects: an empty name (every registry panics on one), a name already
// claimed by a built-in or an earlier plugin, and a duplicate within the response. Secret resolvers
// and dynamic minters share one namespace, so a kind in either, or in both, is a collision. The
// normalization matches each registry so a name that would collide after normalization is caught
// here rather than at registration.
func validate(desc *extproto.DescribeResponse) error {
	tools := desc.GetTools()
	channels := desc.GetNotifiers()
	providers := desc.GetAiProviders()
	// Resolvers and minters share the secret-kind namespace, so they are checked as one list.
	secrets := append(append([]string{}, desc.GetSecretSources()...), desc.GetDynamicSecretSources()...)

	for _, list := range [][]string{tools, channels, providers, secrets} {
		for _, name := range list {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("%w: an empty name", ErrPluginContract)
			}
		}
	}
	if err := checkNamespace(tools, run.ValidTool, run.NormalizeTool); err != nil {
		return err
	}
	if err := checkNamespace(channels, dispatch.NotifierRegistered, identity); err != nil {
		return err
	}
	if err := checkNamespace(providers, aiRegistered, aiNormalize); err != nil {
		return err
	}
	return checkNamespace(secrets, secretsource.Registered, identity)
}

// checkNamespace reports the first name in names that is already registered, per taken, or that
// repeats within names once put through norm, the same normalization the registry keys on.
func checkNamespace(names []string, taken func(string) bool, norm func(string) string) error {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if taken(name) {
			return fmt.Errorf("%w: %q is already registered", ErrPluginContract, name)
		}
		key := norm(name)
		if seen[key] {
			return fmt.Errorf("%w: %q is declared twice", ErrPluginContract, name)
		}
		seen[key] = true
	}
	return nil
}

// identity returns s unchanged, for namespaces the registry keys on verbatim.
func identity(s string) string { return s }

// aiNormalize matches how the AI registry keys a provider name, lowercase and trimmed.
func aiNormalize(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// aiRegistered reports whether an AI provider name is already registered, comparing on the registry's
// own normalized form.
func aiRegistered(name string) bool { return slices.Contains(ai.Names(), aiNormalize(name)) }

// register wires everything a plugin declared into the process registries, through the same SDK
// entry points a compiled-in extension uses.
func register(s seam, desc *extproto.DescribeResponse) {
	for _, tool := range desc.GetTools() {
		sdk.RegisterTool(tool, &toolRunner{seam: s, tool: tool})
	}
	for _, channel := range desc.GetNotifiers() {
		sdk.RegisterNotifier(channel, &notifier{seam: s, channel: channel})
	}
	for _, name := range desc.GetAiProviders() {
		sdk.RegisterAIProvider(name, aiFactory(s, name))
	}
	for _, kind := range desc.GetSecretSources() {
		sdk.RegisterSecretSource(kind, secretResolver(s, kind))
	}
	for _, kind := range desc.GetDynamicSecretSources() {
		sdk.RegisterDynamicSecretSource(kind, secretMinter(s, kind))
	}
}

// newHCLog adapts the zap logger to the hclog interface go-plugin logs through, which also carries
// the plugin process's own stderr. It preserves the level of each line instead of flattening every
// line to Debug: go-plugin routes a plugin crash and its panic trace at Error, and flattening those
// to Debug is what let a production logger drop them, so a plugin that died left nothing to diagnose.
// The intercept logger's own level is Trace so it never pre-filters a line, and the sink maps each
// line to the matching zap level, where the production Info filter keeps errors and warnings and
// drops the go-plugin handshake chatter that arrives at Trace and Debug.
func newHCLog(log *zap.Logger) hclog.Logger {
	l := hclog.NewInterceptLogger(&hclog.LoggerOptions{
		Name:        "extplugin",
		Level:       hclog.Trace,
		Output:      io.Discard,
		DisableTime: true,
	})
	l.RegisterSink(zapSink{log: log.Named("extplugin")})
	return l
}

// zapSink forwards hclog lines to zap at the matching level, so a plugin's severity survives instead
// of collapsing to one level.
type zapSink struct {
	// log is the destination logger.
	log *zap.Logger
}

// Accept forwards one hclog line to zap, mapping the level and turning hclog's trailing key/value
// args into zap fields. Trace, Debug, and any unclassified level map to Debug so go-plugin's verbose
// internal chatter is dropped by a production logger while a crash at Error survives it.
func (s zapSink) Accept(name string, level hclog.Level, msg string, args ...any) {
	fields := make([]zap.Field, 0, len(args)/2+1)
	if name != "" {
		fields = append(fields, zap.String("logger", name))
	}
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			key = fmt.Sprintf("%v", args[i])
		}
		fields = append(fields, zap.Any(key, args[i+1]))
	}
	switch level {
	case hclog.Error:
		s.log.Error(msg, fields...)
	case hclog.Warn:
		s.log.Warn(msg, fields...)
	case hclog.Info:
		s.log.Info(msg, fields...)
	default:
		s.log.Debug(msg, fields...)
	}
}
