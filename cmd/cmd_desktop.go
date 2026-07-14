package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// desktopCmd runs Railwarden as a local desktop application: it serves on a loopback port, stores
// its data in a per-user directory, and opens the web UI in the default browser. It is the engine a
// packaged .app or .exe launches, so a double-click gives a running server and an open UI.
var desktopCmd = &cobra.Command{
	Use:   "desktop",
	Short: "Run Railwarden locally and open its UI in the browser.",
	Long: "Run Railwarden as a local desktop app. It serves on a private loopback port, keeps its " +
		"database in a per-user data directory, and opens the web UI in your default browser. No " +
		"flags to set: it is the one-command way to run Railwarden on your own machine.",
	RunE:          runDesktop,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// runDesktop configures a local single-user serve and opens the UI, then blocks on the server. The
// loopback port persists in the data directory and is reused across launches, so browser storage
// keyed by origin, such as the sign-in token and the tour state, survives a restart. A second
// launch finds the live instance on that port and opens its UI instead of starting another server.
func runDesktop(cmd *cobra.Command, _ []string) error {
	dir, err := desktopDataDir()
	if err != nil {
		return err
	}
	serveDB = filepath.Join(dir, "railwarden.db")

	if port, ok := savedDesktopPort(dir); ok && desktopAlive(port) {
		url := "http://127.0.0.1:" + strconv.Itoa(port) + "/ui/"
		fmt.Fprintln(os.Stderr, "Railwarden is already running at "+url)
		go openWhenReady("127.0.0.1:"+strconv.Itoa(port), url)
		time.Sleep(2 * time.Second)
		return nil
	}

	l, err := desktopListener(dir)
	if err != nil {
		return fmt.Errorf("bind a loopback port: %w", err)
	}
	serveListener = l
	serveAddr = l.Addr().String()

	url := "http://" + serveAddr + "/ui/"
	fmt.Fprintln(os.Stderr, "Railwarden is starting at "+url)
	go openWhenReady(serveAddr, url)
	return runServe(cmd, nil)
}

// desktopDataDir returns the per-user directory where the desktop app keeps its database, creating
// it when missing. The directory is private to the user, since the database holds hashed tokens,
// sealed credentials, and the audit chain.
func desktopDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate a data directory: %w", err)
	}
	dir := filepath.Join(base, "Railwarden")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restrict data directory: %w", err)
	}
	return dir, nil
}

// desktopListener binds the saved loopback port when it is still free, or a fresh OS-assigned one,
// and records the choice. The listener stays open and is handed to the server, so no other process
// can take the port between choosing it and serving on it.
func desktopListener(dir string) (net.Listener, error) {
	if port, ok := savedDesktopPort(dir); ok {
		if l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil {
			return l, nil
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	saveDesktopPort(dir, l.Addr().(*net.TCPAddr).Port)
	return l, nil
}

// savedDesktopPort reads the port recorded by a previous launch, reporting false when there is none
// or the file does not hold a valid port.
func savedDesktopPort(dir string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "port"))
	if err != nil {
		return 0, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// saveDesktopPort records the chosen port for the next launch. A write failure is ignored, since
// the only cost is a new port next time.
func saveDesktopPort(dir string, port int) {
	_ = os.WriteFile(filepath.Join(dir, "port"), []byte(strconv.Itoa(port)), 0o600)
}

// desktopAlive reports whether a Railwarden instance is answering on the loopback port.
func desktopAlive(port int) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/healthz")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// openWhenReady waits for the server to accept connections, then opens the UI in the default
// browser. Setting RAILWARDEN_DESKTOP_NO_BROWSER to any value skips the open, for a headless or
// remote run. It stops trying after roughly ten seconds, and when the browser cannot be opened it
// prints the URL so the user can open it by hand.
func openWhenReady(addr, url string) {
	if os.Getenv("RAILWARDEN_DESKTOP_NO_BROWSER") != "" {
		return
	}
	for range 100 {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			if err := openBrowser(url); err != nil {
				fmt.Fprintln(os.Stderr, "Open "+url+" in your browser.")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openBrowser opens url in the platform's default browser and reaps the launcher process in the
// background so it does not linger as a zombie.
func openBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}
