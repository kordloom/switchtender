package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"gopkg.in/yaml.v3"
)

// fakeVersion is the release tag the fixture release carries.
const fakeVersion = "v9.9.9"

// fakeBase is the download URL the fixture release is served from, which is what the scripts
// derive from the tag they resolved.
const fakeBase = "https://github.com/kordloom/switchtender/releases/download/" + fakeVersion

// curlShim answers the scripts' downloads from the fixture release and records every URL asked
// for. The GitHub API is refused outright, so a script that still resolves the version there
// fails the way a rate-limited office does. With FAKE_LATEST_FAIL set, the latest-release lookup
// fails the way curl does when it cannot resolve the host.
const curlShim = `#!/bin/sh
out=""
redirect=0
url=""
while [ $# -gt 0 ]; do
	case "$1" in
		-o | --output) out="$2"; shift ;;
		-w) redirect=1; shift ;;
		http://* | https://*) url="$1" ;;
	esac
	shift
done
printf '%s\n' "$url" >> "$SHIM_LOG/urls"
case "$url" in
	*api.github.com*)
		: > "$SHIM_LOG/api-called"
		exit 22 ;;
	*/releases/latest/download/*)
		[ -n "${FAKE_LATEST_FAIL:-}" ] && exit 6
		[ "$redirect" = 1 ] && printf '%s' "$FAKE_BASE/${url##*/}"
		exit 0 ;;
	"$FAKE_BASE"/*)
		f="$FAKE_FIXTURES/${url##*/}"
		[ -f "$f" ] || exit 22
		if [ -n "$out" ]; then cp "$f" "$out"; else cat "$f"; fi
		exit 0 ;;
esac
exit 22
`

// wgetShim answers like BusyBox wget, the only downloader on a stock Alpine: it takes -q, -S,
// -O, and --spider, refuses every other long option the way BusyBox does, and under -S prints
// each redirect hop's headers to stderr with the header names spelled as the server sent them.
// The latest-release URL redirects to the tagged asset and from there to a storage host, as
// GitHub does.
const wgetShim = `#!/bin/sh
out=""
headers=0
url=""
while [ $# -gt 0 ]; do
	case "$1" in
		-qO | -O) out="$2"; shift ;;
		-qO-) out="-" ;;
		-q | --spider) ;;
		-S | -qS) headers=1 ;;
		--*)
			printf 'wget: unrecognized option: %s\n' "${1#--}" >&2
			exit 1 ;;
		http://* | https://*) url="$1" ;;
		*)
			printf 'wget: invalid option -- %s\n' "$1" >&2
			exit 1 ;;
	esac
	shift
done
printf '%s\n' "$url" >> "$SHIM_LOG/urls"
case "$url" in
	*api.github.com*)
		: > "$SHIM_LOG/api-called"
		exit 1 ;;
	*/releases/latest/download/*)
		if [ "$headers" = 1 ]; then
			printf '  HTTP/1.1 302 Found\n  location: %s\n' "$FAKE_BASE/${url##*/}" >&2
			printf '  HTTP/1.1 302 Found\n  Location: https://objects.example/asset\n' >&2
			printf '  HTTP/1.1 200 OK\n' >&2
		fi
		exit 0 ;;
	"$FAKE_BASE"/*)
		f="$FAKE_FIXTURES/${url##*/}"
		[ -f "$f" ] || exit 1
		if [ -n "$out" ] && [ "$out" != - ]; then cp "$f" "$out"; else cat "$f"; fi
		exit 0 ;;
esac
exit 1
`

// cosignShim records how it was called and exits as FAKE_COSIGN_EXIT says.
const cosignShim = `#!/bin/sh
printf '%s\n' "$*" >> "$SHIM_LOG/cosign"
exit "${FAKE_COSIGN_EXIT:-0}"
`

// installShim stands in for install(1) on the demo box and records what would have landed.
const installShim = `#!/bin/sh
printf '%s\n' "$*" >> "$SHIM_LOG/install"
`

// dpkgShim answers the architecture question the demo upgrade asks.
const dpkgShim = `#!/bin/sh
printf 'amd64\n'
`

// shimTools are the real commands the scripts need beside the shims, linked into the shim
// directory so it can be the whole PATH: whether cosign is found is then decided by the test
// alone, not by what the machine running it has installed.
var shimTools = []string{
	"awk", "cat", "chmod", "cp", "cut", "grep", "gzip", "head", "id", "mkdir", "mktemp", "mv",
	"rm", "sed", "tar", "tr", "uname",
}

// releaseScripts are the two copies of the installer, which must stay byte for byte the same:
// site/install.sh is what switchtender.com serves and deploy/install.sh is the copy in the
// deployment directory.
var releaseScripts = []string{"site/install.sh", "deploy/install.sh"}

// hostSlug is the archive name suffix the installer picks on the machine running the test.
func hostSlug() string {
	if runtime.GOOS == "darwin" {
		return "darwin_all"
	}
	return "linux_" + runtime.GOARCH
}

// downloaders maps each downloader the installer can use to the shim that stands in for it.
var downloaders = map[string]string{"curl": curlShim, "wget": wgetShim}

// writeShimDir builds a directory that serves as the scripts' entire PATH: the shims, the real
// tools they need, the one downloader named, and cosign only when withCosign is set.
func writeShimDir(t *testing.T, downloader string, withCosign bool) string {
	t.Helper()
	dir := t.TempDir()
	fetcher, ok := downloaders[downloader]
	if !ok {
		t.Fatalf("no shim for the downloader %q", downloader)
	}
	for name, body := range map[string]string{
		downloader: fetcher, "install": installShim, "dpkg": dpkgShim,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s shim: %v", name, err)
		}
	}
	if withCosign {
		shim := filepath.Join(dir, "cosign")
		if err := os.WriteFile(shim, []byte(cosignShim), 0o755); err != nil {
			t.Fatalf("write the cosign shim: %v", err)
		}
	}
	for _, name := range shimTools {
		real, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("the scripts need %s and this machine has none: %v", name, err)
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
	}
	hashers := 0
	for _, name := range []string{"sha256sum", "shasum"} {
		real, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(real, filepath.Join(dir, name)); err != nil {
			t.Fatalf("link %s: %v", name, err)
		}
		hashers++
	}
	if hashers == 0 {
		t.Fatal("the scripts need sha256sum or shasum and this machine has neither")
	}
	return dir
}

// writeFakeRelease writes a release's assets for one archive into dir: the archive holding a
// stand-in binary, the checksums naming it, and a signature and certificate for the shims.
func writeFakeRelease(t *testing.T, dir, slug string) {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	binary := []byte("#!/bin/sh\necho switchtender " + fakeVersion + "\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "switchtender", Mode: 0o755, Size: int64(len(binary)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	name := "switchtender_" + strings.TrimPrefix(fakeVersion, "v") + "_" + slug + ".tar.gz"
	sum := sha256.Sum256(archive.Bytes())
	files := map[string][]byte{
		name:             archive.Bytes(),
		"SHA256SUMS":     []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"),
		"SHA256SUMS.sig": []byte("fake signature\n"),
		"SHA256SUMS.pem": []byte("fake certificate\n"),
	}
	for file, body := range files {
		if err := os.WriteFile(filepath.Join(dir, file), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
	}
}

// scriptEnv is the environment the scripts run under: the shim directory as the whole PATH, the
// fixture release, and a scratch home and temp directory.
func scriptEnv(t *testing.T, shims, fixtures, log, cosignExit string) []string {
	t.Helper()
	home := t.TempDir()
	return []string{
		"PATH=" + shims,
		"HOME=" + home,
		"TMPDIR=" + home,
		"PREFIX=" + filepath.Join(home, "bin"),
		"SHIM_LOG=" + log,
		"FAKE_BASE=" + fakeBase,
		"FAKE_FIXTURES=" + fixtures,
		"FAKE_COSIGN_EXIT=" + cosignExit,
	}
}

// missing returns the wanted substrings that text does not contain.
func missing(text string, wanted []string) []string {
	var out []string
	for _, w := range wanted {
		if !strings.Contains(text, w) {
			out = append(out, w)
		}
	}
	return out
}

// TestInstallScriptVerifiesTheSignatureAndNeverAsksTheAPI runs the installer against a fixture
// release and holds it to its two promises: when cosign is on PATH the signature over the
// checksums is checked with the release identity and a refusal stops the install, and when it is
// not the script says so rather than staying quiet. The version is resolved from the redirect
// GitHub answers rather than the API, which is refused by the shim as a rate-limited office sees
// it refused.
func TestInstallScriptVerifiesTheSignatureAndNeverAsksTheAPI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantOutput     []string
		WantCosignArgs []string
		Downloader     string
		CosignExit     string
		WithCosign     bool
		LatestFails    bool
		WantInstalled  bool
		WantExitOK     bool
	}{{ // Test 0: cosign is installed and the signature verifies.
		Downloader: "curl", WithCosign: true, CosignExit: "0", WantInstalled: true,
		WantExitOK: true,
		WantOutput: []string{
			"Signature verified: the checksums were signed by this project's release workflow.",
			"Checksum verified.",
		},
		WantCosignArgs: []string{
			"verify-blob", "--signature", "--certificate",
			"--certificate-identity-regexp " + `^https://github.com/kordloom/switchtender/` +
				`\.github/workflows/release\.yml@refs/tags/v`,
			"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		},
	}, { // Test 1: cosign refuses the signature, so nothing is installed.
		Downloader: "curl", WithCosign: true, CosignExit: "1", WantInstalled: false,
		WantExitOK: false,
		WantOutput: []string{"the signature over the checksums for v9.9.9 did not verify"},
	}, { // Test 2: cosign is absent: the install goes ahead and says what went unchecked.
		Downloader: "curl", WithCosign: false, WantInstalled: true, WantExitOK: true,
		WantOutput: []string{
			"The signature over the checksums was not checked: cosign is not installed.",
			"Checksum verified.",
		},
	}, { // Test 3: BusyBox wget alone, as on a stock Alpine, with a signature that verifies.
		Downloader: "wget", WithCosign: true, CosignExit: "0", WantInstalled: true,
		WantExitOK: true,
		WantOutput: []string{
			"Signature verified: the checksums were signed by this project's release workflow.",
			"Checksum verified.",
		},
		WantCosignArgs: []string{"verify-blob", "--signature", "--certificate"},
	}, { // Test 4: BusyBox wget alone and no cosign.
		Downloader: "wget", WithCosign: false, WantInstalled: true, WantExitOK: true,
		WantOutput: []string{
			"The signature over the checksums was not checked: cosign is not installed.",
			"Checksum verified.",
		},
	}, { // Test 5: the latest-release lookup fails, and the installer says so before it stops.
		Downloader: "curl", WithCosign: true, CosignExit: "0", LatestFails: true,
		WantInstalled: false, WantExitOK: false,
		WantOutput: []string{"install: could not determine the latest version; set VERSION=vX.Y.Z"},
	}}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to run the installer with: %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fixtures, log := t.TempDir(), t.TempDir()
			writeFakeRelease(t, fixtures, hostSlug())
			shims := writeShimDir(t, test.Downloader, test.WithCosign)
			env := scriptEnv(t, shims, fixtures, log, test.CosignExit)
			if test.LatestFails {
				env = append(env, "FAKE_LATEST_FAIL=1")
			}
			cmd := exec.Command(sh, "site/install.sh")
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if (err == nil) != test.WantExitOK {
				t.Errorf("exit ok = %v, want %v; output:\n%s", err == nil, test.WantExitOK, out)
			}
			prefix := strings.TrimPrefix(env[3], "PREFIX=")
			_, statErr := os.Stat(filepath.Join(prefix, "switchtender"))
			if installed := statErr == nil; installed != test.WantInstalled {
				t.Errorf("installed = %v, want %v; output:\n%s", installed, test.WantInstalled, out)
			}
			if diff := cmp.Diff([]string{}, missing(string(out), test.WantOutput),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("output lacks (-want +got):\n%s\noutput:\n%s", diff, out)
			}
			if _, err := os.Stat(filepath.Join(log, "api-called")); err == nil {
				t.Error("the installer asked api.github.com for the version")
			}
			urls, _ := os.ReadFile(filepath.Join(log, "urls"))
			if !strings.Contains(string(urls), "/releases/latest/download/SHA256SUMS") {
				t.Errorf("the version was not resolved from the latest-release redirect; urls:\n%s",
					urls)
			}
			cosignLog, _ := os.ReadFile(filepath.Join(log, "cosign"))
			if diff := cmp.Diff([]string{}, missing(string(cosignLog), test.WantCosignArgs),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("cosign call lacks (-want +got):\n%s\ncall:\n%s", diff, cosignLog)
			}
		})
	}
}

// TestInstallScriptRunsNothingWhenCutShort feeds every line-boundary prefix of the installer to
// sh the way a dropped connection under curl | sh or wget -qO- | sh would, and requires each to do
// nothing: no download, no install, no progress line. Only the whole script, ending in the call to
// main, acts.
func TestInstallScriptRunsNothingWhenCutShort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Downloader string
	}{{ // Test 0: curl is the downloader.
		Downloader: "curl",
	}, { // Test 1: BusyBox wget is the only downloader.
		Downloader: "wget",
	}}
	raw, err := os.ReadFile("site/install.sh")
	if err != nil {
		t.Fatalf("read the installer: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh to run the installer with: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fixtures, log := t.TempDir(), t.TempDir()
			writeFakeRelease(t, fixtures, hostSlug())
			shims := writeShimDir(t, test.Downloader, true)
			env := scriptEnv(t, shims, fixtures, log, "0")
			prefix := strings.TrimPrefix(env[3], "PREFIX=")
			for n := 1; n < len(lines); n++ {
				cmd := exec.Command(sh)
				cmd.Env = env
				cmd.Stdin = strings.NewReader(strings.Join(lines[:n], "\n") + "\n")
				out, _ := cmd.CombinedOutput()
				progress := []string{"Finding the latest release", "Downloading"}
				if len(missing(string(out), progress)) < len(progress) {
					t.Fatalf("the first %d of %d lines acted on their own; output:\n%s", n,
						len(lines), out)
				}
				if _, err := os.Stat(filepath.Join(log, "urls")); err == nil {
					t.Fatalf("the first %d of %d lines downloaded something", n, len(lines))
				}
				if _, err := os.Stat(filepath.Join(prefix, "switchtender")); err == nil {
					t.Fatalf("the first %d of %d lines installed the binary", n, len(lines))
				}
			}
		})
	}
}

// TestInstallScriptCopiesMatch keeps each copy of the installer identical to the one
// switchtender.com serves, so a fix made to one cannot leave another behind.
func TestInstallScriptCopiesMatch(t *testing.T) {
	t.Parallel()
	served, err := os.ReadFile(releaseScripts[0])
	if err != nil {
		t.Fatalf("read %s: %v", releaseScripts[0], err)
	}
	tests := []struct {
		Copy string
	}{{ // Test 0: the copy in the deployment directory.
		Copy: releaseScripts[1],
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(test.Copy)
			if err != nil {
				t.Fatalf("read %s: %v", test.Copy, err)
			}
			if diff := cmp.Diff(string(served), string(raw)); diff != "" {
				t.Errorf("%s differs from %s (-served +copy):\n%s", test.Copy, releaseScripts[0],
					diff)
			}
		})
	}
}

// TestDemoUpgradeVerifiesTheSignatureBeforeInstalling runs the demo upgrade script against the
// fixture release with install(1) and dpkg stood in for, and requires the cosign check to decide
// whether the binary is installed: a verified signature installs, while a refused signature, a
// box without cosign, and a failed version lookup each install nothing and say why.
func TestDemoUpgradeVerifiesTheSignatureBeforeInstalling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantOutput    []string
		CosignExit    string
		WithCosign    bool
		LatestFails   bool
		WantInstalled bool
	}{{ // Test 0: the signature verifies and the binary is installed.
		WithCosign: true, CosignExit: "0", WantInstalled: true,
		WantOutput: []string{"signature over SHA256SUMS verified", "checksum verified"},
	}, { // Test 1: the signature is refused and nothing is installed.
		WithCosign: true, CosignExit: "1", WantInstalled: false,
		WantOutput: []string{"the signature over the checksums for v9.9.9 did not verify"},
	}, { // Test 2: no cosign on the box, so nothing is installed.
		WithCosign: false, WantInstalled: false,
		WantOutput: []string{
			"cosign is not installed on this box, so the release signature cannot be checked;",
		},
	}, { // Test 3: the latest-release lookup fails, and the upgrade says so before it stops.
		WithCosign: true, CosignExit: "0", LatestFails: true, WantInstalled: false,
		WantOutput: []string{"could not resolve the latest release tag"},
	}}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("no bash to run the upgrade with: %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fixtures, log := t.TempDir(), t.TempDir()
			writeFakeRelease(t, fixtures, "linux_amd64")
			shims := writeShimDir(t, "curl", test.WithCosign)
			cmd := exec.Command(bash, "deploy/demo-upgrade.sh")
			cmd.Env = scriptEnv(t, shims, fixtures, log, test.CosignExit)
			if test.LatestFails {
				cmd.Env = append(cmd.Env, "FAKE_LATEST_FAIL=1")
			}
			out, err := cmd.CombinedOutput()
			_, statErr := os.Stat(filepath.Join(log, "install"))
			if installed := statErr == nil; installed != test.WantInstalled {
				t.Errorf("installed = %v, want %v; output:\n%s", installed, test.WantInstalled, out)
			}
			if !test.WantInstalled && err == nil {
				t.Errorf("the upgrade exited 0 without installing; output:\n%s", out)
			}
			if diff := cmp.Diff([]string{}, missing(string(out), test.WantOutput),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("output lacks (-want +got):\n%s\noutput:\n%s", diff, out)
			}
			if _, err := os.Stat(filepath.Join(log, "api-called")); err == nil {
				t.Error("the upgrade asked api.github.com for the version")
			}
		})
	}
}

// workflowSteps splits a workflow file into its steps, so an assertion about a download can be
// held against the one step that makes it.
func workflowSteps(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return regexp.MustCompile(`(?m)^      - `).Split(string(raw), -1)
}

// TestDemoRefreshChecksTheDropletHostKey requires the refresh workflow to hold the droplet to a
// committed host key rather than accept whatever answers at the address, and the committed key to
// name the host the workflow talks to by default.
func TestDemoRefreshChecksTheDropletHostKey(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(".github/workflows/demo-refresh.yml")
	if err != nil {
		t.Fatalf("read the workflow: %v", err)
	}
	workflow := string(raw)
	tests := []struct {
		Text        string
		WantPresent bool
	}{{ // Test 0: the committed host key becomes the runner's known_hosts.
		Text: "cp deploy/demo_known_hosts ~/.ssh/known_hosts", WantPresent: true,
	}, { // Test 1: ssh refuses a host whose key is not the committed one.
		Text: "-o StrictHostKeyChecking=yes", WantPresent: true,
	}, { // Test 2: ssh reads the file the committed key was copied to.
		Text: `-o UserKnownHostsFile="$HOME/.ssh/known_hosts"`, WantPresent: true,
	}, { // Test 3: nothing accepts an unknown key on first use.
		Text: "StrictHostKeyChecking=accept-new", WantPresent: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := strings.Contains(workflow, test.Text); got != test.WantPresent {
				t.Errorf("demo-refresh.yml contains %q = %v, want %v", test.Text, got,
					test.WantPresent)
			}
		})
	}
	host := regexp.MustCompile(`'[^'@]*@([^']+)'`).FindStringSubmatch(workflow)
	if host == nil {
		t.Fatal("demo-refresh.yml names no default root@host for DEMO_SSH_HOST")
	}
	known, err := os.ReadFile("deploy/demo_known_hosts")
	if err != nil {
		t.Fatalf("read the committed host key: %v", err)
	}
	keyed := false
	for _, line := range strings.Split(string(known), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		hosts := strings.Split(fields[0], ",")
		if fields[1] == "ssh-ed25519" && len(fields[2]) == 68 && slices.Contains(hosts, host[1]) {
			keyed = true
		}
	}
	if !keyed {
		t.Errorf("deploy/demo_known_hosts holds no ed25519 key for %s", host[1])
	}
}

// TestReleasePublishesTheSigstoreBundle requires the release to write the sigstore bundle beside
// the detached signature and certificate, refuse to go on when either detached file is missing,
// upload all of them with the manifests a user verifies against, and document the bundle as the
// way to verify, with the detached pair kept for cosign v2 and a trusted root for a machine with
// no network.
func TestReleasePublishesTheSigstoreBundle(t *testing.T) {
	t.Parallel()
	var sign string
	for _, step := range workflowSteps(t, ".github/workflows/release.yml") {
		if strings.Contains(step, "cosign sign-blob") {
			sign = step
		}
	}
	if sign == "" {
		t.Fatal("release.yml has no step that runs cosign sign-blob")
	}
	doc, err := os.ReadFile("SECURITY.md")
	if err != nil {
		t.Fatalf("read SECURITY.md: %v", err)
	}
	tests := []struct {
		File string
		Text string
		Want []string
	}{{ // Test 0: the signing step writes, checks, and uploads every signature file.
		File: "the signing step", Text: sign,
		Want: []string{
			"--output-signature dist/SHA256SUMS.sig",
			"--output-certificate dist/SHA256SUMS.pem",
			"--bundle dist/SHA256SUMS.sigstore.json",
			"test -s dist/SHA256SUMS.sigstore.json",
			"! test -s dist/SHA256SUMS.sig",
			"! test -s dist/SHA256SUMS.pem",
			`assets="dist/SHA256SUMS dist/SHA256SUMS.sig dist/SHA256SUMS.pem"`,
			`assets="$assets dist/SHA256SUMS.sigstore.json dist/BINARY_SHA256SUMS"`,
		},
	}, { // Test 1: SECURITY.md documents the bundle, the detached pair, and the trusted root.
		File: "SECURITY.md", Text: string(doc),
		Want: []string{
			"--bundle SHA256SUMS.sigstore.json",
			"--signature SHA256SUMS.sig",
			"--certificate SHA256SUMS.pem",
			"--trusted-root trusted_root.json",
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff([]string{}, missing(test.Text, test.Want),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s lacks (-want +got):\n%s", test.File, diff)
			}
		})
	}
}

// workflowFile is the part of a GitHub Actions workflow the release tests read.
type workflowFile struct {
	// Jobs maps each job's ID to the job.
	Jobs map[string]workflowJob `yaml:"jobs"`
}

// workflowJob is one job of a workflow.
type workflowJob struct {
	// Steps are the job's steps in the order they run.
	Steps []workflowStep `yaml:"steps"`
}

// workflowStep is one step of a job.
type workflowStep struct {
	// Name is the step's display name.
	Name string `yaml:"name"`
	// Run is the shell script the step runs, empty for a step that uses an action.
	Run string `yaml:"run"`
}

// signStepScript returns the shell script of the release step that runs cosign sign-blob.
func signStepScript(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read release.yml: %v", err)
	}
	var wf workflowFile
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse release.yml: %v", err)
	}
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "cosign sign-blob") {
				return step.Run
			}
		}
	}
	t.Fatal("release.yml has no step that runs cosign sign-blob")
	return ""
}

// signCosignShim stands in for cosign in the release signing step. It reports the version in
// FAKE_COSIGN_VERSION, lists --new-bundle-format in its help only as cosign v2 does, and signs the
// way that version does: cosign v2 writes the bundle, the signature, and the certificate, and
// cosign v3 writes the bundle alone and ignores the other two outputs.
const signCosignShim = `#!/bin/sh
printf '%s\n' "$*" >> "$SHIM_LOG/cosign"
case "$1" in
	version)
		printf 'GitVersion:    %s\n' "$FAKE_COSIGN_VERSION"
		exit 0 ;;
	sign-blob) shift ;;
	*) exit 1 ;;
esac
if [ "$1" = --help ]; then
	case "$FAKE_COSIGN_VERSION" in v2.*) printf '      --new-bundle-format\n' ;; esac
	exit 0
fi
sig=""
pem=""
bundle=""
while [ $# -gt 0 ]; do
	case "$1" in
		--output-signature) sig="$2"; shift ;;
		--output-certificate) pem="$2"; shift ;;
		--bundle) bundle="$2"; shift ;;
	esac
	shift
done
[ -n "$bundle" ] && printf 'bundle\n' > "$bundle"
case "$FAKE_COSIGN_VERSION" in
	v2.*)
		[ -n "$sig" ] && printf 'signature\n' > "$sig"
		[ -n "$pem" ] && printf 'certificate\n' > "$pem"
		;;
esac
exit 0
`

// ghShim stands in for the GitHub CLI and records each call.
const ghShim = `#!/bin/sh
printf '%s\n' "$*" >> "$SHIM_LOG/gh"
`

// shasumShim stands in for shasum and prints a fixed hash beside each file named, which is all
// the signing step needs from it.
const shasumShim = `#!/bin/sh
shift 2
for f in "$@"; do printf '%064d  %s\n' 0 "$f"; done
`

// TestReleaseSignStepRefusesAMissingDetachedPair runs the release signing step under a stand-in
// cosign of each major version and requires it to upload the bundle, the signature, and the
// certificate when cosign v2 writes all three, and to stop before uploading anything, naming the
// cosign version, when cosign v3 writes the bundle alone.
func TestReleaseSignStepRefusesAMissingDetachedPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		CosignVersion  string
		WantOutput     []string
		WantUpload     []string
		WantCosignArgs []string
		WantExitOK     bool
	}{{ // Test 0: cosign v2 writes all three files and every one is uploaded.
		CosignVersion: "v2.6.1", WantExitOK: true,
		WantUpload: []string{
			"release upload v9.9.9 dist/SHA256SUMS dist/SHA256SUMS.sig dist/SHA256SUMS.pem " +
				"dist/SHA256SUMS.sigstore.json dist/BINARY_SHA256SUMS --clobber",
		},
		WantCosignArgs: []string{"--bundle dist/SHA256SUMS.sigstore.json --new-bundle-format"},
	}, { // Test 1: cosign v3 writes the bundle alone, so the step stops and names the version.
		CosignVersion: "v3.1.3", WantExitOK: false,
		WantOutput: []string{
			"::error::cosign v3.1.3 wrote no SHA256SUMS.sig or SHA256SUMS.pem.",
			"cosign v3 writes only the bundle",
		},
	}}
	script := signStepScript(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("no bash to run the signing step with: %v", err)
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			work, shims, log := t.TempDir(), t.TempDir(), t.TempDir()
			for name, body := range map[string]string{
				"cosign": signCosignShim, "gh": ghShim, "shasum": shasumShim,
			} {
				shim := filepath.Join(shims, name)
				if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
					t.Fatalf("write the %s shim: %v", name, err)
				}
			}
			for _, name := range []string{"awk", "grep"} {
				real, err := exec.LookPath(name)
				if err != nil {
					t.Fatalf("the signing step needs %s and this machine has none: %v", name, err)
				}
				if err := os.Symlink(real, filepath.Join(shims, name)); err != nil {
					t.Fatalf("link %s: %v", name, err)
				}
			}
			dist := filepath.Join(work, "dist")
			if err := os.Mkdir(dist, 0o755); err != nil {
				t.Fatalf("make dist: %v", err)
			}
			for _, name := range []string{"SHA256SUMS", "BINARY_SHA256SUMS"} {
				file := filepath.Join(dist, name)
				if err := os.WriteFile(file, []byte(name+"\n"), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			cmd := exec.Command(bash, "-c", script)
			cmd.Dir = work
			cmd.Env = []string{
				"PATH=" + shims,
				"HOME=" + work,
				"SHIM_LOG=" + log,
				"GITHUB_REF_NAME=v9.9.9",
				"FAKE_COSIGN_VERSION=" + test.CosignVersion,
			}
			out, err := cmd.CombinedOutput()
			if (err == nil) != test.WantExitOK {
				t.Errorf("exit ok = %v, want %v; output:\n%s", err == nil, test.WantExitOK, out)
			}
			if diff := cmp.Diff([]string{}, missing(string(out), test.WantOutput),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("output lacks (-want +got):\n%s\noutput:\n%s", diff, out)
			}
			uploads, _ := os.ReadFile(filepath.Join(log, "gh"))
			var got []string
			if len(uploads) > 0 {
				got = strings.Split(strings.TrimRight(string(uploads), "\n"), "\n")
			}
			if diff := cmp.Diff(test.WantUpload, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("uploads mismatch (-want +got):\n%s", diff)
			}
			cosignLog, _ := os.ReadFile(filepath.Join(log, "cosign"))
			if diff := cmp.Diff([]string{}, missing(string(cosignLog), test.WantCosignArgs),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("cosign calls lack (-want +got):\n%s\ncalls:\n%s", diff, cosignLog)
			}
		})
	}
}

// TestReleaseStaplesTheAppBeforeBuildingTheDmg requires the desktop app to be notarized and
// stapled on its own before the disk image is built around it, so the copy a user drags into
// /Applications carries a ticket, and the image to be stapled after.
func TestReleaseStaplesTheAppBeforeBuildingTheDmg(t *testing.T) {
	t.Parallel()
	var dmg string
	for _, step := range workflowSteps(t, ".github/workflows/release.yml") {
		if strings.Contains(step, "id: dmg") {
			dmg = step
		}
	}
	if dmg == "" {
		t.Fatal("release.yml has no step with id: dmg")
	}
	order := []string{
		`xcrun notarytool submit "$RUNNER_TEMP/SwitchTender.zip"`,
		`xcrun stapler staple "$APP"`,
		`xcrun stapler validate "$APP"`,
		"hdiutil create",
		`xcrun notarytool submit "dist/SwitchTender_${VERSION}.dmg"`,
		`xcrun stapler staple "dist/SwitchTender_${VERSION}.dmg"`,
	}
	last := -1
	for _, want := range order {
		at := strings.Index(dmg, want)
		if at < 0 {
			t.Errorf("the dmg step never runs %q", want)
			continue
		}
		if at < last {
			t.Errorf("the dmg step runs %q before the command that must precede it", want)
		}
		last = at
	}
}

// TestCIToolDownloadsArePinnedByChecksum requires each CI tool download that has a published
// SHA-256 to be checked against that hash, held in the step's env, before the download is run.
func TestCIToolDownloadsArePinnedByChecksum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Workflow  string
		URL       string
		Variable  string
		File      string
		WantSteps int
	}{{ // Test 0: the chart render job's helm.
		Workflow: ".github/workflows/ci.yml", URL: "helm-v3.16.3-linux-amd64.tar.gz",
		Variable: "HELM_SHA256", File: "/tmp/helm.tar.gz", WantSteps: 1,
	}, { // Test 1: the supertest's helm.
		Workflow: ".github/workflows/supertest.yml", URL: "helm-v3.16.3-linux-amd64.tar.gz",
		Variable: "HELM_SHA256", File: "/tmp/helm.tar.gz", WantSteps: 1,
	}, { // Test 2: terraform, installed by both the test shards and the integration job.
		Workflow: ".github/workflows/ci.yml", URL: "releases.hashicorp.com/terraform/",
		Variable: "TERRAFORM_SHA256", File: "/tmp/terraform.zip", WantSteps: 2,
	}, { // Test 3: OpenTofu, installed by both the test shards and the integration job.
		Workflow: ".github/workflows/ci.yml", URL: "github.com/opentofu/opentofu/releases/",
		Variable: "TOFU_SHA256", File: "/tmp/tofu.zip", WantSteps: 2,
	}, { // Test 4: the secret scan's gitleaks.
		Workflow: ".github/workflows/ci.yml", URL: "github.com/gitleaks/gitleaks/releases/",
		Variable: "GITLEAKS_SHA256", File: "/tmp/gitleaks.tar.gz", WantSteps: 1,
	}, { // Test 5: the supertest's kind.
		Workflow: ".github/workflows/supertest.yml", URL: "kind.sigs.k8s.io/dl/",
		Variable: "KIND_SHA256", File: "/tmp/kind", WantSteps: 1,
	}}
	hexHash := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			steps := 0
			for _, step := range workflowSteps(t, test.Workflow) {
				if !strings.Contains(step, test.URL) {
					continue
				}
				steps++
				pinned := regexp.MustCompile(`(?m)^\s+` + test.Variable + `: (\S+)$`)
				pin := pinned.FindStringSubmatch(step)
				if pin == nil || !hexHash.MatchString(pin[1]) {
					t.Errorf("the step downloading %s in %s pins no %s in its env", test.URL,
						test.Workflow, test.Variable)
				}
				check := fmt.Sprintf(`echo "$%s  %s" | sha256sum -c -`, test.Variable, test.File)
				if !strings.Contains(step, check) {
					t.Errorf("the step downloading %s in %s never runs %q", test.URL, test.Workflow,
						check)
				}
				if strings.Contains(step, "curl -sSL "+test.URL) ||
					strings.Contains(step, "| tar -xz") {
					t.Errorf("the step downloading %s in %s still pipes the download straight "+
						"into tar", test.URL, test.Workflow)
				}
			}
			if steps != test.WantSteps {
				t.Errorf("%d steps in %s download %s, want %d", steps, test.Workflow, test.URL,
					test.WantSteps)
			}
		})
	}
}

// downloadCurlShim stands in for curl in the ci-local image's download step: it answers each URL
// with the fixture under FAKE_FILES named by the URL's last path segment, to the -o file or to
// stdout.
const downloadCurlShim = `#!/bin/sh
out=
url=
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
src="$FAKE_FILES/${url##*/}"
if [ ! -f "$src" ]; then
	echo "no fixture for $url" >&2
	exit 22
fi
if [ -n "$out" ]; then
	cp "$src" "$out"
else
	cat "$src"
fi
`

// ciLocalDownloadScript returns the shell script of the ci-local.Dockerfile RUN instruction that
// stores the files CI's steps download, with its download directory taken from
// CI_LOCAL_DOWNLOADS so the test can run it outside the image.
func ciLocalDownloadScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("scripts/ci-local.Dockerfile")
	if err != nil {
		t.Fatalf("read the ci-local Dockerfile: %v", err)
	}
	var instructions []string
	var current []string
	for _, line := range strings.Split(string(data), "\n") {
		if len(current) == 0 && !strings.HasPrefix(line, "RUN ") {
			continue
		}
		current = append(current, line)
		if !strings.HasSuffix(line, `\`) {
			instructions = append(instructions, strings.Join(current, "\n"))
			current = nil
		}
	}
	const dir = "d=/opt/ci-local/downloads;"
	for _, run := range instructions {
		if !strings.Contains(run, `test -n "$HELM_VERSION"`) {
			continue
		}
		if strings.Count(run, dir) != 1 {
			t.Fatalf("the download instruction does not set %q exactly once", dir)
		}
		run = strings.Replace(run, dir, `d="$CI_LOCAL_DOWNLOADS";`, 1)
		return strings.TrimPrefix(run, "RUN ")
	}
	t.Fatal("no RUN instruction in the ci-local Dockerfile stores helm")
	return ""
}

// gzipTar returns a gzipped tar holding one file. The gzip header carries a name, which a repacked
// archive does not, so an archive that was unpacked and packed again never matches it.
func gzipTar(t *testing.T, name, body string) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	gz.Name = "upstream"
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return out.Bytes()
}

// tarFiles returns the regular files in a gzipped tar, keyed by path.
func tarFiles(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		files[strings.TrimPrefix(hdr.Name, "./")] = string(body)
	}
}

// TestCILocalImageKeepsThePinnedDownloads runs the ci-local image's download step against fixture
// downloads. On amd64 every stored file must be the upstream download byte for byte, so the SHA-256
// pins CI's steps check hold locally too. On arm64 helm is this machine's build repacked under the
// linux-amd64 path the steps unpack.
func TestCILocalImageKeepsThePinnedDownloads(t *testing.T) {
	t.Parallel()
	const helmVersion, gitleaksVersion, kindVersion = "3.16.3", "8.21.2", "0.25.0"
	tests := []struct {
		Arch         string
		GitleaksArch string
		WantVerbatim bool
	}{{ // Test 0: amd64 keeps the helm archive CI downloads and pins.
		Arch: "amd64", GitleaksArch: "x64", WantVerbatim: true,
	}, { // Test 1: arm64 serves its own helm under linux-amd64.
		Arch: "arm64", GitleaksArch: "arm64", WantVerbatim: false,
	}}
	script := ciLocalDownloadScript(t)
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fixtures := t.TempDir()
			helmName := "helm-v" + helmVersion + "-linux-" + test.Arch + ".tar.gz"
			gitleaksName := "gitleaks_" + gitleaksVersion + "_linux_" + test.GitleaksArch +
				".tar.gz"
			kindName := "kind-linux-" + test.Arch
			upstream := map[string][]byte{
				helmName:     gzipTar(t, "linux-"+test.Arch+"/helm", "helm for "+test.Arch),
				gitleaksName: []byte("gitleaks for " + test.Arch),
				kindName:     []byte("kind for " + test.Arch),
			}
			for name, body := range upstream {
				if err := os.WriteFile(filepath.Join(fixtures, name), body, 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			shims := t.TempDir()
			for name, body := range map[string]string{
				"curl": downloadCurlShim, "python3": "#!/bin/sh\nexit 0\n",
			} {
				shim := filepath.Join(shims, name)
				if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
					t.Fatalf("write the %s shim: %v", name, err)
				}
			}
			downloads := filepath.Join(t.TempDir(), "downloads")
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = []string{
				"PATH=" + shims + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + t.TempDir(),
				"TARGETARCH=" + test.Arch,
				"HELM_VERSION=" + helmVersion,
				"GITLEAKS_VERSION=" + gitleaksVersion,
				"KIND_VERSION=" + kindVersion,
				"FAKE_FILES=" + fixtures,
				"CI_LOCAL_DOWNLOADS=" + downloads,
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the download step failed: %v\n%s", err, out)
			}
			stored := map[string][]byte{}
			for _, name := range []string{"helm.tar.gz", "gitleaks.tar.gz", "kind"} {
				body, err := os.ReadFile(filepath.Join(downloads, name))
				if err != nil {
					t.Fatalf("read the stored %s: %v", name, err)
				}
				stored[name] = body
			}
			helm := tarFiles(t, stored["helm.tar.gz"])["linux-amd64/helm"]
			if diff := cmp.Diff("helm for "+test.Arch, helm); diff != "" {
				t.Errorf("linux-amd64/helm in the stored archive mismatch (-want +got):\n%s", diff)
			}
			got := bytes.Equal(upstream[helmName], stored["helm.tar.gz"])
			if got != test.WantVerbatim {
				t.Errorf("stored helm archive is the upstream download = %t, want %t", got,
					test.WantVerbatim)
			}
			if diff := cmp.Diff(upstream[gitleaksName], stored["gitleaks.tar.gz"]); diff != "" {
				t.Errorf("stored gitleaks archive mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(upstream[kindName], stored["kind"]); diff != "" {
				t.Errorf("stored kind binary mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
