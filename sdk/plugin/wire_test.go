package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
)

// The plugin side must satisfy the generated service interface and go-plugin's gRPC plugin
// interface, or a binary built against this SDK fails to serve at all. These assertions fail at
// compile time, which is where a broken public surface should fail.
var (
	_ extproto.ExtensionServer = (*server)(nil)
	_ goplugin.GRPCPlugin      = (*grpcPlugin)(nil)
	_ goplugin.Plugin          = (*grpcPlugin)(nil)
	_ io.Writer                = (*streamWriter)(nil)
)

// The exported aliases are the whole SDK contract an extension author codes against. Each Func
// adapter must satisfy the interface it adapts, or the documented one-line registration in the
// package doc does not compile for anyone.
var (
	_ sdk.ToolRunner = sdk.ToolRunnerFunc(nil)
	_ sdk.Notifier   = sdk.NotifierFunc(nil)
	_ sdk.AIProvider = sdk.AIProviderFunc(nil)
)

// dialPlugin serves ext through the real plugin-side gRPC stack, interceptors included, and returns
// the client the host receives from Dispense. Both halves are built the way the two processes build
// them: the plugin passes its extension to Set, the host passes nil.
func dialPlugin(t *testing.T, ext *Extension) extproto.ExtensionClient {
	t.Helper()
	pluginSide, ok := Set(ext)[Key].(*grpcPlugin)
	if !ok {
		t.Fatalf("Set did not install a plugin under %q", Key)
	}
	srv := grpcServer(nil)
	if err := pluginSide.GRPCServer(nil, srv); err != nil {
		t.Fatalf("GRPCServer: %v", err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() {
		_ = srv.Serve(lis)
	}()
	conn, err := grpc.NewClient("passthrough:///plugin",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hostSide, ok := Set(nil)[Key].(*grpcPlugin)
	if !ok {
		t.Fatal("Set(nil) did not install a plugin for the host side")
	}
	raw, err := hostSide.GRPCClient(context.Background(), nil, conn)
	if err != nil {
		t.Fatalf("GRPCClient: %v", err)
	}
	client, ok := raw.(extproto.ExtensionClient)
	if !ok {
		t.Fatalf("GRPCClient returned %T, want an extension client", raw)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return client
}

// TestSetInstallsOneExtensionUnderKey pins the plugin set both processes agree on. The host dispenses
// by this exact key, so a set built under any other name hands the host nothing to talk to, and the
// host's own half must carry no extension since it serves none.
func TestSetInstallsOneExtensionUnderKey(t *testing.T) {
	t.Parallel()
	ext := &Extension{Tools: map[string]sdk.ToolRunner{"hello": okRunner}}
	set := Set(ext)
	if len(set) != 1 {
		t.Fatalf("Set built %d plugins, want exactly 1", len(set))
	}
	p, ok := set[Key].(*grpcPlugin)
	if !ok {
		t.Fatalf("Set[%q] = %T, want *grpcPlugin", Key, set[Key])
	}
	if p.ext != ext {
		t.Error("Set did not carry the extension it was given")
	}
	host, ok := Set(nil)[Key].(*grpcPlugin)
	if !ok {
		t.Fatalf("Set(nil)[%q] = %T, want *grpcPlugin", Key, Set(nil)[Key])
	}
	if host.ext != nil {
		t.Error("the host side of the set carries an extension, and it serves none")
	}
}

// TestHandshakeIsFrozen pins the values every plugin binary in the field was compiled against. A
// changed cookie or protocol version makes every existing extension fail its handshake, and the
// operator sees only a plugin that will not load.
func TestHandshakeIsFrozen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Got        string
		WantResult string
	}{ // Test 0: The cookie key names the variable go-plugin passes to the child.
		{Name: "cookie key", Got: Handshake.MagicCookieKey, WantResult: "SWITCHTENDER_EXTENSION"},
		// Test 1: The cookie value is what a child compares against.
		{Name: "cookie value", Got: Handshake.MagicCookieValue, WantResult: "switchtender-extension-v1"},
		// Test 2: The dispense key names the one plugin in the set.
		{Name: "plugin key", Got: Key, WantResult: "extension"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if test.Got != test.WantResult {
				t.Errorf("test %d (%s): %q, want %q", testNum, test.Name, test.Got, test.WantResult)
			}
		})
	}
	if Handshake.ProtocolVersion != 1 {
		t.Errorf("protocol version = %d, want 1", Handshake.ProtocolVersion)
	}
}

// TestServeRefusesAnUnservableExtension pins that Serve validates before it starts serving. An
// extension that provides nothing would otherwise hand the host a binary that handshakes, describes
// nothing, and registers nothing, which looks to an operator like a plugin that loaded fine.
func TestServeRefusesAnUnservableExtension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Ext  *Extension
	}{ // Test 0: A nil extension never reaches go-plugin.
		{Name: "nil extension", Ext: nil},
		// Test 1: An extension providing nothing never reaches go-plugin.
		{Name: "empty extension", Ext: &Extension{}},
		// Test 2: A nil runner among good entries never reaches go-plugin.
		{Name: "nil runner", Ext: &Extension{Tools: map[string]sdk.ToolRunner{"a": okRunner, "b": nil}}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("test %d (%s): Serve did not panic on an unservable extension",
						testNum, test.Name)
				}
			}()
			Serve(test.Ext)
		})
	}
}

// fullExtension builds an extension that answers every seam with a value derived from its input, so
// a wire test can prove each field crossed rather than that a call merely succeeded.
func fullExtension() *Extension {
	return &Extension{
		Tools: map[string]sdk.ToolRunner{
			"hello": sdk.ToolRunnerFunc(
				func(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
					_, _ = fmt.Fprintf(out, "cmd=%s dry=%v dir=%s env=%v vars=%v",
						spec.Command, spec.DryRun, spec.Dir, spec.Env, spec.ExtraVars)
					return sdk.ToolResult{ExitCode: 9}, nil
				}),
		},
		Notifiers: map[string]sdk.Notifier{
			"chan": sdk.NotifierFunc(func(context.Context, *sdk.Run) error { return nil }),
		},
		AIProviders: map[string]sdk.AIProviderFactory{
			"ai": func(model, url, apiKey string) (sdk.AIProvider, error) {
				return sdk.AIProviderFunc(
					func(_ context.Context, system, user string) (string, error) {
						return strings.Join([]string{model, url, apiKey, system, user}, "|"), nil
					}), nil
			},
		},
		SecretSources: map[string]sdk.SecretResolver{
			"vault": func(_ context.Context, config string) (string, error) {
				return "static:" + config, nil
			},
		},
		DynamicSecretSources: map[string]sdk.SecretMinter{
			"sts": func(_ context.Context, config string) (string, *sdk.SecretLease, error) {
				return "minted:" + config, sdk.NewSecretLease("sts", func(context.Context) error {
					return nil
				}), nil
			},
		},
	}
}

// TestWireCarriesEverySeam drives all seven calls through the real gRPC stack the plugin binary
// serves. Every earlier test calls the server's methods directly, which proves the logic but not the
// wiring: a service registered under the wrong name, or a client built from the wrong constructor,
// passes every direct test and fails for every plugin author.
//
//nolint:funlen // Test function.
func TestWireCarriesEverySeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := dialPlugin(t, fullExtension())

	desc, err := client.Describe(ctx, &extproto.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := desc.GetTools(); len(got) != 1 || got[0] != "hello" {
		t.Errorf("Describe tools = %v, want [hello]", got)
	}
	if got := desc.GetDynamicSecretSources(); len(got) != 1 || got[0] != "sts" {
		t.Errorf("Describe dynamic secret sources = %v, want [sts]", got)
	}

	stream, err := client.RunTool(ctx, &extproto.RunToolRequest{
		Tool: "hello", Command: "ping", DryRun: true, Dir: "/w",
		Env: []string{"K=V"}, ExtraVarsJson: []byte(`{"a":1}`),
	})
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	var out strings.Builder
	exit := int32(-1)
	for {
		reply, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
		switch r := reply.GetReply().(type) {
		case *extproto.RunToolReply_Output:
			out.Write(r.Output)
		case *extproto.RunToolReply_Result:
			exit = r.Result.GetExitCode()
		}
	}
	want := "cmd=ping dry=true dir=/w env=[K=V] vars=map[a:1]"
	if out.String() != want {
		t.Errorf("streamed output = %q, want %q", out.String(), want)
	}
	if exit != 9 {
		t.Errorf("exit = %d, want 9", exit)
	}

	if _, err := client.Notify(ctx, &extproto.NotifyRequest{
		Channel: "chan", RunJson: []byte(`{"id":"r"}`),
	}); err != nil {
		t.Errorf("Notify: %v", err)
	}

	comp, err := client.Complete(ctx, &extproto.CompleteRequest{
		Provider: "ai", Model: "m", Url: "u", ApiKey: "k", System: "s", User: "p",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := comp.GetText(); got != "m|u|k|s|p" {
		t.Errorf("Complete text = %q, want every setting carried", got)
	}

	sec, err := client.ResolveSecret(ctx, &extproto.ResolveSecretRequest{Kind: "vault", Config: "c"})
	if err != nil {
		t.Fatalf("ResolveSecret: %v", err)
	}
	if got := sec.GetValue(); got != "static:c" {
		t.Errorf("ResolveSecret value = %q, want static:c", got)
	}

	mint, err := client.MintSecret(ctx, &extproto.MintSecretRequest{Kind: "sts", Config: "role"})
	if err != nil {
		t.Fatalf("MintSecret: %v", err)
	}
	if got := mint.GetValue(); got != "minted:role" {
		t.Errorf("MintSecret value = %q, want minted:role", got)
	}
	if mint.GetLeaseId() == "" {
		t.Fatal("MintSecret returned no lease id for a revocable lease")
	}
	if _, err := client.RevokeLease(ctx,
		&extproto.RevokeLeaseRequest{LeaseId: mint.GetLeaseId()}); err != nil {
		t.Errorf("RevokeLease: %v", err)
	}
	_, err = client.RevokeLease(ctx, &extproto.RevokeLeaseRequest{LeaseId: mint.GetLeaseId()})
	if status.Code(err) != codes.NotFound {
		t.Errorf("replayed RevokeLease = %v, want NotFound", err)
	}
}

// TestWireStreamsLargeOutput pins that a chatty tool's output crosses whole. A run's log is the
// evidence of what happened on a fleet, so a chunk dropped or truncated at the transport is missing
// evidence, and it would only show up on a verbose run.
func TestWireStreamsLargeOutput(t *testing.T) {
	t.Parallel()
	const chunks, size = 64, 4096
	client := dialPlugin(t, &Extension{Tools: map[string]sdk.ToolRunner{
		"noisy": sdk.ToolRunnerFunc(
			func(_ context.Context, _ sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
				chunk := []byte(strings.Repeat("x", size))
				for range chunks {
					if _, err := out.Write(chunk); err != nil {
						return sdk.ToolResult{}, err
					}
				}
				return sdk.ToolResult{}, nil
			}),
	}})
	stream, err := client.RunTool(context.Background(), &extproto.RunToolRequest{Tool: "noisy"})
	if err != nil {
		t.Fatalf("RunTool: %v", err)
	}
	total := 0
	sawResult := false
	for {
		reply, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
		switch r := reply.GetReply().(type) {
		case *extproto.RunToolReply_Output:
			total += len(r.Output)
		case *extproto.RunToolReply_Result:
			sawResult = true
		}
	}
	if total != chunks*size {
		t.Errorf("received %d output bytes, want %d", total, chunks*size)
	}
	if !sawResult {
		t.Error("the stream ended without a result")
	}
}

// TestWireContainsPanics pins the containment promise the interceptors exist for: one seam that
// panics must not kill the process serving every other seam. A plugin binary serves a fleet's tools,
// notifications, and secret engines at once, so a crash in one takes all of them with it, and the
// host has no way to bring the process back before a restart.
//
//nolint:funlen // Test function.
func TestWireContainsPanics(t *testing.T) {
	t.Parallel()
	const secret = "panic-text-with-a-password"
	var nilRunner sdk.ToolRunnerFunc
	ext := fullExtension()
	ext.Tools["panics"] = sdk.ToolRunnerFunc(
		func(context.Context, sdk.ToolSpec, io.Writer) (sdk.ToolResult, error) {
			panic(secret)
		})
	// A typed nil runner passes validation, since the interface value is not nil, and panics on the
	// first call. The containment has to hold for that too.
	ext.Tools["typednil"] = nilRunner
	ext.Notifiers["panics"] = sdk.NotifierFunc(func(context.Context, *sdk.Run) error {
		panic(secret)
	})
	ext.SecretSources["panics"] = func(context.Context, string) (string, error) { panic(secret) }
	// A factory that reports success and hands back no provider is an author mistake the server
	// cannot check for, so the completion call dereferences nothing and panics. It has to fail closed
	// like any other panic rather than take the process down.
	ext.AIProviders["nilprovider"] = func(_, _, _ string) (sdk.AIProvider, error) { return nil, nil }
	client := dialPlugin(t, ext)
	ctx := context.Background()

	tests := []struct {
		Name string
		Call func() error
	}{{ // Test 0: A panicking tool ends its stream as an error, not a dead process.
		Name: "tool panics",
		Call: func() error {
			stream, err := client.RunTool(ctx, &extproto.RunToolRequest{Tool: "panics"})
			if err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return err
				}
			}
		},
	}, { // Test 1: A runner that is a typed nil panics inside the callback and is contained.
		Name: "typed nil runner",
		Call: func() error {
			stream, err := client.RunTool(ctx, &extproto.RunToolRequest{Tool: "typednil"})
			if err != nil {
				return err
			}
			for {
				if _, err := stream.Recv(); err != nil {
					return err
				}
			}
		},
	}, { // Test 2: A panicking notifier is contained.
		Name: "notifier panics",
		Call: func() error {
			_, err := client.Notify(ctx, &extproto.NotifyRequest{
				Channel: "panics", RunJson: []byte(`{"id":"r"}`),
			})
			return err
		},
	}, { // Test 3: A panicking secret resolver is contained.
		Name: "resolver panics",
		Call: func() error {
			_, err := client.ResolveSecret(ctx, &extproto.ResolveSecretRequest{Kind: "panics"})
			return err
		},
	}, { // Test 4: A factory that returns no provider and no error is contained rather than serving.
		Name: "factory returns a nil provider",
		Call: func() error {
			_, err := client.Complete(ctx, &extproto.CompleteRequest{Provider: "nilprovider"})
			return err
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			err := test.Call()
			if status.Code(err) != codes.Internal {
				t.Fatalf("test %d (%s): code = %v, want Internal (err %v)",
					testNum, test.Name, status.Code(err), err)
			}
			// The panic value can carry secret-bearing text, so only the method name may reach the host.
			msg := status.Convert(err).Message()
			if strings.Contains(msg, secret) {
				t.Errorf("test %d (%s): the panic value reached the host: %q", testNum, test.Name, msg)
			}
			if !strings.HasSuffix(msg, "panicked") || !strings.Contains(msg, "plugin callback /") {
				t.Errorf("test %d (%s): message = %q, want the method name and nothing else",
					testNum, test.Name, msg)
			}
		})
	}

	// The point of containment: after every panic above, the same process still serves every other
	// seam. A crash here would have taken the whole binary down and this call would never answer.
	if _, err := client.Describe(ctx, &extproto.DescribeRequest{}); err != nil {
		t.Fatalf("the plugin process did not survive the panics: %v", err)
	}
	sec, err := client.ResolveSecret(ctx, &extproto.ResolveSecretRequest{Kind: "vault", Config: "c"})
	if err != nil || sec.GetValue() != "static:c" {
		t.Errorf("a healthy seam broke after another seam panicked: %q, %v", sec.GetValue(), err)
	}
}

// TestWireRefusesUndeclaredNames pins that the wire refuses a call for a seam the plugin never
// declared, on every seam, with NotFound. The host registers from Describe, so a call for anything
// else means the host and the plugin disagree, and answering it would run a seam nobody registered.
func TestWireRefusesUndeclaredNames(t *testing.T) {
	t.Parallel()
	client := dialPlugin(t, &Extension{
		SecretSources: map[string]sdk.SecretResolver{
			"vault": func(context.Context, string) (string, error) { return "", nil },
		},
	})
	ctx := context.Background()
	tests := []struct {
		Name string
		Call func() error
	}{{ // Test 0: An undeclared tool is refused.
		Name: "tool",
		Call: func() error {
			stream, err := client.RunTool(ctx, &extproto.RunToolRequest{Tool: "absent"})
			if err != nil {
				return err
			}
			_, err = stream.Recv()
			return err
		},
	}, { // Test 1: An undeclared notifier is refused.
		Name: "notifier",
		Call: func() error {
			_, err := client.Notify(ctx, &extproto.NotifyRequest{
				Channel: "absent", RunJson: []byte(`{}`),
			})
			return err
		},
	}, { // Test 2: An undeclared AI provider is refused.
		Name: "ai provider",
		Call: func() error {
			_, err := client.Complete(ctx, &extproto.CompleteRequest{Provider: "absent"})
			return err
		},
	}, { // Test 3: An undeclared secret source is refused.
		Name: "secret source",
		Call: func() error {
			_, err := client.ResolveSecret(ctx, &extproto.ResolveSecretRequest{Kind: "absent"})
			return err
		},
	}, { // Test 4: An undeclared dynamic secret source is refused.
		Name: "dynamic secret source",
		Call: func() error {
			_, err := client.MintSecret(ctx, &extproto.MintSecretRequest{Kind: "absent"})
			return err
		},
	}, { // Test 5: A lease id no mint handed out is refused.
		Name: "lease",
		Call: func() error {
			_, err := client.RevokeLease(ctx, &extproto.RevokeLeaseRequest{LeaseId: "lease-1"})
			return err
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := status.Code(test.Call()); got != codes.NotFound {
				t.Errorf("test %d (%s): code = %v, want NotFound", testNum, test.Name, got)
			}
		})
	}
}
