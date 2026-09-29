package scenario

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// scenarioDir is where the declared scenarios live.
const scenarioDir = "scenarios"

// TestScenarios builds every declared install, drives its cases, and holds each one against the
// whole invariant battery.
//
// The subtests do not run in parallel. An install is cheap, and several of the settings a scenario
// declares are process-global: the licensed tier, the environment. Running them concurrently would
// make one scenario's environment another's, which is a worse failure than a slower suite because
// it is intermittent.
func TestScenarios(t *testing.T) {
	all, err := Load(scenarioDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", scenarioDir, err)
	}
	for _, s := range all {
		t.Run(s.Name, func(t *testing.T) {
			built := map[GrantMode]*Install{}
			// Closed on the parent, after the comparison below has run. Registering the close on
			// the per-mode subtest tore both installs down before the sibling comparison reached
			// them: on the memory backend a closed install keeps answering, so the check quietly
			// compared two live installs and looked fine, and on sqlite every read failed
			// identically on both sides, so the comparison agreed and reported nothing. A check
			// that cannot fail is worse than one that is missing.
			t.Cleanup(func() {
				for _, in := range built {
					in.Close()
				}
			})
			for _, mode := range s.Modes() {
				t.Run(string(mode), func(t *testing.T) {
					built[mode] = runScenario(t, s, mode)
				})
			}
			// The two modes are compared once both exist. Everything strict grants changes has to
			// be a removal, and the scenario has already been built twice, so the comparison costs
			// nothing beyond the reads.
			open, haveOpen := built[GrantsOpen]
			strict, haveStrict := built[GrantsStrict]
			if !haveOpen || !haveStrict {
				return
			}
			t.Run("strict only narrows", func(t *testing.T) {
				for _, failure := range CheckModeNarrowing(open, strict) {
					t.Errorf("%s", failure)
				}
			})
		})
	}
}

// runScenario builds one install, checks everything that must be true of it, and hands the install
// back so the two grant modes can be compared once both are built.
func runScenario(t *testing.T, s *Scenario, mode GrantMode) *Install {
	t.Helper()
	in, err := Build(s, mode, t.TempDir())
	if err != nil {
		t.Fatalf("building %s: %v", s.Path(), err)
	}

	for _, c := range s.Cases {
		t.Run(c.Name, func(t *testing.T) {
			for _, failure := range RunCase(in, c) {
				t.Errorf("%s\n    this case exists because: %s", failure, c.Why)
			}
		})
	}
	t.Run("invariants", func(t *testing.T) {
		for _, failure := range CheckInvariants(in) {
			t.Errorf("%s", failure)
		}
	})
	return in
}

// TestTheKindTargetRefusesRatherThanPasses pins the one thing a test framework must never do.
//
// A scenario declaring an environment this runner cannot build has to fail. If it were skipped, or
// quietly run in process instead, the scenario would sit in the suite reading as coverage of a real
// cluster and be coverage of nothing. The refusal is what keeps the suite's green honest.
func TestTheKindTargetRefusesRatherThanPasses(t *testing.T) {
	t.Parallel()
	s := &Scenario{
		Name: "kind", Why: "guard",
		Environment: Environment{Target: TargetKind},
		Cases:       []Case{{Name: "c", Why: "w"}},
	}
	_, err := Build(s, GrantsOpen, t.TempDir())
	if err == nil {
		t.Fatal("a scenario targeting a Kind cluster built in process, so it would report a pass " +
			"for an environment it never stood up")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error = %v, want it to name the target it cannot build", err)
	}
}

// fakeCall drives one registered stand-in and reports whether the call succeeded, so one contract
// can be held against every fake however it is reached.
type fakeCall func(t *testing.T, f Fake, address string) bool

// fakeContracts is the call for each registered stand-in. Every name in the registry must appear
// here, which is what the guard below enforces.
var fakeContracts = map[string]fakeCall{
	"submitter": func(t *testing.T, f Fake, _ string) bool {
		t.Helper()
		sub, ok := f.(*fakeSubmitter)
		if !ok {
			t.Fatalf("the submitter stand-in is %T", f)
		}
		_, err := sub.Submit(t.Context(), "site.yml", "prod")
		return err == nil
	},
	"vault": httpFakeCall,
	"http":  httpFakeCall,
}

// httpFakeCall reads an HTTP stand-in and reports whether it answered successfully.
func httpFakeCall(t *testing.T, _ Fake, address string) bool {
	t.Helper()
	res, err := http.Get(address + "/v1/secret")
	if err != nil {
		t.Fatalf("reaching the stand-in: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode < 400
}

// TestEveryRegisteredFakeIsContractTested is the rule that keeps the stand-ins honest.
//
// A fake that drifts from what it stands in for turns the suite into something testing itself: the
// scenarios stay green while the behavior they claim to cover has changed underneath. The contract
// below is the shared half, the failure injection every fake must implement the same way. Where a
// fake stands in for something the product also talks to for real, its own contract test holds the
// two to the same answers on the paths both support.
func TestEveryRegisteredFakeIsContractTested(t *testing.T) {
	t.Parallel()
	for _, name := range FakeNames() {
		if _, ok := fakeContracts[name]; !ok {
			t.Errorf("the %q stand-in is registered and has no contract test, so nothing holds it "+
				"to behaving like what it stands in for", name)
		}
	}
}

// TestFakesInjectFailureTheSameWay holds every stand-in to one meaning of the behavior knobs.
//
// Failure injection reinvented per dependency is failure injection with a different off-by-one per
// dependency, and a scenario that says fail_after: 2 has to mean the same thing everywhere or the
// language is a lie.
func TestFakesInjectFailureTheSameWay(t *testing.T) {
	t.Parallel()
	for _, name := range FakeNames() {
		call, ok := fakeContracts[name]
		if !ok {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				// Name says what the case proves.
				Name string
				// Behavior is what a scenario declared.
				Behavior Behavior
				// Want is whether each of three successive calls must succeed.
				Want [3]bool
			}{{ // Test 0: Nothing declared is a dependency that simply works.
				Name: "healthy", Want: [3]bool{true, true, true},
			}, { // Test 1: fail_after counts the calls that succeed, not the ones that fail.
				Name: "fails after two", Behavior: Behavior{FailAfter: 2},
				Want: [3]bool{true, true, false},
			}, { // Test 2: Unavailable is down from the first call, not flaky.
				Name: "unavailable", Behavior: Behavior{Unavailable: true},
				Want: [3]bool{false, false, false},
			}}
			for testNum, test := range tests {
				t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
					f := fakes[name](test.Behavior)
					address, err := f.Start()
					if err != nil {
						t.Fatalf("Start() error = %v", err)
					}
					defer f.Stop()
					for i, want := range test.Want {
						if got := call(t, f, address); got != want {
							t.Errorf("%s: call %d succeeded = %v, want %v",
								test.Name, i+1, got, want)
						}
					}
					if got := f.Calls(); got != len(test.Want) {
						t.Errorf("%s: Calls() = %d, want %d: a stand-in that does not count the "+
							"calls it took cannot answer whether the install reached it at all",
							test.Name, got, len(test.Want))
					}
				})
			}
		})
	}
}

// TestEveryInvariantRunsSomewhere proves the battery is not dead weight.
//
// An invariant every scenario skips is an invariant nobody checks, dressed as one that everybody
// does. This does not prove each one can fail, which is what a scenario's own negative control is
// for; it proves each one is reached.
func TestEveryInvariantRunsSomewhere(t *testing.T) {
	t.Parallel()
	all, err := Load(scenarioDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", scenarioDir, err)
	}
	for _, name := range InvariantNames() {
		reached := false
		for _, s := range all {
			if _, skipped := s.SkipInvariants[name]; !skipped {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("every scenario skips the %q invariant, so it checks nothing", name)
		}
	}
}

// TestEveryByIDReadHasParityCoverage keeps the parity invariant from going quietly out of date.
//
// The invariant compares a listing against the by-id read behind its rows, over a table this
// package holds. A by-id read added to the server later would simply not be compared, and the
// suite would keep reporting that lists and fetches agree while saying nothing about the new one.
// So the table is held against the routes the server actually mounts, and a route that is neither
// covered nor deliberately excluded fails here, naming itself.
func TestEveryByIDReadHasParityCoverage(t *testing.T) {
	t.Parallel()
	const routesFile = "../../internal/server/server.go"
	src, err := os.ReadFile(routesFile)
	if err != nil {
		t.Fatalf("read %s: %v", routesFile, err)
	}
	// The mounted by-id reads, as the mux declares them.
	pattern := regexp.MustCompile(`mux\.Handle\("GET /v1/([a-z-]+)/\{[a-z]+\}"\)?`)
	covered := map[string]bool{}
	for _, route := range parityRoutes {
		covered[route.Resource] = true
	}
	for _, m := range pattern.FindAllStringSubmatch(string(src), -1) {
		resource := m[1]
		if covered[resource] {
			continue
		}
		if why, excluded := parityExclusions[resource]; excluded {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s is excluded from parity coverage with no reason", resource)
			}
			continue
		}
		t.Errorf("GET /v1/%s/{id} is mounted and has no parity coverage: either add it to "+
			"parityRoutes with a fixture, or name it in parityExclusions with the reason. A by-id "+
			"read nobody compares against its listing is exactly where the two have drifted before.",
			resource)
	}
}

// TestMentionsReadsRowsNotBytes pins the difference between a row being in a list and the bytes of
// its id occurring somewhere in the response.
//
// A run's extra variables are free text somebody typed, and one of the suite's own scenarios plants
// a value in them. A run whose variables name another run's id made the substring form of this
// report that row as present in bodies it had been dropped from, which hides the exact list-versus-
// fetch disagreement the invariant exists to catch, and as present in a tenant's list that
// correctly excluded it, which invents one.
func TestMentionsReadsRowsNotBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Body is the response under test.
		Body string
		// ID is the row being looked for.
		ID string
		// Want is whether the body names that row.
		Want bool
	}{{ // Test 0: The row is in the list.
		Name: "listed", ID: "run_north", Want: true,
		Body: `{"runs":[{"id":"run_north","playbook":"site.yml"}],"count":1}`,
	}, { // Test 1: The id appears only as a value somebody typed into another row's variables.
		// That row is not in this list, and reading it as present masks the loss of a real row.
		Name: "only inside another row's variables", ID: "run_north", Want: false,
		Body: `{"runs":[{"id":"run_south","extra_vars":{"prior_run":"run_north"}}],"count":1}`,
	}, { // Test 2: A derived row references the run under the member that carries a run reference.
		Name: "referenced by a derived row", ID: "run_north", Want: true,
		Body: `{"host":"web1","runs":[{"run_id":"run_north","ok":1}],"count":1}`,
	}, { // Test 3: An empty list names nothing, whatever else the envelope carries.
		Name: "empty list", ID: "run_north", Want: false,
		Body: `{"runs":[],"count":0,"summary":{"total":3,"scope":"install"}}`,
	}, { // Test 4: A body that is not JSON still counts as naming the row, since a response
		// carrying the id is a disclosure whatever its shape.
		Name: "not json", ID: "run_north", Want: true,
		Body: `switchtender_run_last{run="run_north"} 1`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := mentions(test.Body, test.ID); got != test.Want {
				t.Errorf("%s: mentions(%q) = %v, want %v", test.Name, test.ID, got, test.Want)
			}
		})
	}
}

// TestEveryDerivedViewIsActuallyExercised proves the derived-view invariant is asking about
// something.
//
// checkDerivedViewsAgree holds a list of paths built out of runs against the by-id read behind
// them. A path the fixture language cannot put a row into is a path that answers an empty body for
// every caller, agrees with everything, and reports coverage that does not exist. Four of the five
// were in exactly that state: the estate needs host facts, the task table needs task durations, and
// the change log needs a change label, and nothing could seed any of them.
//
// So each listed path has to name at least one fixture run somewhere in the suite. A path that
// names none is either missing a fixture or does not belong in the list.
func TestEveryDerivedViewIsActuallyExercised(t *testing.T) {
	t.Parallel()
	all, err := Load(scenarioDir)
	if err != nil {
		t.Fatalf("Load(%s): %v", scenarioDir, err)
	}
	named := map[string]bool{}
	for _, s := range all {
		in, berr := Build(s, GrantsOpen, t.TempDir())
		if berr != nil {
			t.Fatalf("building %s: %v", s.Path(), berr)
		}
		probed := append([]string{}, derivedViews...)
		for path := range derivedAggregates {
			probed = append(probed, path)
		}
		for _, actor := range in.Users {
			for _, path := range probed {
				body := in.Get(path, actor).Body
				for _, r := range s.Fixtures.Runs {
					if mentions(body, r.ID) {
						named[path] = true
					}
				}
			}
		}
		in.Close()
	}
	for _, path := range derivedViews {
		if !named[path] {
			t.Errorf("%s is checked by derived_views_agree and no scenario puts a row in it, so "+
				"that invariant compares an empty body against every by-id answer and agrees with "+
				"all of them. Seed what the view is built from, or take the path out of the list.",
				path)
		}
	}
	// A path in neither list is a run-derived surface nobody decided about. The aggregates carry no
	// run id and are named with the reason they cannot be checked by id, so the gap is stated
	// rather than being an absence.
	for path, why := range derivedAggregates {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is listed as an aggregate with no reason", path)
		}
		if named[path] {
			t.Errorf("%s is listed as carrying no run id and a scenario found one in it, so it "+
				"belongs in derivedViews where the id invariant will check it", path)
		}
	}
}
