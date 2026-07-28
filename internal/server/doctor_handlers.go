package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// doctorFinding is one problem the doctor found in the control plane's registrations: a reference
// to something that no longer exists, a credential that cannot be used yet, or a schedule that can
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
}

// doctorHandler verifies every registered reference still resolves: template references to
// inventories, projects, and credentials, schedule references to templates, schedule cron
// expressions, and credentials still waiting for a secret. Stores that are not configured are
// skipped rather than reported.
func doctorHandler(templates template.Store, schedules schedule.Store, creds credential.Store,
	invs inventory.Store, projs project.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		report := doctorReport{Findings: []doctorFinding{}}

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
						ObjectName: t.Name, Problem: problem, FixPath: "/ui/templates",
					})
				}
				if t.InventoryID != "" && invs != nil {
					if _, err := invs.Get(ctx, t.InventoryID); errors.Is(err, inventory.ErrNotFound) {
						add("broken", "References stored inventory "+t.InventoryID+", which no longer exists.")
					}
				}
				if t.ProjectID != "" && projs != nil {
					if _, err := projs.Get(ctx, t.ProjectID); errors.Is(err, project.ErrNotFound) {
						add("broken", "References project "+t.ProjectID+", which no longer exists.")
					}
				}
				for _, cid := range t.CredentialIDs {
					if !credExists(cid) {
						add("broken", "References credential "+cid+", which no longer exists.")
					}
				}
				for _, cid := range t.SelectableCredentialIDs {
					if !credExists(cid) {
						add("warning", "Offers selectable credential "+cid+", which no longer exists.")
					}
				}
				if t.PullCredentialID != "" && !credExists(t.PullCredentialID) {
					add("broken", "References pull credential "+t.PullCredentialID+", which no longer exists.")
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
						ObjectName: s.Name, Problem: problem, FixPath: "/ui/schedules",
					})
				}
				if _, err := schedule.NextFire(s.Cron, time.Now()); err != nil {
					add("broken", "Cron expression "+s.Cron+" does not parse, so it never fires.")
				}
				if s.TemplateID != "" && templates != nil {
					if _, err := templates.Get(ctx, s.TemplateID); errors.Is(err, template.ErrNotFound) {
						add("broken", "Fires template "+s.TemplateID+", which no longer exists.")
					}
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
				if c.Secret == "" {
					report.Findings = append(report.Findings, doctorFinding{
						Severity: "warning", ObjectType: "credential", ObjectID: c.ID,
						ObjectName: c.Name,
						Problem:    "Has no secret yet, so any run that uses it fails.",
						FixPath:    "/ui/credentials",
					})
				}
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
