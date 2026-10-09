package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/importer"
	"github.com/kordloom/switchtender/internal/org"
)

// maxImportBody caps an uploaded export document, generous enough for a large AWX export.
//
// It is not generous enough for every Rundeck project archive. An archive carries the project's
// execution logs, run state, and reports alongside the definitions the importer reads, so one from
// a busy project can exceed this while staying well inside the 64 MiB the archive reader bounds
// the definitions it reads by. Such an archive imports from the CLI, which applies no body limit,
// and is refused here with 413.
const maxImportBody = 25 << 20

// importResponse summarizes an import plan, and the result when it is applied.
type importResponse struct {
	// Projects names the git projects the import creates.
	Projects []string `json:"projects"`
	// Inventories names the stored inventories the import creates, including each dynamic source's
	// backing inventory.
	Inventories []string `json:"inventories"`
	// Sources names the dynamic inventory sources the import creates.
	Sources []string `json:"sources"`
	// Credentials names the credential shells the import creates; their secrets must be set after.
	Credentials []string `json:"credentials"`
	// CredentialTypes names the custom credential types the import creates.
	CredentialTypes []string `json:"credential_types,omitempty"`
	// Templates names the job templates the import creates.
	Templates []string `json:"templates"`
	// Report summarizes what comes across, what needs a secret, and what does not transfer. The
	// itemized lists below answer what an import did; this answers whether to attempt one.
	Report *importer.Report `json:"report,omitempty"`
	// Schedules names the schedules the import creates.
	Schedules []string `json:"schedules"`
	// Notifications names the notification targets the import creates. Their addresses and keys
	// are never in the response.
	Notifications []string `json:"notifications"`
	// Attachments counts the attachments the import creates between those targets and the
	// templates, workflows, and organizations that name them.
	Attachments int `json:"attachments"`
	// InventoryContent maps an inventory's name to the content the import would write.
	//
	// A preview that lists only names cannot be reviewed. An inventory is the list of machines a
	// play reaches and the variables it reaches them with, and those come out of somebody else's
	// export: the dry run has to show what apply will actually store, or the review step is a
	// formality performed on a name.
	InventoryContent map[string]string `json:"inventory_content,omitempty"`
	// Organizations names the organizations the import creates for what came from an AWX
	// organization: its smart inventories with the inventories they filter, and its own
	// notification attachments with the templates they cover. Each is created when the import is
	// applied unless exactly one of that name already exists.
	Organizations []string `json:"organizations,omitempty"`
	// InventoryOrganizations maps the name of each inventory placed in an organization to that
	// organization's name, so a smart inventory's reach can be reviewed before and after apply.
	InventoryOrganizations map[string]string `json:"inventory_organizations,omitempty"`
	// TemplateOrganizations maps the name of each template placed in an organization to that
	// organization's name, since placing a template changes who may see it under --strict-grants.
	TemplateOrganizations map[string]string `json:"template_organizations,omitempty"`
	// Warnings names what could not be mapped cleanly or needs follow up.
	Warnings []string `json:"warnings"`
	// SuppressedWarnings counts warnings past the reporting cap, zero when none were dropped.
	SuppressedWarnings int `json:"suppressed_warnings,omitempty"`
	// Applied reports whether the plan was written, not just previewed.
	Applied bool `json:"applied"`
	// Created is how many objects were written when applied.
	Created int `json:"created"`
}

// logUnreferencedFiles warns, once per file, about each file an imported credential type writes
// that no injector references. The import carries such a type as it was, and the report says so to
// whoever read it. The log says so to whoever runs the server, who is the one a later run's warning
// about the same file will reach. Only the type and the file are named, never a template.
func logUnreferencedFiles(log *zap.Logger, plan *importer.Plan) {
	for _, ct := range plan.CredentialTypes {
		for _, file := range ct.UnreferencedFiles() {
			log.Warn("server: imported credential type writes a file no injector references",
				zap.String("credential_type", ct.Name), zap.String("credential_type_id", ct.ID),
				zap.String("file", file))
		}
	}
}

// importStoresFunc returns the stores an import writes to, and whether all are enabled.
type importStoresFunc func() (importer.ApplyStores, bool)

// applyRequested reports whether an import request asks to write, rather than to preview.
//
// Read here to decide whether to apply, and by the read-only gate to decide whether to let the
// request through at all. One accessor rather than the same comparison written twice, because those
// two readings disagreeing is a read-only deployment that quietly accepts a write.
func applyRequested(r *http.Request) bool {
	return r.URL.Query().Get("apply") == "true"
}

// importHandler previews or applies an AWX, Semaphore, Chef, Puppet, Rundeck, or Jenkins export.
// POST /import/{format} with the export as the body returns the plan, and ?apply=true writes it.
// Preview needs no stores; apply needs projects, inventories, credentials, templates, and schedules
// all enabled.
//
// The Rundeck and Jenkins importers need an inventory, since one dispatches by node filter and the
// other picks an agent by label, so neither names hosts. It comes from the ?inventory= query
// parameter, and the plan reports its absence rather than refusing, so a preview still shows what
// the export holds.
//
// Two formats accept a zip body. Jenkins has no single-file export the way the others do, so its
// body may be a zip of a jobs directory as well as one config.xml. Rundeck accepts a project
// archive, which is a zip, as well as a job export. Each importer tells its artifacts apart by
// content, so the format in the path is all a caller sets.
//
// A body over maxImportBody is refused with 413, which is a lower ceiling than the CLI applies: the
// CLI reads the file whole and only the archive readers' own limits bound it. A Rundeck project
// archive from a busy project carries every execution log alongside the definitions this reads, so
// one can exceed this limit and still import from the command line.
func importHandler(stores importStoresFunc, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var mapper func([]byte, time.Time) (*importer.Plan, error)
		switch r.PathValue("format") {
		case "awx":
			mapper = importer.FromAWX
		case "semaphore":
			mapper = importer.FromSemaphore
		case "chef":
			mapper = importer.FromChef
		case "puppet":
			mapper = importer.FromPuppet
		case "rundeck":
			mapper = importer.FromRundeck(r.URL.Query().Get("inventory"))
		case "jenkins":
			mapper = importer.FromJenkins(r.URL.Query().Get("inventory"))
		case "cron":
			// Cron is absent from the list above on purpose, and a caller who read the migration
			// guide knows it is a supported source. Left to the generic refusal they are told cron
			// is not a format, which reads as the guide being wrong rather than this endpoint being
			// narrower than the command line.
			respondError(w, log, http.StatusBadRequest,
				"a crontab imports from the command line only, with switchtender import cron")
			return
		default:
			respondError(w, log, http.StatusBadRequest,
				"format must be awx, semaphore, chef, puppet, rundeck, or jenkins")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBody))
		if err != nil {
			respondError(w, log, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		plan, err := mapper(body, time.Now())
		if errors.Is(err, importer.ErrNothingRecognized) {
			// The document parsed and held nothing this importer reads, which is a different problem
			// from malformed bytes and has a different remedy: the caller exported the wrong thing.
			// Carrying the mapper's own sentence is what tells them which.
			respondError(w, log, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if errors.Is(err, importer.ErrArchive) || errors.Is(err, importer.ErrNotText) {
			// An archive reader's refusal, or a body that is not whole text, is a sentence written
			// for the person who sent it, and the command line prints it to them. It names nothing
			// of the server: the archive's own member names, its counts, and its sizes. It goes
			// back as written, because a line about the format would tell a reader whose zip holds
			// too many entries that the export is the wrong kind. It is logged as a warning with no
			// stack trace, because the input failed and the server did not.
			log.Warn("server: import refused: " + err.Error())
			respondError(w, log, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			log.Error("server: map import export: " + err.Error())
			respondError(w, log, http.StatusBadRequest, "could not read the export, check the format")
			return
		}

		// The same summary the command line prints, so a migration decision made through the page
		// rests on the same numbers as one made from a terminal.
		report := plan.Report()
		resp := importResponse{Warnings: plan.Warnings, SuppressedWarnings: plan.Suppressed(),
			Report: &report}
		for _, p := range plan.Projects {
			resp.Projects = append(resp.Projects, p.Name)
		}
		for _, inv := range plan.Inventories {
			resp.Inventories = append(resp.Inventories, inv.Name)
			if inv.Content != "" {
				if resp.InventoryContent == nil {
					resp.InventoryContent = map[string]string{}
				}
				resp.InventoryContent[inv.Name] = inv.Content
			}
		}
		for _, s := range plan.Sources {
			resp.Sources = append(resp.Sources, s.Name)
		}
		for _, c := range plan.Credentials {
			resp.Credentials = append(resp.Credentials, c.Name)
		}
		for _, ct := range plan.CredentialTypes {
			resp.CredentialTypes = append(resp.CredentialTypes, ct.Name)
		}
		for _, t := range plan.Templates {
			resp.Templates = append(resp.Templates, t.Name)
		}
		for _, s := range plan.Schedules {
			resp.Schedules = append(resp.Schedules, s.Name)
		}
		for _, n := range plan.Notifications {
			resp.Notifications = append(resp.Notifications, n.Name)
		}
		resp.Attachments = len(plan.Attachments)
		// Where each organization lands, and what is placed in it. Apply settles each organization
		// against what is stored, using the one of the same name in place of a new one and leaving
		// what an ambiguous name would hold unplaced, so this is read again once it has run.
		placement := func() {
			resp.Organizations = orgNames(plan.Orgs)
			resp.InventoryOrganizations = plan.InventoryOrganizations()
			resp.TemplateOrganizations = plan.TemplateOrganizations()
		}
		placement()

		if applyRequested(r) {
			applyStores, ok := stores()
			if !ok {
				respondError(w, log, http.StatusConflict,
					"apply needs projects, inventories, credentials, templates, and schedules enabled")
				return
			}
			created, err := plan.Apply(r.Context(), applyStores)
			if errors.Is(err, importer.ErrAlreadyImported) {
				respondError(w, log, http.StatusConflict, err.Error())
				return
			}
			if errors.Is(err, importer.ErrUnstorableText) {
				respondError(w, log, http.StatusBadRequest, err.Error())
				return
			}
			if err != nil {
				log.Error("server: apply import: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not apply import")
				return
			}
			resp.Applied = true
			resp.Created = created
			logUnreferencedFiles(log, plan)
			// Apply resolves template inventory names against what is stored and warns about the ones
			// it could not find, which it does after this response was assembled. Snapshotting the
			// warnings before the write meant the "no stored inventory is named X, so it is used as a
			// path on the server's filesystem" line never reached anybody: the caller saw a clean
			// import and found out only when a run failed on a path that does not exist.
			resp.Warnings = plan.Warnings
			resp.SuppressedWarnings = plan.Suppressed()
			// The apply settles where an organization's notification attachments went and where
			// each placed object landed, so the report and the names are taken again after it
			// rather than kept from the preview.
			applied := plan.Report()
			resp.Report = &applied
			resp.Attachments = len(plan.Attachments)
			placement()
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}

// orgNames names the organizations an import creates.
func orgNames(orgs []*org.Org) []string {
	var out []string
	for _, o := range orgs {
		out = append(out, o.Name)
	}
	return out
}
