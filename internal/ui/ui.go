// Package ui serves the Yardmaster web interface. It renders shell pages from embedded templates
// and ships embedded static assets. The pages call the JSON API to draw the run history, the host
// status matrix, and the task timeline in the browser.
package ui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

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
}

// New parses the embedded templates and returns a UI. It panics if the embedded templates fail to
// parse, which is a build time programming error.
func New(log *zap.Logger) *UI {
	if log == nil {
		log = zap.NewNop()
	}
	return &UI{
		tmpl: template.Must(template.ParseFS(templateFS, "templates/*.html")),
		log:  log,
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
	mux.HandleFunc("GET /ui/", u.index)
	return mux
}

// index renders the run history page.
func (u *UI) index(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "index.html", nil)
}

// detail renders the run detail page for a single run.
func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	u.render(w, "detail.html", map[string]string{"RunID": r.PathValue("id")})
}

// fleet renders the fleet health page.
func (u *UI) fleet(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "fleet.html", nil)
}

// host renders one host's run history page.
func (u *UI) host(w http.ResponseWriter, r *http.Request) {
	u.render(w, "host.html", map[string]string{"Host": r.PathValue("host")})
}

// tasks renders the task duration trends page.
func (u *UI) tasks(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "tasks.html", nil)
}

// credentials renders the credential management page.
func (u *UI) credentials(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "credentials.html", nil)
}

// projects renders the git project management page.
func (u *UI) projects(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "projects.html", nil)
}

// jobTemplates renders the job template management page.
func (u *UI) jobTemplates(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "jobtemplates.html", nil)
}

// users renders the account management page.
func (u *UI) users(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "users.html", nil)
}

// workers renders the executor fleet page.
func (u *UI) workers(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "workers.html", nil)
}

// inventories renders the stored inventory management page.
func (u *UI) inventories(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "inventories.html", nil)
}

// sources renders the dynamic inventory source page.
func (u *UI) sources(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "sources.html", nil)
}

// login renders the token sign in page.
func (u *UI) login(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "login.html", nil)
}

// schedules renders the schedules page.
func (u *UI) schedules(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "schedules.html", nil)
}

// render executes the named template with data.
func (u *UI) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.tmpl.ExecuteTemplate(w, name, data); err != nil {
		u.log.Error("ui: render " + name + ": " + err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
