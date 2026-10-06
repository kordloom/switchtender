package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/ansibleruntime"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// doctorFinding is one problem the doctor found in the control plane's registrations: a reference
// to something that does not exist, a credential that cannot be used yet, or a schedule that can
// never fire.
type doctorFinding struct {
	// Severity is broken for a reference that stops a launch, warning for one that degrades it.
	Severity string `json:"severity"`
	// ObjectType names what holds the problem: template, schedule, or credential.
	ObjectType string `json:"object_type"`
	// ObjectID is the holder's id.
	ObjectID string `json:"object_id"`
	// ObjectName is the holder's display name.
	ObjectName string `json:"object_name"`
	// Problem says what is wrong, in a sentence.
	Problem string `json:"problem"`
	// FixPath is the UI page where the problem is repaired.
	FixPath string `json:"fix_path"`
}

// doctorReport is the doctor's full answer: every finding plus how much was checked, so an empty
// findings list reads as a real all-clear instead of a skipped scan.
type doctorReport struct {
	// Findings lists every detected problem, broken first.
	Findings []doctorFinding `json:"findings"`
	// CheckedTemplates is how many templates were examined.
	CheckedTemplates int `json:"checked_templates"`
	// CheckedSchedules is how many schedules were examined.
	CheckedSchedules int `json:"checked_schedules"`
	// CheckedCredentials is how many credentials were examined.
	CheckedCredentials int `json:"checked_credentials"`
	// Checked counts what each further check examined, by what it examined, such as
	// notification_targets.
	Checked map[string]int `json:"checked,omitempty"`
	// Ansible is the ansible-core this server runs inventories with, and the releases the inventory
	// engine is tested against. Absent when the server cannot tell.
	Ansible *doctorAnsible `json:"ansible,omitempty"`
}

// doctorCheck is one further check the doctor runs beside its own, returning what it found and how
// much it examined.
type doctorCheck func(ctx context.Context) (doctorCheckResult, error)

// doctorCheckResult is what a further doctor check found and examined.
type doctorCheckResult struct {
	// Findings are the problems the check found.
	Findings []doctorFinding
	// Checked counts what the check examined, by what it examined.
	Checked map[string]int
}

// doctorAnsible is what doctor reports about the server's Ansible.
type doctorAnsible struct {
	// Installed reports whether ansible-inventory is on this server.
	Installed bool `json:"installed"`
	// Version is the installed ansible-core version, empty when it is not installed.
	Version string `json:"version,omitempty"`
	// Tested lists the ansible-core releases, by minor version, the inventory engine's conformance
	// corpus runs against in CI.
	Tested []string `json:"tested"`
	// InRange reports whether the installed version is one of them.
	InRange bool `json:"in_range"`
	// Source is where the server's Ansible commands come from: configured for --ansible-bin,
	// managed for the managed runtime, or path.
	Source string `json:"source,omitempty"`
	// Dir is the directory the commands are started from, empty when PATH finds them.
	Dir string `json:"dir,omitempty"`
	// RuntimeDir is the managed runtime's directory the server looks in.
	RuntimeDir string `json:"runtime_dir,omitempty"`
	// Problem says why the Ansible selected cannot be used, such as a managed runtime in use that is
	// broken, empty when it can.
	Problem string `json:"problem,omitempty"`
}

// AnsibleCoreFunc reports the ansible-core version installed on the server and where its commands
// come from, or roundhouse.ErrAnsibleMissing.
type AnsibleCoreFunc func(ctx context.Context) (string, ansibleruntime.Commands, error)

// ansibleCoreOf returns the dispatcher's ansible-core report when the previewer is one that has it,
// with where its Ansible commands come from when it can say.
func ansibleCoreOf(previewer InventoryPreviewer) AnsibleCoreFunc {
	r, ok := previewer.(interface {
		// AnsibleCore reports the server's ansible-core version.
		AnsibleCore(ctx context.Context) (string, error)
	})
	if !ok {
		return nil
	}
	cmds, _ := previewer.(interface {
		// AnsibleCommands reports where the server's Ansible commands come from.
		AnsibleCommands() (ansibleruntime.Commands, bool)
	})
	return func(ctx context.Context) (string, ansibleruntime.Commands, error) {
		var c ansibleruntime.Commands
		if cmds != nil {
			c, _ = cmds.AnsibleCommands()
		}
		v, err := r.AnsibleCore(ctx)
		return v, c, err
	}
}

// ansibleFindings reports the server's ansible-core: a warning when it is outside the releases the
// inventory engine is tested against, and, when it is missing, one warning per inventory whose
// definition needs it, since those cannot resolve here.
func ansibleFindings(ctx context.Context, ansible AnsibleCoreFunc, invs inventory.Store,
	report *doctorReport, log *zap.Logger) {
	if ansible == nil {
		return
	}
	tested := strings.Join(inventory.TestedAnsibleCore, ", ")
	version, cmds, err := ansible(ctx)
	info := &doctorAnsible{Tested: inventory.TestedAnsibleCore, Source: cmds.Source, Dir: cmds.Dir,
		RuntimeDir: cmds.Root, Problem: cmds.Problem}
	report.Ansible = info
	if cmds.Problem != "" {
		report.Findings = append(report.Findings, doctorFinding{
			Severity: "broken", ObjectType: "install", ObjectID: "ansible",
			ObjectName: "Ansible runtime",
			Problem: "The Ansible this server is set to run cannot be used, so every run that needs " +
				"Ansible fails rather than running another one: " + cmds.Problem + ". Install it " +
				"again with: " + inventory.AnsibleInstallHintFor(cmds.Root),
			FixPath: "/ui/docs/ansible-runtime",
		})
	}
	switch {
	case errors.Is(err, roundhouse.ErrAnsibleMissing):
	case err != nil:
		log.Warn("server: doctor ansible-core version: " + err.Error())
		return
	default:
		info.Installed, info.Version = true, version
		info.InRange = inventory.AnsibleCoreTested(version)
		if !info.InRange {
			report.Findings = append(report.Findings, doctorFinding{
				Severity: "warning", ObjectType: "install", ObjectID: "ansible",
				ObjectName: "ansible-core " + version,
				Problem: "ansible-core " + version + " is outside the releases the native " +
					"inventory engine is tested against (" + tested + "). Every Ansible run " +
					"against a natively resolved inventory is still checked against this " +
					"Ansible's own reading before it starts, and refused on any difference, so " +
					"an untested release shows up as refused runs rather than wrong ones.",
				FixPath: "/ui/docs/inventories",
			})
		}
		return
	}
	if invs == nil {
		return
	}
	list, err := invs.List(ctx)
	if err != nil {
		log.Warn("server: doctor inventories: " + err.Error())
		return
	}
	for _, inv := range list {
		var why string
		switch {
		case inv.Kind == inventory.KindConstructed:
			why = "it is a constructed inventory, whose groups and variables Ansible's " +
				"constructed plugin evaluates"
		case !inv.Composed() && (inv.ContentSource == "" || inv.ContentSource == "local"):
			if _, rerr := inventory.ResolveNative(inv.Content); rerr != nil {
				if reason, ok := inventory.NeedsAnsibleReason(rerr); ok {
					why = reason
				}
			}
		}
		if why == "" {
			continue
		}
		report.Findings = append(report.Findings, doctorFinding{
			Severity: "warning", ObjectType: "inventory", ObjectID: inv.ID,
			ObjectName: namedOr(inv.Name, inv.ID),
			Problem: "Needs Ansible because " + why + ", and ansible-inventory is not installed " +
				"on this server, so it cannot resolve here. Install it with: " +
				inventory.AnsibleInstallHintFor(cmds.Root),
			FixPath: "/ui/inventories",
		})
	}
}

// doctorHandler verifies every registered reference still resolves: template references to
// inventories, projects, and credentials, schedule references to templates, schedule cron
// expressions, and credentials still waiting for a secret. Stores that are not configured are
// skipped rather than reported. Each further check adds its own findings and counts, such as
// notification targets still needing a secret or work that has needed attention past its alert
// threshold, and one that cannot run is logged and reported as a finding of its own, since the rest
// of the report still stands.
func doctorHandler(templates template.Store, schedules schedule.Store, creds credential.Store,
	invs inventory.Store, projs project.Store, canSign func() bool, runFiles *runFilesState,
	ansible AnsibleCoreFunc, log *zap.Logger, checks ...doctorCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		report := doctorReport{Findings: []doctorFinding{}}
		ansibleFindings(ctx, ansible, invs, &report, log)

		// An install that cannot sign is the doctor's most important finding, and it reported
		// nothing at all.
		//
		// A shared-database install will not mint a signing key: every process must sign as the same
		// install, so a key created by one of them would be that host's alone. That is deliberate and
		// documented. What was not handled is the consequence: such an install serves no receipt, no
		// bundle and no trust document, which is the whole artifact this product is sold on, and the
		// interface goes on offering a Download receipt button. The health check whose job is to say
		// what is wrong with an install said nothing, so the first sign of it was an error message
		// after a click.
		if canSign != nil && !canSign() {
			report.Findings = append(report.Findings, doctorFinding{
				Severity: "broken", ObjectType: "install", ObjectID: "identity",
				ObjectName: "signing identity",
				Problem: "This install has no signing identity, so it cannot produce a receipt, a " +
					"signed bundle, or a trust document. An install sharing one database will not " +
					"create a key by itself, because every process has to sign as the same install. " +
					"Generate one seed and set " + identity.KeyEnv + " on every server and worker, " +
					"or place " + identity.File + " beside the database for each of them.",
				FixPath: "/ui/docs/configuration",
			})
		}
		report.Findings = append(report.Findings, runFilesFindings(runFiles)...)

		credExists := func(id string) bool {
			if creds == nil || id == "" {
				return true
			}
			_, err := creds.Get(ctx, id)
			return !errors.Is(err, credential.ErrNotFound)
		}

		if templates != nil {
			list, err := templates.List(ctx)
			if err != nil {
				log.Error("server: doctor templates: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not run the doctor")
				return
			}
			report.CheckedTemplates = len(list)
			for _, t := range list {
				add := func(severity, problem string) {
					report.Findings = append(report.Findings, doctorFinding{
						Severity: severity, ObjectType: "template", ObjectID: t.ID,
						ObjectName: namedOr(t.Name, t.ID), Problem: problem, FixPath: "/ui/templates",
					})
				}
				if t.InventoryID != "" && invs != nil {
					if _, err := invs.Get(ctx, t.InventoryID); errors.Is(err, inventory.ErrNotFound) {
						add("broken", "References stored inventory "+t.InventoryID+", which does not exist.")
					}
				}
				if t.ProjectID != "" && projs != nil {
					if _, err := projs.Get(ctx, t.ProjectID); errors.Is(err, project.ErrNotFound) {
						add("broken", "References project "+t.ProjectID+", which does not exist.")
					}
				}
				for _, cid := range t.CredentialIDs {
					if !credExists(cid) {
						add("broken", "References credential "+cid+", which does not exist.")
					}
				}
				for _, cid := range t.SelectableCredentialIDs {
					if !credExists(cid) {
						add("warning", "Offers selectable credential "+cid+", which does not exist.")
					}
				}
				if t.PullCredentialID != "" && !credExists(t.PullCredentialID) {
					add("broken", "References pull credential "+t.PullCredentialID+", which does not exist.")
				}
			}
		}

		if schedules != nil {
			list, err := schedules.List(ctx)
			if err != nil {
				log.Error("server: doctor schedules: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not run the doctor")
				return
			}
			report.CheckedSchedules = len(list)
			for _, s := range list {
				add := func(severity, problem string) {
					report.Findings = append(report.Findings, doctorFinding{
						Severity: severity, ObjectType: "schedule", ObjectID: s.ID,
						ObjectName: namedOr(s.Name, s.ID), Problem: problem, FixPath: "/ui/schedules",
					})
				}
				_, err := s.NextFire(time.Now())
				switch {
				case err == nil:
				case s.RRule != "" && errors.Is(err, schedule.ErrExhausted):
					// A bounded recurrence that has run its course is finished, not broken.
					add("warning", "Recurrence has fired its last time and will not fire again.")
				case s.RRule != "":
					add("broken", "Recurrence rule does not evaluate, so it never fires: "+
						err.Error())
				default:
					add("broken", "Cron expression "+s.Cron+" does not parse, so it never fires.")
				}
				if s.TemplateID != "" && templates != nil {
					t, err := templates.Get(ctx, s.TemplateID)
					if errors.Is(err, template.ErrNotFound) {
						add("broken", "Fires template "+s.TemplateID+", which does not exist.")
					}
					// A survey nobody can answer refuses every fire, which the schedule only says
					// after the first one has come due. Said here, it is fixed before then.
					if err == nil {
						if _, uerr := t.UnattendedOptions(); uerr != nil {
							severity := "broken"
							if !s.Enabled {
								severity = "warning"
							}
							add(severity, "Fires template "+namedOr(t.Name, t.ID)+", whose survey asks "+
								strings.Join(template.UnansweredVars(uerr), ", ")+" with no usable "+
								"default, so every fire is refused with nobody there to answer. Give "+
								"the question a default.")
						}
					}
				}
				if s.SkippedFires >= schedule.SkipBadgeFires {
					report.Findings = append(report.Findings,
						scheduleSkipFinding(ctx, s, templates, invs))
				}
			}
		}

		if creds != nil {
			list, err := creds.List(ctx)
			if err != nil {
				log.Error("server: doctor credentials: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not run the doctor")
				return
			}
			report.CheckedCredentials = len(list)
			for _, c := range list {
				// A federated credential stores no secret by design. What can break it is settings
				// that would not mint, such as a role ARN typed wrong.
				if credential.Federated(c.Kind) {
					if _, err := federation.ParseSettings(c.Kind, c.Settings); err != nil {
						report.Findings = append(report.Findings, doctorFinding{
							Severity: "broken", ObjectType: "credential", ObjectID: c.ID,
							ObjectName: namedOr(c.Name, c.ID),
							Problem:    "Its settings would not mint a token: " + err.Error() + ".",
							FixPath:    "/ui/credentials",
						})
					}
					continue
				}
				if c.Secret == "" {
					report.Findings = append(report.Findings, doctorFinding{
						Severity: "warning", ObjectType: "credential", ObjectID: c.ID,
						ObjectName: namedOr(c.Name, c.ID),
						Problem:    "Has no secret yet, so any run that uses it fails.",
						FixPath:    "/ui/credentials",
					})
				}
			}
		}

		for _, check := range checks {
			res, err := check(ctx)
			if err != nil {
				log.Error("server: doctor check: " + err.Error())
				report.Findings = append(report.Findings, doctorFinding{
					Severity: "warning", ObjectType: "install", ObjectID: "doctor",
					ObjectName: "doctor check",
					Problem: "One of the doctor's checks could not run, so this report may be " +
						"missing what it would have found. The server log says why.",
				})
				continue
			}
			report.Findings = append(report.Findings, res.Findings...)
			for what, n := range res.Checked {
				if report.Checked == nil {
					report.Checked = map[string]int{}
				}
				report.Checked[what] += n
			}
		}

		sort.SliceStable(report.Findings, func(i, j int) bool {
			if report.Findings[i].Severity != report.Findings[j].Severity {
				return report.Findings[i].Severity == "broken"
			}
			return report.Findings[i].ObjectName < report.Findings[j].ObjectName
		})
		respondJSON(w, log, http.StatusOK, report, wantsPretty(r))
	}
}

// namedOr returns an object's name, falling back to its id when it has none.
//
// A name is optional on a template, a schedule and a credential, and the API creates unnamed ones
// without complaint: the tutorial's own copyable schedule command produces one. The doctor then
// reported "schedule  | Fires template tpl_abc123, which no longer exists" with an empty space where
// the identity should be, twice, and an operator reading a list of problems could not tell which
// object to open. A finding nobody can act on is not a finding.
func namedOr(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
