// Package ui serves the SwitchTender web interface. It renders shell pages from embedded templates
// and ships embedded static assets. The pages call the JSON API to draw the run history, the host
// status matrix, and the task timeline in the browser.
package ui

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// templateFS holds the page templates.
//
//go:embed templates/*.html
var templateFS embed.FS

// assetFS holds the static CSS and JavaScript. The entries are named individually so the jstest
// tree beside them, the node --test suite for the js/ parts, is never embedded or served.
//
//go:embed assets/app.css assets/favicon.png assets/fonts assets/js
//go:embed assets/logo-train-tracks.png assets/logo-train-tracks-dark.png
var assetFS embed.FS

// UI renders the web interface.
type UI struct {
	// tmpl holds the parsed page templates.
	tmpl *template.Template
	// log records render failures.
	log *zap.Logger
	// docs is the documentation tree rendered in-app, nil when not wired.
	docs fs.FS
	// docCache holds each rendered documentation page by slug. The tree is embedded, so a page's
	// HTML is the same for the life of the binary and is built on its first request only.
	docCache sync.Map
	// md renders documentation markdown to HTML.
	md goldmark.Markdown
	// readOnly hides mutating controls in the pages for a read-only demo.
	readOnly bool
	// matrixCap is the largest host matrix, in cells, the detail page draws before showing a
	// notice instead. Zero or less means no limit.
	matrixCap int
	// oidcEnabled shows the single sign-on button on the sign-in page when set.
	oidcEnabled bool
	// oidcBrand names the OIDC provider ("google", "microsoft", ...) so the sign-in button carries
	// that provider's label and mark. Empty renders a generic single sign-on button.
	oidcBrand string
	// samlEnabled shows the SAML sign-in button on the sign-in page when set.
	samlEnabled bool
	// hasAccounts reports whether this install holds any user account, asked at render time because
	// an install gains its first account while the server is running. Nil counts as having them, so
	// nothing changes where the caller said nothing.
	hasAccounts func() bool
	// hasTokens reports whether any API token exists. An install with tokens and no accounts is
	// authenticated, not open, and the sign-in page said the opposite. Nil counts as having them.
	hasTokens func() bool
	// aiEnabled reports whether an advisory AI provider is configured, so the overview can make the
	// ask panel clearly unavailable rather than looking usable and failing on the first question.
	aiEnabled bool
}

// New parses the embedded templates and returns a UI. It panics if the embedded templates fail to
// parse, which is a build time programming error. docs, when non-nil, is the documentation tree
// served under /ui/docs; readOnly hides the launch panel and run action buttons for a demo.
func New(log *zap.Logger, docs fs.FS, readOnly bool, matrixCap int, oidcEnabled, samlEnabled, aiEnabled bool,
	oidcBrand string, opts ...Option) *UI {
	if log == nil {
		log = zap.NewNop()
	}
	u := &UI{
		tmpl: template.Must(template.ParseFS(templateFS, "templates/*.html")),
		log:  log,
		docs: docs,
		// Heading ids, the same option cmd/sitegen renders the published docs with. Without them
		// every in-page anchor in the in-app docs was inert: a visible "download" link moved
		// nothing but the address bar, and each cross-guide link landed at the top of the right
		// guide rather than the section it named. The identical markdown jumped correctly on the
		// marketing site, so the product shipped the broken copy of its own documentation.
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		),
		readOnly:    readOnly,
		matrixCap:   matrixCap,
		oidcEnabled: oidcEnabled,
		oidcBrand:   oidcBrand,
		samlEnabled: samlEnabled,
		aiEnabled:   aiEnabled,
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// Option configures a UI beyond the arguments every caller passes.
type Option func(*UI)

// WithAccountCheck tells the sign-in page whether this install has any accounts.
//
// A fresh install has none, so the username and password form on it cannot work and every attempt
// answers "bad credentials" with nothing on the page saying why. The read-only demo got an
// explanation in that exact spot and the fresh install did not, which is the case where a stranger
// is most likely to be stuck.
func WithAccountCheck(f func() bool) Option {
	return func(u *UI) { u.hasAccounts = f }
}

// accountsExist reports whether the sign-in page should present the account form as usable.
func (u *UI) accountsExist() bool {
	if u.hasAccounts == nil {
		return true
	}
	return u.hasAccounts()
}

// WithTokenCheck tells the sign-in page whether this install holds any API token.
//
// Without it the page could not tell an install that is genuinely open from one that authenticates
// with tokens and simply has no user accounts. It told the second kind it "runs open" and offered a
// link straight to the interface, which bounced back to this same page, so the only advice on the
// screen was a loop.
func WithTokenCheck(f func() bool) Option {
	return func(u *UI) { u.hasTokens = f }
}

// tokensExist reports whether any API token is present. Unknown counts as present, so a caller that
// said nothing never produces the "runs open" claim.
func (u *UI) tokensExist() bool {
	if u.hasTokens == nil {
		return true
	}
	return u.hasTokens()
}

// Handler returns the HTTP handler for the web interface, served under /ui/.
func (u *UI) Handler() http.Handler {
	assets, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic("ui: assets subtree: " + err.Error())
	}
	mux := http.NewServeMux()
	mux.Handle("GET /ui/assets/", http.StripPrefix("/ui/assets/", newAssetHandler(assets)))
	mux.HandleFunc("GET /ui/runs/{id}", u.detail)
	mux.HandleFunc("GET /ui/runs/{id}/compare", u.compare)
	mux.HandleFunc("GET /ui/runs", u.runs)
	mux.HandleFunc("GET /ui/fleet", u.fleet)
	mux.HandleFunc("GET /ui/activity", u.activity)
	mux.HandleFunc("GET /ui/doctor", u.doctor)
	mux.HandleFunc("GET /ui/drift", u.drift)
	mux.HandleFunc("GET /ui/estate", u.estate)
	mux.HandleFunc("GET /ui/hosts/{host}", u.host)
	mux.HandleFunc("GET /ui/tasks", u.tasks)
	mux.HandleFunc("GET /ui/login", u.login)
	mux.HandleFunc("GET /ui/users", u.users)
	mux.HandleFunc("GET /ui/workers", u.workers)
	mux.HandleFunc("GET /ui/inventories", u.inventories)
	mux.HandleFunc("GET /ui/sources", u.sources)
	mux.HandleFunc("GET /ui/credentials", u.credentials)
	mux.HandleFunc("GET /ui/audit", u.audit)
	mux.HandleFunc("GET /ui/policies", u.policies)
	mux.HandleFunc("GET /ui/projects", u.projects)
	mux.HandleFunc("GET /ui/templates", u.jobTemplates)
	mux.HandleFunc("GET /ui/schedules", u.schedules)
	mux.HandleFunc("GET /ui/workflows", u.workflows)
	mux.HandleFunc("GET /ui/migrate", u.migrate)
	if u.docs != nil {
		mux.HandleFunc("GET /ui/docs", u.docsPage)
		mux.HandleFunc("GET /ui/docs/{page}", u.docsPage)
	}
	mux.HandleFunc("GET /ui/", u.index)
	return mux
}

// index renders the overview home page. It is registered on the subtree pattern
// "/ui/", which also matches every path beneath it that no other route claims, so
// a path this handler does not own is refused rather than answered with the
// overview page: a mistyped or renamed route must say so instead of rendering
// something plausible.
func (u *UI) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ui/" {
		u.notFound(w, r)
		return
	}
	u.render(w, "overview.html", map[string]any{"ReadOnly": u.readOnly, "AIOff": !u.aiEnabled})
}

// notFound answers a path under /ui/ that no route owns. Go's stock handler answers with bare
// text on a blank page: no nav, no way back, and nothing naming what went wrong. A mistyped or
// stale address is the one moment a reader most needs a way onward, so this renders the real
// chrome and points at the pages they probably wanted.
func (u *UI) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	u.render(w, "notfound.html", map[string]any{"ReadOnly": u.readOnly, "Path": r.URL.Path})
}

// runs renders the run history page.
func (u *UI) runs(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "runs.html", map[string]any{"ReadOnly": u.readOnly, "AIOff": !u.aiEnabled, "ExtraTools": run.ExtraToolNames()})
}

// detail renders the run detail page for a single run.
func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	u.render(w, "detail.html", map[string]any{
		"RunID": r.PathValue("id"), "ReadOnly": u.readOnly, "AIOff": !u.aiEnabled, "MatrixCap": u.matrixCap,
	})
}

// compare renders the run comparison page.
func (u *UI) compare(w http.ResponseWriter, r *http.Request) {
	u.render(w, "compare.html", map[string]any{
		"RunID": r.PathValue("id"), "ReadOnly": u.readOnly,
	})
}

// fleet renders the fleet health page.
func (u *UI) fleet(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "fleet.html", map[string]any{"ReadOnly": u.readOnly})
}

// doctor renders the reference health page.
func (u *UI) doctor(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "doctor.html", map[string]any{"ReadOnly": u.readOnly})
}

// drift renders the fleet drift page.
func (u *UI) drift(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "drift.html", map[string]any{"ReadOnly": u.readOnly})
}

// estate renders the point-in-time estate page, which answers what the fleet looked like on a date
// and what has moved since. Both questions read the same history, so they are one page rather than
// two: an operator asking the second almost always wants the first in front of them.
func (u *UI) estate(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "estate.html", map[string]any{"ReadOnly": u.readOnly})
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

// audit renders the audit trail page with chain verification and signed bundle download.
func (u *UI) audit(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "audit.html", map[string]any{"ReadOnly": u.readOnly})
}

// policies renders the approval policy management page.
func (u *UI) policies(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "policies.html", map[string]any{"ReadOnly": u.readOnly, "ExtraTools": run.ExtraToolNames()})
}

// projects renders the git project management page.
func (u *UI) projects(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "projects.html", map[string]any{"ReadOnly": u.readOnly})
}

// migrate renders the import page, which offers the four export formats the server reads: AWX,
// Semaphore, Rundeck, and Jenkins.
func (u *UI) migrate(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "migrate.html", map[string]any{"ReadOnly": u.readOnly})
}

// jobTemplates renders the job template management page.
func (u *UI) jobTemplates(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "jobtemplates.html", map[string]any{"ReadOnly": u.readOnly, "ExtraTools": run.ExtraToolNames()})
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
	u.render(w, "login.html", map[string]any{"OIDCEnabled": u.oidcEnabled, "OIDCBrand": u.oidcBrand,
		"SAMLEnabled": u.samlEnabled, "ReadOnly": u.readOnly,
		"NoAccounts": !u.accountsExist(), "TokenOnly": !u.accountsExist() && u.tokensExist()})
}

// schedules renders the schedules page.
func (u *UI) schedules(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "schedules.html", map[string]any{"ReadOnly": u.readOnly})
}

// activity renders the full-page activity view: the windowed run chart with an outcome breakdown and
// a CSV export of the bucketed data.
func (u *UI) activity(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "activity.html", map[string]any{"ReadOnly": u.readOnly})
}

// workflows renders the visual workflow editor, where steps are wired into a graph and run as a
// pipeline.
func (u *UI) workflows(w http.ResponseWriter, _ *http.Request) {
	u.render(w, "workflows.html", map[string]any{"ReadOnly": u.readOnly, "AIOff": !u.aiEnabled, "ExtraTools": run.ExtraToolNames()})
}

// render executes the named template with data.
//
// The page is built in memory and only written once it is whole. Executing straight to the response
// put the part that rendered before a fault on the wire, which sent the status line with it: the
// reader got a two hundred, a truncated page, and the error message appended to it, and nothing
// counting statuses ever saw a failure. Every page here is a single template with no partial-write
// protection in front of it, so any fault past the first action landed that way.
func (u *UI) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := u.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		u.log.Error("ui: render " + name + ": " + err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write(buf.Bytes()); err != nil {
		// The status line is already sent, so the reader can only be told by the connection ending.
		// The record of the short write belongs in the log.
		u.log.Error("ui: write " + name + ": " + err.Error())
	}
}
