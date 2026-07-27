// Command sitegen renders the repository's docs/*.md files into a themed, static documentation
// section under site/docs/, for the switchtender.com marketing site. It reuses goldmark, the same
// markdown renderer the app serves docs with. Run from the repo root: go run ./cmd/sitegen
package main

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
)

// docsDir is where the source markdown lives; outDir is where the rendered pages are written.
const (
	docsDir = "docs"
	outDir  = "site/docs"
)

// order is the sidebar order; pages not listed follow, sorted by slug.
var order = []string{
	"README", "quickstart", "faq", "switching-from-awx", "migration",
	"tutorials", "tutorial-run-a-job", "tutorial-save-a-template", "tutorial-schedule-a-job",
	"tutorial-set-a-secret", "tutorial-migrate",
	"tool-ansible", "tool-bash", "tool-terraform", "tool-opentofu", "tool-python", "tool-powershell", "tool-go",
	"concepts", "reliability", "configuration", "desktop", "features", "secrets", "drift", "api", "comparison",
}

// titles overrides the sidebar label for a slug where its first heading reads poorly.
var titles = map[string]string{
	"README":             "Overview",
	"switching-from-awx": "Switching from AWX",
}

// logoBlock matches the centered logo and badge blocks the markdown files open with, which reference
// repo-relative images that do not exist on the site.
var logoBlock = regexp.MustCompile(`(?s)<p align="center">.*?</p>`)

// mdLink matches links between documentation pages, which are markdown file names on disk.
var mdLink = regexp.MustCompile(`href="([a-zA-Z0-9._/-]+?)\.md(#[^"]*)?"`)

// mdInline strips inline markdown links and emphasis for plain-text descriptions.
var mdInline = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)|\*\*([^*]*)\*\*`)

// page is one rendered documentation page.
type page struct {
	// Slug is the file name without its extension.
	Slug string
	// Title is the sidebar label.
	Title string
	// Href is the page URL.
	Href string
	// Description is the first prose paragraph, clipped for the meta description.
	Description string
	// Content is the rendered HTML body.
	Content template.HTML
}

// sidebar is one navigation entry passed to the layout.
type sidebar struct {
	// Title is the label.
	Title string
	// Href is the link.
	Href string
	// Active marks the page currently shown.
	Active bool
}

// main renders the documentation site and exits non-zero on failure.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sitegen:", err)
		os.Exit(1)
	}
}

// run renders every markdown page and writes the docs section.
func run() error {
	entries, err := filepath.Glob(filepath.Join(docsDir, "*.md"))
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, e := range entries {
		present[strings.TrimSuffix(filepath.Base(e), ".md")] = true
	}
	slugs := orderedSlugs(present)

	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(ghtml.WithUnsafe()),
	)
	tmpl := template.Must(template.New("layout").Parse(layout))

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, slug := range slugs {
		p, err := render(md, slug)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, map[string]any{
			"Title": p.Title, "Content": p.Content, "Pages": sidebarFor(slug, slugs),
			"Description": p.Description, "Canonical": canonicalFor(slug),
		}); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, outFile(slug)), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	if err := writeSitemap(slugs); err != nil {
		return err
	}
	fmt.Printf("sitegen: wrote %d pages and the sitemap to %s\n", len(slugs), outDir)
	return nil
}

// canonicalFor returns the canonical URL for a docs slug, the directory URL for the index. Pages
// serves extensionless clean URLs and redirects .html paths to them, so canonicals stay clean.
func canonicalFor(slug string) string {
	return "https://switchtender.com" + hrefFor(slug)
}

// hrefFor returns the site-relative clean URL for a docs slug, the directory URL for the index.
func hrefFor(slug string) string {
	if slug == "README" {
		return "/docs/"
	}
	return "/docs/" + slug
}

// writeSitemap emits site/sitemap.xml covering the landing pages and every docs page, so crawlers
// discover the whole site from one file.
func writeSitemap(slugs []string) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	urls := []string{
		"https://switchtender.com/", "https://switchtender.com/get-started",
		"https://switchtender.com/awx-alternative", "https://switchtender.com/ascender-alternative",
		"https://switchtender.com/semaphore-alternative", "https://switchtender.com/aap-alternative",
		"https://switchtender.com/privacy",
	}
	for _, slug := range slugs {
		urls = append(urls, canonicalFor(slug))
	}
	for _, u := range urls {
		b.WriteString("  <url><loc>" + u + "</loc></url>\n")
	}
	b.WriteString("</urlset>\n")
	return os.WriteFile(filepath.Join("site", "sitemap.xml"), []byte(b.String()), 0o644)
}

// description returns the page's first prose paragraph as plain text, clipped for a meta
// description. Headings, badges, tables, and code fences are skipped, and a paragraph wrapped
// across source lines is joined before clipping.
func description(src []byte) string {
	inFence := false
	var para []string
	flush := func() string {
		text := mdInline.ReplaceAllString(strings.Join(para, " "), "$1$2")
		text = strings.ReplaceAll(text, "`", "")
		if len(text) <= 200 {
			return text
		}
		// Prefer ending on a full sentence. Fall back to a word boundary.
		if dot := strings.LastIndex(text[:200], ". "); dot >= 60 {
			return text[:dot+1]
		}
		cut := 200
		for cut > 0 && text[cut] != ' ' {
			cut--
		}
		return strings.TrimRight(text[:cut], " ,")
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		skip := inFence || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "|") ||
			strings.HasPrefix(line, "<") || strings.HasPrefix(line, "-")
		if line == "" || skip {
			if len(para) > 0 {
				return flush()
			}
			continue
		}
		para = append(para, line)
	}
	if len(para) > 0 {
		return flush()
	}
	return "SwitchTender documentation."
}

// orderedSlugs returns the present slugs in sidebar order, extras appended sorted.
func orderedSlugs(present map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range order {
		if present[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	var extra []string
	for s := range present {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// render reads a markdown page, strips its logo block, rewrites cross-page links, and renders it.
func render(md goldmark.Markdown, slug string) (page, error) {
	src, err := os.ReadFile(filepath.Join(docsDir, slug+".md"))
	if err != nil {
		return page{}, err
	}
	src = logoBlock.ReplaceAll(src, nil)
	var body bytes.Buffer
	if err := md.Convert(src, &body); err != nil {
		return page{}, err
	}
	html := mdLink.ReplaceAllStringFunc(body.String(), func(m string) string {
		parts := mdLink.FindStringSubmatch(m)
		return fmt.Sprintf("href=%q", hrefFor(filepath.Base(parts[1]))+parts[2])
	})
	return page{
		Slug: slug, Title: title(slug, src), Description: description(src),
		Content: template.HTML(html), //nolint:gosec // trusted docs
	}, nil
}

// sidebarFor builds the navigation for the page being rendered.
func sidebarFor(active string, slugs []string) []sidebar {
	out := make([]sidebar, 0, len(slugs))
	for _, s := range slugs {
		src, _ := os.ReadFile(filepath.Join(docsDir, s+".md"))
		out = append(out, sidebar{Title: title(s, src), Href: hrefFor(s), Active: s == active})
	}
	return out
}

// outFile maps a slug to its output file name; README becomes the docs index.
func outFile(slug string) string {
	if slug == "README" {
		return "index.html"
	}
	return slug + ".html"
}

// title returns the sidebar label for a slug: an override, else the first heading, else the slug.
func title(slug string, src []byte) string {
	if t, ok := titles[slug]; ok {
		return t
	}
	for _, line := range strings.Split(string(src), "\n") {
		if h, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(h)
		}
	}
	return slug
}

// layout is the themed documentation page shell: a top bar, a sidebar of pages, and the content.
const layout = `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>{{.Title}} · SwitchTender docs</title>
	<meta name="description" content="{{.Description}}">
	<link rel="canonical" href="{{.Canonical}}">
	<meta property="og:title" content="{{.Title}} · SwitchTender docs">
	<meta property="og:description" content="{{.Description}}">
	<meta property="og:type" content="article">
	<meta property="og:url" content="{{.Canonical}}">
	<meta property="og:image" content="https://switchtender.com/assets/screenshot-fleet.png?v=7">
	<meta name="twitter:card" content="summary_large_image">
	<meta name="twitter:title" content="{{.Title}} · SwitchTender docs">
	<meta name="twitter:description" content="{{.Description}}">
	<meta name="twitter:image" content="https://switchtender.com/assets/screenshot-fleet.png?v=7">
	<link rel="icon" href="/favicon.ico" sizes="any">
	<link rel="icon" type="image/png" href="/assets/favicon.png?v=2">
	<link rel="apple-touch-icon" href="/assets/apple-touch-icon.png">
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=Space+Grotesk:wght@500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
	<link rel="stylesheet" href="/docs.css?v=1">
</head>
<body>
	<header class="dnav">
		<a class="brand" href="/"><img src="/assets/logo-train-tracks-dark.png" alt="SwitchTender" class="brand-train"><span class="brand-word">SwitchTender</span></a>
		<span class="dnav-tag">Docs</span>
		<nav class="dnav-links">
			<a href="/">Home</a>
			<a href="/#compare">Compare</a>
			<a href="https://github.com/kordloom/switchtender">GitHub</a>
		</nav>
	</header>
	<div class="dshell">
		<aside class="dside">
			<nav>
				{{range .Pages}}<a href="{{.Href}}"{{if .Active}} class="active"{{end}}>{{.Title}}</a>{{end}}
			</nav>
		</aside>
		<main class="dcontent prose">
			{{.Content}}
		</main>
	</div>
	<script>document.addEventListener("click",e=>{const a=e.target.closest(".dside a");if(a)document.querySelector(".dside")?.classList.remove("open")});</script>
</body>
</html>
`
