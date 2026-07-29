// Command bench measures the numbers published in docs/benchmarks.md: cold boot to a served
// health check, with and without credential encryption, and resident memory at idle. It builds a
// release binary the same way a release does, so the figures describe what people actually run.
//
// Usage:
//
//	go run ./cmd/bench
//
// The first trial of each path is discarded as a warm-up, since it measures the filesystem cache
// rather than the program.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// trials is how many timed runs each path gets after its warm-up.
const trials = 5

// port is the loopback port the benchmarked server binds. It is closed between trials.
const port = 18188

// result is one path's measurements.
type result struct {
	// label names the path being measured.
	label string
	// bootMillis holds each trial's boot time.
	bootMillis []float64
	// rssMB holds each trial's resident memory three seconds after serving.
	rssMB []float64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

// run builds the binary, measures both boot paths, and prints the table.
func run() error {
	dir, err := os.MkdirTemp("", "switchtender-bench-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bin := filepath.Join(dir, "switchtender")
	fmt.Println("Building a release binary.")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	info, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("stat binary: %w", err)
	}

	paths := []struct {
		label string
		env   []string
	}{
		{label: "no encryption key"},
		{label: "credential encryption on", env: []string{
			"SWITCHTENDER_ENCRYPTION_KEY=bench-key-material",
			"SWITCHTENDER_ENCRYPTION_SALT=bench-salt-material",
		}},
	}

	results := make([]result, 0, len(paths))
	for _, p := range paths {
		fmt.Printf("Measuring %s: warm-up then %d trials.\n", p.label, trials)
		// The warm-up measures the filesystem cache rather than the program, so it is discarded.
		if _, _, err := trial(bin, dir, p.env); err != nil {
			return fmt.Errorf("%s warm-up: %w", p.label, err)
		}
		r := result{label: p.label}
		for range trials {
			boot, rss, err := trial(bin, dir, p.env)
			if err != nil {
				return fmt.Errorf("%s: %w", p.label, err)
			}
			r.bootMillis = append(r.bootMillis, boot)
			r.rssMB = append(r.rssMB, rss)
		}
		results = append(results, r)
	}

	fmt.Printf("\n%s, %s/%s\n\n", hostLabel(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("%-42s %10s  %s\n", "Measurement", "median", "range")
	for _, r := range results {
		boots := append([]float64(nil), r.bootMillis...)
		sort.Float64s(boots)
		fmt.Printf("%-42s %7.0f ms  %.0f-%.0f ms\n",
			"Cold boot, "+r.label, median(boots), boots[0], boots[len(boots)-1])
	}
	for _, r := range results {
		rss := append([]float64(nil), r.rssMB...)
		sort.Float64s(rss)
		if median(rss) == 0 {
			continue
		}
		fmt.Printf("%-42s %7.0f MB  %.0f-%.0f MB\n",
			"Idle memory, "+r.label, median(rss), rss[0], rss[len(rss)-1])
	}
	fmt.Printf("%-42s %7.0f MB\n", "Stripped release binary", float64(info.Size())/(1<<20))
	fmt.Println("\nOn macOS the encryption-on memory figure counts reclaimable pages, so it reads")
	fmt.Println("high. On Linux, where servers run, both paths idle in the same band.")
	return nil
}

// trial starts the server on a fresh database, times it to a served health check, samples resident
// memory after it settles, and stops it.
func trial(bin, dir string, env []string) (bootMillis, rssMB float64, err error) {
	db := filepath.Join(dir, "bench.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(db + suffix)
	}

	cmd := exec.Command(bin, "serve", "--db", db, "--addr", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), env...)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("start: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		waitForPortFree()
	}()

	client := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, reqErr := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if reqErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return 0, 0, fmt.Errorf("server did not serve within 30s")
		}
	}
	bootMillis = float64(time.Since(start).Microseconds()) / 1000

	// Let allocation settle before sampling, so the figure describes idle rather than startup.
	time.Sleep(3 * time.Second)
	return bootMillis, residentMB(cmd.Process.Pid), nil
}

// residentMB reports a process's resident memory in mebibytes, zero when it cannot be read.
func residentMB(pid int) float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return kb / 1024
}

// waitForPortFree blocks until the benchmark port accepts a new listener, so the next trial does
// not race the previous server's shutdown.
func waitForPortFree() {
	for range 100 {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = l.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// median returns the middle value of a sorted slice.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// hostLabel describes the machine in one line, so a pasted result says what it ran on.
func hostLabel() string {
	name, err := os.Hostname()
	if err != nil {
		return fmt.Sprintf("%d cores", runtime.NumCPU())
	}
	return fmt.Sprintf("%s, %d cores", name, runtime.NumCPU())
}
