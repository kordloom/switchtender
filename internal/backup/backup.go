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
	Version = 1
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
	sum.CreatedAt = time.Now().UTC()
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
	raw, err := io.ReadAll(zr)
	if err != nil {
		return Summary{}, fmt.Errorf("restore: decompress: %w", err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Summary{}, fmt.Errorf("restore: decode payload: %w", err)
	}
	sum, err := apply(ctx, s, &p)
	if err != nil {
		return Summary{}, err
	}
	sum.CreatedAt = env.CreatedAt
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

	return &p, sum, nil
}

// apply upserts every object in the payload and reports the counts. Identity and access objects are
// written first so later objects that reference an org resolve against a present owner.
func apply(ctx context.Context, s Stores, p *payload) (Summary, error) {
	var sum Summary

	for _, o := range p.Orgs {
		if err := s.Orgs.Save(ctx, o); err != nil {
			return sum, fmt.Errorf("restore: save org %s: %w", o.ID, err)
		}
	}
	sum.Orgs = len(p.Orgs)

	for _, t := range p.Teams {
		if err := s.Teams.Save(ctx, t); err != nil {
			return sum, fmt.Errorf("restore: save team %s: %w", t.ID, err)
		}
	}
	sum.Teams = len(p.Teams)

	for _, u := range p.Users {
		acct := u.User
		acct.PasswordHash = u.PasswordHash
		if err := s.Users.Save(ctx, &acct); err != nil {
			return sum, fmt.Errorf("restore: save user %s: %w", acct.ID, err)
		}
	}
	sum.Users = len(p.Users)

	for _, g := range p.Grants {
		if err := s.Grants.Save(ctx, g); err != nil {
			return sum, fmt.Errorf("restore: save grant %s: %w", g.ID, err)
		}
	}
	sum.Grants = len(p.Grants)

	for _, pr := range p.Projects {
		if err := s.Projects.Save(ctx, pr); err != nil {
			return sum, fmt.Errorf("restore: save project %s: %w", pr.ID, err)
		}
	}
	sum.Projects = len(p.Projects)

	for _, c := range p.Credentials {
		cred := c.Credential
		cred.Secret = c.Secret
		if err := s.Credentials.Save(ctx, &cred); err != nil {
			return sum, fmt.Errorf("restore: save credential %s: %w", cred.ID, err)
		}
	}
	sum.Credentials = len(p.Credentials)

	for _, i := range p.Inventories {
		inv := i.Inventory
		inv.ContentConfig = i.ContentConfig
		if err := s.Inventories.Save(ctx, &inv); err != nil {
			return sum, fmt.Errorf("restore: save inventory %s: %w", inv.ID, err)
		}
	}
	sum.Inventories = len(p.Inventories)

	for _, src := range p.InventorySources {
		if err := s.InventorySources.Save(ctx, src); err != nil {
			return sum, fmt.Errorf("restore: save inventory source %s: %w", src.ID, err)
		}
	}
	sum.InventorySources = len(p.InventorySources)

	for _, t := range p.Templates {
		if err := s.Templates.Save(ctx, t); err != nil {
			return sum, fmt.Errorf("restore: save template %s: %w", t.ID, err)
		}
	}
	sum.Templates = len(p.Templates)

	for _, sc := range p.Schedules {
		if err := s.Schedules.Save(ctx, sc); err != nil {
			return sum, fmt.Errorf("restore: save schedule %s: %w", sc.ID, err)
		}
	}
	sum.Schedules = len(p.Schedules)

	for _, t := range p.Triggers {
		trig := t.Trigger
		trig.TokenHash = t.TokenHash
		trig.SigningSecret = t.SigningSecret
		if err := s.Triggers.Save(ctx, &trig); err != nil {
			return sum, fmt.Errorf("restore: save trigger %s: %w", trig.ID, err)
		}
	}
	sum.Triggers = len(p.Triggers)

	return sum, nil
}
