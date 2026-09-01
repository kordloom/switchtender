// Command sitegen renders the repository's docs/*.md files into a themed, static documentation
// section under site/docs/, for the switchtender.com marketing site. It reuses goldmark, the same
// markdown renderer the app serves docs with. Run from the repo root: go run ./cmd/sitegen
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
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
	"concepts", "reliability", "configuration", "desktop", "features", "secrets", "drift",
	"compliance", "threat-model", "sample-evidence-pack", "api", "comparison",
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
	// HeadExtra is page-specific head markup, such as the FAQ page's structured data.
	HeadExtra template.HTML
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
			"HeadExtra": p.HeadExtra,
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
	entities, err := writeEntityPages()
	if err != nil {
		return err
	}
	fmt.Printf("sitegen: wrote %d pages and the sitemap to %s, and refreshed the entity on %d "+
		"hand-written pages\n", len(slugs), outDir, entities)
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
		"https://switchtender.com/", "https://switchtender.com/pricing",
		"https://switchtender.com/get-started", "https://switchtender.com/agents",
		"https://switchtender.com/awx-alternative", "https://switchtender.com/ascender-alternative",
		"https://switchtender.com/semaphore-alternative", "https://switchtender.com/aap-alternative",
		"https://switchtender.com/rundeck-alternative", "https://switchtender.com/jenkins-alternative",
		"https://switchtender.com/migration",
		"https://switchtender.com/privacy",
		"https://switchtender.com/verify",
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
	p := page{
		Slug: slug, Title: title(slug, src), Description: description(src),
		Content: template.HTML(html), //nolint:gosec // trusted docs
	}
	p.HeadExtra = docsSchema(slug, p)
	if slug == "faq" {
		// The FAQ carries both: the questions a summarizer can quote, and the entity they are about.
		p.HeadExtra += "\n\t" + faqSchema(src)
	}
	return p, nil
}

// faqQuestion is one question and answer pair in the FAQ structured data.
type faqQuestion struct {
	// Type is the schema.org type, always Question.
	Type string `json:"@type"`
	// Name is the question text.
	Name string `json:"name"`
	// AcceptedAnswer is the answer object.
	AcceptedAnswer faqAnswer `json:"acceptedAnswer"`
}

// faqAnswer is the answer half of a FAQ pair.
type faqAnswer struct {
	// Type is the schema.org type, always Answer.
	Type string `json:"@type"`
	// Text is the plain-text answer.
	Text string `json:"text"`
}

// inlineMarkdown strips the inline markdown that survives in plain answer text: links keep their
// label, and code spans and emphasis keep their content.
var inlineMarkdown = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

// faqSchema builds FAQPage structured data from the FAQ markdown, one entry per level-two heading
// with the prose beneath it as the answer. Generating it from the same source as the page keeps the
// structured data true to the content on every regeneration.
func faqSchema(src []byte) template.HTML {
	var questions []faqQuestion
	sections := strings.Split("\n"+string(src), "\n## ")
	for _, sec := range sections[1:] {
		lines := strings.SplitN(sec, "\n", 2)
		if len(lines) < 2 {
			continue
		}
		name := strings.TrimSpace(lines[0])
		answer := lines[1]
		if at := strings.Index(answer, "\n# "); at >= 0 {
			answer = answer[:at]
		}
		answer = inlineMarkdown.ReplaceAllString(answer, "$1")
		answer = strings.NewReplacer("`", "", "**", "", "\n", " ").Replace(answer)
		answer = strings.Join(strings.Fields(answer), " ")
		if name == "" || answer == "" {
			continue
		}
		questions = append(questions, faqQuestion{
			Type: "Question", Name: name,
			AcceptedAnswer: faqAnswer{Type: "Answer", Text: answer},
		})
	}
	if len(questions) == 0 {
		return ""
	}
	data, err := json.MarshalIndent(map[string]any{
		"@context": "https://schema.org", "@type": "FAQPage", "mainEntity": questions,
	}, "", " ")
	if err != nil {
		// A struct of strings cannot fail to marshal; a failure here is a developer error.
		panic("sitegen: marshal faq schema: " + err.Error())
	}
	//nolint:gosec // built from trusted docs and JSON-escaped
	return template.HTML("<script type=\"application/ld+json\">\n" + string(data) + "\n</script>")
}

// switchtenderFeatures is the canonical capability list the structured data advertises.
//
// It exists because answer engines were crediting competitors with capabilities this product has.
// A comparison summary named an external secret manager and fine-grained access control as reasons
// to pick AWX, and a Terraform-then-approve-then-Ansible pipeline as a reason to pick Semaphore UI,
// all three of which are listed below. Prose in a table on one page is not something a summarizer
// reliably attributes; a machine-readable claim on the page it lands on is.
//
// Every line here must be true and checkable in the docs. This is the one place they are written,
// so the landing pages and the docs cannot drift into saying different things.
var switchtenderFeatures = []string{
	"Agentless: reaches managed hosts over SSH with nothing installed on them",
	"Runs Ansible, Terraform, OpenTofu, Bash, PowerShell, Python, and Go",
	"Reads secrets from HashiCorp Vault KV v1 and v2, Vault dynamic secrets minted per run and " +
		"revoked when it ends, AWS Secrets Manager, and Azure Key Vault",
	"Per-object access grants: read, use, or manage on one project, template, inventory, or " +
		"credential, layered over global roles",
	"Single sign-on through OIDC and LDAP",
	"Pipelines with a dependency graph and parallel branches, built in a drag-and-drop editor",
	"Approval gates enforced by policy before a run executes",
	"Tamper-evident hash-chained audit trail a third party can verify offline",
	"Live host-by-task matrix and per-host history across runs",
	"Drift detection from a dry run",
	"One static Go binary and one SQLite file, with PostgreSQL optional",
	"One-command import from AWX, Semaphore UI, Rundeck, Jenkins, and crontab",
}

// entitySlugs are the docs pages that carry the full application entity rather than only an article
// one. These are the pages a capability question lands on, so they are the ones worth answering.
var entitySlugs = map[string]bool{"features": true, "secrets": true, "comparison": true}

// switchtenderEntity is the SoftwareApplication description shared by every page that advertises
// what the product does.
func switchtenderEntity() map[string]any {
	return map[string]any{
		"@context": "https://schema.org", "@type": "SoftwareApplication",
		"name": "SwitchTender", "url": "https://switchtender.com/",
		"applicationCategory": "DeveloperApplication",
		"operatingSystem":     "Linux, macOS, Windows",
		"description": "An agentless, single-binary automation controller for Ansible, Terraform, " +
			"OpenTofu, Bash, PowerShell, Python, and Go, with enforced approval policies and a " +
			"tamper-evident audit trail that can be verified offline.",
		"license":     "https://mariadb.com/bsl11/",
		"featureList": switchtenderFeatures,
		"offers": map[string]any{
			"@type": "Offer", "price": "0", "priceCurrency": "USD",
		},
		"publisher": map[string]any{
			"@type": "Organization", "name": "KordLoom LLC", "url": "https://kordloom.com",
			"sameAs": []string{
				"https://github.com/kordloom",
				"https://www.linkedin.com/company/kordloom",
				"https://x.com/kordloom",
			},
		},
		"sameAs": []string{"https://github.com/kordloom/switchtender"},
	}
}

// landingQA is one question and its short answer on a comparison landing page.
type landingQA struct {
	// Question is the question as somebody would type it.
	Question string
	// Answer is a card-length answer, plain text.
	Answer string
}

// landingFAQ answers the three capabilities that comparison summaries were handing to competitors.
//
// Each of these is a thing this product does, described on the page already, in prose inside a
// table. A summarizer did not attribute them here and credited the incumbent instead. Stating them
// as the literal question somebody asks, on the page a comparison query lands on, is what makes the
// answer quotable rather than inferable.
var landingFAQ = []landingQA{{
	Question: "Does it need an agent on each host?",
	Answer: "No. It reaches the machines it manages over SSH, the same way Ansible does, and " +
		"installs nothing on them. There is no per-host daemon to deploy, patch, or account for. " +
		"You run the one server binary, and optionally a few more worker processes against the " +
		"same store for throughput, which are pool members rather than agents belonging to a host.",
}, {
	Question: "Can it read secrets from AWS Secrets Manager, Azure Key Vault, or Vault?",
	Answer: "All three, resolved at launch rather than copied into this database. Vault dynamic " +
		"secrets go further: a short-lived credential is minted for each run and revoked when the " +
		"run ends. AWS and Azure both authenticate from an instance role or managed identity with " +
		"no stored key, and anything else resolves through a command whose output is the secret.",
}, {
	Question: "Can I run a Terraform plan, hold it for approval, then run Ansible?",
	Answer: "Yes, and it is what pipelines are for here. Steps mix tools freely on a dependency " +
		"graph with parallel branches, built on a drag-and-drop canvas. The approval is not a " +
		"convention somebody can skip: a policy decides which runs are held, the core enforces " +
		"the hold, and the approval binds to the exact plan reviewed, so a run cannot be approved " +
		"as one thing and executed as another.",
}}

// faqMarkers bracket the shared landing-page questions, the same way the entity markers work.
const (
	faqOpen  = "<!-- st:faq -->"
	faqClose = "<!-- /st:faq -->"
)

// landingFAQBlock renders the shared questions as a section matching the page around it, followed by
// the structured data describing the same words. The markup and the schema are generated together
// from one source, which is what keeps the structured data honest: it can only ever describe
// questions and answers a reader can actually see on the page.
func landingFAQBlock() string {
	var b strings.Builder
	b.WriteString("\n\t\t<section class=\"section wrap\">\n")
	b.WriteString("\t\t\t<div class=\"section-head reveal\">\n")
	b.WriteString("\t\t\t\t<h2>Common questions</h2>\n")
	b.WriteString("\t\t\t\t<p>The three asked most often when somebody is comparing this against " +
		"what they already run.</p>\n")
	b.WriteString("\t\t\t</div>\n")
	b.WriteString("\t\t\t<div class=\"cards cards-3\">\n")
	questions := make([]faqQuestion, 0, len(landingFAQ))
	for _, qa := range landingFAQ {
		fmt.Fprintf(&b, "\t\t\t\t<article class=\"card reveal\">\n\t\t\t\t\t<h3>%s</h3>\n"+
			"\t\t\t\t\t<p>%s</p>\n\t\t\t\t</article>\n",
			html.EscapeString(qa.Question), html.EscapeString(qa.Answer))
		questions = append(questions, faqQuestion{
			Type: "Question", Name: qa.Question,
			AcceptedAnswer: faqAnswer{Type: "Answer", Text: qa.Answer},
		})
	}
	b.WriteString("\t\t\t</div>\n")
	b.WriteString("\t\t\t" + string(marshalSchema(map[string]any{
		"@context": "https://schema.org", "@type": "FAQPage", "mainEntity": questions,
	})) + "\n")
	b.WriteString("\t\t</section>\n\t\t")
	return b.String()
}

// entityMarkers bracket the shared application entity inside a hand-written page, so the generator
// can refresh it without touching anything the page's author wrote around it.
const (
	entityOpen  = "<!-- st:entity -->"
	entityClose = "<!-- /st:entity -->"
)

// writeEntityPages refreshes the shared application entity inside every hand-written page that opts
// in by carrying the markers.
//
// The landing pages are written by hand while the docs are generated, so the same capability claims
// used to live in two places and were free to disagree. They are now written once, here, and copied
// into both. A page opts in by holding the marker pair; nothing else about it is touched.
func writeEntityPages() (int, error) {
	paths, err := filepath.Glob(filepath.Join("site", "*.html"))
	if err != nil {
		return 0, err
	}
	entity := "\n\t" + string(marshalSchema(switchtenderEntity())) + "\n\t"
	changed := 0
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		text := string(src)
		updated, err := fillMarkers(path, text, entityOpen, entityClose, entity)
		if err != nil {
			return 0, err
		}
		updated, err = fillMarkers(path, updated, faqOpen, faqClose, landingFAQBlock())
		if err != nil {
			return 0, err
		}
		if updated == text {
			continue
		}
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			return 0, err
		}
		changed++
	}
	return changed, nil
}

// fillMarkers replaces whatever sits between one marker pair, leaving a page without them alone.
func fillMarkers(path, text, openTag, closeTag, body string) (string, error) {
	start := strings.Index(text, openTag)
	if start < 0 {
		return text, nil
	}
	end := strings.Index(text[start:], closeTag)
	if end < 0 {
		return "", fmt.Errorf("sitegen: %s opens %s and never closes it", path, openTag)
	}
	end += start + len(closeTag)
	return text[:start] + openTag + body + closeTag + text[end:], nil
}

// docsSchema builds the structured data for one docs page: the application entity on the pages that
// describe what it does, and a technical article entity everywhere else.
func docsSchema(slug string, p page) template.HTML {
	var doc map[string]any
	if entitySlugs[slug] {
		doc = switchtenderEntity()
		doc["url"] = canonicalFor(slug)
	} else {
		doc = map[string]any{
			"@context": "https://schema.org", "@type": "TechArticle",
			"headline": p.Title, "description": p.Description, "url": canonicalFor(slug),
			"isPartOf": map[string]any{
				"@type": "WebSite", "name": "SwitchTender docs",
				"url": "https://switchtender.com/docs/",
			},
			"about": map[string]any{
				"@type": "SoftwareApplication", "name": "SwitchTender",
				"url": "https://switchtender.com/",
			},
			"publisher": map[string]any{
				"@type": "Organization", "name": "KordLoom LLC", "url": "https://kordloom.com",
			},
		}
	}
	return marshalSchema(doc)
}

// marshalSchema renders one structured data document as a script tag.
func marshalSchema(doc map[string]any) template.HTML {
	data, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		// The documents are built from strings and maps of strings, so a failure is a code error.
		panic("sitegen: marshal schema: " + err.Error())
	}
	//nolint:gosec // built from trusted constants and JSON-escaped
	return template.HTML("<script type=\"application/ld+json\">\n" + string(data) + "\n</script>")
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
	<title>{{.Title}} | SwitchTender docs</title>
	<meta name="description" content="{{.Description}}">
	<link rel="canonical" href="{{.Canonical}}">
	<meta property="og:title" content="{{.Title}} | SwitchTender docs">
	<meta property="og:description" content="{{.Description}}">
	<meta property="og:type" content="article">
	<meta property="og:url" content="{{.Canonical}}">
	<meta property="og:image" content="https://switchtender.com/assets/switchtender-social-preview.png?v=9">
	<meta name="twitter:card" content="summary_large_image">
	<meta name="twitter:title" content="{{.Title}} | SwitchTender docs">
	<meta name="twitter:description" content="{{.Description}}">
	<meta name="twitter:image" content="https://switchtender.com/assets/switchtender-social-preview.png?v=9">
{{- if .HeadExtra}}
	{{.HeadExtra}}
{{- end}}
	<link rel="icon" href="/favicon.ico" sizes="any">
	<link rel="icon" type="image/png" href="/assets/favicon.png?v=2">
	<link rel="apple-touch-icon" href="/assets/apple-touch-icon.png">
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=Space+Grotesk:wght@500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
	<link rel="stylesheet" href="/docs.css?v=2">
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
	<div class="foot-legal" style="text-align:center;margin:26px auto 34px;font-size:12px;opacity:.75">SwitchTender&trade; is a trademark of KordLoom LLC. Copyright 2026 KordLoom LLC.</div>
	<script>document.addEventListener("click",e=>{const a=e.target.closest(".dside a");if(a)document.querySelector(".dside")?.classList.remove("open")});</script>
</body>
</html>
`
