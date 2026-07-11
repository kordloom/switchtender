// Command sitegen renders the repository's docs/*.md files into a themed, static documentation
// section under site/docs/, for the yardmaster.dev marketing site. It reuses goldmark, the same
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
	"tool-bash", "tool-terraform", "tool-python",
	"concepts", "configuration", "features", "api", "comparison",
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

// page is one rendered documentation page.
type page struct {
	// Slug is the file name without its extension.
	Slug string
	// Title is the sidebar label.
	Title string
	// Href is the page URL.
	Href string
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
		}); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, outFile(slug)), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("sitegen: wrote %d pages to %s\n", len(slugs), outDir)
	return nil
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
		return fmt.Sprintf("href=%q", "/docs/"+outFile(filepath.Base(parts[1]))+parts[2])
	})
	return page{Slug: slug, Title: title(slug, src), Content: template.HTML(html)}, nil //nolint:gosec // trusted docs
}

// sidebarFor builds the navigation for the page being rendered.
func sidebarFor(active string, slugs []string) []sidebar {
	out := make([]sidebar, 0, len(slugs))
	for _, s := range slugs {
		src, _ := os.ReadFile(filepath.Join(docsDir, s+".md"))
		out = append(out, sidebar{Title: title(s, src), Href: "/docs/" + outFile(s), Active: s == active})
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
	<title>{{.Title}} · Yardmaster docs</title>
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
		<a class="brand" href="/"><img src="/assets/logo-train-tracks-dark.png" alt="Yardmaster" class="brand-train"><span class="brand-word">Yardmaster</span></a>
		<span class="dnav-tag">Docs</span>
		<nav class="dnav-links">
			<a href="/">Home</a>
			<a href="/#compare">Compare</a>
			<a href="https://github.com/dcadolph/yardmaster">GitHub</a>
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
