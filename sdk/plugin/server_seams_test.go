package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/kordloom/switchtender/internal/extproto"
	"github.com/kordloom/switchtender/sdk"
)

// errBoom is the failure a test seam returns when it wants the server's error path.
var errBoom = errors.New("boom")

// okRunner is a tool runner that succeeds with exit code zero and writes nothing.
var okRunner = sdk.ToolRunnerFunc(
	func(context.Context, sdk.ToolSpec, io.Writer) (sdk.ToolResult, error) {
		return sdk.ToolResult{}, nil
	})

// recordStream stands in for the run's reply stream. It records every reply the server sends and
// can fail a chosen send, which is what a host that hangs up mid-run looks like from inside the
// plugin process.
type recordStream struct {
	grpc.ServerStream
	// mu guards the recorded replies and the send counter.
	mu sync.Mutex
	// replies holds every reply the server sent, in order.
	replies []*extproto.RunToolReply
	// sends counts sends attempted so far.
	sends int
	// failAt is the one-based send number that returns an error, zero for a stream that never fails.
	failAt int
	// runCtx is the context the server hands the runner.
	runCtx context.Context
}

// Context returns the stream's context, the one a tool runner receives.
func (s *recordStream) Context() context.Context {
	if s.runCtx == nil {
		return context.Background()
	}
	return s.runCtx
}

// Send records one reply, or fails when this is the send the test wants broken.
func (s *recordStream) Send(r *extproto.RunToolReply) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends++
	if s.failAt != 0 && s.sends == s.failAt {
		return errBoom
	}
	s.replies = append(s.replies, r)
	return nil
}

// output returns every output chunk the server streamed, joined.
func (s *recordStream) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, r := range s.replies {
		if o, ok := r.GetReply().(*extproto.RunToolReply_Output); ok {
			b.Write(o.Output)
		}
	}
	return b.String()
}

// result returns the exit code the stream ended with and whether it ended with a result at all.
func (s *recordStream) result() (int32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.replies {
		if res, ok := r.GetReply().(*extproto.RunToolReply_Result); ok {
			return res.Result.GetExitCode(), true
		}
	}
	return 0, false
}

// TestDescribeListsEverySeamSorted pins the one call the host registers a plugin from. The host
// registers exactly the names Describe returns, so a dropped or reordered name is a seam that
// silently never exists. Sorted output keeps the host's startup log and its registration order
// stable across runs of the same binary.
func TestDescribeListsEverySeamSorted(t *testing.T) {
	t.Parallel()
	runner := okRunner
	notifier := sdk.NotifierFunc(func(context.Context, *sdk.Run) error { return nil })
	factory := sdk.AIProviderFactory(func(_, _, _ string) (sdk.AIProvider, error) { return nil, nil })
	resolver := sdk.SecretResolver(func(context.Context, string) (string, error) { return "", nil })
	minter := sdk.SecretMinter(
		func(context.Context, string) (string, *sdk.SecretLease, error) { return "", nil, nil })
	tests := []struct {
		Ext           *Extension
		WantTools     []string
		WantNotifiers []string
		WantAI        []string
		WantSecrets   []string
		WantDynamic   []string
	}{{ // Test 0: An extension with nothing in a list reports an empty list, never a missing one.
		Ext:       &Extension{Tools: map[string]sdk.ToolRunner{"only": runner}},
		WantTools: []string{"only"},
	}, { // Test 1: Names come back sorted whatever order the author built the map in.
		Ext: &Extension{Tools: map[string]sdk.ToolRunner{
			"zeta": runner, "alpha": runner, "middle": runner,
		}},
		WantTools: []string{"alpha", "middle", "zeta"},
	}, { // Test 2: Every seam is listed, each under its own heading.
		Ext: &Extension{
			Tools:                map[string]sdk.ToolRunner{"t": runner},
			Notifiers:            map[string]sdk.Notifier{"n": notifier},
			AIProviders:          map[string]sdk.AIProviderFactory{"a": factory},
			SecretSources:        map[string]sdk.SecretResolver{"s": resolver},
			DynamicSecretSources: map[string]sdk.SecretMinter{"d": minter},
		},
		WantTools: []string{"t"}, WantNotifiers: []string{"n"}, WantAI: []string{"a"},
		WantSecrets: []string{"s"}, WantDynamic: []string{"d"},
	}, { // Test 3: Unicode and very long names survive the trip and sort by byte order.
		Ext: &Extension{Notifiers: map[string]sdk.Notifier{
			"ünicode": notifier, "zebra": notifier, strings.Repeat("x", 512): notifier,
		}},
		WantNotifiers: []string{strings.Repeat("x", 512), "zebra", "ünicode"},
	}, { // Test 4: A plugin that provides nothing describes nothing rather than failing.
		Ext: &Extension{},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			desc, err := newServer(test.Ext).Describe(context.Background(), &extproto.DescribeRequest{})
			if err != nil {
				t.Fatalf("test %d: Describe error: %v", testNum, err)
			}
			got := [][]string{
				desc.GetTools(), desc.GetNotifiers(), desc.GetAiProviders(),
				desc.GetSecretSources(), desc.GetDynamicSecretSources(),
			}
			want := [][]string{
				test.WantTools, test.WantNotifiers, test.WantAI,
				test.WantSecrets, test.WantDynamic,
			}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d: Describe mismatch (-want +got):\n%s", testNum, diff)
			}
		})
	}
}

// TestDescribeNeverReturnsNilSlices pins that an absent seam is an empty list rather than a nil one.
// The host logs and iterates these lists directly, so the shape has to be the same whether a plugin
// serves one seam or all five.
func TestDescribeNeverReturnsNilSlices(t *testing.T) {
	t.Parallel()
	desc, err := newServer(&Extension{}).Describe(context.Background(), &extproto.DescribeRequest{})
	if err != nil {
		t.Fatalf("Describe error: %v", err)
	}
	for _, list := range [][]string{
		desc.GetTools(), desc.GetNotifiers(), desc.GetAiProviders(),
		desc.GetSecretSources(), desc.GetDynamicSecretSources(),
	} {
		if list == nil {
			t.Error("Describe returned a nil list, want an empty one")
		}
	}
}

// TestNewServerInstallsLeaseTable pins the constructor's own work rather than a rule it shares. A
// server built without the lease map would take every mint and panic on the first assignment into a
// nil map, and the panic recovery would hide that as a generic Internal error.
func TestNewServerInstallsLeaseTable(t *testing.T) {
	t.Parallel()
	s := newServer(&Extension{})
	if s.leases == nil {
		t.Fatal("newServer left the lease table nil, so the first mint would panic")
	}
	if got := len(s.leases); got != 0 {
		t.Errorf("lease table holds %d entries at construction, want 0", got)
	}
}

// TestRunToolRefusals pins every way one run is refused rather than half executed. A tool the plugin
// never declared must be NotFound so the host can tell a stale registration from a broken tool, and
// extra vars that are not a JSON object must be refused before the runner sees them, since a runner
// handed the wrong shape would act on inputs the host never sent.
func TestRunToolRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Req      *extproto.RunToolRequest
		Runner   sdk.ToolRunner
		WantCode codes.Code
	}{{ // Test 0: A tool the plugin does not provide is refused as NotFound.
		Name:     "unknown tool",
		Req:      &extproto.RunToolRequest{Tool: "absent"},
		Runner:   okRunner,
		WantCode: codes.NotFound,
	}, { // Test 1: An empty tool name matches no entry and is refused, not defaulted.
		Name:     "empty tool name",
		Req:      &extproto.RunToolRequest{},
		Runner:   okRunner,
		WantCode: codes.NotFound,
	}, { // Test 2: Extra vars that are not valid JSON are refused before the runner starts.
		Name:     "malformed extra vars",
		Req:      &extproto.RunToolRequest{Tool: "hello", ExtraVarsJson: []byte("{not json")},
		Runner:   okRunner,
		WantCode: codes.InvalidArgument,
	}, { // Test 3: Extra vars that are valid JSON but not an object are refused.
		Name:     "extra vars are an array",
		Req:      &extproto.RunToolRequest{Tool: "hello", ExtraVarsJson: []byte(`[1,2,3]`)},
		Runner:   okRunner,
		WantCode: codes.InvalidArgument,
	}, { // Test 4: A runner that fails ends the call as Internal with no result on the stream.
		Name: "runner error",
		Req:  &extproto.RunToolRequest{Tool: "hello"},
		Runner: sdk.ToolRunnerFunc(
			func(context.Context, sdk.ToolSpec, io.Writer) (sdk.ToolResult, error) {
				return sdk.ToolResult{ExitCode: 3}, errBoom
			}),
		WantCode: codes.Internal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{Tools: map[string]sdk.ToolRunner{"hello": test.Runner}})
			stream := &recordStream{}
			err := s.RunTool(test.Req, stream)
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if _, ok := stream.result(); ok {
				t.Errorf("test %d (%s): a refused run still sent a result", testNum, test.Name)
			}
		})
	}
}

// TestRunToolCarriesSpec pins the request-to-spec translation. Every field a runner reads comes from
// this one conversion, so a dropped field is a run that quietly executes with the wrong input: a lost
// dry run flag turns a check into a change.
func TestRunToolCarriesSpec(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Req      *extproto.RunToolRequest
		WantSpec sdk.ToolSpec
	}{{ // Test 0: Every field on the request reaches the spec.
		Name: "full request",
		Req: &extproto.RunToolRequest{
			Tool: "hello", Command: "ping", DryRun: true,
			ExtraVarsJson: []byte(`{"n":1,"s":"x","b":true}`),
			Env:           []string{"K=V"}, Dir: "/work",
		},
		WantSpec: sdk.ToolSpec{
			Tool: "hello", Command: "ping", DryRun: true,
			ExtraVars: map[string]any{"n": float64(1), "s": "x", "b": true},
			Env:       []string{"K=V"}, Dir: "/work",
		},
	}, { // Test 1: A run with no extra vars leaves the map nil rather than an empty object.
		Name:     "no extra vars",
		Req:      &extproto.RunToolRequest{Tool: "hello", Command: "c"},
		WantSpec: sdk.ToolSpec{Tool: "hello", Command: "c"},
	}, { // Test 2: An empty JSON object decodes to an empty map, not an error.
		Name:     "empty extra vars object",
		Req:      &extproto.RunToolRequest{Tool: "hello", ExtraVarsJson: []byte(`{}`)},
		WantSpec: sdk.ToolSpec{Tool: "hello", ExtraVars: map[string]any{}},
	}, { // Test 3: JSON null decodes to no extra vars rather than failing the run.
		Name:     "null extra vars",
		Req:      &extproto.RunToolRequest{Tool: "hello", ExtraVarsJson: []byte(`null`)},
		WantSpec: sdk.ToolSpec{Tool: "hello"},
	}, { // Test 4: Unicode and nested values survive the decode intact.
		Name: "unicode and nesting",
		Req: &extproto.RunToolRequest{
			Tool: "hello", Command: "échò",
			ExtraVarsJson: []byte(`{"k":{"inner":["ü",null]}}`),
		},
		WantSpec: sdk.ToolSpec{
			Tool: "hello", Command: "échò",
			ExtraVars: map[string]any{"k": map[string]any{"inner": []any{"ü", nil}}},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got sdk.ToolSpec
			s := newServer(&Extension{Tools: map[string]sdk.ToolRunner{
				"hello": sdk.ToolRunnerFunc(
					func(_ context.Context, spec sdk.ToolSpec, _ io.Writer) (sdk.ToolResult, error) {
						got = spec
						return sdk.ToolResult{}, nil
					}),
			}})
			if err := s.RunTool(test.Req, &recordStream{}); err != nil {
				t.Fatalf("test %d (%s): RunTool error: %v", testNum, test.Name, err)
			}
			if diff := cmp.Diff(test.WantSpec, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("test %d (%s): spec mismatch (-want +got):\n%s", testNum, test.Name, diff)
			}
		})
	}
}

// TestRunToolStreamsOutputThenResult pins the stream's shape: output while the tool runs, then
// exactly one result carrying the exit code. The host reads the exit code from that final message
// alone, so a run that ends without one is reported as a protocol error rather than a success.
func TestRunToolStreamsOutputThenResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Chunks     []string
		ExitCode   int
		WantOutput string
		WantExit   int32
	}{{ // Test 0: A quiet tool still ends with its result.
		Name: "no output", ExitCode: 0, WantOutput: "", WantExit: 0,
	}, { // Test 1: Several writes arrive in order and whole.
		Name: "chunked output", Chunks: []string{"one ", "two ", "three"},
		ExitCode: 0, WantOutput: "one two three", WantExit: 0,
	}, { // Test 2: A failing tool reports its exit code, which is not an error on this call.
		Name: "nonzero exit", ExitCode: 42, WantExit: 42,
	}, { // Test 3: A negative exit code crosses unchanged, since the host maps it, not the plugin.
		Name: "negative exit", ExitCode: -1, WantExit: -1,
	}, { // Test 4: An empty write is still a send, and changes no output.
		Name: "empty write", Chunks: []string{""}, WantOutput: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{Tools: map[string]sdk.ToolRunner{
				"hello": sdk.ToolRunnerFunc(
					func(_ context.Context, _ sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
						for _, c := range test.Chunks {
							n, err := out.Write([]byte(c))
							if err != nil {
								return sdk.ToolResult{}, err
							}
							if n != len(c) {
								return sdk.ToolResult{}, fmt.Errorf("short write %d of %d", n, len(c))
							}
						}
						return sdk.ToolResult{ExitCode: test.ExitCode}, nil
					}),
			}})
			stream := &recordStream{}
			if err := s.RunTool(&extproto.RunToolRequest{Tool: "hello"}, stream); err != nil {
				t.Fatalf("test %d (%s): RunTool error: %v", testNum, test.Name, err)
			}
			if got := stream.output(); got != test.WantOutput {
				t.Errorf("test %d (%s): output = %q, want %q", testNum, test.Name, got, test.WantOutput)
			}
			exit, ok := stream.result()
			if !ok {
				t.Fatalf("test %d (%s): stream ended without a result", testNum, test.Name)
			}
			if exit != test.WantExit {
				t.Errorf("test %d (%s): exit = %d, want %d", testNum, test.Name, exit, test.WantExit)
			}
		})
	}
}

// TestRunToolReportsBrokenStream pins that a host that hangs up mid-run surfaces as an error to the
// runner and to the call, rather than a run that appears to have finished. A swallowed send failure
// would leave the host waiting on a stream the plugin thinks it completed.
func TestRunToolReportsBrokenStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		FailAt       int
		WantWriteErr bool
	}{{ // Test 0: The first output send fails, so the runner's own Write reports it.
		Name: "output send fails", FailAt: 1, WantWriteErr: true,
	}, { // Test 1: The final result send fails, so the call ends with that error.
		Name: "result send fails", FailAt: 2, WantWriteErr: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var writeErr error
			s := newServer(&Extension{Tools: map[string]sdk.ToolRunner{
				"hello": sdk.ToolRunnerFunc(
					func(_ context.Context, _ sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
						n, err := out.Write([]byte("chunk"))
						writeErr = err
						if err != nil {
							if n != 0 {
								return sdk.ToolResult{}, fmt.Errorf("failed write reported %d bytes", n)
							}
							return sdk.ToolResult{}, err
						}
						return sdk.ToolResult{}, nil
					}),
			}})
			stream := &recordStream{failAt: test.FailAt}
			err := s.RunTool(&extproto.RunToolRequest{Tool: "hello"}, stream)
			if err == nil {
				t.Fatalf("test %d (%s): RunTool succeeded on a broken stream", testNum, test.Name)
			}
			if test.WantWriteErr && !errors.Is(writeErr, errBoom) {
				t.Errorf("test %d (%s): runner saw write error %v, want the send failure",
					testNum, test.Name, writeErr)
			}
		})
	}
}

// TestStreamWriterSerializesConcurrentWrites pins the promise in streamWriter's own comment: a runner
// may write from several goroutines, and a gRPC stream allows one sender at a time. Run under -race,
// an unsynchronized writer trips the detector or interleaves a send, which corrupts a run's log.
func TestStreamWriterSerializesConcurrentWrites(t *testing.T) {
	t.Parallel()
	stream := &recordStream{}
	w := &streamWriter{stream: stream}
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range each {
				if _, err := w.Write([]byte{byte('a' + i)}); err != nil {
					t.Errorf("Write error: %v", err)
					return
				}
			}
		}(i)
	}
	// A result may race with output in a real run too, so send one from a competing goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := w.sendResult(0); err != nil {
			t.Errorf("sendResult error: %v", err)
		}
	}()
	wg.Wait()
	if got := len(stream.output()); got != writers*each {
		t.Errorf("streamed %d output bytes, want %d", got, writers*each)
	}
	if _, ok := stream.result(); !ok {
		t.Error("the result send was lost")
	}
}

// TestNotifyRefusalsAndDelivery pins the notification seam. A channel the plugin never declared must
// be NotFound rather than silently dropped, and a run body that will not decode must be refused
// rather than delivered as a zero-valued run, which would page an operator about run "" succeeding.
func TestNotifyRefusalsAndDelivery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Channel    string
		RunJSON    []byte
		NotifyErr  error
		WantCode   codes.Code
		WantRunID  string
		WantCalled bool
	}{{ // Test 0: A declared channel with a decodable run delivers it.
		Name: "delivered", Channel: "chan", RunJSON: []byte(`{"id":"run-1"}`),
		WantCode: codes.OK, WantRunID: "run-1", WantCalled: true,
	}, { // Test 1: An undeclared channel is NotFound and the notifier never runs.
		Name: "unknown channel", Channel: "absent", RunJSON: []byte(`{"id":"run-1"}`),
		WantCode: codes.NotFound,
	}, { // Test 2: An empty channel name matches nothing and is refused.
		Name: "empty channel", RunJSON: []byte(`{"id":"run-1"}`), WantCode: codes.NotFound,
	}, { // Test 3: A missing run body is refused rather than delivered as an empty run.
		Name: "nil run json", Channel: "chan", WantCode: codes.InvalidArgument,
	}, { // Test 4: A malformed run body is refused.
		Name: "malformed run json", Channel: "chan", RunJSON: []byte(`{"id":`),
		WantCode: codes.InvalidArgument,
	}, { // Test 5: A newer host's unknown fields are ignored, so an old plugin keeps working.
		Name:    "unknown fields tolerated",
		Channel: "chan", RunJSON: []byte(`{"id":"run-2","field_from_the_future":{"a":1}}`),
		WantCode: codes.OK, WantRunID: "run-2", WantCalled: true,
	}, { // Test 6: A notifier that fails ends the call as Internal.
		Name: "notifier error", Channel: "chan", RunJSON: []byte(`{"id":"run-3"}`),
		NotifyErr: errBoom, WantCode: codes.Internal, WantRunID: "run-3", WantCalled: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var gotID string
			called := false
			s := newServer(&Extension{Notifiers: map[string]sdk.Notifier{
				"chan": sdk.NotifierFunc(func(_ context.Context, r *sdk.Run) error {
					called, gotID = true, r.ID
					return test.NotifyErr
				}),
			}})
			_, err := s.Notify(context.Background(), &extproto.NotifyRequest{
				Channel: test.Channel, RunJson: test.RunJSON,
			})
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if called != test.WantCalled {
				t.Errorf("test %d (%s): notifier called = %v, want %v",
					testNum, test.Name, called, test.WantCalled)
			}
			if gotID != test.WantRunID {
				t.Errorf("test %d (%s): run id = %q, want %q", testNum, test.Name, gotID, test.WantRunID)
			}
		})
	}
}

// TestNotifyRoundTripsTheRun pins that the run a notifier receives is the run the host sent, field for
// field. A notifier reports a run's outcome to people, so a status or exit code lost in the decode
// tells a team the wrong thing about their fleet.
func TestNotifyRoundTripsTheRun(t *testing.T) {
	t.Parallel()
	exit := 7
	sent := sdk.Run{
		ID: "run-9", Tool: "bash", Command: "echo hi", Status: "succeeded", ExitCode: &exit,
		Labels:    map[string]string{"env": "prod"},
		ExtraVars: map[string]any{"who": "ünicode"},
	}
	body, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	var got sdk.Run
	s := newServer(&Extension{Notifiers: map[string]sdk.Notifier{
		"chan": sdk.NotifierFunc(func(_ context.Context, r *sdk.Run) error {
			got = *r
			return nil
		}),
	}})
	if _, err := s.Notify(context.Background(), &extproto.NotifyRequest{
		Channel: "chan", RunJson: body,
	}); err != nil {
		t.Fatalf("Notify error: %v", err)
	}
	if diff := cmp.Diff(sent, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("run round trip mismatch (-sent +received):\n%s", diff)
	}
}

// TestNotifyNeverPopulatesTheClaimSecret pins that a plugin's notifier cannot be handed a run's
// per-claim capability. That secret authorizes a worker's reports back to the server and is kept out
// of every serialized run by its json tag, so a decode that filled it from a JSON field would hand a
// third-party binary the one value a worker cannot otherwise read back.
func TestNotifyNeverPopulatesTheClaimSecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		RunJSON []byte
	}{ // Test 0: The Go field name in the body is ignored.
		{Name: "go field name", RunJSON: []byte(`{"id":"r","ClaimSecret":"stolen"}`)},
		// Test 1: The snake case name a caller might guess is ignored.
		{Name: "snake case name", RunJSON: []byte(`{"id":"r","claim_secret":"stolen"}`)},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var got sdk.Run
			s := newServer(&Extension{Notifiers: map[string]sdk.Notifier{
				"chan": sdk.NotifierFunc(func(_ context.Context, r *sdk.Run) error {
					got = *r
					return nil
				}),
			}})
			if _, err := s.Notify(context.Background(), &extproto.NotifyRequest{
				Channel: "chan", RunJson: test.RunJSON,
			}); err != nil {
				t.Fatalf("test %d (%s): Notify error: %v", testNum, test.Name, err)
			}
			if got.ClaimSecret != "" {
				t.Errorf("test %d (%s): the notifier received claim secret %q",
					testNum, test.Name, got.ClaimSecret)
			}
		})
	}
}

// TestCompleteRefusalsAndSettings pins the AI seam. The host's model, endpoint, and API key are built
// into a provider on every call, so a dropped setting sends a prompt to the wrong backend, and a
// factory that rejects its settings must fail the call rather than complete against a default.
func TestCompleteRefusalsAndSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Provider    string
		FactoryErr  error
		CompleteErr error
		WantCode    codes.Code
		WantText    string
	}{{ // Test 0: A declared provider completes and returns its text.
		Name: "completed", Provider: "ai", WantCode: codes.OK,
		WantText: "model=m url=u key=k system=s user=u2",
	}, { // Test 1: An undeclared provider is NotFound.
		Name: "unknown provider", Provider: "absent", WantCode: codes.NotFound,
	}, { // Test 2: An empty provider name matches nothing.
		Name: "empty provider", WantCode: codes.NotFound,
	}, { // Test 3: A factory that rejects its settings fails the call as InvalidArgument.
		Name: "factory error", Provider: "ai", FactoryErr: errBoom, WantCode: codes.InvalidArgument,
	}, { // Test 4: A provider that fails mid-completion ends the call as Internal.
		Name: "completion error", Provider: "ai", CompleteErr: errBoom, WantCode: codes.Internal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{AIProviders: map[string]sdk.AIProviderFactory{
				"ai": func(model, url, apiKey string) (sdk.AIProvider, error) {
					if test.FactoryErr != nil {
						return nil, test.FactoryErr
					}
					return sdk.AIProviderFunc(
						func(_ context.Context, system, user string) (string, error) {
							if test.CompleteErr != nil {
								return "", test.CompleteErr
							}
							return fmt.Sprintf("model=%s url=%s key=%s system=%s user=%s",
								model, url, apiKey, system, user), nil
						}), nil
				},
			}})
			resp, err := s.Complete(context.Background(), &extproto.CompleteRequest{
				Provider: test.Provider, Model: "m", Url: "u", ApiKey: "k", System: "s", User: "u2",
			})
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if got := resp.GetText(); got != test.WantText {
				t.Errorf("test %d (%s): text = %q, want %q", testNum, test.Name, got, test.WantText)
			}
		})
	}
}

// TestCompleteErrorsWithholdTheAPIKey pins that a failing AI seam names the provider and nothing else
// from the request. The host hands the plugin its configured API key on every completion, and a
// failure message travels back over the wire and into the host's logs, so the key must not ride along.
func TestCompleteErrorsWithholdTheAPIKey(t *testing.T) {
	t.Parallel()
	const key = "sk-live-must-not-appear"
	tests := []struct {
		Name    string
		Factory sdk.AIProviderFactory
	}{{ // Test 0: A factory failure names the provider, not the settings it was handed.
		Name: "factory failure",
		Factory: func(_, _, _ string) (sdk.AIProvider, error) {
			return nil, errBoom
		},
	}, { // Test 1: A completion failure is reported without the key the provider was built with.
		Name: "completion failure",
		Factory: func(_, _, _ string) (sdk.AIProvider, error) {
			return sdk.AIProviderFunc(
				func(context.Context, string, string) (string, error) { return "", errBoom }), nil
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{AIProviders: map[string]sdk.AIProviderFactory{"ai": test.Factory}})
			_, err := s.Complete(context.Background(), &extproto.CompleteRequest{
				Provider: "ai", Model: "m", Url: "u", ApiKey: key, System: "s", User: "u",
			})
			if err == nil {
				t.Fatalf("test %d (%s): Complete succeeded, want an error", testNum, test.Name)
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("test %d (%s): the API key reached the error message: %v",
					testNum, test.Name, err)
			}
		})
	}
}

// TestResolveSecretRefusals pins the static secret seam. An undeclared kind must be NotFound so the
// host reports a missing engine rather than an empty secret, and a resolver failure must fail the
// call: a resolve that returned an empty value with no error would inject an empty credential into a
// run and let it proceed as if the secret were fetched.
func TestResolveSecretRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Kind       string
		Config     string
		Value      string
		Err        error
		WantCode   codes.Code
		WantValue  string
		WantConfig string
	}{{ // Test 0: A declared kind resolves and returns its value.
		Name: "resolved", Kind: "vault", Config: "path/a", Value: "s3cret",
		WantCode: codes.OK, WantValue: "s3cret", WantConfig: "path/a",
	}, { // Test 1: An undeclared kind is NotFound.
		Name: "unknown kind", Kind: "absent", WantCode: codes.NotFound,
	}, { // Test 2: An empty kind matches nothing.
		Name: "empty kind", WantCode: codes.NotFound,
	}, { // Test 3: A resolver failure fails the call rather than returning an empty value.
		Name: "resolver error", Kind: "vault", Err: errBoom, WantCode: codes.Internal,
	}, { // Test 4: An empty config is passed through, since its meaning belongs to the plugin.
		Name: "empty config", Kind: "vault", Value: "v", WantCode: codes.OK, WantValue: "v",
	}, { // Test 5: A resolved empty value is a success, not a silent failure.
		Name: "empty value", Kind: "vault", Config: "c", WantCode: codes.OK, WantConfig: "c",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var gotConfig string
			s := newServer(&Extension{SecretSources: map[string]sdk.SecretResolver{
				"vault": func(_ context.Context, config string) (string, error) {
					gotConfig = config
					return test.Value, test.Err
				},
			}})
			resp, err := s.ResolveSecret(context.Background(), &extproto.ResolveSecretRequest{
				Kind: test.Kind, Config: test.Config,
			})
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if got := resp.GetValue(); got != test.WantValue {
				t.Errorf("test %d (%s): value = %q, want %q", testNum, test.Name, got, test.WantValue)
			}
			if gotConfig != test.WantConfig {
				t.Errorf("test %d (%s): config = %q, want %q",
					testNum, test.Name, gotConfig, test.WantConfig)
			}
		})
	}
}

// TestSecretErrorsWithholdTheValue pins that a secret seam which fails after producing a value does
// not put that value in the error. A plugin can return both a value and an error, and the error
// travels back to the host and into its logs, so the message must carry the kind and nothing more.
func TestSecretErrorsWithholdTheValue(t *testing.T) {
	t.Parallel()
	const value = "top-secret-value"
	tests := []struct {
		Name string
		Call func(*server) error
	}{{ // Test 0: A static resolver that fails with a value in hand keeps it off the wire.
		Name: "resolve",
		Call: func(s *server) error {
			_, err := s.ResolveSecret(context.Background(),
				&extproto.ResolveSecretRequest{Kind: "vault", Config: "c"})
			return err
		},
	}, { // Test 1: A dynamic minter that fails with a value in hand keeps it off the wire.
		Name: "mint",
		Call: func(s *server) error {
			_, err := s.MintSecret(context.Background(),
				&extproto.MintSecretRequest{Kind: "sts", Config: "c"})
			return err
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{
				SecretSources: map[string]sdk.SecretResolver{
					"vault": func(context.Context, string) (string, error) { return value, errBoom },
				},
				DynamicSecretSources: map[string]sdk.SecretMinter{
					"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
						return value, nil, errBoom
					},
				},
			})
			err := test.Call(s)
			if err == nil {
				t.Fatalf("test %d (%s): call succeeded, want an error", testNum, test.Name)
			}
			if strings.Contains(err.Error(), value) {
				t.Errorf("test %d (%s): the secret value reached the error message: %v",
					testNum, test.Name, err)
			}
		})
	}
}

// TestMintSecretLeases pins how a minted secret is parked. The host revokes by the id this call
// returns, so an id that is not recorded is a live credential nobody can end early, and an empty id
// has to mean the secret expires on its own rather than naming a lease the host cannot revoke.
func TestMintSecretLeases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name        string
		Kind        string
		Lease       *sdk.SecretLease
		Err         error
		WantCode    codes.Code
		WantValue   string
		WantLeaseID string
		WantParked  int
	}{{ // Test 0: A mint with a lease returns an id and parks the lease for revocation.
		Name: "leased", Kind: "sts", Lease: sdk.NewSecretLease("sts", nil),
		WantCode: codes.OK, WantValue: "minted", WantLeaseID: "lease-1", WantParked: 1,
	}, { // Test 1: A mint with no lease returns an empty id and parks nothing.
		Name: "no lease", Kind: "sts", WantCode: codes.OK, WantValue: "minted", WantParked: 0,
	}, { // Test 2: An undeclared kind is NotFound.
		Name: "unknown kind", Kind: "absent", WantCode: codes.NotFound,
	}, { // Test 3: An empty kind matches nothing.
		Name: "empty kind", WantCode: codes.NotFound,
	}, { // Test 4: A minter failure fails the call and parks nothing.
		Name: "minter error", Kind: "sts", Lease: sdk.NewSecretLease("sts", nil), Err: errBoom,
		WantCode: codes.Internal,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
				"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
					return "minted", test.Lease, test.Err
				},
			}})
			resp, err := s.MintSecret(context.Background(),
				&extproto.MintSecretRequest{Kind: test.Kind, Config: "cfg"})
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if got := resp.GetValue(); got != test.WantValue {
				t.Errorf("test %d (%s): value = %q, want %q", testNum, test.Name, got, test.WantValue)
			}
			if got := resp.GetLeaseId(); got != test.WantLeaseID {
				t.Errorf("test %d (%s): lease id = %q, want %q",
					testNum, test.Name, got, test.WantLeaseID)
			}
			s.leaseMu.Lock()
			parked := len(s.leases)
			s.leaseMu.Unlock()
			if parked != test.WantParked {
				t.Errorf("test %d (%s): parked leases = %d, want %d",
					testNum, test.Name, parked, test.WantParked)
			}
		})
	}
}

// TestMintSecretIDsAreDistinct pins that two mints from one process never share a lease id. A reused
// id lets one run's revocation end another run's credential, or leave a live one behind.
func TestMintSecretIDsAreDistinct(t *testing.T) {
	t.Parallel()
	s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
		"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
			return "v", sdk.NewSecretLease("sts", nil), nil
		},
	}})
	seen := map[string]bool{}
	for i := range 3 {
		resp, err := s.MintSecret(context.Background(), &extproto.MintSecretRequest{Kind: "sts"})
		if err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
		if seen[resp.GetLeaseId()] {
			t.Fatalf("mint %d reused lease id %q", i, resp.GetLeaseId())
		}
		seen[resp.GetLeaseId()] = true
	}
	if diff := cmp.Diff([]string{"lease-1", "lease-2", "lease-3"}, sortedKeys(seen)); diff != "" {
		t.Errorf("lease ids mismatch (-want +got):\n%s", diff)
	}
}

// TestRevokeLease pins the end of a minted secret's life. An unknown id must be NotFound rather than
// a quiet success, since a host told the revocation worked would stop trying while the credential
// stays live, and a lease must revoke once so a replayed id cannot end a later run's secret.
func TestRevokeLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Mint       bool
		RevokeErr  error
		ID         string
		WantCode   codes.Code
		WantCalled int
	}{{ // Test 0: The id a mint returned revokes the lease exactly once.
		Name: "revoked", Mint: true, ID: "lease-1", WantCode: codes.OK, WantCalled: 1,
	}, { // Test 1: An id that was never minted is NotFound.
		Name: "unknown id", Mint: true, ID: "lease-99", WantCode: codes.NotFound,
	}, { // Test 2: An empty id is NotFound, which is what a mint with no lease returns.
		Name: "empty id", Mint: true, WantCode: codes.NotFound,
	}, { // Test 3: A revoke that fails is reported as Internal.
		Name: "revoke error", Mint: true, ID: "lease-1", RevokeErr: errBoom,
		WantCode: codes.Internal, WantCalled: 1,
	}, { // Test 4: Revoking on a process that minted nothing is NotFound.
		Name: "nothing minted", ID: "lease-1", WantCode: codes.NotFound,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			calls := 0
			s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
				"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
					lease := sdk.NewSecretLease("sts", func(context.Context) error {
						calls++
						return test.RevokeErr
					})
					return "v", lease, nil
				},
			}})
			if test.Mint {
				if _, err := s.MintSecret(context.Background(),
					&extproto.MintSecretRequest{Kind: "sts"}); err != nil {
					t.Fatalf("test %d (%s): mint: %v", testNum, test.Name, err)
				}
			}
			_, err := s.RevokeLease(context.Background(),
				&extproto.RevokeLeaseRequest{LeaseId: test.ID})
			if got := status.Code(err); got != test.WantCode {
				t.Fatalf("test %d (%s): code = %v, want %v (err %v)",
					testNum, test.Name, got, test.WantCode, err)
			}
			if calls != test.WantCalled {
				t.Errorf("test %d (%s): revoke called %d times, want %d",
					testNum, test.Name, calls, test.WantCalled)
			}
		})
	}
}

// TestRevokeLeaseIsOnce pins that a second revocation of the same id is refused. The lease is
// forgotten on the first call, so a replayed request cannot reach a revoke func again.
func TestRevokeLeaseIsOnce(t *testing.T) {
	t.Parallel()
	s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
		"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
			return "v", sdk.NewSecretLease("sts", func(context.Context) error { return nil }), nil
		},
	}})
	resp, err := s.MintSecret(context.Background(), &extproto.MintSecretRequest{Kind: "sts"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := &extproto.RevokeLeaseRequest{LeaseId: resp.GetLeaseId()}
	if _, err := s.RevokeLease(context.Background(), req); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	if _, err := s.RevokeLease(context.Background(), req); status.Code(err) != codes.NotFound {
		t.Errorf("second revoke = %v, want NotFound", err)
	}
}

// TestRevokeLeaseAfterFailureIsForgotten records what a failed revocation leaves behind. The lease is
// removed before the revoke func runs, so a transient failure ends the host's ability to try again
// and the minted credential lives to its own TTL. This is the current contract, and it is worth
// pinning because a change to it changes what an operator can do about a stuck credential.
func TestRevokeLeaseAfterFailureIsForgotten(t *testing.T) {
	t.Parallel()
	calls := 0
	s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
		"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
			return "v", sdk.NewSecretLease("sts", func(context.Context) error {
				calls++
				return errBoom
			}), nil
		},
	}})
	resp, err := s.MintSecret(context.Background(), &extproto.MintSecretRequest{Kind: "sts"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	req := &extproto.RevokeLeaseRequest{LeaseId: resp.GetLeaseId()}
	if _, err := s.RevokeLease(context.Background(), req); status.Code(err) != codes.Internal {
		t.Fatalf("first revoke = %v, want Internal", err)
	}
	if _, err := s.RevokeLease(context.Background(), req); status.Code(err) != codes.NotFound {
		t.Errorf("retry after a failed revoke = %v, want NotFound", err)
	}
	if calls != 1 {
		t.Errorf("revoke func ran %d times, want 1: a retry never reaches it", calls)
	}
}

// TestLeaseTableConcurrentMintAndRevoke pins the lease table under the load it actually sees: several
// runs minting and revoking at once. Under -race an unguarded map trips the detector, and a lost or
// duplicated id means a live credential with no way to end it.
func TestLeaseTableConcurrentMintAndRevoke(t *testing.T) {
	t.Parallel()
	var revoked sync.Map
	s := newServer(&Extension{DynamicSecretSources: map[string]sdk.SecretMinter{
		"sts": func(context.Context, string) (string, *sdk.SecretLease, error) {
			return "v", sdk.NewSecretLease("sts", func(context.Context) error { return nil }), nil
		},
	}})
	const workers = 32
	ids := make(chan string, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := s.MintSecret(context.Background(), &extproto.MintSecretRequest{Kind: "sts"})
			if err != nil {
				t.Errorf("mint: %v", err)
				return
			}
			ids <- resp.GetLeaseId()
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("lease id %q was handed out twice", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("minted %d distinct leases, want %d", len(seen), workers)
	}
	for id := range seen {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := s.RevokeLease(context.Background(),
				&extproto.RevokeLeaseRequest{LeaseId: id}); err != nil {
				t.Errorf("revoke %s: %v", id, err)
				return
			}
			revoked.Store(id, true)
		}(id)
	}
	wg.Wait()
	count := 0
	revoked.Range(func(any, any) bool { count++; return true })
	if count != workers {
		t.Errorf("revoked %d leases, want %d", count, workers)
	}
	s.leaseMu.Lock()
	left := len(s.leases)
	s.leaseMu.Unlock()
	if left != 0 {
		t.Errorf("%d leases left parked after every revoke, want 0", left)
	}
}
