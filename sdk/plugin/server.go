package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dcadolph/yardmaster/internal/extproto"
	"github.com/dcadolph/yardmaster/sdk"
)

// server serves one Extension over the wire protocol inside the plugin process.
type server struct {
	extproto.UnimplementedExtensionServer
	// ext is the extension being served.
	ext *Extension
	// leaseSeq numbers minted leases within this process.
	leaseSeq atomic.Uint64
	// leaseMu guards leases.
	leaseMu sync.Mutex
	// leases holds live minted leases by id until the host revokes them.
	leases map[string]*sdk.SecretLease
}

// newServer returns a server for the extension.
func newServer(ext *Extension) *server {
	return &server{ext: ext, leases: map[string]*sdk.SecretLease{}}
}

// sortedKeys returns the map's keys, sorted, so Describe output is stable.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Describe lists everything the extension provides.
func (s *server) Describe(context.Context, *extproto.DescribeRequest) (*extproto.DescribeResponse, error) {
	return &extproto.DescribeResponse{
		Tools:                sortedKeys(s.ext.Tools),
		Notifiers:            sortedKeys(s.ext.Notifiers),
		AiProviders:          sortedKeys(s.ext.AIProviders),
		SecretSources:        sortedKeys(s.ext.SecretSources),
		DynamicSecretSources: sortedKeys(s.ext.DynamicSecretSources),
	}, nil
}

// streamWriter turns a run's reply stream into the io.Writer a tool runner writes output to. The
// mutex serializes sends, since a runner may write from more than one goroutine and a gRPC stream
// allows one sender at a time.
type streamWriter struct {
	// mu serializes sends on the stream.
	mu sync.Mutex
	// stream is the run's reply stream.
	stream extproto.Extension_RunToolServer
}

// Write sends one chunk of tool output to the host.
func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.stream.Send(&extproto.RunToolReply{
		Reply: &extproto.RunToolReply_Output{Output: p},
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// sendResult ends the stream with the run's exit code, serialized behind the same mutex as output.
func (w *streamWriter) sendResult(exitCode int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stream.Send(&extproto.RunToolReply{
		Reply: &extproto.RunToolReply_Result{
			Result: &extproto.ToolResult{ExitCode: int32(exitCode)},
		},
	})
}

// RunTool executes one run of a declared tool, streaming its output and ending with its result.
func (s *server) RunTool(req *extproto.RunToolRequest, stream extproto.Extension_RunToolServer) error {
	runner, ok := s.ext.Tools[req.GetTool()]
	if !ok {
		return status.Errorf(codes.NotFound, "plugin does not provide tool %q", req.GetTool())
	}
	var extraVars map[string]any
	if raw := req.GetExtraVarsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &extraVars); err != nil {
			return status.Errorf(codes.InvalidArgument, "decode extra vars: %v", err)
		}
	}
	spec := sdk.ToolSpec{
		Tool:      req.GetTool(),
		Command:   req.GetCommand(),
		DryRun:    req.GetDryRun(),
		ExtraVars: extraVars,
		Env:       req.GetEnv(),
		Dir:       req.GetDir(),
	}
	out := &streamWriter{stream: stream}
	res, err := runner.Run(stream.Context(), spec, out)
	if err != nil {
		return status.Errorf(codes.Internal, "tool %s: %v", req.GetTool(), err)
	}
	return out.sendResult(res.ExitCode)
}

// Notify delivers a terminal run to a declared notification channel.
func (s *server) Notify(ctx context.Context, req *extproto.NotifyRequest) (*extproto.NotifyResponse, error) {
	n, ok := s.ext.Notifiers[req.GetChannel()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin does not provide notifier %q", req.GetChannel())
	}
	var r sdk.Run
	if err := json.Unmarshal(req.GetRunJson(), &r); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode run: %v", err)
	}
	if err := n.Notify(ctx, &r); err != nil {
		return nil, status.Errorf(codes.Internal, "notifier %s: %v", req.GetChannel(), err)
	}
	return &extproto.NotifyResponse{}, nil
}

// Complete returns a declared AI provider's reply, building the provider from the host's settings
// on each call.
func (s *server) Complete(ctx context.Context, req *extproto.CompleteRequest) (*extproto.CompleteResponse, error) {
	factory, ok := s.ext.AIProviders[req.GetProvider()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin does not provide AI provider %q", req.GetProvider())
	}
	provider, err := factory(req.GetModel(), req.GetUrl(), req.GetApiKey())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "provider %s: %v", req.GetProvider(), err)
	}
	text, err := provider.Complete(ctx, req.GetSystem(), req.GetUser())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider %s: %v", req.GetProvider(), err)
	}
	return &extproto.CompleteResponse{Text: text}, nil
}

// ResolveSecret fetches a value from a declared static secret source.
func (s *server) ResolveSecret(ctx context.Context, req *extproto.ResolveSecretRequest) (*extproto.ResolveSecretResponse, error) {
	resolver, ok := s.ext.SecretSources[req.GetKind()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin does not provide secret source %q", req.GetKind())
	}
	value, err := resolver(ctx, req.GetConfig())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "secret source %s: %v", req.GetKind(), err)
	}
	return &extproto.ResolveSecretResponse{Value: value}, nil
}

// MintSecret mints a short-lived value from a declared dynamic secret source and parks its lease
// until the host revokes it.
func (s *server) MintSecret(ctx context.Context, req *extproto.MintSecretRequest) (*extproto.MintSecretResponse, error) {
	minter, ok := s.ext.DynamicSecretSources[req.GetKind()]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "plugin does not provide dynamic secret source %q", req.GetKind())
	}
	value, lease, err := minter(ctx, req.GetConfig())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "dynamic secret source %s: %v", req.GetKind(), err)
	}
	id := ""
	if lease != nil {
		id = "lease-" + strconv.FormatUint(s.leaseSeq.Add(1), 10)
		s.leaseMu.Lock()
		s.leases[id] = lease
		s.leaseMu.Unlock()
	}
	return &extproto.MintSecretResponse{Value: value, LeaseId: id}, nil
}

// RevokeLease ends a minted secret early and forgets its lease.
func (s *server) RevokeLease(ctx context.Context, req *extproto.RevokeLeaseRequest) (*extproto.RevokeLeaseResponse, error) {
	s.leaseMu.Lock()
	lease, ok := s.leases[req.GetLeaseId()]
	delete(s.leases, req.GetLeaseId())
	s.leaseMu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, fmt.Sprintf("unknown lease %q", req.GetLeaseId()))
	}
	if err := lease.Revoke(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke lease %s: %v", req.GetLeaseId(), err)
	}
	return &extproto.RevokeLeaseResponse{}, nil
}
