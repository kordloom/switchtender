// Package scenario is the declarative test language for a whole SwitchTender install.
//
// A scenario file names four things: the environment the install runs in, the dependencies it
// stands on and whether each is real or standing in, the fixtures and cases to drive through it,
// and what must be true afterward. The runner builds that install for real, out of the stores and
// handlers the product ships, and then holds the result against a battery of invariants every
// install must satisfy whatever the scenario was written for.
//
// The battery is the reason this exists. The defects that survive ordinary unit tests are not wrong
// endpoints, they are endpoints that are individually right and collectively inconsistent: a list
// that shows what a fetch refuses, a derived view that names a run both of them hid, a receipt that
// verifies under one of an install's names and not the other. No test of one handler can see any of
// those, because each handler is correct on its own terms. So every scenario, whatever it declares,
// is checked for all of them, as every actor it defines.
//
// Scenarios run in both grant modes by default. What --strict-grants decides is narrow and
// documented, so a difference anywhere else is a finding, and that comparison is free once the same
// scenario has run twice.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Target names where a scenario's install runs.
type Target string

const (
	// TargetInProcess builds the stores and handlers in the test process. It answers every question
	// about the server layer at the speed that lets the whole matrix run on every push.
	TargetInProcess Target = "inprocess"
	// TargetKind deploys the built image into a Kind cluster through the shipped chart, which is
	// the only place the packaging, the migrations, and the upgrade path are real.
	TargetKind Target = "kind"
)

// GrantMode names how an install treats an object nobody has granted.
type GrantMode string

const (
	// GrantsOpen is the default install: an ungranted object defers to the caller's global role.
	GrantsOpen GrantMode = "open"
	// GrantsStrict denies a non-admin any object nobody has granted them.
	GrantsStrict GrantMode = "strict"
	// GrantsBoth runs the scenario once in each mode, which is the default when none is named.
	GrantsBoth GrantMode = "both"
)

// DependencyMode says whether a dependency is the real thing, a stand-in, or a stand-in that fails.
type DependencyMode string

const (
	// ModeReal uses the actual dependency, which needs the environment to provide it.
	ModeReal DependencyMode = "real"
	// ModeFake uses a registered stand-in that behaves like the real one on the paths both support.
	ModeFake DependencyMode = "fake"
	// ModeBroken uses a stand-in configured to fail, which is how a failure path is exercised
	// without waiting for the real dependency to have a bad day.
	ModeBroken DependencyMode = "broken"
)

// Scenario is one declared install and what must be true of it.
type Scenario struct {
	// Name identifies the scenario in test output. It reads as a sentence about the install.
	Name string `yaml:"name"`
	// Why says what defect or property this scenario exists for, in the words a reader needs to
	// decide whether a failure matters. A scenario with no why is refused by the loader.
	Why string `yaml:"why"`
	// Environment is where and how the install runs.
	Environment Environment `yaml:"environment"`
	// Dependencies are the outside systems the install stands on, and what stands in for each.
	Dependencies map[string]Dependency `yaml:"dependencies"`
	// Fixtures are the objects and people every case in this file starts with.
	Fixtures Fixtures `yaml:"fixtures"`
	// Cases are the runs of steps to drive, each with its own name and reason.
	Cases []Case `yaml:"cases"`
	// SkipInvariants names invariants this scenario deliberately does not satisfy, each with the
	// reason. An install meant to be inconsistent has to say which consistency it breaks and why,
	// rather than quietly opting out of the battery.
	SkipInvariants map[string]string `yaml:"skip_invariants"`
	// path is where the scenario was loaded from, for failure messages.
	path string
}

// Environment is the deployment shape a scenario runs against.
type Environment struct {
	// Target is where the install runs, defaulting to inprocess.
	Target Target `yaml:"target"`
	// Grants is the grant mode, defaulting to both.
	Grants GrantMode `yaml:"grants"`
	// Store names the backing store: memory or sqlite. Sqlite exercises the SQL the product ships
	// with; memory is faster and answers the same questions about the server layer.
	Store string `yaml:"store"`
	// Tier is the licensed tier: community or team. A scenario about a paid feature says so here
	// rather than assuming whatever the runner happened to configure.
	Tier string `yaml:"tier"`
	// Producer gives the install a signing identity, so the audit chain binds to it and receipts
	// can be verified. Off by default, since most scenarios export nothing.
	Producer bool `yaml:"producer"`
	// Env are environment variables set for the install, for the settings that are only reachable
	// that way.
	Env map[string]string `yaml:"env"`
	// Image describes how to build the product image. It is read only by the kind target.
	Image Image `yaml:"image"`
}

// Image is how the kind target builds what it deploys.
type Image struct {
	// From names the release to start at, for an upgrade scenario. Empty builds only the working
	// tree, so nothing is upgraded from.
	From string `yaml:"from"`
	// Build sets whether the working tree is built into an image. Default true on the kind target.
	Build *bool `yaml:"build"`
}

// Dependency is one outside system and what stands in for it.
type Dependency struct {
	// Mode is real, fake, or broken.
	Mode DependencyMode `yaml:"mode"`
	// Provider names the stand-in. "go:name" selects a registered Go fake, compiled with the suite
	// so the compiler still checks it; "script:path" runs an executable for anything not worth a
	// Go fake. Empty with a fake or broken mode selects the registered fake of the dependency's
	// own name.
	Provider string `yaml:"provider"`
	// Behavior tunes the stand-in: how many calls succeed before it fails, what it fails with, how
	// slow it is. What each key means belongs to the fake, which documents its own.
	Behavior Behavior `yaml:"behavior"`
}

// Behavior is the knobs a stand-in dependency answers to.
type Behavior struct {
	// FailAfter is how many calls succeed before every later one fails. Zero fails none.
	FailAfter int `yaml:"fail_after"`
	// Status is the HTTP status a failing call answers with, where the dependency speaks HTTP.
	Status int `yaml:"status"`
	// Message is the error text a failing call returns, so a scenario can assert what an operator
	// is actually told.
	Message string `yaml:"message"`
	// Latency delays every call, for the timeout and cancellation paths.
	Latency time.Duration `yaml:"latency"`
	// Unavailable refuses every call from the start, which is the dependency being down rather
	// than flaky.
	Unavailable bool `yaml:"unavailable"`
}

// Fixtures are the rows an install starts with.
type Fixtures struct {
	// Orgs are the organizations, each with the members it holds and their org roles.
	Orgs []OrgFixture `yaml:"orgs"`
	// Users are the accounts. Every one is issued a token, so a step may act as any of them.
	Users []UserFixture `yaml:"users"`
	// Teams are the groups a grant may be written to, which is the delegation an operator reaches
	// for when the same access has to go to several people and keep going to them as the group
	// changes.
	Teams []TeamFixture `yaml:"teams"`
	// Projects are the projects, optionally owned by an organization.
	Projects []ProjectFixture `yaml:"projects"`
	// Grants are the object grants.
	Grants []GrantFixture `yaml:"grants"`
	// Runs are the run records, each with the host it touched so the derived views have rows.
	Runs []RunFixture `yaml:"runs"`
	// Credentials are the sealed secrets, which are the objects whose material must never leave the
	// install at all, through any response, at any role.
	Credentials []CredentialFixture `yaml:"credentials"`
	// Inventories are the stored inventories a run can target, which govern reading a run the same
	// way its project does.
	Inventories []InventoryFixture `yaml:"inventories"`
	// Templates are the stored job templates, which carry a command and variables and so are one of
	// the places a value somebody pasted ends up.
	Templates []TemplateFixture `yaml:"templates"`
	// Schedules are the recurring runs. They have a by-id read, so they are under the parity
	// invariant as well as the leak one.
	Schedules []ScheduleFixture `yaml:"schedules"`
	// Secrets are values that must never appear in any response. Each is planted where the scenario
	// says and then looked for everywhere.
	Secrets []SecretFixture `yaml:"secrets"`
}

// OrgFixture is one organization and its membership.
type OrgFixture struct {
	// ID is the organization id, which is also a grant subject.
	ID string `yaml:"id"`
	// Members maps a user id to its organization role, admin or member.
	Members map[string]string `yaml:"members"`
}

// UserFixture is one account.
type UserFixture struct {
	// ID is the user id, which is also a grant subject.
	ID string `yaml:"id"`
	// Name is the login name.
	Name string `yaml:"name"`
	// Role is the global role: admin, operator, or viewer.
	Role string `yaml:"role"`
}

// TeamFixture is one team and its membership.
type TeamFixture struct {
	// ID is the team id, which is also a grant subject.
	ID string `yaml:"id"`
	// Members are the user ids in the team.
	Members []string `yaml:"members"`
}

// ProjectFixture is one project.
type ProjectFixture struct {
	// ID is the project id, which is a grant object.
	ID string `yaml:"id"`
	// Name is the display name.
	Name string `yaml:"name"`
	// Org is the owning organization id, empty for an unowned project.
	Org string `yaml:"org"`
}

// GrantFixture is one object grant.
type GrantFixture struct {
	// Subject is the user, team, or organization the grant is written to.
	Subject string `yaml:"subject"`
	// Object is the object id the grant is written on.
	Object string `yaml:"object"`
	// Access is the level: read, use, or manage.
	Access string `yaml:"access"`
}

// RunFixture is one recorded run.
type RunFixture struct {
	// ID is the run id.
	ID string `yaml:"id"`
	// Project is the project the run used, which is one of the objects that governs reading it.
	Project string `yaml:"project"`
	// Inventory is the stored inventory the run targeted, a second object governing the read. A run
	// is readable only when every object it names is, so a run naming several is where that rule is
	// actually exercised.
	Inventory string `yaml:"inventory"`
	// Credentials are the credentials the run used, each one governing the read as well.
	Credentials []string `yaml:"credentials"`
	// Org is the organization the run was stamped with, empty for an unowned run.
	Org string `yaml:"org"`
	// Host is the host the run touched, so fleet, drift and host history have a row for it.
	Host string `yaml:"host"`
	// ExtraVars are the run's variables, where a survey answer lands.
	ExtraVars map[string]string `yaml:"extra_vars"`
	// Change groups this run with others into one change, which is what an auditor asks about and
	// what /v1/changes is built from.
	Change string `yaml:"change"`
	// Facts are the system facts this run gathered on its host, which is what /v1/estate is built
	// from. Without them that view has no row to decide, so an invariant asking about it asks
	// about nothing.
	Facts map[string]string `yaml:"facts"`
	// Tasks maps a task name to its duration in seconds, which is what /v1/tasks is built from.
	Tasks map[string]float64 `yaml:"tasks"`
}

// InventoryFixture is one stored inventory.
type InventoryFixture struct {
	// ID is the inventory id, which is a grant object.
	ID string `yaml:"id"`
	// Name labels it.
	Name string `yaml:"name"`
	// Org is the owning organization id, empty for an unowned inventory.
	Org string `yaml:"org"`
}

// TemplateFixture is one stored job template.
type TemplateFixture struct {
	// ID is the template id, which is a grant object.
	ID string `yaml:"id"`
	// Name labels it.
	Name string `yaml:"name"`
	// Project is the project the template sources its playbook from.
	Project string `yaml:"project"`
	// Org is the owning organization id, empty for an unowned template.
	Org string `yaml:"org"`
	// ExtraVars are the template's variables, which is where a value an operator pasted lands.
	ExtraVars map[string]string `yaml:"extra_vars"`
}

// ScheduleFixture is one recurring run.
type ScheduleFixture struct {
	// ID is the schedule id.
	ID string `yaml:"id"`
	// Name labels it.
	Name string `yaml:"name"`
	// Cron is the cadence, defaulting to hourly.
	Cron string `yaml:"cron"`
	// Template is the stored template it fires, empty for an inline playbook.
	Template string `yaml:"template"`
	// Org is the owning organization id, empty for an unowned schedule.
	Org string `yaml:"org"`
}

// CredentialFixture is one sealed credential.
type CredentialFixture struct {
	// ID is the credential id, which is a grant object.
	ID string `yaml:"id"`
	// Name labels it.
	Name string `yaml:"name"`
	// Kind classifies the secret, defaulting to an SSH key.
	Kind string `yaml:"kind"`
	// Org is the owning organization id, empty for an unowned credential.
	Org string `yaml:"org"`
	// Secret is the material to seal. It is entitled to nobody: no role, no grant, and no endpoint
	// may return it, which is why it is registered as a secret with no owning run.
	Secret string `yaml:"secret"`
}

// SecretFixture is a value and who is entitled to see it.
type SecretFixture struct {
	// Name labels the secret in a failure message.
	Name string `yaml:"name"`
	// Value is the literal string to hunt for in every response body.
	Value string `yaml:"value"`
	// OnRun is the run the value was planted on. A caller who may read that run may read the value
	// in it, so the leak is the value reaching anyone the run itself is refused to.
	OnRun string `yaml:"on_run"`
	// ToRoles are the global roles entitled to the value whatever else is true, for a value the
	// install deliberately shows one audience and redacts for the rest. A template variable whose
	// name looks like a secret is the case: it is the template's own content to an administrator
	// and is masked for everyone else, so both halves have to be declared or the scenario asserts
	// only the half that happens to pass.
	ToRoles []string `yaml:"to_roles"`
	// A value with neither OnRun nor ToRoles is entitled to nobody and must never appear at all,
	// which is what a credential's material is.
}

// Case is one named run of steps through the install.
type Case struct {
	// Name identifies the case in test output.
	Name string `yaml:"name"`
	// Why says what this case establishes. A case with no why is refused by the loader.
	Why string `yaml:"why"`
	// Steps are the requests to make, in order.
	Steps []Step `yaml:"steps"`
}

// Step is one request and what it must answer.
type Step struct {
	// Note says what the step is establishing, and appears in the failure.
	Note string `yaml:"note"`
	// As is the user id to act as. Empty acts with no credentials at all.
	As string `yaml:"as"`
	// Get is the path to read. Exactly one verb may be set on a step.
	Get string `yaml:"get"`
	// Post is the path to write to.
	Post string `yaml:"post"`
	// Body is the JSON request body for a write, as YAML. A scenario about bad input writes the
	// bad input here, including the shapes a typed client could not produce.
	Body map[string]any `yaml:"body"`
	// RawBody is the request body as a literal string, for input no YAML mapping can express:
	// malformed JSON, a truncated document, a field with the wrong type.
	RawBody string `yaml:"raw_body"`
	// Expect is what the response must satisfy.
	Expect Expect `yaml:"expect"`
}

// Expect is the assertion on one response and on the state behind it.
type Expect struct {
	// Status is the required HTTP status. Zero accepts any status the other assertions allow.
	Status int `yaml:"status"`
	// Includes are substrings the body must contain.
	Includes []string `yaml:"includes"`
	// Excludes are substrings the body must not contain.
	Excludes []string `yaml:"excludes"`
	// State are assertions about what the install holds after the step, which is how a write that
	// answers 200 and stores nothing is caught.
	State []StateCheck `yaml:"state"`
}

// StateCheck is one assertion about stored state rather than about a response.
type StateCheck struct {
	// Runs counts run records matching the filter below, which is enough for the writes a scenario
	// makes today. It grows a field at a time, deliberately, so the language never claims to check
	// something the runner does not.
	Runs *RunStateCheck `yaml:"runs"`
}

// RunStateCheck asserts what the run store holds.
type RunStateCheck struct {
	// Count is how many runs must match.
	Count int `yaml:"count"`
	// Project restricts the count to runs on one project, empty for every run.
	Project string `yaml:"project"`
}

// Verb returns the step's HTTP method and path, and an error when a step names none or several.
func (s Step) Verb() (method, path string, err error) {
	switch {
	case s.Get != "" && s.Post != "":
		return "", "", fmt.Errorf("step %q names both get and post", s.Note)
	case s.Get != "":
		return "GET", s.Get, nil
	case s.Post != "":
		return "POST", s.Post, nil
	default:
		return "", "", fmt.Errorf("step %q names no request", s.Note)
	}
}

// Modes returns the grant modes this scenario runs in.
func (s *Scenario) Modes() []GrantMode {
	switch s.Environment.Grants {
	case GrantsOpen, GrantsStrict:
		return []GrantMode{s.Environment.Grants}
	default:
		return []GrantMode{GrantsOpen, GrantsStrict}
	}
}

// Path returns where the scenario was loaded from.
func (s *Scenario) Path() string { return s.path }

// Load reads every scenario in dir, refusing one that is malformed rather than skipping it.
//
// A scenario that fails to load is a scenario that is not running, and a suite that quietly runs
// fewer cases than it holds reports a pass it did not earn.
func Load(dir string) ([]*Scenario, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no scenarios in %s", dir)
	}
	out := make([]*Scenario, 0, len(names))
	for _, name := range names {
		s, err := loadOne(name)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// loadOne reads and validates one scenario file.
func loadOne(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// An unknown field is a typo in a test, which is worse than a typo in code: it reads as a case
	// that is covered and is not.
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.path = path
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// validate refuses a scenario that cannot mean what it says.
func (s *Scenario) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("scenario has no name")
	}
	if strings.TrimSpace(s.Why) == "" {
		return fmt.Errorf("scenario %q has no why: a case nobody can explain is a case nobody can "+
			"judge the failure of", s.Name)
	}
	if err := s.Environment.validate(s.Name); err != nil {
		return err
	}
	for name, dep := range s.Dependencies {
		if err := dep.validate(s.Name, name); err != nil {
			return err
		}
	}
	users := make(map[string]bool, len(s.Fixtures.Users))
	for _, u := range s.Fixtures.Users {
		if u.ID == "" || u.Name == "" || u.Role == "" {
			return fmt.Errorf("scenario %q has a user missing id, name or role", s.Name)
		}
		users[u.ID] = true
	}
	if len(s.Cases) == 0 {
		return fmt.Errorf("scenario %q declares no cases", s.Name)
	}
	for _, c := range s.Cases {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("scenario %q has an unnamed case", s.Name)
		}
		if strings.TrimSpace(c.Why) == "" {
			return fmt.Errorf("scenario %q case %q has no why", s.Name, c.Name)
		}
		for i, st := range c.Steps {
			if _, _, err := st.Verb(); err != nil {
				return fmt.Errorf("scenario %q case %q step %d: %w", s.Name, c.Name, i, err)
			}
			if st.Body != nil && st.RawBody != "" {
				return fmt.Errorf("scenario %q case %q step %d names both body and raw_body",
					s.Name, c.Name, i)
			}
			if st.As != "" && !users[st.As] {
				return fmt.Errorf("scenario %q case %q step %d acts as %q, which is not a fixture "+
					"user", s.Name, c.Name, i, st.As)
			}
		}
	}
	for name, why := range s.SkipInvariants {
		if !knownInvariant(name) {
			return fmt.Errorf("scenario %q skips unknown invariant %q", s.Name, name)
		}
		if strings.TrimSpace(why) == "" {
			return fmt.Errorf("scenario %q skips %q with no reason", s.Name, name)
		}
	}
	return nil
}

// validate refuses an environment the runner cannot build.
func (e Environment) validate(scenario string) error {
	switch e.Target {
	case "", TargetInProcess, TargetKind:
	default:
		return fmt.Errorf("scenario %q names target %q", scenario, e.Target)
	}
	switch e.Grants {
	case "", GrantsOpen, GrantsStrict, GrantsBoth:
	default:
		return fmt.Errorf("scenario %q names grant mode %q", scenario, e.Grants)
	}
	switch e.Store {
	case "", "memory", "sqlite":
	default:
		return fmt.Errorf("scenario %q names store %q", scenario, e.Store)
	}
	switch e.Tier {
	case "", "community", "team":
	default:
		return fmt.Errorf("scenario %q names tier %q", scenario, e.Tier)
	}
	return nil
}

// validate refuses a dependency declaration that names something that does not exist.
//
// A stand-in nobody registered is the worst kind of test failure: silent. The scenario reads as
// covering a Vault outage and covers nothing, because the dependency it names was never wired.
func (d Dependency) validate(scenario, name string) error {
	switch d.Mode {
	case "", ModeReal, ModeFake, ModeBroken:
	default:
		return fmt.Errorf("scenario %q dependency %q names mode %q", scenario, name, d.Mode)
	}
	if d.Mode == ModeReal || d.Mode == "" {
		return nil
	}
	provider := d.Provider
	if provider == "" {
		provider = "go:" + name
	}
	switch {
	case strings.HasPrefix(provider, "go:"):
		if !knownFake(strings.TrimPrefix(provider, "go:")) {
			return fmt.Errorf("scenario %q dependency %q names Go fake %q, which is not "+
				"registered: a stand-in nobody wired makes the scenario read as covering a "+
				"failure it never reaches", scenario, name, provider)
		}
	case strings.HasPrefix(provider, "script:"):
		path := strings.TrimPrefix(provider, "script:")
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("scenario %q dependency %q names script %q: %w",
				scenario, name, path, err)
		}
	default:
		return fmt.Errorf("scenario %q dependency %q names provider %q, which is neither a go: "+
			"nor a script: provider", scenario, name, provider)
	}
	return nil
}
