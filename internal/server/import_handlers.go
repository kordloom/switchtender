package server

import (
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/railwarden/internal/importer"
)

// maxImportBody caps an uploaded export document, generous enough for a large AWX export.
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
	// Warnings names what could not be mapped cleanly or needs follow up.
	Warnings []string `json:"warnings"`
	// Applied reports whether the plan was written, not just previewed.
	Applied bool `json:"applied"`
	// Created is how many objects were written when applied.
	Created int `json:"created"`
}

// importStoresFunc returns the stores an import writes to, and whether all are enabled.
type importStoresFunc func() (importer.ApplyStores, bool)

// importHandler previews or applies an AWX or Semaphore export. POST /import/{format} with the
// export as the body returns the plan; add ?apply=true to write it. Preview needs no stores; apply
// needs projects, inventories, credentials, templates, and schedules all enabled.
func importHandler(stores importStoresFunc, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var mapper func([]byte, time.Time) (*importer.Plan, error)
		switch r.PathValue("format") {
		case "awx":
			mapper = importer.FromAWX
		case "semaphore":
			mapper = importer.FromSemaphore
		default:
			respondError(w, log, http.StatusBadRequest, "format must be awx or semaphore")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxImportBody))
		if err != nil {
			respondError(w, log, http.StatusRequestEntityTooLarge, "export too large")
			return
		}
		plan, err := mapper(body, time.Now())
		if err != nil {
			respondError(w, log, http.StatusBadRequest, "could not read export: "+err.Error())
			return
		}

		resp := importResponse{Warnings: plan.Warnings}
		for _, p := range plan.Projects {
			resp.Projects = append(resp.Projects, p.Name)
		}
		for _, inv := range plan.Inventories {
			resp.Inventories = append(resp.Inventories, inv.Name)
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
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}
