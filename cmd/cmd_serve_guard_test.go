package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/roundhouse"
)

const (
	// probeInterval is how long the serve probe waits between attempts on the listener.
	probeInterval = 100 * time.Millisecond
	// probeTimeout bounds one probe request, since a listener nobody serves accepts the connection
	// and then never answers.
	probeTimeout = 500 * time.Millisecond
)

// setString points a package-level flag variable at value for the duration of the test and restores
// the previous value afterward. The serve and worker flags are package globals, so a test that
// drives the real wiring has to set them the way cobra and the desktop command already do.
func setString(t *testing.T, p *string, value string) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = value
}

// setBool points a package-level bool flag variable at value and restores it after the test.
func setBool(t *testing.T, p *bool, value bool) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = value
}

// setInt points a package-level int flag variable at value and restores it after the test.
func setInt(t *testing.T, p *int, value int) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = value
}

// fakeRuntime writes a stub container CLI named binary into a directory that becomes the only entry
// on PATH, and returns a func reporting the argv the stub was called with. The stub records its
// arguments and exits zero, so a run reaches the real exec without a container daemon. The second
// return value is false when the stub was never invoked, which is how a refusal is proved.
func fakeRuntime(t *testing.T, binary string) func() ([]string, bool) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(dir, binary), []byte(script), 0o700); err != nil {
		t.Fatalf("write fake %s: %v", binary, err)
	}
	t.Setenv("PATH", dir)
	return func() ([]string, bool) {
		data, err := os.ReadFile(record)
		if err != nil {
			return nil, false
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), true
	}
}

// TestSelectiveRunnerFromFlagsAppliesContainerFlags drives the executor the serve and worker
// commands actually build and proves the container flags reach the launched container: the caps
// appear on the command line and an unpinned image never launches one at all. Without the flag
// wiring the runner would launch uncapped containers from an unpinned image, which the argv and the
// launched-at-all check both catch.
//
// The test is serial: it sets package-level flag variables and PATH, neither of which survives
// t.Parallel.
func TestSelectiveRunnerFromFlagsAppliesContainerFlags(t *testing.T) {
	const pinned = "quay.io/ansible/creator-ee@sha256:abc123"

	tests := []struct {
		Name        string
		Image       string
		Want        error
		WantArgs    []string
		WantLaunch  bool
		WantExit    int
		WantRuntime string
	}{
		{ // Test 0: A digest-pinned image launches with every flag-configured cap on the argv.
			Name: "pinned image carries the caps", Image: pinned, WantLaunch: true,
			WantArgs: []string{
				"--pull", "always", "--memory", "512m", "--cpus", "1.5",
				"--pids-limit", "64", "--network", "none",
			},
		},
		{ // Test 1: A tag-only image is refused and no container is ever launched.
			Name: "unpinned image never launches", Image: "quay.io/ansible/creator-ee:latest",
			Want: roundhouse.ErrUnpinnedImage, WantExit: -1,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			setString(t, &containerMemory, "512m")
			setString(t, &containerCPUs, "1.5")
			setInt(t, &containerPidsLimit, 64)
			setString(t, &containerNetwork, "none")
			setString(t, &containerRuntime, "podman")
			setString(t, &containerPullPolicy, "always")
			argv := fakeRuntime(t, "podman")

			runner := newSelectiveRunnerFromFlags(true, true)
			spec := roundhouse.Spec{
				Playbook: "/checkout/site.yml", Dir: "/checkout", Image: test.Image,
			}
			res, err := runner.Run(context.Background(), spec, os.Stderr)
			if !errors.Is(err, test.Want) {
				t.Fatalf("%s: Run() error = %v, want %v", test.Name, err, test.Want)
			}
			if res.ExitCode != test.WantExit {
				t.Errorf("%s: ExitCode = %d, want %d", test.Name, res.ExitCode, test.WantExit)
			}

			got, launched := argv()
			if launched != test.WantLaunch {
				t.Fatalf("%s: container launched = %v, want %v (argv %v)",
					test.Name, launched, test.WantLaunch, got)
			}
			if !test.WantLaunch {
				return
			}
			for i := 0; i+1 < len(test.WantArgs); i += 2 {
				name, value := test.WantArgs[i], test.WantArgs[i+1]
				if !argPair(got, name, value) {
					t.Errorf("%s: argv %v missing %s %s", test.Name, got, name, value)
				}
			}
			if diff := cmp.Diff([]string{test.Image}, tailImage(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: image mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// argPair reports whether argv contains name immediately followed by value.
func argPair(argv []string, name, value string) bool {
	for i, a := range argv {
		if a == name && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
	}
	return false
}

// tailImage returns the image reference from a container run argv: the element before the tool
// argv, which for Ansible is ansible-playbook. It returns nil when the tool is absent.
func tailImage(argv []string) []string {
	for i, a := range argv {
		if a == "ansible-playbook" && i > 0 {
			return []string{argv[i-1]}
		}
	}
	return nil
}

// TestServeBootstrapsTokenOnPublicBind drives runServe with an empty token store and a public bind
// address and proves the startup guard is actually consulted.
//
// Refusing this bind outright is what the guard used to do, and it made the first command in the
// quickstart, the README, and the homepage exit 1 on every fresh install. Minting a token is the
// replacement, so the property to hold is no longer "nothing answers" but "nothing answers
// unauthenticated": the listener comes up, an anonymous caller is refused, and a credential exists
// for the operator to use. Asserting only that serve returned would pass on a serve that bound an
// open API, so the probe checks the status the API actually gives an anonymous caller.
//
// The test is serial: it sets the package-level serve flag variables.
func TestServeBootstrapsTokenOnPublicBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// serveListener stands in for the public bind so a mutated guard exposes a loopback port to the
	// test instead of every interface on the machine. serveAddr still names the public bind, which
	// is what the guard reads.
	old := serveListener
	t.Cleanup(func() { serveListener = old })
	serveListener = ln

	setString(t, &serveDB, filepath.Join(t.TempDir(), "serve.db"))
	setString(t, &serveAddr, "0.0.0.0:8080")
	setBool(t, &serveReadOnly, false)
	setString(t, &serveOIDCIssuer, "")
	setString(t, &serveLDAPURL, "")
	setString(t, &serveSAMLIDPMetadataURL, "")
	setString(t, &serveJWTJWKSURL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()

	body, served := probeListener(ln.Addr().String(), 1500*time.Millisecond)
	if !served {
		t.Fatalf("nothing answered on %s; the bind was refused rather than bootstrapped", ln.Addr())
	}
	if !strings.Contains(body, "401") {
		t.Errorf("an anonymous caller got %q from %s, want 401: the public bind is unauthenticated",
			body, ln.Addr())
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runServe() error = %v, want a clean shutdown after the context expired", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServe() did not return; it is still serving")
	}

	// The credential the operator was handed must actually be in the store, or the printed token is
	// a value nothing will ever accept.
	bundle, err := openBundle(serveDB)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	n, err := bundle.Tokens().Count(context.Background())
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if n != 1 {
		t.Errorf("token count = %d, want the one minted at startup", n)
	}
}

// probeListener asks the address for the run list until it answers or the window closes. It reports
// the response status and true when anything answered, which means a server is running there.
func probeListener(addr string, window time.Duration) (string, bool) {
	client := &http.Client{Timeout: probeTimeout}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/v1/runs")
		if err == nil {
			_ = resp.Body.Close()
			return resp.Status, true
		}
		time.Sleep(probeInterval)
	}
	return "", false
}
