package extplugin

import (
	"errors"
	"testing"

	"github.com/hashicorp/go-hclog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/kordloom/switchtender/internal/extproto"
)

// TestNewHCLogPreservesLevel checks a plugin's log lines reach zap at their own level, not flattened
// to Debug. A production logger runs at Info, so a plugin crash that go-plugin routes at Error must
// survive that filter, and go-plugin's own Trace and Debug handshake chatter must be dropped by it.
func TestNewHCLogPreservesLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		HCLevel  hclog.Level
		Msg      string
		WantKept bool
	}{ // Test 0: A crash diagnostic at Error survives the production Info filter.
		{Name: "error kept", HCLevel: hclog.Error, Msg: "panic: boom", WantKept: true},
		{Name: "warn kept", HCLevel: hclog.Warn, Msg: "deprecated", WantKept: true},
		{Name: "info kept", HCLevel: hclog.Info, Msg: "loaded plugin", WantKept: true},
		// Test 3: go-plugin's verbose handshake at Debug is dropped by the production filter.
		{Name: "debug dropped", HCLevel: hclog.Debug, Msg: "grpc handshake", WantKept: false},
		{Name: "trace dropped", HCLevel: hclog.Trace, Msg: "internal", WantKept: false},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			// An Info-level core mimics production, which drops Debug.
			core, logs := observer.New(zapcore.InfoLevel)
			hl := newHCLog(zap.New(core))
			hl.Log(test.HCLevel, test.Msg)

			kept := logs.Len() > 0
			if kept != test.WantKept {
				t.Fatalf("test %d: kept = %v, want %v (entries: %d)",
					testNum, kept, test.WantKept, logs.Len())
			}
			if test.WantKept && logs.All()[0].Message != test.Msg {
				t.Errorf("test %d: message = %q, want %q", testNum, logs.All()[0].Message, test.Msg)
			}
		})
	}
}

// TestValidateDescribe checks a plugin's Describe response is refused before any of it is registered
// when it names something the registry would panic on: an empty name, a collision with a built-in or
// an earlier plugin, a duplicate within the response, or a secret kind declared as both a resolver
// and a minter. A valid, all-distinct response passes.
func TestValidateDescribe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Desc *extproto.DescribeResponse
		Want error
	}{ // Test 0: Distinct, unclaimed names across every list pass.
		{
			Name: "all valid",
			Desc: &extproto.DescribeResponse{
				Tools: []string{"plugintool"}, Notifiers: []string{"pluginchan"},
				AiProviders: []string{"pluginai"}, SecretSources: []string{"pluginvault"},
				DynamicSecretSources: []string{"plugindyn"},
			},
			Want: nil,
		},
		// Test 1: An empty tool name is refused.
		{Name: "empty tool", Desc: &extproto.DescribeResponse{Tools: []string{""}}, Want: ErrPluginContract},
		// Test 2: An empty secret source is refused.
		{Name: "empty secret", Desc: &extproto.DescribeResponse{SecretSources: []string{""}},
			Want: ErrPluginContract},
		// An empty notifier is refused by the empty-name guard alone: an unregistered empty name is
		// not a collision and not yet a duplicate, so only the explicit guard catches it.
		{Name: "empty notifier", Desc: &extproto.DescribeResponse{Notifiers: []string{""}},
			Want: ErrPluginContract},
		// Test 3: A tool colliding with a built-in is refused.
		{Name: "builtin tool", Desc: &extproto.DescribeResponse{Tools: []string{"bash"}},
			Want: ErrPluginContract},
		// Test 4: A notifier declared twice in one response is refused.
		{Name: "dup notifier", Desc: &extproto.DescribeResponse{Notifiers: []string{"x", "x"}},
			Want: ErrPluginContract},
		// Test 5: A kind in both the resolver and minter lists shares one namespace and is refused.
		{
			Name: "secret kind in both lists",
			Desc: &extproto.DescribeResponse{
				SecretSources: []string{"vault"}, DynamicSecretSources: []string{"vault"},
			},
			Want: ErrPluginContract,
		},
	}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := validate(test.Desc)
			if !errors.Is(err, test.Want) {
				t.Errorf("test %d: validate() = %v, want %v", testNum, err, test.Want)
			}
		})
	}
}
