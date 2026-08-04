package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestParseSums(t *testing.T) {
	t.Parallel()
	hash := strings.Repeat("a", 64)
	body := hash + "  switchtender_1.61.0_linux_amd64\n" +
		strings.Repeat("B", 64) + " *switchtender_1.61.0_windows_amd64.exe\n" +
		"garbage line\n" +
		"deadbeef  too-short-hash\n"
	sums := parseSums(body)
	if sums["switchtender_1.61.0_linux_amd64"] != hash {
		t.Errorf("linux sum = %q, want the parsed hash", sums["switchtender_1.61.0_linux_amd64"])
	}
	// The binary-mode star is stripped and hashes come back lowercased.
	if sums["switchtender_1.61.0_windows_amd64.exe"] != strings.Repeat("b", 64) {
		t.Errorf("windows sum = %q, want the starred entry lowercased",
			sums["switchtender_1.61.0_windows_amd64.exe"])
	}
	if len(sums) != 2 {
		t.Errorf("parsed %d entries, want the two well-formed ones", len(sums))
	}
}

func TestMatchPlatformSum(t *testing.T) {
	t.Parallel()
	sums := map[string]string{
		"switchtender_1.61.0_darwin_all":        "aa",
		"switchtender_1.61.0_darwin_arm64":      "bb",
		"switchtender_1.61.0_linux_amd64":       "cc",
		"switchtender_1.61.0_windows_amd64.exe": "dd",
	}
	tests := []struct {
		// Goos and Goarch name the platform being resolved.
		Goos, Goarch string
		// WantAsset and WantSum are the expected pick.
		WantAsset, WantSum string
	}{{ // Test 0: macOS prefers the universal binary, which is what the archive carries.
		Goos: "darwin", Goarch: "arm64",
		WantAsset: "switchtender_1.61.0_darwin_all", WantSum: "aa",
	}, { // Test 1: Linux matches its exact platform.
		Goos: "linux", Goarch: "amd64",
		WantAsset: "switchtender_1.61.0_linux_amd64", WantSum: "cc",
	}, { // Test 2: Windows carries the .exe suffix.
		Goos: "windows", Goarch: "amd64",
		WantAsset: "switchtender_1.61.0_windows_amd64.exe", WantSum: "dd",
	}, { // Test 3: An unlisted platform matches nothing rather than something else.
		Goos: "linux", Goarch: "riscv64",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			asset, sum := matchPlatformSum(sums, "1.61.0", test.Goos, test.Goarch)
			if asset != test.WantAsset || sum != test.WantSum {
				t.Errorf("match = %q, %q, want %q, %q", asset, sum, test.WantAsset, test.WantSum)
			}
		})
	}
}

func TestMacUniversalFallsBackToTheArchEntry(t *testing.T) {
	t.Parallel()
	sums := map[string]string{"switchtender_1.61.0_darwin_arm64": "bb"}
	asset, sum := matchPlatformSum(sums, "1.61.0", "darwin", "arm64")
	if asset != "switchtender_1.61.0_darwin_arm64" || sum != "bb" {
		t.Errorf("match = %q, %q, want the arch entry when no universal exists", asset, sum)
	}
}

// serveSums runs a release-asset server answering the sums path with body, and points the
// download base at it for the duration of the test.
func serveSums(t *testing.T, version string, status int, body string) {
	t.Helper()
	path := "/v" + version + "/" + binarySumsAsset
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	releaseDownloadBase = srv.URL
	t.Cleanup(func() { releaseDownloadBase = "https://github.com/kordloom/switchtender/releases/download" })
}

func TestVersionVerify(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	self, err := fileSHA256(exe)
	if err != nil {
		t.Fatalf("fileSHA256() error = %v", err)
	}
	name, _ := matchPlatformSum(map[string]string{
		fmt.Sprintf("switchtender_9.9.9_%s_%s", runtime.GOOS, runtime.GOARCH): "x",
		"switchtender_9.9.9_darwin_all":                                       "x",
	}, "9.9.9", runtime.GOOS, runtime.GOARCH)

	Version = "9.9.9"
	versionVerify = true
	defer func() { Version, versionVerify = "0.0.0-dev", false }()

	// Test 0: The published hash matches this very executable.
	serveSums(t, "9.9.9", http.StatusOK, self+"  "+name+"\n")
	if err := versionCmd.RunE(testCommand(), nil); err != nil {
		t.Errorf("verify with a matching hash error = %v", err)
	}

	// Test 1: A different published hash refuses, since this file is not the released binary.
	serveSums(t, "9.9.9", http.StatusOK, strings.Repeat("0", 64)+"  "+name+"\n")
	if err := versionCmd.RunE(testCommand(), nil); err == nil {
		t.Error("verify with a mismatched hash = nil error, want refusal")
	}

	// Test 2: A release without the asset is an error that names the answer, not a pass and not
	// a bare "no binary for this platform".
	serveSums(t, "9.9.9", http.StatusNotFound, "")
	if err := versionCmd.RunE(testCommand(), nil); err == nil ||
		!strings.Contains(err.Error(), "answered") {
		t.Errorf("verify against a missing asset error = %v, want the release's answer named", err)
	}

	// Test 3: A development build refuses to pretend it has a release, even when a sums file
	// exists that would happily match it. The refusal must come from the guard, not from luck.
	Version = "0.0.0-dev"
	devName := fmt.Sprintf("switchtender_0.0.0-dev_%s_%s", runtime.GOOS, runtime.GOARCH)
	serveSums(t, "0.0.0-dev", http.StatusOK,
		self+"  "+devName+"\n"+self+"  switchtender_0.0.0-dev_darwin_all\n")
	if err := versionCmd.RunE(testCommand(), nil); err == nil ||
		!strings.Contains(err.Error(), "development") {
		t.Errorf("verify on a dev build error = %v, want the dev refusal", err)
	}
}
