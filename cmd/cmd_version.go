package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kordloom/switchtender/internal/jsonutil"
)

// versionVerify turns the print into a check against the release's published binary hashes.
var versionVerify bool

// releaseDownloadBase is where release assets live, split out so tests can point it elsewhere.
var releaseDownloadBase = "https://github.com/kordloom/switchtender/releases/download"

// binarySumsAsset is the release asset naming each platform binary's SHA-256. SHA256SUMS covers
// the archives; this covers the file that actually runs, which is the one an operator wants to
// hold against the release after it has been extracted, copied, and forgotten about.
const binarySumsAsset = "BINARY_SHA256SUMS"

// sumsBodyCap bounds the fetched sums file; a few dozen hash lines is under a kilobyte.
const sumsBodyCap = 1 << 20

// versionCmd prints the SwitchTender version, or verifies the running binary against its release.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the SwitchTender version, or verify this binary against its release.",
	Long: `Print the SwitchTender version.

With --verify, hash the running executable and compare it against the BINARY_SHA256SUMS asset
published with this version's GitHub release. A match proves this file is byte-identical to the
released binary. A mismatch does not by itself mean tampering: source builds, Homebrew bottles,
and repackaged binaries hash differently. Nothing is fetched unless --verify is given.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if !versionVerify {
			fmt.Println(resolveVersion())
			return nil
		}
		return runVersionVerify(cmd)
	},
}

// init registers the version flags.
func init() {
	versionCmd.Flags().BoolVar(&versionVerify, "verify", false,
		"Fetch this version's published binary hashes and verify the running executable.")
}

// runVersionVerify hashes the running executable and holds it against the published sums.
func runVersionVerify(cmd *cobra.Command) error {
	version := strings.TrimPrefix(resolveVersion(), "v")
	if version == "0.0.0-dev" || strings.Contains(version, "devel") {
		return fmt.Errorf("this is a development build; there is no release to verify against")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	sum, err := fileSHA256(exe)
	if err != nil {
		return fmt.Errorf("hash executable: %w", err)
	}

	url := fmt.Sprintf("%s/v%s/%s", releaseDownloadBase, version, binarySumsAsset)
	sums, err := fetchSums(cmd.Context(), url)
	if err != nil {
		return fmt.Errorf("fetch %s for v%s: %w", binarySumsAsset, version, err)
	}

	verdict := map[string]any{
		"version": version, "binary": exe, "sha256": sum, "ok": false,
	}
	asset, expected := matchPlatformSum(sums, version, runtime.GOOS, runtime.GOARCH)
	switch expected {
	case "":
		verdict["problem"] = fmt.Sprintf("the release lists no binary for %s/%s",
			runtime.GOOS, runtime.GOARCH)
	case sum:
		verdict["ok"], verdict["asset"] = true, asset
	default:
		verdict["asset"], verdict["expected"] = asset, expected
		verdict["problem"] = "this file is not the released binary; a source build, a package " +
			"manager build, or a modified file all hash differently"
	}
	out, jerr := jsonutil.Marshal(verdict, true)
	if jerr != nil {
		return jerr
	}
	fmt.Println(string(out))
	if verdict["ok"] != true {
		return fmt.Errorf("binary does not verify against the v%s release", version)
	}
	return nil
}

// fileSHA256 returns the hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchSums downloads and parses a sums asset into name to hash.
func fetchSums(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the release answered %s; releases before v1.61.0 do not carry %s",
			res.Status, binarySumsAsset)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, sumsBodyCap))
	if err != nil {
		return nil, err
	}
	return parseSums(string(data)), nil
}

// parseSums reads "hash  name" lines the way shasum writes them.
func parseSums(body string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

// matchPlatformSum returns the sums entry for the running platform. On macOS the universal binary
// is preferred, since that is the file the release archives actually carry, whatever architecture
// the kernel reports it running as.
func matchPlatformSum(sums map[string]string, version, goos, goarch string) (asset, sum string) {
	var names []string
	if goos == "darwin" {
		names = append(names, fmt.Sprintf("switchtender_%s_darwin_all", version))
	}
	name := fmt.Sprintf("switchtender_%s_%s_%s", version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	names = append(names, name)
	for _, n := range names {
		if s, ok := sums[n]; ok {
			return n, s
		}
	}
	return "", ""
}
