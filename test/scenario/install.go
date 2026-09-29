package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
)

// fixtureTime is the instant every fixture row is stamped from, so a scenario's ordering is the
// order it declared rather than whatever the clock did during the run.
var fixtureTime = time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)

// Install is a built scenario: the stores behind it, the handler in front of it, and the tokens
// that let a step act as any of its people.
type Install struct {
	// Scenario is what was declared.
	Scenario *Scenario
	// Mode is the grant mode this build runs in, since a scenario runs once per mode.
	Mode GrantMode
	// Runs is the run store, read directly by state assertions and by the invariants.
	Runs run.Store
	// Handler is the whole API, the same mux serve mounts.
	Handler http.Handler
	// Tokens maps a fixture user id to the bearer token that authenticates as them.
	Tokens map[string]string
	// Users is every fixture user id, in declared order, so the invariants can ask each in turn.
	Users []string
	// Roles maps a fixture user id to its global role, for the entitlements that turn on it.
	Roles map[string]string
	// Fakes are the stand-in dependencies, by dependency name, so a step or an assertion can ask
	// one how many times it was reached.
	Fakes map[string]Fake
	// Sealed are the ciphertexts of the declared credentials, hunted beside the plaintext, since
	// the material at rest leaving the install is also the material leaving the install.
	Sealed []SecretFixture
	// send carries one request to whatever is under test. It is the only seam between running
	// against handlers in this process and running against a deployed server, so every case and
	// every invariant is written once and asks the same questions of both.
	send transport
	// auditCount reports how many entries the audit chain holds, which is what makes "this write
	// was recorded" checkable after every write rather than asserted once and hoped for.
	auditCount func() (int, error)
	// runCount reports how many runs the install holds, optionally on one project. In process it
	// reads the store; against a deployed server it asks the API as an administrator, which is the
	// closest thing to the store a relying party has.
	runCount func(project string) (int, error)
	// closers release what the build took, in reverse order.
	closers []func()
}

// Close releases everything the install took.
func (in *Install) Close() {
	for i := len(in.closers) - 1; i >= 0; i-- {
		in.closers[i]()
	}
}

// Build constructs the install a scenario declares, in one grant mode.
//
// Everything here is the product's own: the stores it ships, the handler serve mounts, and the
// token middleware a real request passes through. A scenario that passed against a hand-rolled
// stand-in for any of those would be testing the stand-in.
func Build(s *Scenario, mode GrantMode, dir string) (*Install, error) {
	if target := s.Environment.Target; target != "" && target != TargetInProcess {
		return nil, fmt.Errorf("scenario %q targets %q, which this runner does not implement: it "+
			"must fail rather than pass, since a scenario that silently did not run reads as "+
			"covered", s.Name, target)
	}
	in := &Install{Scenario: s, Mode: mode, Tokens: map[string]string{},
		Roles: map[string]string{}, Fakes: map[string]Fake{}}
	ctx := context.Background()

	st, err := openStores(s.Environment.Store, dir)
	if err != nil {
		return nil, err
	}
	in.closers = append(in.closers, func() { _ = st.close() })
	in.Runs = st.runs

	if err := seedOrgs(ctx, st, s.Fixtures.Orgs); err != nil {
		return nil, err
	}
	if err := seedTeams(ctx, st, s.Fixtures.Teams); err != nil {
		return nil, err
	}
	if err := in.seedUsers(ctx, st, s.Fixtures.Users); err != nil {
		return nil, err
	}
	if err := seedProjects(ctx, st, s.Fixtures.Projects); err != nil {
		return nil, err
	}
	if err := seedGrants(ctx, st, s.Fixtures.Grants); err != nil {
		return nil, err
	}
	if err := seedRuns(ctx, st, s.Fixtures.Runs); err != nil {
		return nil, err
	}
	sealer := credential.NewSealer("scenario-passphrase", "scenario-salt")
	sealedForms, err := seedCredentials(ctx, st, sealer, s.Fixtures.Credentials)
	if err != nil {
		return nil, err
	}
	in.Sealed = sealedForms
	if err := seedInventories(ctx, st, s.Fixtures.Inventories); err != nil {
		return nil, err
	}
	if err := seedTemplates(ctx, st, s.Fixtures.Templates); err != nil {
		return nil, err
	}
	if err := seedSchedules(ctx, st, s.Fixtures.Schedules); err != nil {
		return nil, err
	}

	submitter, err := in.startFakes(s)
	if err != nil {
		return nil, err
	}

	opts := []server.Option{
		server.WithGrants(st.grants, mode == GrantsStrict),
		server.WithOrgs(st.orgs),
		server.WithProjects(st.projects),
		server.WithUsers(st.users),
		server.WithTokens(st.tokens),
		server.WithAudit(st.audits),
		server.WithCredentials(st.credentials, sealer),
		server.WithTemplates(st.templates),
		server.WithSchedules(st.schedules),
		server.WithInventories(st.inventories),
		server.WithTeams(st.teams),
	}
	if s.Environment.Producer {
		id, ierr := audit.LoadIdentity(filepath.Join(dir, "identity"))
		if ierr != nil {
			return nil, fmt.Errorf("producer identity: %w", ierr)
		}
		opts = append(opts, server.WithProducerIdentity(&id, "scenario"))
	}
	in.Handler = server.New(st.runs, submitter, zap.NewNop(), opts...).Handler()
	in.send = handlerTransport{handler: in.Handler}
	in.auditCount = func() (int, error) {
		chain, cerr := st.audits.Chain(ctx)
		if cerr != nil {
			return 0, cerr
		}
		return len(chain), nil
	}
	in.runCount = func(project string) (int, error) {
		rows, lerr := st.runs.ListPage(ctx, run.ListFilter{}, 0, 0)
		if lerr != nil {
			return 0, lerr
		}
		n := 0
		for _, r := range rows {
			if project == "" || r.ProjectID == project {
				n++
			}
		}
		return n, nil
	}
	return in, nil
}

// startFakes brings up every stand-in the scenario declared and returns the submitter the server is
// built on, which is a stand-in whether or not the scenario named one.
func (in *Install) startFakes(s *Scenario) (server.Submitter, error) {
	submitter := &fakeSubmitter{}
	for name, dep := range s.Dependencies {
		if dep.Mode == ModeReal || dep.Mode == "" {
			return nil, fmt.Errorf("scenario %q wants the real %s, which the in-process target "+
				"cannot provide: declare a fake, or move the scenario to the kind target",
				s.Name, name)
		}
		f, err := newFake(name, dep)
		if err != nil {
			return nil, err
		}
		if _, err := f.Start(); err != nil {
			return nil, fmt.Errorf("start %s stand-in: %w", name, err)
		}
		in.closers = append(in.closers, f.Stop)
		in.Fakes[name] = f
		if sub, ok := f.(*fakeSubmitter); ok {
			submitter = sub
		}
	}
	if _, named := in.Fakes["submitter"]; !named {
		in.Fakes["submitter"] = submitter
	}
	return submitter, nil
}

// stores is the set of stores an install is built on, from whichever backend it declared.
type stores struct {
	// runs holds run records.
	runs run.Store
	// users holds accounts.
	users user.Store
	// orgs holds organizations and their membership.
	orgs org.Store
	// projects holds projects.
	projects project.Store
	// grants holds object grants.
	grants grant.Store
	// tokens holds API tokens.
	tokens auth.Store
	// audits holds the audit chain.
	audits audit.Store
	// credentials holds sealed credentials.
	credentials credential.Store
	// templates holds stored job templates.
	templates template.Store
	// schedules holds recurring runs.
	schedules schedule.Store
	// inventories holds stored inventories.
	inventories inventory.Store
	// teams holds teams and their membership.
	teams team.Store
	// close releases the backend.
	close func() error
}

// openStores builds the declared backend. Memory is the default because it answers the same
// questions about the server layer in a fraction of the time; sqlite is there for the scenarios
// whose question is about the SQL.
func openStores(backend, dir string) (*stores, error) {
	switch backend {
	case "", "memory":
		return &stores{
			runs: run.NewMemStore(), users: user.NewMemStore(), orgs: org.NewMemStore(),
			projects: project.NewMemStore(), grants: grant.NewMemStore(),
			tokens: auth.NewMemStore(), audits: audit.NewMemStore(),
			credentials: credential.NewMemStore(), templates: template.NewMemStore(),
			schedules: schedule.NewMemStore(), inventories: inventory.NewMemStore(),
			teams: team.NewMemStore(),
			close: func() error { return nil },
		}, nil
	case "sqlite":
		db, err := sqlitestore.Open(filepath.Join(dir, "scenario.db"))
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		return &stores{
			runs: db.Runs(), users: db.Users(), orgs: db.Orgs(), projects: db.Projects(),
			grants: db.Grants(), tokens: db.Tokens(), audits: db.Audits(),
			credentials: db.Credentials(), templates: db.Templates(),
			schedules: db.Schedules(), inventories: db.Inventories(), teams: db.Teams(),
			close: db.Close,
		}, nil
	default:
		return nil, fmt.Errorf("unknown store backend %q", backend)
	}
}

// seedOrgs writes the organizations and their membership.
func seedOrgs(ctx context.Context, st *stores, orgs []OrgFixture) error {
	for _, o := range orgs {
		if err := st.orgs.Save(ctx, &org.Org{ID: o.ID, Name: o.ID}); err != nil {
			return fmt.Errorf("save org %s: %w", o.ID, err)
		}
		for uid, role := range o.Members {
			r := org.Role(role)
			if r != org.RoleAdmin && r != org.RoleMember {
				return fmt.Errorf("org %s: member %s has role %q", o.ID, uid, role)
			}
			if err := st.orgs.AddMember(ctx, o.ID, uid, r); err != nil {
				return fmt.Errorf("add %s to %s: %w", uid, o.ID, err)
			}
		}
	}
	return nil
}

// seedUsers writes the accounts and issues each one a token, so any step can act as any of them
// through the same middleware a real request passes.
func (in *Install) seedUsers(ctx context.Context, st *stores, users []UserFixture) error {
	for _, u := range users {
		role := user.Role(u.Role)
		if !user.ValidRole(role) {
			return fmt.Errorf("user %s has role %q", u.ID, u.Role)
		}
		acct, err := user.New(u.Name, "scenario-password", role)
		if err != nil {
			return fmt.Errorf("new user %s: %w", u.ID, err)
		}
		acct.ID = u.ID
		if err := st.users.Save(ctx, acct); err != nil {
			return fmt.Errorf("save user %s: %w", u.ID, err)
		}
		plain, tok, err := auth.New(u.Name)
		if err != nil {
			return fmt.Errorf("mint token for %s: %w", u.ID, err)
		}
		tok.UserID = u.ID
		if err := st.tokens.Save(ctx, tok); err != nil {
			return fmt.Errorf("save token for %s: %w", u.ID, err)
		}
		in.Tokens[u.ID] = plain
		in.Roles[u.ID] = u.Role
		in.Users = append(in.Users, u.ID)
	}
	return nil
}

// seedProjects writes the projects.
func seedProjects(ctx context.Context, st *stores, projects []ProjectFixture) error {
	for _, p := range projects {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		if err := st.projects.Save(ctx, &project.Project{
			ID: p.ID, Name: name, RepoURL: "https://example.com/" + p.ID + ".git", OrgID: p.Org,
		}); err != nil {
			return fmt.Errorf("save project %s: %w", p.ID, err)
		}
	}
	return nil
}

// seedGrants writes the object grants.
func seedGrants(ctx context.Context, st *stores, grants []GrantFixture) error {
	for i, g := range grants {
		access := grant.Access(g.Access)
		switch access {
		case grant.AccessRead, grant.AccessUse, grant.AccessManage:
		default:
			return fmt.Errorf("grant %d names access %q", i, g.Access)
		}
		if err := st.grants.Save(ctx, &grant.Grant{
			ID: fmt.Sprintf("grant_%d", i), Subject: g.Subject, Object: g.Object,
			Access: access, CreatedAt: fixtureTime,
		}); err != nil {
			return fmt.Errorf("save grant %d: %w", i, err)
		}
	}
	return nil
}

// seedRuns writes the run records and the per-host summary each one produced.
//
// The summary is written while the run is still going, because the store fences a terminal run's
// summary against a reclaimed worker overwriting it. Writing it after the run finished stored
// nothing, and every derived view then had no rows to disagree about.
func seedRuns(ctx context.Context, st *stores, runs []RunFixture) error {
	for i, rf := range runs {
		vars := make(map[string]any, len(rf.ExtraVars))
		for k, v := range rf.ExtraVars {
			vars[k] = v
		}
		r := &run.Run{
			ID: rf.ID, Playbook: "site.yml", Inventory: "prod", ProjectID: rf.Project,
			InventoryID: rf.Inventory, CredentialIDs: rf.Credentials,
			OrgID: rf.Org, Status: run.StatusRunning, ExtraVars: vars,
			CreatedAt: fixtureTime.Add(time.Duration(i) * time.Minute),
		}
		if rf.Change != "" {
			r.Labels = map[string]string{run.ChangeLabel: rf.Change}
		}
		if err := st.runs.Save(ctx, r); err != nil {
			return fmt.Errorf("save run %s: %w", rf.ID, err)
		}
		if rf.Host != "" {
			if err := st.runs.SaveHostSummary(ctx, rf.ID,
				[]run.HostSummary{{Host: rf.Host, OK: 1, Changed: 1}}); err != nil {
				return fmt.Errorf("save host summary for %s: %w", rf.ID, err)
			}
			if len(rf.Facts) > 0 {
				if err := st.runs.SaveHostFacts(ctx, rf.ID, []run.HostFacts{{
					Host: rf.Host, Facts: rf.Facts, RunID: rf.ID, GatheredAt: r.CreatedAt,
				}}); err != nil {
					return fmt.Errorf("save host facts for %s: %w", rf.ID, err)
				}
			}
		}
		if len(rf.Tasks) > 0 {
			tasks := make([]run.TaskSummary, 0, len(rf.Tasks))
			for name, seconds := range rf.Tasks {
				tasks = append(tasks, run.TaskSummary{RunID: rf.ID, Task: name, Seconds: seconds})
			}
			if err := st.runs.SaveTaskSummary(ctx, rf.ID, tasks); err != nil {
				return fmt.Errorf("save task summaries for %s: %w", rf.ID, err)
			}
		}
		r.Status = run.StatusSucceeded
		if err := st.runs.Save(ctx, r); err != nil {
			return fmt.Errorf("finish run %s: %w", rf.ID, err)
		}
	}
	return nil
}

// seedCredentials writes the sealed credentials.
//
// The material is sealed the way the product seals it rather than stored as given, so a response
// that returned it would have had to open it, which is the leak worth catching rather than a
// fixture shortcut that could never happen in a deployment.
func seedCredentials(ctx context.Context, st *stores, sealer *credential.Sealer,
	creds []CredentialFixture) (sealedForms []SecretFixture, err error) {
	for _, c := range creds {
		kind := credential.Kind(c.Kind)
		if kind == "" {
			kind = credential.KindSSHKey
		}
		sealed, serr := sealer.Seal(c.Secret)
		if serr != nil {
			return nil, fmt.Errorf("seal credential %s: %w", c.ID, serr)
		}
		// The ciphertext is hunted alongside the plaintext. Shipping the material at rest out of
		// the install is still shipping the material out: it reduces a sealed secret to a
		// passphrase somebody now has an offline copy to guess against.
		sealedForms = append(sealedForms,
			SecretFixture{Name: "sealed credential " + c.ID, Value: sealed})
		name := c.Name
		if name == "" {
			name = c.ID
		}
		if err := st.credentials.Save(ctx, &credential.Credential{
			ID: c.ID, Name: name, Kind: kind, OrgID: c.Org, Secret: sealed,
			CreatedAt: fixtureTime,
		}); err != nil {
			return nil, fmt.Errorf("save credential %s: %w", c.ID, err)
		}
	}
	return sealedForms, nil
}

// seedTeams writes the teams and their membership.
func seedTeams(ctx context.Context, st *stores, teams []TeamFixture) error {
	for _, t := range teams {
		if err := st.teams.Save(ctx, &team.Team{ID: t.ID, Name: t.ID, CreatedAt: fixtureTime}); err != nil {
			return fmt.Errorf("save team %s: %w", t.ID, err)
		}
		for _, member := range t.Members {
			if err := st.teams.AddMember(ctx, t.ID, member); err != nil {
				return fmt.Errorf("add %s to %s: %w", member, t.ID, err)
			}
		}
	}
	return nil
}

// seedInventories writes the stored inventories.
func seedInventories(ctx context.Context, st *stores, inventories []InventoryFixture) error {
	for _, inv := range inventories {
		name := inv.Name
		if name == "" {
			name = inv.ID
		}
		if err := st.inventories.Save(ctx, &inventory.Inventory{
			ID: inv.ID, Name: name, Content: "web1\nweb2\n", OrgID: inv.Org,
			CreatedAt: fixtureTime,
		}); err != nil {
			return fmt.Errorf("save inventory %s: %w", inv.ID, err)
		}
	}
	return nil
}

// seedTemplates writes the stored job templates.
func seedTemplates(ctx context.Context, st *stores, templates []TemplateFixture) error {
	for _, tf := range templates {
		vars := make(map[string]any, len(tf.ExtraVars))
		for k, v := range tf.ExtraVars {
			vars[k] = v
		}
		name := tf.Name
		if name == "" {
			name = tf.ID
		}
		if err := st.templates.Save(ctx, &template.Template{
			ID: tf.ID, Name: name, ProjectID: tf.Project, OrgID: tf.Org,
			Playbook: "site.yml", ExtraVars: vars, CreatedAt: fixtureTime,
		}); err != nil {
			return fmt.Errorf("save template %s: %w", tf.ID, err)
		}
	}
	return nil
}

// seedSchedules writes the recurring runs.
func seedSchedules(ctx context.Context, st *stores, schedules []ScheduleFixture) error {
	for _, sf := range schedules {
		cron := sf.Cron
		if cron == "" {
			cron = "0 * * * *"
		}
		name := sf.Name
		if name == "" {
			name = sf.ID
		}
		sc := &schedule.Schedule{
			ID: sf.ID, Name: name, Cron: cron, TemplateID: sf.Template, OrgID: sf.Org,
			CreatedAt: fixtureTime,
		}
		if sf.Template == "" {
			sc.Playbook, sc.Inventory = "site.yml", "prod"
		}
		if err := st.schedules.Save(ctx, sc); err != nil {
			return fmt.Errorf("save schedule %s: %w", sf.ID, err)
		}
	}
	return nil
}

// Response is one answered request.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Body is the whole response body as text, which is what the assertions read.
	Body string
}

// JSON decodes the body into v, reporting a decode failure as an error rather than an empty value.
func (r Response) JSON(v any) error { return json.Unmarshal([]byte(r.Body), v) }

// transport carries one request to the install under test and returns what it answered.
//
// It is the single seam between the two targets. Everything above it, every case and every
// invariant, is written once and asks the same questions of a handler in this process and of a
// server deployed in a cluster, which is what keeps the two from drifting into different suites
// that happen to share a file format.
type transport interface {
	// send issues one request, with the given bearer token or none when it is empty.
	send(method, path, bearer string, body io.Reader) Response
}

// handlerTransport serves requests from the handler mux in this process.
type handlerTransport struct {
	// handler is the whole API, the same mux serve mounts.
	handler http.Handler
}

// send dispatches straight to the handler, with no network in the way.
func (t handlerTransport) send(method, path, bearer string, body io.Reader) Response {
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return Response{Status: rec.Code, Body: rec.Body.String()}
}

// Do issues one request as the given fixture user, or with no credentials when as is empty.
func (in *Install) Do(method, path, as string, body io.Reader) Response {
	return in.send.send(method, path, in.Tokens[as], body)
}

// Get issues a read as the given fixture user.
func (in *Install) Get(path, as string) Response { return in.Do(http.MethodGet, path, as, nil) }

// Post issues a write as the given fixture user, with the given body.
func (in *Install) Post(path, as string, body []byte) Response {
	return in.Do(http.MethodPost, path, as, bytes.NewReader(body))
}
