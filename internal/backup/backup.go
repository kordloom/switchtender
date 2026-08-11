// Package backup writes and reads a portable snapshot of a SwitchTender control plane: its
// credentials, projects, templates, inventories, inventory sources, schedules, triggers, and the
// identity and access objects. The snapshot is a logical export, so it restores into either the
// SQLite or the PostgreSQL backend, which makes it a migration tool as well as a disaster-recovery
// one.
//
// The whole payload is sealed with the deployment's AES-256-GCM key before it touches disk, so the
// file is confidential and tamper-evident: it never exposes configuration, password hashes, or
// sealed secrets in the clear, and a restore into a deployment with a different key, or of an altered
// file, fails the GCM authentication check instead of importing bad data. Run history and the audit
// chain are out of scope: the audit chain has its own signed, self-verifying export.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/team"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

const (
	// Format identifies a SwitchTender backup file and guards against reading an unrelated file.
	Format = "switchtender-backup"
	// Version is the payload schema version. A file at a different version is refused rather than
	// misread.
	// Version 2 moved the snapshot time inside the seal. A version 1 file is refused rather than
	// read, because its header was unauthenticated.
	Version = 2
	// maxPayloadBytes bounds how much a backup may expand to. A sealed file is compressed, so a
	// small one can decompress to an unbounded amount of memory.
	maxPayloadBytes = 1 << 30
)

var (
	// ErrDisabled is returned when backup or restore runs without an encryption key configured, since
	// the snapshot is sealed with it.
	ErrDisabled = errors.New("backup requires an encryption key: set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT")
	// ErrFormat is returned when a file is not a SwitchTender backup or is a version this build cannot
	// read.
	ErrFormat = errors.New("not a readable switchtender backup")
	// ErrOpen is returned when the sealed payload fails to decrypt, meaning the file was produced with
	// a different encryption key or has been altered.
	ErrOpen = errors.New("could not decrypt backup: wrong encryption key or the file was altered")
)

// Sealer is the subset of the credential sealer backup needs: authenticated encryption of the whole
// payload.
type Sealer interface {
	// Enabled reports whether an encryption key is configured.
	Enabled() bool
	// Seal encrypts plaintext and returns an opaque string.
	Seal(plaintext string) (string, error)
	// Open decrypts a value produced by Seal.
	Open(sealed string) (string, error)
}

// Stores is the set of control-plane stores a backup reads from and a restore writes to. A caller
// wires these from whichever database backend is open.
type Stores struct {
	// Credentials holds execution secrets, sealed.
	Credentials credential.Store
	// Projects holds git projects.
	Projects project.Store
	// Templates holds launch presets.
	Templates template.Store
	// Inventories holds stored inventories.
	Inventories inventory.Store
	// InventorySources holds dynamic inventory sources.
	InventorySources invsource.Store
	// Schedules holds recurring launches.
	Schedules schedule.Store
	// Triggers holds webhook triggers.
	Triggers trigger.Store
	// Users holds accounts.
	Users user.Store
	// Teams holds teams.
	Teams team.Store
	// Orgs holds organizations.
	Orgs org.Store
	// Grants holds per-object access grants.
	Grants grant.Store
	// Policies holds the approval policies that decide which runs wait for a person. Nil when the
	// install pins them from a file, which is its own source of truth and is backed up with the file.
	Policies policy.Store
}

// Summary reports how many objects of each kind a backup or restore moved, so the operator sees what
// the snapshot held.
type Summary struct {
	// CreatedAt is when the snapshot was written.
	CreatedAt time.Time `json:"created_at"`
	// Credentials is the credential count.
	Credentials int `json:"credentials"`
	// Projects is the project count.
	Projects int `json:"projects"`
	// Templates is the template count.
	Templates int `json:"templates"`
	// Inventories is the inventory count.
	Inventories int `json:"inventories"`
	// InventorySources is the inventory source count.
	InventorySources int `json:"inventory_sources"`
	// Schedules is the schedule count.
	Schedules int `json:"schedules"`
	// Triggers is the trigger count.
	Triggers int `json:"triggers"`
	// Users is the account count.
	Users int `json:"users"`
	// Teams is the team count.
	Teams int `json:"teams"`
	// Orgs is the organization count.
	Orgs int `json:"orgs"`
	// Policies is the approval policy count.
	Policies int `json:"policies"`
	// Memberships is the number of team and organization memberships.
	Memberships int `json:"memberships"`
	// Grants is the access grant count.
	Grants int `json:"grants"`
}

// envelope is the on-disk wrapper. Only Format, Version, and CreatedAt are in the clear so a file is
// self-describing and its version is checked before any decryption is attempted. Everything sensitive
// lives inside the sealed payload.
type envelope struct {
	// Format identifies the file as a SwitchTender backup.
	Format string `json:"format"`
	// Version is the payload schema version.
	Version int `json:"version"`
	// CreatedAt is when the snapshot was written.
	CreatedAt time.Time `json:"created_at"`
	// Sealed is the AES-256-GCM sealed, gzip-compressed JSON payload.
	Sealed string `json:"sealed"`
}

// payload is the decrypted content of a backup: every exported object. Types with fields hidden from
// JSON, such as sealed secrets and password hashes, use a wrapper that carries those fields so the
// snapshot is faithful.
type payload struct {
	// CreatedAt is the snapshot time, carried inside the seal so it cannot be edited.
	//
	// The envelope states it too, and that copy is not authenticated: the seal covers content but
	// binds nothing to the header around it. Somebody with write access to wherever backups are
	// kept, and no key at all, could put an old file back with a fresh-looking date. The restore
	// would succeed, offboarded accounts and their password hashes would come back, and the one
	// thing the operator checks would read correct. The two copies are compared on restore.
	CreatedAt        time.Time            `json:"created_at"`
	Credentials      []credentialDTO      `json:"credentials,omitempty"`
	Projects         []*project.Project   `json:"projects,omitempty"`
	Templates        []*template.Template `json:"templates,omitempty"`
	Inventories      []inventoryDTO       `json:"inventories,omitempty"`
	InventorySources []*invsource.Source  `json:"inventory_sources,omitempty"`
	Schedules        []*schedule.Schedule `json:"schedules,omitempty"`
	Triggers         []triggerDTO         `json:"triggers,omitempty"`
	Users            []userDTO            `json:"users,omitempty"`
	Teams            []*team.Team         `json:"teams,omitempty"`
	Orgs             []*org.Org           `json:"orgs,omitempty"`
	Grants           []*grant.Grant       `json:"grants,omitempty"`
	// Policies are the approval policies. Without them a restored control plane runs every change
	// unapproved while still reporting a healthy restore, which is the failure that looks like
	// success: the gates are simply gone.
	Policies []*policy.Policy `json:"policies,omitempty"`
	// TeamMembers and OrgMembers carry who belongs to what. A team or an organization restores as an
	// empty shell without them, so every grant written to one reaches nobody and access silently
	// narrows to whoever holds a direct grant.
	TeamMembers []teamMemberDTO `json:"team_members,omitempty"`
	OrgMembers  []orgMemberDTO  `json:"org_members,omitempty"`
}

// teamMemberDTO is one user's membership of one team.
type teamMemberDTO struct {
	// TeamID is the team joined.
	TeamID string `json:"team_id"`
	// UserID is the member.
	UserID string `json:"user_id"`
}

// orgMemberDTO is one user's membership of one organization, with the role it carries there.
type orgMemberDTO struct {
	// OrgID is the organization joined.
	OrgID string `json:"org_id"`
	// UserID is the member.
	UserID string `json:"user_id"`
	// Role is the member's organization role.
	Role org.Role `json:"role"`
}

// credentialDTO carries a credential with its sealed Secret, which the entity hides from JSON.
type credentialDTO struct {
	credential.Credential
	// Secret is the sealed secret material, restored onto the entity's hidden field.
	Secret string `json:"secret"`
}

// inventoryDTO carries an inventory with its sealed ContentConfig, which the entity hides from JSON.
type inventoryDTO struct {
	inventory.Inventory
	// ContentConfig is the sealed dynamic-source config, restored onto the entity's hidden field.
	ContentConfig string `json:"content_config"`
}

// triggerDTO carries a trigger with its hidden token hash and sealed signing secret.
type triggerDTO struct {
	trigger.Trigger
	// TokenHash is the hashed webhook token, restored onto the entity's hidden field.
	TokenHash string `json:"token_hash"`
	// SigningSecret is the sealed body-signing secret, restored onto the entity's hidden field.
	SigningSecret string `json:"signing_secret"`
}

// userDTO carries a user with its password hash, which the entity hides from JSON.
type userDTO struct {
	user.User
	// PasswordHash is the hashed password, restored onto the entity's hidden field.
	PasswordHash string `json:"password_hash"`
}

// Write gathers every control-plane object, seals it with the deployment key, and writes the backup
// envelope to w. It returns the object counts, or ErrDisabled when no encryption key is configured.
func Write(ctx context.Context, s Stores, sealer Sealer, w io.Writer) (Summary, error) {
	if sealer == nil || !sealer.Enabled() {
		return Summary{}, ErrDisabled
	}
	p, sum, err := gather(ctx, s)
	if err != nil {
		return Summary{}, err
	}
	// Stamped before the payload is sealed, so the time is covered by the seal rather than sitting
	// editable in the header beside it.
	sum.CreatedAt = time.Now().UTC()
	p.CreatedAt = sum.CreatedAt
	raw, err := json.Marshal(p)
	if err != nil {
		return Summary{}, fmt.Errorf("backup: encode payload: %w", err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw); err != nil {
		return Summary{}, fmt.Errorf("backup: compress: %w", err)
	}
	if err := zw.Close(); err != nil {
		return Summary{}, fmt.Errorf("backup: compress: %w", err)
	}
	sealed, err := sealer.Seal(gz.String())
	if err != nil {
		return Summary{}, fmt.Errorf("backup: seal: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(envelope{
		Format: Format, Version: Version, CreatedAt: sum.CreatedAt, Sealed: sealed,
	}); err != nil {
		return Summary{}, fmt.Errorf("backup: write: %w", err)
	}
	return sum, nil
}

// Read decrypts a backup from r and upserts every object into the stores by id, applying nothing
// until the whole payload has decrypted and decoded so a bad file cannot half-apply. It never deletes
// objects absent from the backup. It returns ErrDisabled without a key, ErrFormat for a foreign or
// unsupported file, and ErrOpen when the file will not decrypt with this deployment's key.
func Read(ctx context.Context, s Stores, sealer Sealer, r io.Reader) (Summary, error) {
	if sealer == nil || !sealer.Enabled() {
		return Summary{}, ErrDisabled
	}
	var env envelope
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return Summary{}, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if env.Format != Format {
		return Summary{}, fmt.Errorf("%w: wrong file format", ErrFormat)
	}
	if env.Version != Version {
		return Summary{}, fmt.Errorf("%w: unsupported version %d", ErrFormat, env.Version)
	}
	gzStr, err := sealer.Open(env.Sealed)
	if err != nil {
		return Summary{}, fmt.Errorf("%w: %v", ErrOpen, err)
	}
	zr, err := gzip.NewReader(strings.NewReader(gzStr))
	if err != nil {
		return Summary{}, fmt.Errorf("restore: decompress: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxPayloadBytes))
	if err != nil {
		return Summary{}, fmt.Errorf("restore: decompress: %w", err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Summary{}, fmt.Errorf("restore: decode payload: %w", err)
	}
	// The header is held against the copy inside the seal. Only the inner one is authenticated, so
	// a header that disagrees means the file was edited around its own contents.
	if !p.CreatedAt.Equal(env.CreatedAt) {
		return Summary{}, fmt.Errorf("%w: the file says it was taken at %s and its sealed contents "+
			"say %s, so the header was changed after the backup was made",
			ErrFormat, env.CreatedAt.UTC().Format(time.RFC3339), p.CreatedAt.UTC().Format(time.RFC3339))
	}
	// Everything the file asks for is checked before anything is written. A restore is not atomic
	// across stores, and accounts are written early, so a file that fails partway through used to
	// leave the identity tables rewritten while reporting that nothing had been restored.
	if err := check(ctx, s, &p); err != nil {
		return Summary{}, err
	}
	sum, err := apply(ctx, s, &p)
	// The counts come back even when it failed. Returning an empty summary told the operator that
	// nothing had happened at the exact moment something had.
	sum.CreatedAt = p.CreatedAt
	if err != nil {
		return sum, err
	}
	return sum, nil
}

// gather reads every store and builds the payload plus the object counts.
func gather(ctx context.Context, s Stores) (*payload, Summary, error) {
	var p payload
	var sum Summary

	creds, err := s.Credentials.List(ctx)
	if err != nil {
		return nil, sum, fmt.Errorf("backup: list credentials: %w", err)
	}
	for _, c := range creds {
		p.Credentials = append(p.Credentials, credentialDTO{Credential: *c, Secret: c.Secret})
	}
	sum.Credentials = len(creds)

	if p.Projects, err = s.Projects.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list projects: %w", err)
	}
	sum.Projects = len(p.Projects)

	if p.Templates, err = s.Templates.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list templates: %w", err)
	}
	sum.Templates = len(p.Templates)

	invs, err := s.Inventories.List(ctx)
	if err != nil {
		return nil, sum, fmt.Errorf("backup: list inventories: %w", err)
	}
	for _, i := range invs {
		p.Inventories = append(p.Inventories, inventoryDTO{Inventory: *i, ContentConfig: i.ContentConfig})
	}
	sum.Inventories = len(invs)

	if p.InventorySources, err = s.InventorySources.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list inventory sources: %w", err)
	}
	sum.InventorySources = len(p.InventorySources)

	if p.Schedules, err = s.Schedules.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list schedules: %w", err)
	}
	sum.Schedules = len(p.Schedules)

	trigs, err := s.Triggers.List(ctx)
	if err != nil {
		return nil, sum, fmt.Errorf("backup: list triggers: %w", err)
	}
	for _, t := range trigs {
		p.Triggers = append(p.Triggers, triggerDTO{
			Trigger: *t, TokenHash: t.TokenHash, SigningSecret: t.SigningSecret,
		})
	}
	sum.Triggers = len(trigs)

	users, err := s.Users.List(ctx)
	if err != nil {
		return nil, sum, fmt.Errorf("backup: list users: %w", err)
	}
	for _, u := range users {
		p.Users = append(p.Users, userDTO{User: *u, PasswordHash: u.PasswordHash})
	}
	sum.Users = len(users)

	if p.Teams, err = s.Teams.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list teams: %w", err)
	}
	sum.Teams = len(p.Teams)

	if p.Orgs, err = s.Orgs.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list orgs: %w", err)
	}
	sum.Orgs = len(p.Orgs)

	if p.Grants, err = s.Grants.List(ctx); err != nil {
		return nil, sum, fmt.Errorf("backup: list grants: %w", err)
	}
	sum.Grants = len(p.Grants)

	// Policies are skipped when the install pins them from a file: that file is the source of truth,
	// the API refuses writes to them, and restoring a copy would put a second answer in the database.
	if s.Policies != nil {
		if p.Policies, err = s.Policies.List(ctx); err != nil {
			return nil, sum, fmt.Errorf("backup: list policies: %w", err)
		}
		sum.Policies = len(p.Policies)
	}

	// Membership is stored separately from the team and organization rows, so listing those alone
	// captured empty shells.
	for _, t := range p.Teams {
		members, err := s.Teams.Members(ctx, t.ID)
		if err != nil {
			return nil, sum, fmt.Errorf("backup: list members of team %s: %w", t.ID, err)
		}
		for _, uid := range members {
			p.TeamMembers = append(p.TeamMembers, teamMemberDTO{TeamID: t.ID, UserID: uid})
		}
	}
	for _, o := range p.Orgs {
		members, err := s.Orgs.Members(ctx, o.ID)
		if err != nil {
			return nil, sum, fmt.Errorf("backup: list members of organization %s: %w", o.ID, err)
		}
		for _, m := range members {
			p.OrgMembers = append(p.OrgMembers, orgMemberDTO{OrgID: o.ID, UserID: m.UserID, Role: m.Role})
		}
	}
	sum.Memberships = len(p.TeamMembers) + len(p.OrgMembers)

	return &p, sum, nil
}

// check verifies everything a restore would write before any of it is written.
//
// A restore is an upsert into a live database across several stores with no transaction around it,
// and accounts are written near the front. A file that failed partway left orgs, teams, and users
// rewritten, and the operator saw an error and a summary of zero. The realistic trigger is not
// exotic: usernames are unique but accounts are keyed by id, so a backup holding a username the
// target already uses under a different id aborts the restore after the identity tables are in.
//
// It also applies the validation every API path applies. Restore wrote straight to the stores, so a
// file could set a role no check recognizes, a grant naming nothing, or a profile link carrying a
// script URL. The role and grant cases fail closed, but the profile link is rendered as an anchor in
// the users page on the strength of the server having validated it.
func check(ctx context.Context, s Stores, p *payload) error {
	for _, u := range p.Users {
		acct := u.User
		if !user.ValidRole(acct.Role) {
			return fmt.Errorf("%w: user %s has role %q, which is not a role", ErrFormat,
				acct.ID, acct.Role)
		}
		if err := acct.NormalizeProfile(); err != nil {
			return fmt.Errorf("%w: user %s: %v", ErrFormat, acct.ID, err)
		}
		// A username belongs to one account. Restoring one that the target already uses under a
		// different id fails at the unique index, and by then the identity tables are written.
		existing, err := s.Users.FindByUsername(ctx, acct.Username)
		switch {
		case errors.Is(err, user.ErrNotFound):
		case err != nil:
			return fmt.Errorf("restore: check user %s: %w", acct.ID, err)
		case existing.ID != acct.ID:
			return fmt.Errorf("%w: this backup holds an account named %q with id %s, and this "+
				"install already has one with that name under id %s, so restoring would collide",
				ErrFormat, acct.Username, acct.ID, existing.ID)
		}
	}
	for _, g := range p.Grants {
		switch {
		case !grant.ValidSubject(g.Subject):
			return fmt.Errorf("%w: grant %s names subject %q, which is not one", ErrFormat,
				g.ID, g.Subject)
		case !grant.ValidObject(g.Object):
			return fmt.Errorf("%w: grant %s names object %q, which is not one", ErrFormat,
				g.ID, g.Object)
		case !grant.ValidAccess(g.Access):
			return fmt.Errorf("%w: grant %s grants %q, which is not an access level", ErrFormat,
				g.ID, g.Access)
		}
	}
	return nil
}

// apply upserts every object in the payload and reports the counts. Identity and access objects are
// written first so later objects that reference an org resolve against a present owner. Each counter
// is raised as its object is saved, not after the store's loop finishes, so a failure partway through
// returns the true number written rather than zero for rows already committed.
func apply(ctx context.Context, s Stores, p *payload) (Summary, error) {
	var sum Summary
	// One clock reading for the whole restore, so every schedule recomputes against the same instant.
	now := time.Now()

	for _, o := range p.Orgs {
		if err := s.Orgs.Save(ctx, o); err != nil {
			return sum, fmt.Errorf("restore: save org %s: %w", o.ID, err)
		}
		sum.Orgs++
	}

	for _, t := range p.Teams {
		if err := s.Teams.Save(ctx, t); err != nil {
			return sum, fmt.Errorf("restore: save team %s: %w", t.ID, err)
		}
		sum.Teams++
	}

	for _, u := range p.Users {
		acct := u.User
		acct.PasswordHash = u.PasswordHash
		if err := s.Users.Save(ctx, &acct); err != nil {
			return sum, fmt.Errorf("restore: save user %s: %w", acct.ID, err)
		}
		sum.Users++
	}

	for _, g := range p.Grants {
		if err := s.Grants.Save(ctx, g); err != nil {
			return sum, fmt.Errorf("restore: save grant %s: %w", g.ID, err)
		}
		sum.Grants++
	}

	// Memberships come after the users, teams, and organizations they join, since they reference all
	// three.
	for _, m := range p.TeamMembers {
		if err := s.Teams.AddMember(ctx, m.TeamID, m.UserID); err != nil {
			return sum, fmt.Errorf("restore: add %s to team %s: %w", m.UserID, m.TeamID, err)
		}
		sum.Memberships++
	}
	for _, m := range p.OrgMembers {
		if err := s.Orgs.AddMember(ctx, m.OrgID, m.UserID, m.Role); err != nil {
			return sum, fmt.Errorf("restore: add %s to organization %s: %w", m.UserID, m.OrgID, err)
		}
		sum.Memberships++
	}

	// Policies last among the access objects, and only when this install manages them in the
	// database. A restore that silently dropped them left every gated change running unapproved.
	if s.Policies != nil {
		for _, pol := range p.Policies {
			if err := s.Policies.Save(ctx, pol); err != nil {
				return sum, fmt.Errorf("restore: save policy %s: %w", pol.ID, err)
			}
			sum.Policies++
		}
	}

	for _, pr := range p.Projects {
		if err := s.Projects.Save(ctx, pr); err != nil {
			return sum, fmt.Errorf("restore: save project %s: %w", pr.ID, err)
		}
		sum.Projects++
	}

	for _, c := range p.Credentials {
		cred := c.Credential
		cred.Secret = c.Secret
		if err := s.Credentials.Save(ctx, &cred); err != nil {
			return sum, fmt.Errorf("restore: save credential %s: %w", cred.ID, err)
		}
		sum.Credentials++
	}

	for _, i := range p.Inventories {
		inv := i.Inventory
		inv.ContentConfig = i.ContentConfig
		if err := s.Inventories.Save(ctx, &inv); err != nil {
			return sum, fmt.Errorf("restore: save inventory %s: %w", inv.ID, err)
		}
		sum.Inventories++
	}

	for _, src := range p.InventorySources {
		if err := s.InventorySources.Save(ctx, src); err != nil {
			return sum, fmt.Errorf("restore: save inventory source %s: %w", src.ID, err)
		}
		sum.InventorySources++
	}

	for _, t := range p.Templates {
		if err := s.Templates.Save(ctx, t); err != nil {
			return sum, fmt.Errorf("restore: save template %s: %w", t.ID, err)
		}
		sum.Templates++
	}

	for _, sc := range p.Schedules {
		// The next fire time is recomputed from the cron rather than restored as it was written.
		//
		// The scheduler reads any next-run time that is not in the future as due, so a backup taken
		// yesterday restored today made every enabled schedule due at once: the restore finished and
		// the whole estate's nightly work fired together, against an install somebody was still
		// bringing up. Missed occurrences are skipped the way cron skips them, so the schedule
		// resumes on its own cadence instead of catching up.
		restored := *sc
		if restored.Enabled {
			if next, err := restored.NextFire(now); err == nil {
				restored.NextRunAt = &next
			} else {
				// A cadence that can never come due was already broken before the backup. It is
				// restored without a fire time, which the scheduler skips, rather than with a stale
				// one that would fire it immediately.
				restored.NextRunAt = nil
			}
		}
		if err := s.Schedules.Save(ctx, &restored); err != nil {
			return sum, fmt.Errorf("restore: save schedule %s: %w", sc.ID, err)
		}
		sum.Schedules++
	}

	for _, t := range p.Triggers {
		trig := t.Trigger
		trig.TokenHash = t.TokenHash
		trig.SigningSecret = t.SigningSecret
		if err := s.Triggers.Save(ctx, &trig); err != nil {
			return sum, fmt.Errorf("restore: save trigger %s: %w", trig.ID, err)
		}
		sum.Triggers++
	}

	return sum, nil
}
