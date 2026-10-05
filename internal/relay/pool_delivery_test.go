package relay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// fixedOpener opens the same small payload for every run that names credentials.
type fixedOpener struct{}

// NeedsSecrets reports whether the run names any credential.
func (fixedOpener) NeedsSecrets(_ context.Context, r *run.Run) bool { return len(r.CredentialIDs) > 0 }

// OpenSecrets returns one opened SSH key credential.
func (fixedOpener) OpenSecrets(_ context.Context, r *run.Run) (*handoff.Payload, func(), error) {
	return &handoff.Payload{RunID: r.ID, CredentialIDs: r.CredentialIDs,
		Credentials: []*handoff.Credential{handoff.NewCredential(&credential.Credential{
			ID: r.CredentialIDs[0], Kind: credential.KindSSHKey}, "pool-file-key-material")}}, nil, nil
}

// writePoolFile writes doc to a pool file and returns its path.
func writePoolFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "workers.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// TestAPoolFileRegistersADeliveryKey pins the registration path an operator uses: a delivery_key on
// a pool entry, loaded from the file, is the key that pool's runs are sealed to, end to end through
// a claim.
func TestAPoolFileRegistersADeliveryKey(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	pools, err := relay.LoadPools(writePoolFile(t, "workers:\n"+
		"  - name: dmz\n    token_sha256: "+relay.HashToken("tok-dmz")+"\n    queues: [dmz]\n"+
		"    delivery_key: "+key.Public().String()+"\n"+
		"  - name: bare\n    token_sha256: "+relay.HashToken("tok-bare")+"\n    queues: [bare]\n"))
	if err != nil {
		t.Fatalf("LoadPools() error = %v", err)
	}
	if diff := cmp.Diff(map[string]string{"dmz": key.ID()}, pools.DeliveryKeyIDs()); diff != "" {
		t.Errorf("registered delivery keys mismatch (-want +got):\n%s", diff)
	}
	store := run.NewMemStore()
	if err := store.Save(context.Background(), &run.Run{ID: "run_pf", Playbook: "site.yml",
		Status: run.StatusPending, Queue: "dmz", CredentialIDs: []string{"cred_pf"},
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ts := httptest.NewServer(relay.NewHandler(store, pools, nil, nil, audit.NewMemStore(),
		relay.WithSecretOpener(fixedOpener{})))
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/relay/v1/claim", strings.NewReader(`{"owner":"w1","queues":["dmz"]}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok-dmz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	var answer struct {
		// Delivery is the claim answer's delivery part.
		Delivery struct {
			// Sealed is the sealed envelope.
			Sealed *handoff.Envelope `json:"sealed"`
		} `json:"relay_secret_delivery"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Delivery.Sealed == nil {
		t.Fatalf("claim answer %d %s carries no sealed delivery (%v)", resp.StatusCode, body, err)
	}
	if answer.Delivery.Sealed.KeyID != key.ID() {
		t.Errorf("sealed to key %s, want the registered key %s", answer.Delivery.Sealed.KeyID, key.ID())
	}
	ring, err := handoff.NewKeyRing(key)
	if err != nil {
		t.Fatalf("NewKeyRing() error = %v", err)
	}
	p, err := handoff.Open(ring, handoff.Binding{RunID: "run_pf",
		Lease: resp.Header.Get("X-Switchtender-Lease"), Owner: "w1"}, answer.Delivery.Sealed)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := p.Credential("cred_pf"); got == nil || got.Value != "pool-file-key-material" {
		t.Errorf("the opened delivery does not carry the run's credential")
	}
}

// TestLoadPoolsRefusesADeliveryKeyItCannotTrust pins the startup refusals. A key that is not a key,
// a key on a pool not bound to explicit queues, and one key shared by two pools all stop the
// server, rather than starting with a delivery that would go somewhere nobody intended.
func TestLoadPoolsRefusesADeliveryKeyItCannotTrust(t *testing.T) {
	t.Parallel()
	key, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	pub := key.Public().String()
	entry := func(name, token, queues, deliveryKey string) string {
		out := "  - name: " + name + "\n    token_sha256: " + relay.HashToken(token) + "\n"
		if queues != "" {
			out += "    queues: " + queues + "\n"
		}
		if deliveryKey != "" {
			out += "    delivery_key: \"" + deliveryKey + "\"\n"
		}
		return out
	}
	tests := []struct {
		Name       string
		Doc        string
		WantReason string
	}{{ // Test 0: A pool with a key and explicit queues loads.
		Name: "valid", Doc: "workers:\n" + entry("dmz", "a", "[dmz]", pub), WantReason: "",
	}, { // Test 1: A pool with no key loads and simply receives nothing.
		Name: "no key", Doc: "workers:\n" + entry("dmz", "a", "[dmz]", ""), WantReason: "",
	}, { // Test 2: A value that is not a delivery key.
		Name: "not a key", Doc: "workers:\n" + entry("dmz", "a", "[dmz]", "x25519:not-a-key"),
		WantReason: `pool "dmz" delivery_key`,
	}, { // Test 3: A key on a pool that may claim from every queue.
		Name: "no queues", Doc: "workers:\n" + entry("any", "a", "", pub),
		WantReason: `pool "any" registers a delivery_key and declares no queues`,
	}, { // Test 4: One key on two pools.
		Name: "shared key",
		Doc: "workers:\n" + entry("dmz", "a", "[dmz]", pub) +
			entry("edge", "b", "[edge]", pub),
		WantReason: `pool "edge" registers the same delivery_key as pool "dmz"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := relay.LoadPools(writePoolFile(t, test.Doc))
			switch {
			case test.WantReason == "" && err != nil:
				t.Fatalf("LoadPools() error = %v, want the file to load", err)
			case test.WantReason != "" && (err == nil || !strings.Contains(err.Error(), test.WantReason)):
				t.Fatalf("LoadPools() error = %v, want one naming %q", err, test.WantReason)
			}
		})
	}
}
