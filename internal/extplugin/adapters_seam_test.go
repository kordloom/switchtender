package extplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
)

// errWire is the transport failure a fake plugin client returns when a test wants the seam's error
// path, standing in for a gRPC error from a plugin that is unreachable or broken.
var errWire = errors.New("the client connection is closing")

// fakeStream is the reply stream a fake plugin client hands the tool runner. It replays a scripted
// sequence of replies and errors, so a test can end a stream any way a real plugin can.
type fakeStream struct {
	grpc.ClientStream
	// replies are handed out in order, one per Recv.
	replies []*extproto.RunToolReply
	// errs holds the error to return at each Recv position, nil to return the reply instead.
	errs []error
	// pos is the next Recv position.
	pos int
}

// Recv returns the next scripted reply or error, ending with io.EOF once the script runs out.
func (s *fakeStream) Recv() (*extproto.RunToolReply, error) {
	if s.pos >= len(s.replies) && s.pos >= len(s.errs) {
		return nil, io.EOF
	}
	i := s.pos
	s.pos++
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	if i < len(s.replies) {
		return s.replies[i], nil
	}
	return nil, io.EOF
}

// fakeClient is an extproto.ExtensionClient whose every call is scripted by the test, so each seam's
// failure path can be driven without a plugin process.
type fakeClient struct {
	// runTool answers RunTool, recording the request it was given.
	runTool func(*extproto.RunToolRequest) (grpc.ServerStreamingClient[extproto.RunToolReply], error)
	// notify answers Notify.
	notify func(*extproto.NotifyRequest) (*extproto.NotifyResponse, error)
	// complete answers Complete.
	complete func(*extproto.CompleteRequest) (*extproto.CompleteResponse, error)
	// resolve answers ResolveSecret.
	resolve func(*extproto.ResolveSecretRequest) (*extproto.ResolveSecretResponse, error)
	// mint answers MintSecret.
	mint func(*extproto.MintSecretRequest) (*extproto.MintSecretResponse, error)
	// revoke answers RevokeLease.
	revoke func(*extproto.RevokeLeaseRequest) (*extproto.RevokeLeaseResponse, error)
}

// Describe is unused by the adapters and always reports an empty extension.
func (c *fakeClient) Describe(context.Context, *extproto.DescribeRequest,
	...grpc.CallOption) (*extproto.DescribeResponse, error) {
	return &extproto.DescribeResponse{}, nil
}

// RunTool answers with the scripted stream.
func (c *fakeClient) RunTool(_ context.Context, req *extproto.RunToolRequest,
	_ ...grpc.CallOption) (grpc.ServerStreamingClient[extproto.RunToolReply], error) {
	return c.runTool(req)
}

// Notify answers with the scripted reply.
func (c *fakeClient) Notify(_ context.Context, req *extproto.NotifyRequest,
	_ ...grpc.CallOption) (*extproto.NotifyResponse, error) {
	return c.notify(req)
}

// Complete answers with the scripted reply.
func (c *fakeClient) Complete(_ context.Context, req *extproto.CompleteRequest,
	_ ...grpc.CallOption) (*extproto.CompleteResponse, error) {
	return c.complete(req)
}

// ResolveSecret answers with the scripted reply.
func (c *fakeClient) ResolveSecret(_ context.Context, req *extproto.ResolveSecretRequest,
	_ ...grpc.CallOption) (*extproto.ResolveSecretResponse, error) {
	return c.resolve(req)
}

// MintSecret answers with the scripted reply.
func (c *fakeClient) MintSecret(_ context.Context, req *extproto.MintSecretRequest,
	_ ...grpc.CallOption) (*extproto.MintSecretResponse, error) {
	return c.mint(req)
}

// RevokeLease answers with the scripted reply.
func (c *fakeClient) RevokeLease(_ context.Context, req *extproto.RevokeLeaseRequest,
	_ ...grpc.CallOption) (*extproto.RevokeLeaseResponse, error) {
	return c.revoke(req)
}

// outputReply builds one output chunk reply.
func outputReply(s string) *extproto.RunToolReply {
	return &extproto.RunToolReply{Reply: &extproto.RunToolReply_Output{Output: []byte(s)}}
}

// resultReply builds the reply that ends a stream with an exit code.
func resultReply(code int32) *extproto.RunToolReply {
	return &extproto.RunToolReply{
		Reply: &extproto.RunToolReply_Result{Result: &extproto.ToolResult{ExitCode: code}},
	}
}

// errWriter fails every write, standing in for a run log that can no longer be written.
type errWriter struct{}

// Write always fails.
func (errWriter) Write([]byte) (int, error) { return 0, errWire }

// TestSeamFailNamesADeadPlugin pins the one thing an operator reads when a plugin call fails.
// Nothing unregisters a plugin's names, so a plugin that died stays registered and every later call
// fails deep in the transport with a message naming neither the plugin nor the reason. A seam that
// did not check the process would leave an operator unable to tell a dead plugin from a network
// fault or a bug in their own playbook.
func TestSeamFailNamesADeadPlugin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Exited   func() bool
		Want     error
		WantText string
	}{{ // Test 0: A live plugin's failure names the seam and keeps the cause.
		Name: "process alive", Exited: func() bool { return false },
		WantText: "plugin tool exttest",
	}, { // Test 1: A seam with no process report behaves like a live one.
		Name: "no exit report", Exited: nil, WantText: "plugin tool exttest",
	}, { // Test 2: A dead plugin says so, and says it stays broken until a restart.
		Name: "process gone", Exited: func() bool { return true }, Want: ErrPluginGone,
		WantText: "still registered but its process is gone",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := seam{exited: test.Exited}.fail("tool", "exttest", errWire)
			if !errors.Is(err, errWire) {
				t.Errorf("test %d (%s): the underlying cause was dropped: %v", testNum, test.Name, err)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.Want)
			}
			if test.Want == nil && errors.Is(err, ErrPluginGone) {
				t.Errorf("test %d (%s): a live plugin was reported as gone: %v", testNum, test.Name, err)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("test %d (%s): err = %q, want it to contain %q",
					testNum, test.Name, err, test.WantText)
			}
		})
	}
}

// TestToolRunnerRunRefusals pins every way a proxied tool run fails rather than reporting a result
// the plugin never sent. Exit code zero means the change succeeded on a fleet, so a failure that
// returned a zero code would record work that never happened; each of these must return -1 and an
// error instead.
//
//nolint:funlen // Test function.
func TestToolRunnerRunRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Spec     sdk.ToolSpec
		Client   *fakeClient
		Out      io.Writer
		Exited   func() bool
		Want     error
		WantText string
	}{{ // Test 0: Extra vars that cannot be encoded fail before the call is made.
		Name: "unencodable extra vars",
		Spec: sdk.ToolSpec{ExtraVars: map[string]any{"fn": func() {}}},
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			t.Error("the call was made despite unencodable extra vars")
			return nil, nil
		}},
		WantText: "encode extra vars",
	}, { // Test 1: A transport failure opening the stream is reported against the named tool.
		Name: "run call fails",
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			return nil, errWire
		}},
		WantText: "plugin tool exttest",
	}, { // Test 2: A stream that ends without a result is a protocol error, not a success.
		Name: "stream ends without a result",
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			return &fakeStream{replies: []*extproto.RunToolReply{outputReply("partial")}}, nil
		}},
		Want:     ErrProtocol,
		WantText: "ended without a result",
	}, { // Test 3: A stream that breaks mid-run is reported against the named tool.
		Name: "stream breaks mid run",
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			return &fakeStream{errs: []error{errWire}}, nil
		}},
		WantText: "plugin tool exttest",
	}, { // Test 4: A run log that cannot be written fails the run rather than dropping output.
		Name: "output cannot be written",
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			return &fakeStream{replies: []*extproto.RunToolReply{
				outputReply("hello"), resultReply(0),
			}}, nil
		}},
		Out:      errWriter{},
		WantText: "write tool output",
	}, { // Test 5: A call through a plugin whose process is gone says so.
		Name: "plugin process gone",
		Client: &fakeClient{runTool: func(*extproto.RunToolRequest) (
			grpc.ServerStreamingClient[extproto.RunToolReply], error) {
			return nil, errWire
		}},
		Exited:   func() bool { return true },
		Want:     ErrPluginGone,
		WantText: "until the server restarts",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			out := test.Out
			if out == nil {
				out = io.Discard
			}
			r := &toolRunner{seam: seam{client: test.Client, exited: test.Exited}, tool: "exttest"}
			res, err := r.Run(context.Background(), test.Spec, out)
			if err == nil {
				t.Fatalf("test %d (%s): Run succeeded, want a failure", testNum, test.Name)
			}
			if res.ExitCode != -1 {
				t.Errorf("test %d (%s): exit code = %d, want -1 on a failed run",
					testNum, test.Name, res.ExitCode)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("test %d (%s): err = %q, want it to contain %q",
					testNum, test.Name, err, test.WantText)
			}
		})
	}
}

// TestToolRunnerRunStreams pins what a healthy run returns: every output chunk relayed in order and
// the exit code the plugin ended the stream with. A dropped chunk is missing evidence of what ran on
// a host, and a lost exit code is a failed change recorded as a success.
func TestToolRunnerRunStreams(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Replies    []*extproto.RunToolReply
		WantOutput string
		WantExit   int
	}{{ // Test 0: Chunks are relayed in order, then the exit code.
		Name: "output then result",
		Replies: []*extproto.RunToolReply{
			outputReply("one "), outputReply("two"), resultReply(0),
		},
		WantOutput: "one two",
	}, { // Test 1: A nonzero exit code crosses unchanged.
		Name:     "failure exit code",
		Replies:  []*extproto.RunToolReply{resultReply(42)},
		WantExit: 42,
	}, { // Test 2: Replies after the result are never read, since the result ends the run.
		Name: "result ends the stream",
		Replies: []*extproto.RunToolReply{
			resultReply(1), outputReply("after the end"),
		},
		WantExit: 1,
	}, { // Test 3: A reply carrying no known variant is skipped rather than ending the run, so a
		// newer plugin's reply kind does not break an older host.
		Name: "unknown reply variant skipped",
		Replies: []*extproto.RunToolReply{
			{}, outputReply("still here"), resultReply(0),
		},
		WantOutput: "still here",
	}, { // Test 4: An empty output chunk writes nothing and does not end the run.
		Name: "empty chunk",
		Replies: []*extproto.RunToolReply{
			outputReply(""), resultReply(3),
		},
		WantExit: 3,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			r := &toolRunner{tool: "exttest", seam: seam{client: &fakeClient{
				runTool: func(*extproto.RunToolRequest) (
					grpc.ServerStreamingClient[extproto.RunToolReply], error) {
					return &fakeStream{replies: test.Replies}, nil
				},
			}}}
			res, err := r.Run(context.Background(), sdk.ToolSpec{}, &out)
			if err != nil {
				t.Fatalf("test %d (%s): Run error: %v", testNum, test.Name, err)
			}
			if out.String() != test.WantOutput {
				t.Errorf("test %d (%s): output = %q, want %q",
					testNum, test.Name, out.String(), test.WantOutput)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("test %d (%s): exit code = %d, want %d",
					testNum, test.Name, res.ExitCode, test.WantExit)
			}
		})
	}
}

// TestToolRunnerSendsTheSpec pins the run request the plugin receives. The tool name comes from the
// registration rather than the spec, and a dropped field is a run that executes with input the
// operator never gave: a lost dry run flag turns a check into a change on a live fleet.
func TestToolRunnerSendsTheSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name          string
		Spec          sdk.ToolSpec
		WantExtraVars map[string]any
		WantHasVars   bool
	}{{ // Test 0: Extra vars are encoded as one JSON object.
		Name: "with extra vars",
		Spec: sdk.ToolSpec{
			Tool: "ignored", Command: "ping", DryRun: true,
			ExtraVars: map[string]any{"a": float64(1), "u": "ü"},
			Env:       []string{"K=V"}, Dir: "/w",
		},
		WantExtraVars: map[string]any{"a": float64(1), "u": "ü"},
		WantHasVars:   true,
	}, { // Test 1: A run with no extra vars sends no JSON at all, so the plugin sees none.
		Name: "no extra vars",
		Spec: sdk.ToolSpec{Command: "ping"},
	}, { // Test 2: An empty extra vars map is the same as none, not an empty object.
		Name: "empty extra vars map",
		Spec: sdk.ToolSpec{Command: "ping", ExtraVars: map[string]any{}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got *extproto.RunToolRequest
			r := &toolRunner{tool: "exttest", seam: seam{client: &fakeClient{
				runTool: func(req *extproto.RunToolRequest) (
					grpc.ServerStreamingClient[extproto.RunToolReply], error) {
					got = req
					return &fakeStream{replies: []*extproto.RunToolReply{resultReply(0)}}, nil
				},
			}}}
			if _, err := r.Run(context.Background(), test.Spec, io.Discard); err != nil {
				t.Fatalf("test %d (%s): Run error: %v", testNum, test.Name, err)
			}
			if got.GetTool() != "exttest" {
				t.Errorf("test %d (%s): tool = %q, want the registered name",
					testNum, test.Name, got.GetTool())
			}
			if got.GetCommand() != test.Spec.Command || got.GetDryRun() != test.Spec.DryRun ||
				got.GetDir() != test.Spec.Dir {
				t.Errorf("test %d (%s): request = %+v, want the spec's command, dry run, and dir",
					testNum, test.Name, got)
			}
			if diff := cmp.Diff(test.Spec.Env, got.GetEnv(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d (%s): env mismatch (-want +got):\n%s", testNum, test.Name, diff)
			}
			if !test.WantHasVars {
				if len(got.GetExtraVarsJson()) != 0 {
					t.Errorf("test %d (%s): extra vars json = %q, want none",
						testNum, test.Name, got.GetExtraVarsJson())
				}
				return
			}
			var decoded map[string]any
			if err := json.Unmarshal(got.GetExtraVarsJson(), &decoded); err != nil {
				t.Fatalf("test %d (%s): extra vars json does not decode: %v", testNum, test.Name, err)
			}
			if diff := cmp.Diff(test.WantExtraVars, decoded); diff != "" {
				t.Errorf("test %d (%s): extra vars mismatch (-want +got):\n%s", testNum, test.Name, diff)
			}
		})
	}
}

// TestNotifierNotify pins the notification seam's failures and its request. A notification is how a
// team learns a change finished, so a delivery that failed must be reported as a failure, and the run
// must reach the plugin as the same JSON the v1 API serves.
func TestNotifierNotify(t *testing.T) {
	t.Parallel()
	exit := 0
	tests := []struct {
		Name     string
		Run      *sdk.Run
		Err      error
		Exited   func() bool
		Want     error
		WantText string
	}{{ // Test 0: A run that cannot be encoded fails before the call.
		Name:     "unencodable run",
		Run:      &sdk.Run{ID: "r", ExtraVars: map[string]any{"fn": func() {}}},
		WantText: "encode run",
	}, { // Test 1: A transport failure names the notifier.
		Name: "delivery fails", Run: &sdk.Run{ID: "r"}, Err: errWire,
		WantText: "plugin notifier exttest-chan",
	}, { // Test 2: A dead plugin's notifier says the process is gone.
		Name: "plugin process gone", Run: &sdk.Run{ID: "r"}, Err: errWire,
		Exited: func() bool { return true }, Want: ErrPluginGone,
		WantText: "until the server restarts",
	}, { // Test 3: A healthy delivery reports success.
		Name: "delivered", Run: &sdk.Run{ID: "r", Status: "succeeded", ExitCode: &exit},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got *extproto.NotifyRequest
			n := &notifier{channel: "exttest-chan", seam: seam{exited: test.Exited, client: &fakeClient{
				notify: func(req *extproto.NotifyRequest) (*extproto.NotifyResponse, error) {
					got = req
					return &extproto.NotifyResponse{}, test.Err
				},
			}}}
			err := n.Notify(context.Background(), test.Run)
			if test.WantText == "" {
				if err != nil {
					t.Fatalf("test %d (%s): Notify error: %v", testNum, test.Name, err)
				}
				if got.GetChannel() != "exttest-chan" {
					t.Errorf("test %d (%s): channel = %q, want the registered name",
						testNum, test.Name, got.GetChannel())
				}
				var decoded sdk.Run
				if err := json.Unmarshal(got.GetRunJson(), &decoded); err != nil {
					t.Fatalf("test %d (%s): run json does not decode: %v", testNum, test.Name, err)
				}
				if diff := cmp.Diff(*test.Run, decoded, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("test %d (%s): run mismatch (-sent +received):\n%s",
						testNum, test.Name, diff)
				}
				return
			}
			if err == nil {
				t.Fatalf("test %d (%s): Notify succeeded, want a failure", testNum, test.Name)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("test %d (%s): err = %q, want it to contain %q",
					testNum, test.Name, err, test.WantText)
			}
		})
	}
}

// TestAIFactoryCarriesHostSettings pins that the host's model, endpoint, and key ride every
// completion rather than being held in the plugin. A plugin that kept configuration would answer with
// stale settings after an operator changed them, and a dropped setting sends a prompt somewhere the
// operator did not choose.
func TestAIFactoryCarriesHostSettings(t *testing.T) {
	t.Parallel()
	var got *extproto.CompleteRequest
	s := seam{client: &fakeClient{
		complete: func(req *extproto.CompleteRequest) (*extproto.CompleteResponse, error) {
			got = req
			return &extproto.CompleteResponse{Text: "reply"}, nil
		},
	}}
	provider, err := aiFactory(s, "exttest-ai")("model-x", "https://host", "key-x")
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	text, err := provider.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if text != "reply" {
		t.Errorf("text = %q, want reply", text)
	}
	want := &extproto.CompleteRequest{
		Provider: "exttest-ai", Model: "model-x", Url: "https://host", ApiKey: "key-x",
		System: "sys", User: "user",
	}
	fields := [][2]string{
		{want.GetProvider(), got.GetProvider()}, {want.GetModel(), got.GetModel()},
		{want.GetUrl(), got.GetUrl()}, {want.GetApiKey(), got.GetApiKey()},
		{want.GetSystem(), got.GetSystem()}, {want.GetUser(), got.GetUser()},
	}
	for i, f := range fields {
		if f[0] != f[1] {
			t.Errorf("completion field %d = %q, want %q", i, f[1], f[0])
		}
	}
}

// TestAIFactoryReportsFailures pins that a failing AI seam names the provider, and names a dead
// plugin as dead. An advisory feature that failed silently would return an empty completion, which
// reads as the model having nothing to say.
func TestAIFactoryReportsFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Exited   func() bool
		Want     error
		WantText string
	}{ // Test 0: A live plugin's failure names the provider.
		{Name: "call fails", WantText: "plugin ai provider exttest-ai"},
		// Test 1: A dead plugin says the process is gone.
		{Name: "plugin process gone", Exited: func() bool { return true }, Want: ErrPluginGone,
			WantText: "until the server restarts"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := seam{exited: test.Exited, client: &fakeClient{
				complete: func(*extproto.CompleteRequest) (*extproto.CompleteResponse, error) {
					return nil, errWire
				},
			}}
			provider, err := aiFactory(s, "exttest-ai")("m", "", "")
			if err != nil {
				t.Fatalf("test %d (%s): factory error: %v", testNum, test.Name, err)
			}
			text, err := provider.Complete(context.Background(), "sys", "user")
			if err == nil {
				t.Fatalf("test %d (%s): Complete succeeded, want a failure", testNum, test.Name)
			}
			if text != "" {
				t.Errorf("test %d (%s): text = %q, want empty on a failure", testNum, test.Name, text)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("test %d (%s): err = %q, want it to contain %q",
					testNum, test.Name, err, test.WantText)
			}
		})
	}
}

// TestSecretResolverSeam pins the static secret seam. A resolver that failed quietly would inject an
// empty credential into a run and let it proceed as though the secret were fetched, so a failure has
// to be an error, and it has to name the source rather than surface as a bare transport message.
func TestSecretResolverSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Config    string
		Value     string
		Err       error
		Exited    func() bool
		Want      error
		WantValue string
		WantText  string
	}{{ // Test 0: A resolved value comes back with the config that produced it.
		Name: "resolved", Config: "path/a", Value: "s3cret", WantValue: "s3cret",
	}, { // Test 1: An empty config is passed through, since the plugin defines its meaning.
		Name: "empty config", Value: "v", WantValue: "v",
	}, { // Test 2: A failure names the secret source and returns no value.
		Name: "resolve fails", Config: "c", Value: "leaked", Err: errWire,
		WantText: "plugin secret source exttest-vault",
	}, { // Test 3: A dead plugin says the process is gone.
		Name: "plugin process gone", Err: errWire, Exited: func() bool { return true },
		Want: ErrPluginGone, WantText: "until the server restarts",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got *extproto.ResolveSecretRequest
			s := seam{exited: test.Exited, client: &fakeClient{
				resolve: func(req *extproto.ResolveSecretRequest) (
					*extproto.ResolveSecretResponse, error) {
					got = req
					return &extproto.ResolveSecretResponse{Value: test.Value}, test.Err
				},
			}}
			value, err := secretResolver(s, "exttest-vault")(context.Background(), test.Config)
			if value != test.WantValue {
				t.Errorf("test %d (%s): value = %q, want %q",
					testNum, test.Name, value, test.WantValue)
			}
			if test.WantText == "" {
				if err != nil {
					t.Fatalf("test %d (%s): resolve error: %v", testNum, test.Name, err)
				}
				if got.GetKind() != "exttest-vault" || got.GetConfig() != test.Config {
					t.Errorf("test %d (%s): request = %+v, want the registered kind and the config",
						testNum, test.Name, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("test %d (%s): resolve succeeded, want a failure", testNum, test.Name)
			}
			if test.Want != nil && !errors.Is(err, test.Want) {
				t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.Want)
			}
			if !strings.Contains(err.Error(), test.WantText) {
				t.Errorf("test %d (%s): err = %q, want it to contain %q",
					testNum, test.Name, err, test.WantText)
			}
		})
	}
}

// TestSecretMinterSeam pins the dynamic secret seam and the lease it hands back. The lease is the
// only way a run's short-lived credential is ended early, so a lease that revokes the wrong id, or
// one that reports success when the revocation failed, leaves a live credential behind.
//
//nolint:funlen // Test function.
func TestSecretMinterSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		LeaseID      string
		MintErr      error
		RevokeErr    error
		Exited       func() bool
		WantValue    string
		WantMintErr  error
		WantMintText string
		WantRevoked  string
		WantRevText  string
	}{{ // Test 0: A mint with a lease id revokes by that exact id.
		Name: "revocable lease", LeaseID: "lease-7", WantValue: "minted", WantRevoked: "lease-7",
	}, { // Test 1: A mint with no lease id revokes as a no-op, never with an empty id.
		Name: "expiring lease", WantValue: "minted",
	}, { // Test 2: A failed mint names the source and returns no value or lease.
		Name: "mint fails", MintErr: errWire,
		WantMintText: "plugin dynamic secret source exttest-sts",
	}, { // Test 3: A dead plugin says the process is gone.
		Name: "plugin process gone", MintErr: errWire, Exited: func() bool { return true },
		WantMintErr: ErrPluginGone, WantMintText: "until the server restarts",
	}, { // Test 4: A revocation that fails is reported, not swallowed.
		Name: "revoke fails", LeaseID: "lease-7", RevokeErr: errWire, WantValue: "minted",
		WantRevoked: "lease-7", WantRevText: "plugin dynamic secret source lease revoke exttest-sts",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			revoked := ""
			s := seam{exited: test.Exited, client: &fakeClient{
				mint: func(req *extproto.MintSecretRequest) (*extproto.MintSecretResponse, error) {
					if req.GetKind() != "exttest-sts" {
						t.Errorf("test %d: mint kind = %q, want the registered kind",
							testNum, req.GetKind())
					}
					return &extproto.MintSecretResponse{
						Value: "minted", LeaseId: test.LeaseID,
					}, test.MintErr
				},
				revoke: func(req *extproto.RevokeLeaseRequest) (*extproto.RevokeLeaseResponse, error) {
					revoked = req.GetLeaseId()
					return &extproto.RevokeLeaseResponse{}, test.RevokeErr
				},
			}}
			value, lease, err := secretMinter(s, "exttest-sts")(context.Background(), "role")
			if value != test.WantValue {
				t.Errorf("test %d (%s): value = %q, want %q",
					testNum, test.Name, value, test.WantValue)
			}
			if test.WantMintText != "" {
				if err == nil {
					t.Fatalf("test %d (%s): mint succeeded, want a failure", testNum, test.Name)
				}
				if lease != nil {
					t.Errorf("test %d (%s): a failed mint returned a lease", testNum, test.Name)
				}
				if test.WantMintErr != nil && !errors.Is(err, test.WantMintErr) {
					t.Errorf("test %d (%s): err = %v, want %v", testNum, test.Name, err, test.WantMintErr)
				}
				if !strings.Contains(err.Error(), test.WantMintText) {
					t.Errorf("test %d (%s): err = %q, want it to contain %q",
						testNum, test.Name, err, test.WantMintText)
				}
				return
			}
			if err != nil {
				t.Fatalf("test %d (%s): mint error: %v", testNum, test.Name, err)
			}
			if lease.Kind() != "exttest-sts" {
				t.Errorf("test %d (%s): lease kind = %q, want the registered kind",
					testNum, test.Name, lease.Kind())
			}
			revErr := lease.Revoke(context.Background())
			if revoked != test.WantRevoked {
				t.Errorf("test %d (%s): revoked lease id = %q, want %q",
					testNum, test.Name, revoked, test.WantRevoked)
			}
			if test.WantRevText == "" {
				if revErr != nil {
					t.Errorf("test %d (%s): Revoke error: %v", testNum, test.Name, revErr)
				}
				return
			}
			if revErr == nil {
				t.Fatalf("test %d (%s): Revoke succeeded, want a failure", testNum, test.Name)
			}
			if !strings.Contains(revErr.Error(), test.WantRevText) {
				t.Errorf("test %d (%s): Revoke err = %q, want it to contain %q",
					testNum, test.Name, revErr, test.WantRevText)
			}
		})
	}
}

// TestSecretMinterLeaseReportsADeadPlugin pins that revoking through a plugin that has since died
// says so. A lease is revoked at the end of a run, long after the mint, which is exactly the window a
// plugin process can die in, and an operator needs to know the credential is still live.
func TestSecretMinterLeaseReportsADeadPlugin(t *testing.T) {
	t.Parallel()
	alive := true
	s := seam{
		exited: func() bool { return !alive },
		client: &fakeClient{
			mint: func(*extproto.MintSecretRequest) (*extproto.MintSecretResponse, error) {
				return &extproto.MintSecretResponse{Value: "v", LeaseId: "lease-1"}, nil
			},
			revoke: func(*extproto.RevokeLeaseRequest) (*extproto.RevokeLeaseResponse, error) {
				return nil, errWire
			},
		},
	}
	_, lease, err := secretMinter(s, "exttest-sts")(context.Background(), "role")
	if err != nil {
		t.Fatalf("mint error: %v", err)
	}
	alive = false
	err = lease.Revoke(context.Background())
	if !errors.Is(err, ErrPluginGone) {
		t.Errorf("Revoke after the process died = %v, want ErrPluginGone", err)
	}
}
