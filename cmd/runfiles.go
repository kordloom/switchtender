package cmd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/coredump"
	"github.com/kordloom/switchtender/internal/runfiles"
	"github.com/kordloom/switchtender/internal/safedial"
)

// egressProxyEnv names the variable that routes safedial's outbound requests through a proxy. It is
// read explicitly at startup, never inherited from the ambient HTTP(S)_PROXY, because an inherited
// proxy would send every guarded request to it and leave the dial-time metadata check seeing only the
// proxy. With one set, each target is validated before anything reaches the proxy.
const egressProxyEnv = "SWITCHTENDER_EGRESS_PROXY"

// applyEgressProxy configures safedial's egress proxy from the environment, failing when it is set to
// something that is not a usable proxy URL.
func applyEgressProxy(log *zap.Logger) error {
	raw := strings.TrimSpace(os.Getenv(egressProxyEnv))
	if err := safedial.SetEgressProxy(raw); err != nil {
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if raw != "" {
		log.Info("outbound requests route through the configured egress proxy, after each target is " +
			"validated; the proxy's own egress policy is then the boundary for DNS rebinding")
	}
	return nil
}

// runFilesDirEnv names the variable that sets --runfiles-dir when the flag is absent.
const runFilesDirEnv = "SWITCHTENDER_RUNFILES_DIR"

// runFilesDir holds the --runfiles-dir flag, shared by serve and worker.
var runFilesDir string

// containerRunFilesSize holds the --container-runfiles-size flag, shared by serve and worker.
var containerRunFilesSize string

// tmpfsSize matches a size the container runtimes pass to the kernel's tmpfs: a positive count of
// bytes, or of kilobytes, megabytes, or gigabytes with the k, m, or g suffix.
var tmpfsSize = regexp.MustCompile(`^[1-9][0-9]*[kmgKMG]?$`)

// registerRunFilesFlag adds --runfiles-dir to cmd.
func registerRunFilesFlag(cmd *cobra.Command) {
	cmd.Flags().StringVar(&runFilesDir, "runfiles-dir", os.Getenv(runFilesDirEnv),
		"Private directory under which each run stages its keys, tokens, and secret files. Empty "+
			"picks the systemd RuntimeDirectory, then a private XDG_RUNTIME_DIR, then the "+
			"temporary directory. It must be on a local filesystem, a tmpfs for preference. "+
			runFilesDirEnv+" sets it when this flag is absent.")
}

// checkContainerRunFilesSize refuses a --container-runfiles-size the runtimes would not take as a
// tmpfs size.
func checkContainerRunFilesSize() error {
	if !tmpfsSize.MatchString(containerRunFilesSize) {
		return fmt.Errorf("%w: --container-runfiles-size %q is not a size such as 64m", ErrUsage,
			containerRunFilesSize)
	}
	return nil
}

// protectProcess turns off core dumps for this process and every tool it starts, before any run
// holds a decrypted secret in memory. A crash then writes no core file for anything to find later.
// A platform with no core limit says so in the log rather than stopping the process.
func protectProcess(log *zap.Logger) {
	switch err := coredump.Disable(); {
	case err == nil:
		log.Info("core dumps are off for this process and every tool it runs")
	case errors.Is(err, errors.ErrUnsupported):
		log.Info("this platform has no core dump limit to set; see the run files page for what " +
			"to check instead")
	default:
		log.Warn("could not turn off core dumps, so a crash could write process memory, " +
			"credentials included, to disk: " + err.Error())
	}
}

// prepareRunFiles picks the directory runs stage their secret files under and proves it before
// the process serves: private to this account, on a known local filesystem, and holding a lock the
// way a run directory needs. A root that fails any of that stops the process, since a sweep that
// misjudged a live run on it would delete credentials from under a running job. It logs where run
// files go and warns when that is on persistent disk.
func prepareRunFiles(log *zap.Logger) (runfiles.Choice, runfiles.Report, error) {
	choice, err := runfiles.ChooseRoot(runFilesDir, os.Getenv)
	if err != nil {
		return runfiles.Choice{}, runfiles.Report{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}
	report, err := runfiles.Prepare(choice.Path)
	if err != nil {
		return choice, runfiles.Report{}, fmt.Errorf("prepare the run files directory: %w", err)
	}
	if len(choice.Passed) > 0 {
		log.Info("run files: passed over " + strings.Join(choice.Passed, "; "))
	}
	fields := []zap.Field{zap.String("root", report.Root), zap.String("source", string(choice.Source)),
		zap.String("filesystem", report.Filesystem), zap.Bool("memory_backed", report.MemoryBacked)}
	switch {
	case !report.MemoryBacked:
		log.Warn("run files are staged on persistent disk; run under the shipped systemd unit or "+
			"set --runfiles-dir to a private tmpfs", fields...)
	case choice.Source == runfiles.SourceTemp:
		log.Warn("run files are staged in the temporary directory; run under the shipped systemd "+
			"unit or set --runfiles-dir to a private tmpfs", fields...)
	default:
		log.Info("run files are staged in memory", fields...)
	}
	return choice, report, nil
}
