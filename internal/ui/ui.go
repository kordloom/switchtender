// Package ui serves the Yardmaster web interface. It renders shell pages from embedded templates
// and ships embedded static assets. The pages call the JSON API to draw the run history, the host
// status matrix, and the task timeline in the browser.
package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"go.uber.org/zap"
)

// templateFS holds the page templates.
//
//go:embed templates/*.html
var templateFS embed.FS

// assetFS holds the static CSS and JavaScript.
//
//go:embed assets/*
var assetFS embed.FS

// UI renders the web interface.
type UI struct {
	// tmpl holds the parsed page templates.
	tmpl *template.Template
	// log records render failures.
	log *zap.Logger
	// docs is the documentation tree rendered in-app, nil when not wired.
	docs fs.FS
	// md renders documentation markdown to HTML.
	md goldmark.Markdown
	// readOnly hides mutating controls in the pages for a read-only demo.
	readOnly bool
	// matrixCap is the largest host matrix, in cells, the detail page draws before showing a
	// notice instead. Zero or less means no limit.
	matrixCap int
	// oidcEnabled shows the single sign-on button on the sign-in page when set.
	oidcEnabled bool
}

// New parses the embedded templates and returns a UI. It panics if the embedded templates fail to
// parse, which is a build time programming error. docs, when non-nil, is the documentation tree
// served under /ui/docs; readOnly hides the launch panel and run action buttons for a demo.
func New(log *zap.Logger, docs fs.FS, readOnly bool, matrixCap int, oidcEnabled bool) *UI {
	if log == nil {
		log = zap.NewNop()
	}
	return &UI{
		tmpl:        template.Must(template.ParseFS(templateFS, "templates/*.html")),
		log:         log,
		docs:        docs,
		md:          goldmark.New(goldmark.WithExtensions(extension.GFM)),
		readOnly:    readOnly,
		matrixCap:   matrixCap,
		oidcEnabled: oidcEnabled,
	}
}

// Handler returns the HTTP handler for the web interface, served under /ui/.
func (u *UI) Handler() http.Handler {
	assets, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic("ui: assets subtree: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.Handle("GET /ui/assets/", http.StripPrefix("/ui/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /ui/runs/{id}", u.detail)
	mux.HandleFunc("GET /ui/runs", u.runs)
	mux.HandleFunc("GET /ui/fleet", u.fleet)
	mux.HandleFunc("GET /ui/hosts/{host}", u.host)
	mux.HandleFunc("GET /ui/tasks", u.tasks)
	mux.HandleFunc("GET /ui/login", u.login)
	mux.HandleFunc("GET /ui/users", u.users)
	mux.HandleFunc("GET /ui/workers", u.workers)
	mux.HandleFunc("GET /ui/inventories", u.inventories)
	mux.HandleFunc("GET /ui/sources", u.sources)
	mux.HandleFunc("GET /ui/credentials", u.credentials)
	mux.HandleFunc("GET /ui/projects", u.projects)
	mux.HandleFunc("GET /ui/templates", u.jobTemplates)
	mux.HandleFunc("GET /ui/schedules", u.schedules)
	if u.docs != nil {
		mux.HandleFunc("GET /ui/docs", u.docsPage)
		mux.HandleFunc("GET /ui/docs/{page}", u.docsPage)
	}
	mux.HandleFunc("GET /ui/", u.index)
	return mux
}

// index renders the overview home page.
func (u *UI) index(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "overview.html", map[string]any{"ReadOnly": u.readOnly})
}

// runs renders the run history page.
func (u *UI) runs(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "runs.html", map[string]any{"ReadOnly": u.readOnly})
}

// detail renders the run detail page for a single run.
func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	u.render(w, "detail.html", map[string]any{
		"RunID": r.PathValue("id"), "ReadOnly": u.readOnly, "MatrixCap": u.matrixCap,
	})
}

// fleet renders the fleet health page.
func (u *UI) fleet(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "fleet.html", map[string]any{"ReadOnly": u.readOnly})
}

// host renders one host's run history page.
func (u *UI) host(w http.ResponseWriter, r *http.Request) {
	u.render(w, "host.html", map[string]any{"Host": r.PathValue("host"), "ReadOnly": u.readOnly})
}

// tasks renders the task duration trends page.
func (u *UI) tasks(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "tasks.html", map[string]any{"ReadOnly": u.readOnly})
}

// credentials renders the credential management page.
func (u *UI) credentials(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "credentials.html", map[string]any{"ReadOnly": u.readOnly})
}

// projects renders the git project management page.
func (u *UI) projects(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "projects.html", map[string]any{"ReadOnly": u.readOnly})
}

// jobTemplates renders the job template management page.
func (u *UI) jobTemplates(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "jobtemplates.html", map[string]any{"ReadOnly": u.readOnly})
}

// users renders the account management page.
func (u *UI) users(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "users.html", map[string]any{"ReadOnly": u.readOnly})
}

// workers renders the executor fleet page.
func (u *UI) workers(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "workers.html", map[string]any{"ReadOnly": u.readOnly})
}

// inventories renders the stored inventory management page.
func (u *UI) inventories(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "inventories.html", map[string]any{"ReadOnly": u.readOnly})
}

// sources renders the dynamic inventory source page.
func (u *UI) sources(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "sources.html", map[string]any{"ReadOnly": u.readOnly})
}

// login renders the token sign in page.
func (u *UI) login(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "login.html", map[string]any{"OIDCEnabled": u.oidcEnabled})
}

// schedules renders the schedules page.
func (u *UI) schedules(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "schedules.html", map[string]any{"ReadOnly": u.readOnly})
}

// render executes the named template with data.
func (u *UI) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		u.log.Error("ui: render " + name + ": " + err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
