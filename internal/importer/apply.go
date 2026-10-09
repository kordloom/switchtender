package importer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// ApplyStores are the stores an import plan is written to.
type ApplyStores struct {
	// Projects receives the imported git projects.
	Projects project.Store
	// Inventories receives the imported stored inventories, including each dynamic source's backing
	// inventory.
	Inventories inventory.Store
	// Sources receives the imported dynamic inventory sources.
	Sources invsource.Store
	// Credentials receives the imported credential shells.
	Credentials credential.Store
	// CredentialTypes receives the imported custom credential types.
	CredentialTypes credential.TypeStore
	// Templates receives the imported job templates.
	Templates template.Store
	// Schedules receives the imported schedules.
	Schedules schedule.Store
	// Notifications receives the imported notification targets and their attachments.
	Notifications notification.Store
	// Orgs receives the organizations the import places smart inventories and templates in, and
	// that an AWX organization's own notification attachments go on. Without it nothing is placed,
	// those attachments fall back to the organization's templates, and the plan's warnings say so.
	Orgs org.Store
	// Sealer seals what the import carries in the clear until it is stored: each notification
	// target's address and key, and each template's provisioning callback key. Without an enabled
	// one, a target that carries a secret arrives waiting for it, and a template keeps its callback
	// setting and arrives with no key, so it refuses every callback until one is minted, which the
	// plan's warnings say. Neither is ever stored in the clear.
	Sealer *credential.Sealer
}

// Apply persists the plan through the stores in dependency order and returns the number of objects
// created. Credentials arrive as shells, so their secrets must be set before templates that need
// them run. It stops at the first store error, returning what was created so far.
//
// The plan's AWX job template ids are claimed before anything else is written, and given back if
// the import stops before its templates are stored. See template.ClaimAWX.
func (p *Plan) Apply(ctx context.Context, s ApplyStores) (created int, err error) {
	if err := p.checkText(); err != nil {
		return 0, err
	}
	// Refuse before writing anything if the plan has dynamic sources but nowhere to store them, so a
	// source's backing inventory is never created without its source.
	if len(p.Sources) > 0 && s.Sources == nil {
		return 0, fmt.Errorf("cannot import %d inventory sources: inventory sources not enabled",
			len(p.Sources))
	}
	// The same for custom types: a typed credential written without its type cannot be used or
	// given values, so neither is written.
	if len(p.CredentialTypes) > 0 && s.CredentialTypes == nil {
		return 0, fmt.Errorf("cannot import %d credential types: credential types not enabled",
			len(p.CredentialTypes))
	}
	if len(p.Notifications)+len(p.Attachments) > 0 && s.Notifications == nil {
		return 0, fmt.Errorf("cannot import %d notification targets: notification targets not "+
			"enabled", len(p.Notifications))
	}
	// The inventory an operator named on the command line is resolved here, against the inventories
	// this install actually holds, because the mapping stage has no store to ask. A crontab and a
	// Rundeck export name no inventory file, so the caller supplies one, and the obvious thing to type
	// is the name of a stored inventory. That value used to be written straight into the field that
	// holds a filesystem path, so the imported templates pointed at a file that does not exist and the
	// operator found out when one launched.
	if err := p.resolveInventoryNames(ctx, s.Inventories); err != nil {
		return 0, err
	}
	if err := p.resolveAWXOrgs(ctx, s.Orgs); err != nil {
		return 0, err
	}
	if err := p.refuseExisting(ctx, s); err != nil {
		return 0, err
	}
	if err := p.refuseAWXConflicts(ctx, s.Templates); err != nil {
		return 0, err
	}
	release, err := p.claimAWX(ctx, s.Templates)
	if err != nil {
		return 0, err
	}
	stored := false
	defer func() {
		// An import that stopped before its templates were stored made nothing its claimed ids
		// should reach, so they are given back.
		if err != nil && !stored {
			err = errors.Join(err, release(ctx))
		}
	}()
	// The organizations go first, since every inventory and template placed in one names it by id.
	created, err = p.applyOrgs(ctx, s.Orgs)
	if err != nil {
		return created, err
	}
	for _, pr := range p.Projects {
		if err := s.Projects.Save(ctx, pr); err != nil {
			return created, fmt.Errorf("save project %q: %w", pr.Name, err)
		}
		created++
	}
	for _, inv := range p.Inventories {
		if err := s.Inventories.Save(ctx, inv); err != nil {
			return created, fmt.Errorf("save inventory %q: %w", inv.Name, err)
		}
		created++
	}
	// Types go before the credentials that name them, so a credential never exists without its type.
	for _, ct := range p.CredentialTypes {
		if err := s.CredentialTypes.Save(ctx, ct); err != nil {
			return created, fmt.Errorf("save credential type %q: %w", ct.Name, err)
		}
		created++
	}
	for _, c := range p.Credentials {
		if err := s.Credentials.Save(ctx, c); err != nil {
			return created, fmt.Errorf("save credential %q: %w", c.Name, err)
		}
		created++
	}
	for _, src := range p.Sources {
		if err := s.Sources.Save(ctx, src); err != nil {
			return created, fmt.Errorf("save inventory source %q: %w", src.Name, err)
		}
		created++
	}
	for _, t := range p.Templates {
		if err := p.sealCallbackKey(t, s.Sealer); err != nil {
			return created, fmt.Errorf("seal callback key for template %q: %w", t.Name, err)
		}
		if err := s.Templates.Save(ctx, t); err != nil {
			return created, fmt.Errorf("save template %q: %w", t.Name, err)
		}
		created++
	}
	stored = true
	if err := p.bindAWX(ctx, s.Templates); err != nil {
		return created, err
	}
	for _, sc := range p.Schedules {
		if err := s.Schedules.Save(ctx, sc); err != nil {
			return created, fmt.Errorf("save schedule %q: %w", sc.Name, err)
		}
		created++
	}
	n, err := p.applyNotifications(ctx, s)
	return created + n, err
}

// applyNotifications seals and stores the notification targets, then the plan's Attachments, and
// returns how many records it created.
//
// A target's address and key are sealed here and nowhere earlier, so the plan a preview returns
// never holds one. An install with no encryption key cannot seal them, and a target that needs one
// is then stored waiting for its secret, the way a credential arrives as a shell, rather than
// written in the clear. The plaintext is dropped from the plan once it is sealed.
func (p *Plan) applyNotifications(ctx context.Context, s ApplyStores) (int, error) {
	created := 0
	for _, n := range p.Notifications {
		if t, ok := p.notifySecrets[n.ID]; ok {
			sealed := n.Clone()
			// A target still waiting for its secret keeps the parts the export did carry, sealed,
			// so finishing it asks only for what is missing.
			seal := sealed.SetTarget
			if n.NeedsSecret {
				seal = sealed.SetKnown
			}
			switch err := seal(t, notifySealer(s.Sealer)); {
			case errors.Is(err, notification.ErrSealing):
				// Said in the report, because the export did carry this part and the import is
				// dropping it. Stored silently, the target would read as one more secret the export
				// never held, and the loss would show only when a failed run notified nobody.
				n.NeedsSecret = true
				p.warn("notification target %q arrives waiting for its %s: the export carried it, "+
					"and this install has no encryption key to seal it with, so it was dropped. Set "+
					"SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT before importing "+
					"to keep it, or enter it on the target afterward", n.Name, notifyParts(t))
			case err != nil:
				return created, fmt.Errorf("seal notification target %q: %w", n.Name, err)
			default:
				*n = *sealed
			}
			delete(p.notifySecrets, n.ID)
		}
		if err := s.Notifications.Save(ctx, n); err != nil {
			return created, fmt.Errorf("save notification target %q: %w", n.Name, err)
		}
		created++
	}
	for _, a := range p.Attachments {
		err := s.Notifications.Attach(ctx, a)
		switch {
		case errors.Is(err, notification.ErrDuplicate):
		case err != nil:
			return created, fmt.Errorf("attach notification target %s: %w", a.NotificationID, err)
		default:
			created++
		}
	}
	return created, nil
}

// sealCallbackKey seals the template's imported callback key onto it when the install can seal one.
// The plaintext is dropped either way, so it lives no longer than the apply.
func (p *Plan) sealCallbackKey(t *template.Template, sealer *credential.Sealer) error {
	key, ok := p.callbackKeys[t.ID]
	if !ok {
		return nil
	}
	delete(p.callbackKeys, t.ID)
	if sealer == nil || !sealer.Enabled() {
		// Said only for a template that takes callbacks, since the key is what its hosts present.
		// Without this line, the plan's own line about the key reads as though it came across.
		if t.AllowCallbacks {
			p.warn("template %q accepts provisioning callbacks, and the key the export carried "+
				"for it was dropped, because this install has no encryption key to seal it with, "+
				"so it refuses every callback until a key is minted on its page. Set "+
				"SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT before importing "+
				"to keep the key the hosts hold", t.Name)
		}
		return nil
	}
	sealed, err := sealer.Seal(key)
	if err != nil {
		return err
	}
	t.HostConfigKey = sealed
	return nil
}

// notifySealer returns s as a notification.Sealer, or nil when s is nil, so a missing sealer reads
// as no sealer rather than as a nil pointer the target would call.
func notifySealer(s *credential.Sealer) notification.Sealer {
	if s == nil {
		return nil
	}
	return s
}

// NeedsSealer reports whether applying the plan has anything to seal: a notification target's
// address or key, or a template's provisioning callback key. A caller derives the install's key,
// which is deliberately expensive, only when it does.
func (p *Plan) NeedsSealer() bool {
	return len(p.Notifications) > 0 || len(p.callbackKeys) > 0
}

// Unsealed counts what the plan holds in the clear for Apply to seal: the notification targets
// whose address or key the export carried, and the templates that accept provisioning callbacks
// whose key it carried. Applied without an enabled sealer, each target arrives waiting for a secret
// the export held and each key is dropped, so a caller that knows no key is set says so before
// applying. A key held for a template that refuses callbacks is not counted, since no host presents
// it and Apply says nothing when it drops one, so the count matches the lines Apply adds.
func (p *Plan) Unsealed() (targets, keys int) {
	for _, t := range p.Templates {
		if _, ok := p.callbackKeys[t.ID]; ok && t.AllowCallbacks {
			keys++
		}
	}
	return len(p.notifySecrets), keys
}

// notifyParts names the parts of a target an install without a key cannot seal: its address, its
// key, or both.
func notifyParts(t run.NotifyTarget) string {
	switch {
	case t.URL != "" && t.Key != "":
		return "address and key"
	case t.Key != "":
		return "key"
	default:
		return "address"
	}
}

// resolveInventoryNames rewrites a template's inventory path into an inventory id when the path names
// a stored inventory. Anything that matches nothing is left as a path and reported, so an operator who
// meant a file gets a file and an operator who meant a stored inventory gets the object.
//
// It runs before anything is written, and only for templates that carry a path and no id, so an import
// that already wired an inventory by id is untouched.
func (p *Plan) resolveInventoryNames(ctx context.Context, store inventory.Store) error {
	needed := map[string]bool{}
	for _, t := range p.Templates {
		if t.InventoryID == "" && t.Inventory != "" {
			needed[t.Inventory] = true
		}
	}
	if len(needed) == 0 || store == nil {
		return nil
	}
	// The plan's own inventories are not stored yet, so both they and the existing ones are consulted.
	byName := map[string]string{}
	for _, inv := range p.Inventories {
		byName[inv.Name] = inv.ID
	}
	stored, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("read inventories to resolve the named one: %w", err)
	}
	for _, inv := range stored {
		byName[inv.Name] = inv.ID
	}
	reported := map[string]bool{}
	for _, t := range p.Templates {
		if t.InventoryID != "" || t.Inventory == "" {
			continue
		}
		if id, ok := byName[t.Inventory]; ok {
			t.InventoryID = id
			t.Inventory = ""
			continue
		}
		if !reported[t.Inventory] {
			reported[t.Inventory] = true
			p.warn("no stored inventory is named %q, so it is used as a path on the server's "+
				"filesystem. Create an inventory with that name, or point the templates at one, if "+
				"that is not what you meant.", t.Inventory)
		}
	}
	return nil
}

// refuseExisting refuses an apply that would create an object of the same kind and name as a
// different object the install already holds, before anything is written. An object holding the
// same id is this plan's own, written by an earlier call that failed part way, and saving it again
// replaces it.
//
// Applying the same export twice created a second copy of every object, and a second copy of a
// schedule fires as well: every imported cadence ran twice on the next tick. Nothing here can tell a
// second copy of the same thing from a different thing that shares its name, so neither is guessed
// at. The apply names what is already there and stops.
func (p *Plan) refuseExisting(ctx context.Context, s ApplyStores) error {
	// The kinds that act on their own come first, so a long list names them before it is cut.
	checks := []func() ([]string, error){
		func() ([]string, error) {
			return clashes(ctx, "schedule", p.Schedules, s.Schedules,
				func(v *schedule.Schedule) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "template", p.Templates, s.Templates,
				func(v *template.Template) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "inventory source", p.Sources, s.Sources,
				func(v *invsource.Source) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "credential", p.Credentials, s.Credentials,
				func(v *credential.Credential) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "credential type", p.CredentialTypes, s.CredentialTypes,
				func(v *credential.CredentialType) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "inventory", p.Inventories, s.Inventories,
				func(v *inventory.Inventory) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "project", p.Projects, s.Projects,
				func(v *project.Project) namedObject { return namedObject{v.Name, v.ID} })
		},
		func() ([]string, error) {
			return clashes(ctx, "notification target", p.Notifications, s.Notifications,
				func(v *notification.Notification) namedObject { return namedObject{v.Name, v.ID} })
		},
	}
	var found []string
	for _, check := range checks {
		names, err := check()
		if err != nil {
			return err
		}
		found = append(found, names...)
	}
	if len(found) == 0 {
		return nil
	}
	suffix := ""
	if len(found) > 10 {
		found, suffix = found[:10], fmt.Sprintf(", and %d more", len(found)-10)
	}
	return fmt.Errorf("%w: %s%s. Importing again would create a second copy of each, and a second "+
		"copy of a schedule fires as well. Delete those objects first, or import into a fresh database",
		ErrAlreadyImported, strings.Join(found, ", "), suffix)
}

// namedObject is an object reduced to what an import clash is judged on.
type namedObject struct {
	// name is what an operator tells objects apart by.
	name string
	// id is what saving an object again replaces it by.
	id string
}

// lister is a store that can list everything it holds.
type lister[T any] interface {
	// List returns every object the store holds.
	List(ctx context.Context) ([]T, error)
}

// clashes names the planned objects of one kind that share a name with a different object the store
// already holds. A store left unset holds nothing.
func clashes[T any](ctx context.Context, kind string, planned []T, store lister[T],
	describe func(T) namedObject) ([]string, error) {
	if len(planned) == 0 || store == nil {
		return nil, nil
	}
	held, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("look for an existing %s of the same name: %w", kind, err)
	}
	ids := make(map[string][]string, len(held))
	for _, h := range held {
		o := describe(h)
		ids[o.name] = append(ids[o.name], o.id)
	}
	var out []string
	for _, pl := range planned {
		want := describe(pl)
		for _, id := range ids[want.name] {
			if id != want.id {
				out = append(out, kind+" "+quoteName(want.name))
				break
			}
		}
	}
	return out, nil
}
