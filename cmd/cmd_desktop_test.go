package cmd

import (
	"net"
	"strconv"
	"testing"
)

func TestFreeLoopbackPort(t *testing.T) {
	t.Parallel()
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("freeLoopbackPort() error = %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port = %d, want a valid TCP port", port)
	}
	// The port must be free for the server to claim after the helper returns it.
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("port %d is not bindable: %v", port, err)
	}
	_ = l.Close()
}
