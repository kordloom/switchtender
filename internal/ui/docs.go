package ui

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
)

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

// docPage is one documentation page prepared for the shell.
type docPage struct {
	// Title is the page's first heading.
	Title string
	// Content is the rendered HTML with its cross-page links rewritten.
	Content template.HTML
	// Pages is the sidebar with this page marked active.
	Pages []docLink
}

// docsPage renders one documentation page from markdown into the UI shell with a sidebar of the
// other pages.
//
// A page is built once and kept. The documentation is embedded, so its markdown cannot change while
// the binary runs, yet every request re-ran goldmark over the page, re-ran a regular expression
// across the whole rendered result, and then read every other page off the tree to rebuild the same
// sidebar. The cache is keyed by slug and only ever holds pages that exist, so it is bounded by the
// embedded tree rather than by what a caller asks for.
func (u *UI) docsPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("page")
	if slug == "" {
		slug = "README"
	}
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	cached, ok := u.docCache.Load(slug)
	if !ok {
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
		// LoadOrStore rather than Store, so two first requests for the same page settle on one copy.
		cached, _ = u.docCache.LoadOrStore(slug, &docPage{
			Title: docTitle(data, slug),
			//nolint:gosec // Rendered from trusted embedded docs.
			Content: template.HTML(rewriteDocLinks(body.String())),
			Pages:   u.docList(slug),
		})
	}
	page, ok := cached.(*docPage)
	if !ok {
		u.log.Error("ui: docs cache holds " + slug + " as something other than a page")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	u.render(w, "docs.html", map[string]any{
		"Title": page.Title, "Content": page.Content, "Pages": page.Pages,
	})
}

// docList builds the sidebar in alphabetical order by title, so a reader finds a guide by name
// without learning a curated sequence. The active page is marked.
func (u *UI) docList(active string) []docLink {
	entries, err := fs.Glob(u.docs, "*.md")
	if err != nil {
		return nil
	}
	links := make([]docLink, 0, len(entries))
	for _, e := range entries {
		slug := strings.TrimSuffix(e, ".md")
		title := slug
		if data, readErr := fs.ReadFile(u.docs, e); readErr == nil {
			title = docTitle(data, slug)
		}
		links = append(links, docLink{
			Slug: slug, Title: title, Href: docHref(slug), Active: slug == active,
		})
	}
	sort.Slice(links, func(i, j int) bool {
		return strings.ToLower(links[i].Title) < strings.ToLower(links[j].Title)
	})
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
