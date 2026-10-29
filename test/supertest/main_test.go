package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestSelectPhasesKeepsTheBaseAndTheNamedPhases holds -only to what it promises: the base phases
// every run needs, then the named ones in run order, and a refusal for a name the run lacks, since
// a typo that selected nothing would run the base phases and pass.
func TestSelectPhasesKeepsTheBaseAndTheNamedPhases(t *testing.T) {
	t.Parallel()
	var all []phase
	for _, name := range []string{"cluster", "fleet", "community", "team", "ha", "relay"} {
		all = append(all, phase{Name: name})
	}
	tests := []struct {
		Only      string
		WantNames []string
		Want      error
	}{{ // Test 0: Empty keeps every phase.
		Only: "", WantNames: []string{"cluster", "fleet", "community", "team", "ha", "relay"},
	}, { // Test 1: Named phases keep run order, not the order named.
		Only: "relay,team", WantNames: []string{"cluster", "fleet", "team", "relay"},
	}, { // Test 2: Spaces and empty items are ignored.
		Only: " relay , ,", WantNames: []string{"cluster", "fleet", "relay"},
	}, { // Test 3: Naming a base phase changes nothing.
		Only: "fleet", WantNames: []string{"cluster", "fleet"},
	}, { // Test 4: A name the run lacks is refused.
		Only: "team,rely", Want: ErrUnknownPhase,
	}, { // Test 5: A phase this run left out, as -skip-team leaves out relay, is refused.
		Only: "upgrade-team", Want: ErrUnknownPhase,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := selectPhases(all, test.Only)
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantNames, phaseNames(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("phases mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestSelectPhasesNamesTheUnknownAndTheKnown requires the refusal to say what was misspelled and
// what the choices were, so the fix needs no second run.
func TestSelectPhasesNamesTheUnknownAndTheKnown(t *testing.T) {
	t.Parallel()
	_, err := selectPhases([]phase{{Name: "cluster"}, {Name: "relay"}}, "rely")
	if err == nil {
		t.Fatal("selectPhases accepted an unknown phase")
	}
	for _, want := range []string{"rely", "cluster, relay"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}

// TestRelayWorkerScriptKeepsTheWorkerOffPIDOne holds the relay worker pod's script to the shape
// the killed-mid-run check depends on: the worker in the background with its pid kept, the shell
// staying up after it, and the delivery key copied to a private file only for the keyed pool.
func TestRelayWorkerScriptKeepsTheWorkerOffPIDOne(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WithKey     bool
		WantPresent []string
		WantAbsent  []string
	}{{ // Test 0: The keyed pool copies the key and passes it.
		WithKey: true,
		WantPresent: []string{"chmod 600 /run/switchtender/delivery.key",
			"--delivery-key /run/switchtender/delivery.key &", "echo $! > /run/switchtender/worker.pid",
			"the worker process exited", "while true; do sleep 3600; done"},
	}, { // Test 1: The bare pool holds no key at all.
		WithKey:     false,
		WantPresent: []string{"--queue bare &", "echo $! > /run/switchtender/worker.pid"},
		WantAbsent:  []string{"delivery", "/keysrc"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			queue := "bare"
			if test.WithKey {
				queue = "relay"
			}
			got := relayWorkerScript("http://relay-switchtender:8080", "w", queue, test.WithKey)
			for _, want := range test.WantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("the script lacks %q:\n%s", want, got)
				}
			}
			for _, unwanted := range test.WantAbsent {
				if strings.Contains(got, unwanted) {
					t.Errorf("the script carries %q:\n%s", unwanted, got)
				}
			}
		})
	}
}

// TestOutcomeRulesReadsTheOutcomeEntry reads the rules from the run's own outcome entry and from no
// other claim, and refuses a receipt that discloses none.
func TestOutcomeRulesReadsTheOutcomeEntry(t *testing.T) {
	t.Parallel()
	const receipt = `{"claims":[
		{"payload":{"method":"RUN","path":"/runs/r2/outcome/x","outcome_body":"{\"policy_set\":{\"rules\":[\"other\"]}}"}},
		{"payload":{"method":"DECISION","path":"/runs/r1/decision/approved"}},
		{"payload":{"method":"RUN","path":"/runs/r1/outcome/y","outcome_body":"{\"policy_set\":{\"rules\":[\"a\",\"b\"]}}"}}
	]}`
	tests := []struct {
		RunID     string
		WantRules []string
		Want      error
	}{{ // Test 0: The run's own outcome entry.
		RunID: "r1", WantRules: []string{"a", "b"},
	}, { // Test 1: A run with no outcome entry.
		RunID: "r3", Want: ErrNothingRead,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, _, err := outcomeRules(receipt, test.RunID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantRules, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("rules mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseRelayKeyFindsTheJSONLine reads the key worker key new printed among the notice kubectl
// carries beside it, and refuses output that holds none.
func TestParseRelayKeyFindsTheJSONLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Out       string
		WantKeyID string
		Want      error
	}{{ // Test 0: The notice and then the key.
		Out:       "wrote the private key to /data/delivery.key.\n{\"key_id\":\"k1\",\"delivery_key\":\"pub\"}\n",
		WantKeyID: "k1",
	}, { // Test 1: No key line.
		Out: "error: something\n", Want: ErrNothingRead,
	}, { // Test 2: A line with a public key and no id is not the key.
		Out: "{\"delivery_key\":\"pub\"}\n", Want: ErrNothingRead,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseRelayKey(test.Out)
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantKeyID, got.KeyID); diff != "" {
				t.Errorf("key id mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
