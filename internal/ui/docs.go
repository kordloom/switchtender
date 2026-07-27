package ui

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// docOrder is the order documentation pages appear in the sidebar. Files not listed follow, sorted.
var docOrder = []string{
	"README", "quickstart", "faq", "switching-from-awx",
	"tutorials", "tutorial-run-a-job", "tutorial-save-a-template", "tutorial-schedule-a-job",
	"tutorial-set-a-secret", "tutorial-migrate",
	"tool-bash", "tool-terraform", "tool-opentofu", "tool-python", "tool-powershell", "tool-go",
	"sdk",
	"concepts", "reliability", "benchmarks", "configuration", "backup", "desktop", "features", "secrets", "drift", "ai",
	"api", "migration", "comparison",
}

// mdLinkPattern matches links between documentation pages, which are relative markdown file names on
// disk. In the app they must point at the docs routes instead.
var mdLinkPattern = regexp.MustCompile(`href="([a-zA-Z0-9._/-]+?)\.md(#[^"]*)?"`)

// rewriteDocLinks turns relative markdown links into app documentation routes, so cross-page links
// that resolve on disk also resolve at /ui/docs.
func rewriteDocLinks(html string) string {
	return mdLinkPattern.ReplaceAllStringFunc(html, func(match string) string {
		parts := mdLinkPattern.FindStringSubmatch(match)
		slug, anchor := path.Base(parts[1]), parts[2]
		if slug == "README" {
			return `href="/ui/docs` + anchor + `"`
		}
		return `href="/ui/docs/` + slug + anchor + `"`
	})
}

// docLink is one sidebar entry.
type docLink struct {
	// Slug is the page file name without its extension.
	Slug string
	// Title is the page's first heading, or the slug when it has none.
	Title string
	// Href is the page URL.
	Href string
	// Active marks the page currently shown.
	Active bool
}

// docsPage renders one documentation page from markdown into the UI shell with a sidebar of the
// other pages.
func (u *UI) docsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("page")
	if slug == "" {
		slug = "README"
	}
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(u.docs, slug+".md")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var body bytes.Buffer
	if err := u.md.Convert(data, &body); err != nil {
		u.log.Error("ui: render docs " + slug + ": " + err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u.render(w, "docs.html", map[string]any{
		"Title": docTitle(data, slug),
		//nolint:gosec // Rendered from trusted embedded docs.
		"Content": template.HTML(rewriteDocLinks(body.String())),
		"Pages":   u.docList(slug),
	})
}

// docList builds the sidebar, ordered by docOrder with any extra pages appended, marking the active
// one.
func (u *UI) docList(active string) []docLink {
	present := map[string]bool{}
	if entries, err := fs.Glob(u.docs, "*.md"); err == nil {
		for _, e := range entries {
			present[strings.TrimSuffix(e, ".md")] = true
		}
	}

	seen := map[string]bool{}
	var order []string
	for _, slug := range docOrder {
		if present[slug] {
			order = append(order, slug)
			seen[slug] = true
		}
	}
	for slug := range present {
		if !seen[slug] {
			order = append(order, slug)
		}
	}

	links := make([]docLink, 0, len(order))
	for _, slug := range order {
		title := slug
		if data, err := fs.ReadFile(u.docs, slug+".md"); err == nil {
			title = docTitle(data, slug)
		}
		links = append(links, docLink{
			Slug: slug, Title: title, Href: docHref(slug), Active: slug == active,
		})
	}
	return links
}

// docHref returns the URL for a documentation slug; the overview lives at the docs root.
func docHref(slug string) string {
	if slug == "README" {
		return "/ui/docs"
	}
	return "/ui/docs/" + slug
}

// docTitle returns a page's first level-one heading, or the slug when it has none.
func docTitle(data []byte, slug string) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		if heading, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(heading)
		}
	}
	return slug
}

// validSlug reports whether s is a safe documentation slug: lowercase letters, digits, and dashes.
func validSlug(s string) bool {
	if s == "" || s == "README" {
		return true
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
