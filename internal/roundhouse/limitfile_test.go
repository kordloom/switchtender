package roundhouse

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// shardLimit returns the limit a split shard carries for n hosts with 24-byte names.
func shardLimit(n int) string {
	hosts := make([]string, n)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host-%019d", i)
	}
	return strings.Join(hosts, ",")
}

// TestALongLimitTravelsInAFile pins how a run's limit reaches ansible-playbook. Linux refuses any
// single argument over 128 KiB, and a split shard of about 5,400 hosts passed its hosts as one
// --limit argument past that, so the run could not start. A limit over 64 KiB now goes in a file in
// the run's private directory, one pattern per line, passed as --limit @file, and a shorter one stays
// an argument byte for byte, so every run that started before starts the same way.
func TestALongLimitTravelsInAFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Limit        string
		WantFile     bool
		WantArgLimit string
	}{{ // Test 0: No limit passes none.
		Limit: "",
	}, { // Test 1: A short limit is an argument, unchanged.
		Limit: "web1,web2", WantArgLimit: "web1,web2",
	}, { // Test 2: A limit exactly at the threshold is still an argument.
		Limit: strings.Repeat("a", maxLimitArg), WantArgLimit: strings.Repeat("a", maxLimitArg),
	}, { // Test 3: A 6,000-host shard, about 150 KiB, goes in a file.
		Limit: shardLimit(6000), WantFile: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spec := Spec{Playbook: "site.yml", Limit: test.Limit, RunDir: t.TempDir()}
			cleanup, err := materializeLimit(&spec)
			if err != nil {
				t.Fatalf("materializeLimit() error = %v", err)
			}
			defer cleanup()
			args, err := playbookArgs(spec)
			if err != nil {
				t.Fatalf("playbookArgs() error = %v", err)
			}
			for _, a := range args {
				if len(a) > maxLimitArg {
					t.Fatalf("an argument is %d bytes, over the %d the run may pass", len(a),
						maxLimitArg)
				}
			}
			var got string
			for i, a := range args {
				if a == "--limit" && i+1 < len(args) {
					got = args[i+1]
				}
			}
			if !test.WantFile {
				if diff := cmp.Diff(test.WantArgLimit, got); diff != "" {
					t.Errorf("--limit mismatch (-want +got):\n%s", diff)
				}
				return
			}
			path, ok := strings.CutPrefix(got, "@")
			if !ok || spec.LimitFile != path {
				t.Fatalf("--limit %q, want @%s", got, spec.LimitFile)
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			if diff := cmp.Diff(strings.ReplaceAll(test.Limit, ",", "\n")+"\n", string(body)); diff != "" {
				t.Errorf("limit file mismatch (-want +got):\n%s", diff)
			}
			cleanup()
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("the limit file outlived its cleanup: %v", err)
			}
		})
	}
}
