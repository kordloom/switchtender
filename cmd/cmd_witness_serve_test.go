package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// writeAttestation signs an attestation with a fresh identity and writes it, returning the path
// and the signer's key.
func writeAttestation(t *testing.T, mutate func(*witness.Attestation)) (path, signer string) {
	t.Helper()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	a := &witness.Attestation{
		Server: "https://st.example", MintedAt: time.Now().UTC(),
		CheckpointAt: time.Now().UTC(), LastBeat: 7, LastSeq: 70, LastHead: "aa",
		BeatsRemembered: 7, FindingsTotal: 2,
	}
	if err := witness.SignAttestation(a, id); err != nil {
		t.Fatalf("SignAttestation() error = %v", err)
	}
	if mutate != nil {
		mutate(a)
	}
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	path = filepath.Join(t.TempDir(), "attestation.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path, id.PublicKeyHex()
}

func TestWitnessVerifyAttestation(t *testing.T) {
	good, signer := writeAttestation(t, nil)
	tampered, _ := writeAttestation(t, func(a *witness.Attestation) { a.FindingsTotal = 0 })

	tests := []struct {
		// Name says what the case proves.
		Name string
		// Path is the attestation argument.
		Path string
		// Pin is the --pubkey value.
		Pin string
		// WantErr is whether verification must fail.
		WantErr bool
	}{{ // Test 0: A signed attestation with the right pin verifies.
		Name: "right pin", Path: good, Pin: signer,
	}, { // Test 1: Without a pin the document is only checked for internal consistency.
		Name: "no pin", Path: good,
	}, { // Test 2: The pin is the trust decision; a different key is refused even when the
		// signature itself is sound.
		Name: "wrong pin", Path: good, Pin: "deadbeef", WantErr: true,
	}, { // Test 3: An altered field breaks the signature.
		Name: "tampered", Path: tampered, WantErr: true,
	}, { // Test 4: A missing file is an error, not a pass.
		Name: "missing", Path: filepath.Join(t.TempDir(), "nope.json"), WantErr: true,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessVerifyPubkey = test.Pin
			defer func() { witnessVerifyPubkey = "" }()
			err := runWitnessVerify(testCommand(), []string{test.Path})
			if (err != nil) != test.WantErr {
				t.Errorf("runWitnessVerify() error = %v, want error %v", err, test.WantErr)
			}
		})
	}
}

func TestWitnessServeRefusesBadFlags(t *testing.T) {
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Watch is the --watch list.
		Watch []string
		// Interval is the --interval value.
		Interval time.Duration
	}{{ // Test 0: No servers to watch is a misconfiguration, not an idle witness.
		Name: "no watch", Interval: time.Minute,
	}, { // Test 1: A watch target must be a web URL.
		Name: "bad scheme", Watch: []string{"ftp://st.example"}, Interval: time.Minute,
	}, { // Test 2: An interval under the floor would hammer the watched servers.
		Name: "short interval", Watch: []string{"https://st.example"}, Interval: time.Second,
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			witnessWatch, witnessServeInterval = test.Watch, test.Interval
			defer func() { witnessWatch, witnessServeInterval = nil, time.Minute }()
			if err := runWitnessServe(testCommand(), nil); err == nil {
				t.Error("runWitnessServe() = nil error, want the misconfiguration refused")
			}
		})
	}
}

func TestPostWitnessFindingTreatsRefusalAsFailure(t *testing.T) {
	t.Parallel()
	var status int32 = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&status)))
	}))
	defer srv.Close()
	f := witness.Finding{Kind: "head_regression", Detail: "d"}
	if err := postWitnessFinding(context.Background(), srv.Client(), srv.URL, "s", f); err == nil {
		t.Error("postWitnessFinding() = nil error on a 500; a refused delivery did not happen")
	}
	atomic.StoreInt32(&status, 204)
	if err := postWitnessFinding(context.Background(), srv.Client(), srv.URL, "s", f); err != nil {
		t.Errorf("postWitnessFinding() error = %v on a 204", err)
	}
}

func TestDeliverFindingsRetriesUntilTheChannelTakesThem(t *testing.T) {
	var accepted []string
	var refuse atomic.Bool
	refuse.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var body struct {
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		accepted = append(accepted, body.Kind)
	}))
	defer srv.Close()
	witnessWebhook = srv.URL
	defer func() { witnessWebhook = "" }()

	queue := []witness.Finding{{Kind: "a"}, {Kind: "b"}}
	// The channel is down: everything stays queued instead of being dropped.
	queue = deliverFindings(context.Background(), srv.Client(), "s", queue)
	if len(queue) != 2 {
		t.Fatalf("undelivered = %d, want both retained while the webhook refuses", len(queue))
	}
	// The channel recovers: the queue drains in order.
	refuse.Store(false)
	queue = deliverFindings(context.Background(), srv.Client(), "s", queue)
	if len(queue) != 0 || len(accepted) != 2 || accepted[0] != "a" || accepted[1] != "b" {
		t.Errorf("after recovery queue = %d accepted = %v, want both delivered in order", len(queue), accepted)
	}
}

// TestListenIsLoopback pins the boundary the open-witness refusal turns on. A wildcard or empty
// host binds every interface, so calling it loopback would let a public witness serve its watched
// server list to anyone; a hostname other than localhost cannot be trusted to resolve locally.
func TestListenIsLoopback(t *testing.T) {
	tests := []struct {
		Addr string
		Want bool
	}{{ // Test 0: The IPv4 loopback is loopback.
		Addr: "127.0.0.1:9440", Want: true,
	}, { // Test 1: Any address in the loopback block counts.
		Addr: "127.0.0.2:9440", Want: true,
	}, { // Test 2: The IPv6 loopback is loopback.
		Addr: "[::1]:9440", Want: true,
	}, { // Test 3: localhost is loopback.
		Addr: "localhost:9440", Want: true,
	}, { // Test 4: The IPv4 wildcard binds every interface.
		Addr: "0.0.0.0:9440", Want: false,
	}, { // Test 5: The IPv6 wildcard binds every interface.
		Addr: "[::]:9440", Want: false,
	}, { // Test 6: An empty host binds every interface.
		Addr: ":9440", Want: false,
	}, { // Test 7: A private address is still not loopback.
		Addr: "10.0.0.5:9440", Want: false,
	}, { // Test 8: A hostname other than localhost is not trusted to be local.
		Addr: "witness.example.com:9440", Want: false,
	}, { // Test 9: A bare loopback address with no port still reads as loopback.
		Addr: "127.0.0.1", Want: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if got := listenIsLoopback(test.Addr); got != test.Want {
				t.Errorf("listenIsLoopback(%q) = %v, want %v", test.Addr, got, test.Want)
			}
		})
	}
}

// TestWitnessServeRefusesOpenOnNonLoopback proves the serve command will not expose the watched
// server list to the network: a non-loopback bind with no read token is refused before anything
// starts, and setting the token through the environment satisfies the check.
func TestWitnessServeRefusesOpenOnNonLoopback(t *testing.T) {
	witnessWatch, witnessServeInterval = []string{"https://st.example"}, time.Minute
	witnessListen = "0.0.0.0:0"
	defer func() {
		witnessWatch, witnessServeInterval = nil, time.Minute
		witnessListen = "127.0.0.1:9440"
		witnessServeToken = ""
	}()

	// No token anywhere: refused, and the error says how to fix it.
	t.Setenv(witnessTokenEnv, "")
	err := runWitnessServe(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), witnessTokenEnv) {
		t.Fatalf("runWitnessServe() open on 0.0.0.0 = %v, want a refusal naming %s", err, witnessTokenEnv)
	}

	// A token from the environment passes the exposure check. The state directory is forced under
	// a file so the run then fails fast at the key directory, proving the refusal above was the
	// token check and not a later failure.
	t.Setenv(witnessTokenEnv, "read-token")
	file := filepath.Join(t.TempDir(), "occupied")
	if werr := os.WriteFile(file, []byte("x"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	witnessStateDir, witnessServeKeyDir = filepath.Join(file, "state"), ""
	defer func() { witnessStateDir = "switchtender-witness" }()
	err = runWitnessServe(testCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "witness key directory") {
		t.Fatalf("runWitnessServe() with env token = %v, want it past the exposure check and "+
			"failing at the key directory", err)
	}
}
