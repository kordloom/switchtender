package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// desktopCmd runs Yardmaster as a local desktop application: it serves on a loopback port, stores
// its data in a per-user directory, and opens the web UI in the default browser. It is the engine a
// packaged .app or .exe launches, so a double-click gives a running server and an open UI.
var desktopCmd = &cobra.Command{
	Use:   "desktop",
	Short: "Run Yardmaster locally and open its UI in the browser.",
	Long: "Run Yardmaster as a local desktop app. It serves on a private loopback port, keeps its " +
		"database in a per-user data directory, and opens the web UI in your default browser. No " +
		"flags to set: it is the one-command way to run Yardmaster on your own machine.",
	RunE:          runDesktop,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// runDesktop configures a local single-user serve and opens the UI, then blocks on the server.
func runDesktop(cmd *cobra.Command, _ []string) error {
	dir, err := desktopDataDir()
	if err != nil {
		return err
	}
	serveDB = filepath.Join(dir, "yardmaster.db")

	port, err := freeLoopbackPort()
	if err != nil {
		return fmt.Errorf("find a free port: %w", err)
	}
	serveAddr = "127.0.0.1:" + strconv.Itoa(port)

	url := "http://" + serveAddr + "/ui/"
	fmt.Fprintln(os.Stderr, "Yardmaster is starting at "+url)
	go openWhenReady(serveAddr, url)
	return runServe(cmd, nil)
}

// desktopDataDir returns the per-user directory where the desktop app keeps its database, creating
// it when missing. It follows the platform convention through os.UserConfigDir.
func desktopDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate a data directory: %w", err)
	}
	dir := filepath.Join(base, "Yardmaster")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	return dir, nil
}

// freeLoopbackPort asks the OS for an unused loopback port by listening on port zero and reading
// back the assigned port. The listener is closed immediately, so the server can claim it.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// openWhenReady waits for the server to accept connections, then opens the UI in the default
// browser. Setting YARDMASTER_DESKTOP_NO_BROWSER skips the open, for a headless or remote run. It
// gives up quietly after a few seconds so a headless environment does not hang.
func openWhenReady(addr, url string) {
	if os.Getenv("YARDMASTER_DESKTOP_NO_BROWSER") != "" {
		return
	}
	for range 100 {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			_ = openBrowser(url)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openBrowser opens url in the platform's default browser.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
