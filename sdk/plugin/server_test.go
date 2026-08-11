package plugin

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRecoverUnaryContainsPanics checks a panic in a unary callback becomes an Internal error rather
// than crashing the plugin process, and that a clean call passes through untouched. The test itself
// completing is half the proof: an uncontained panic would take the test binary down with it.
func TestRecoverUnaryContainsPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Handler  grpc.UnaryHandler
		WantCode codes.Code
		WantResp any
		WantErr  bool
	}{ // Test 0: A panicking handler is contained and reported as Internal.
		{
			Name:     "panic contained",
			Handler:  func(context.Context, any) (any, error) { panic("boom with secret-ish text") },
			WantCode: codes.Internal, WantErr: true,
		},
		// Test 1: A clean handler passes its result straight through.
		{
			Name:     "clean passthrough",
			Handler:  func(context.Context, any) (any, error) { return "ok", nil },
			WantResp: "ok", WantErr: false,
		},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/plugin.Extension/RunTool"}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			resp, err := recoverUnary(context.Background(), nil, info, test.Handler)
			if test.WantErr {
				if status.Code(err) != test.WantCode {
					t.Errorf("test %d: code = %v, want %v", testNum, status.Code(err), test.WantCode)
				}
				if resp != nil {
					t.Errorf("test %d: resp = %v, want nil on panic", testNum, resp)
				}
				// The panic value must never reach the wire; only the method name may.
				if err != nil && status.Convert(err).Message() != "plugin callback /plugin.Extension/RunTool panicked" {
					t.Errorf("test %d: message = %q, must not echo the panic value",
						testNum, status.Convert(err).Message())
				}
				return
			}
			if err != nil {
				t.Errorf("test %d: err = %v, want nil", testNum, err)
			}
			if resp != test.WantResp {
				t.Errorf("test %d: resp = %v, want %v", testNum, resp, test.WantResp)
			}
		})
	}
}

// panicStream is a minimal grpc.ServerStream; the handlers under test never touch it.
type panicStream struct{ grpc.ServerStream }

func (panicStream) Context() context.Context { return context.Background() }

// TestRecoverStreamContainsPanics checks a panic in the streaming callback path is contained as an
// Internal error and a clean stream handler returns nil.
func TestRecoverStreamContainsPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Handler  grpc.StreamHandler
		WantCode codes.Code
		WantErr  bool
	}{ // Test 0: A panicking stream handler is contained.
		{
			Name:     "panic contained",
			Handler:  func(any, grpc.ServerStream) error { panic("stream boom") },
			WantCode: codes.Internal, WantErr: true,
		},
		// Test 1: A clean stream handler returns nil.
		{
			Name:    "clean passthrough",
			Handler: func(any, grpc.ServerStream) error { return nil },
			WantErr: false,
		},
	}
	info := &grpc.StreamServerInfo{FullMethod: "/plugin.Extension/RunTool"}
	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			err := recoverStream(nil, panicStream{}, info, test.Handler)
			switch {
			case test.WantErr && status.Code(err) != test.WantCode:
				t.Errorf("test %d: code = %v, want %v", testNum, status.Code(err), test.WantCode)
			case !test.WantErr && err != nil:
				t.Errorf("test %d: err = %v, want nil", testNum, err)
			}
		})
	}
}
