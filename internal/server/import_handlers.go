package server

import (
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/importer"
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
	// Templates names the job templates the import creates.
	Templates []string `json:"templates"`
	// Schedules names the schedules the import creates.
	Schedules []string `json:"schedules"`
	// InventoryContent maps an inventory's name to the content the import would write.
	//
	// A preview that lists only names cannot be reviewed. An inventory is the list of machines a
	// play reaches and the variables it reaches them with, and those come out of somebody else's
	// export: the dry run has to show what apply will actually store, or the review step is a
	// formality performed on a name.
	InventoryContent map[string]string `json:"inventory_content,omitempty"`
	// Warnings names what could not be mapped cleanly or needs follow up.
	Warnings []string `json:"warnings"`
	// SuppressedWarnings counts warnings past the reporting cap, zero when none were dropped.
	SuppressedWarnings int `json:"suppressed_warnings,omitempty"`
	// Applied reports whether the plan was written, not just previewed.
	Applied bool `json:"applied"`
	// Created is how many objects were written when applied.
	Created int `json:"created"`
}

// importStoresFunc returns the stores an import writes to, and whether all are enabled.
type importStoresFunc func() (importer.ApplyStores, bool)

// importHandler previews or applies an AWX, Semaphore, Rundeck, or Jenkins export. POST
// /import/{format} with the export as the body returns the plan; add ?apply=true to write it.
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
		case "rundeck":
			mapper = importer.FromRundeck(r.URL.Query().Get("inventory"))
		case "jenkins":
			mapper = importer.FromJenkins(r.URL.Query().Get("inventory"))
		default:
			respondError(w, log, http.StatusBadRequest,
				"format must be awx, semaphore, rundeck, or jenkins")
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
		if err != nil {
			log.Error("server: map import export: " + err.Error())
			respondError(w, log, http.StatusBadRequest, "could not read the export, check the format")
			return
		}

		resp := importResponse{Warnings: plan.Warnings, SuppressedWarnings: plan.Suppressed()}
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
		for _, t := range plan.Templates {
			resp.Templates = append(resp.Templates, t.Name)
		}
		for _, s := range plan.Schedules {
			resp.Schedules = append(resp.Schedules, s.Name)
		}

		if r.URL.Query().Get("apply") == "true" {
			applyStores, ok := stores()
			if !ok {
				respondError(w, log, http.StatusConflict,
					"apply needs projects, inventories, credentials, templates, and schedules enabled")
				return
			}
			created, err := plan.Apply(r.Context(), applyStores)
			if err != nil {
				log.Error("server: apply import: " + err.Error())
				respondError(w, log, http.StatusInternalServerError, "could not apply import")
				return
			}
			resp.Applied = true
			resp.Created = created
			// Apply resolves template inventory names against what is stored and warns about the ones
			// it could not find, which it does after this response was assembled. Snapshotting the
			// warnings before the write meant the "no stored inventory is named X, so it is used as a
			// path on the server's filesystem" line never reached anybody: the caller saw a clean
			// import and found out only when a run failed on a path that does not exist.
			resp.Warnings = plan.Warnings
			resp.SuppressedWarnings = plan.Suppressed()
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}
