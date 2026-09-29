package main

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// linkAttr matches the attributes that name another resource: a link, a script, an image, a
// stylesheet. srcset is read separately because it holds a list.
var linkAttr = regexp.MustCompile(`\s(?:href|src)="([^"]*)"`)

// srcsetAttr matches an image candidate list.
var srcsetAttr = regexp.MustCompile(`\ssrcset="([^"]*)"`)

// idAttr matches the anchors a fragment can land on.
var idAttr = regexp.MustCompile(`\s(?:id|name)="([^"]*)"`)

// sitePage is one HTML page under site/, with the path it is served at.
type sitePage struct {
	// File is the page's path on disk.
	File string
	// URL is the path the static host serves it at, which relative links resolve against.
	URL string
}

// TestEverySiteLinkResolves fails for any same-site link or asset reference on a published page
// that resolves to nothing, and for a fragment that names no anchor on the page it points at.
//
// The docs are written in the repository and published to the site, and a link that works in one
// is not a link that works in the other. The supertest page linked the harness, its workflow, and
// its Dockerfile by paths relative to the repository, which GitHub resolves and the site serves as
// 404s, and nothing noticed, because nothing had ever walked the published pages. A page on a
// security product that sends a reader to a missing page reads as something taken down.
//
// Resolution follows the static host: a page is served without its .html suffix and a directory at
// its index.html, and a path in _redirects counts as found, since the host answers it. So does a
// file the site deploy builds, such as a page's WebAssembly and its loader, which is gitignored and
// so absent from every checkout, CI's included, while the host serves it.
func TestEverySiteLinkResolves(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "site")
	answered := redirectSources(t, filepath.Join(root, "_redirects"))
	workflow := filepath.Join("..", "..", ".github", "workflows", "deploy-site.yml")
	for _, built := range deployOutputs(t, workflow) {
		answered[built] = true
	}

	var pages []sitePage
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		url := "/" + filepath.ToSlash(strings.TrimSuffix(rel, ".html"))
		switch {
		case url == "/index":
			url = "/"
		case strings.HasSuffix(url, "/index"):
			url = strings.TrimSuffix(url, "index")
		}
		pages = append(pages, sitePage{File: p, URL: url})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(pages) < 20 {
		t.Fatalf("found %d pages under %s, so the walk did not read the site", len(pages), root)
	}

	var dead []string
	for _, page := range pages {
		body, err := os.ReadFile(page.File)
		if err != nil {
			t.Fatalf("read %s: %v", page.File, err)
		}
		var refs []string
		for _, m := range linkAttr.FindAllStringSubmatch(string(body), -1) {
			refs = append(refs, m[1])
		}
		for _, m := range srcsetAttr.FindAllStringSubmatch(string(body), -1) {
			for _, candidate := range strings.Split(m[1], ",") {
				if fields := strings.Fields(candidate); len(fields) > 0 {
					refs = append(refs, fields[0])
				}
			}
		}
		for _, ref := range refs {
			if why := resolveRef(root, page, ref, answered, string(body)); why != "" {
				dead = append(dead, page.URL+": "+ref+" ("+why+")")
			}
		}
	}
	sort.Strings(dead)
	for _, d := range dead {
		t.Errorf("dead link on %s", d)
	}
}

// resolveRef reports why ref, found on page, resolves to nothing, or returns the empty string when
// it resolves. Links off the site are not this test's to check.
func resolveRef(root string, page sitePage, ref string, answered map[string]bool, pageBody string) string {
	ref = strings.ReplaceAll(ref, "&amp;", "&")
	for _, external := range []string{
		"http:", "https:", "//", "mailto:", "tel:", "javascript:", "data:",
	} {
		if strings.HasPrefix(ref, external) {
			return ""
		}
	}
	target, fragment, _ := strings.Cut(ref, "#")
	target, _, _ = strings.Cut(target, "?")
	if target == "" {
		if hasAnchor(pageBody, fragment) {
			return ""
		}
		return "no anchor " + fragment + " on the page"
	}
	resolved := target
	if !strings.HasPrefix(target, "/") {
		base := page.URL
		if !strings.HasSuffix(base, "/") {
			base = path.Dir(base) + "/"
		}
		resolved = path.Join(base, target)
		if strings.HasSuffix(target, "/") && !strings.HasSuffix(resolved, "/") {
			resolved += "/"
		}
	}
	if answered[strings.TrimSuffix(resolved, "/")] {
		return ""
	}
	disk := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(resolved, "/")))
	for _, candidate := range []string{disk, disk + ".html", filepath.Join(disk, "index.html")} {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if fragment == "" || !strings.HasSuffix(candidate, ".html") {
			return ""
		}
		body, err := os.ReadFile(candidate)
		if err == nil && hasAnchor(string(body), fragment) {
			return ""
		}
		return "no anchor " + fragment + " on " + resolved
	}
	return "nothing is served at " + resolved
}

// hasAnchor reports whether body holds an element a fragment lands on. An empty fragment and #top
// land on the top of any page.
func hasAnchor(body, fragment string) bool {
	if fragment == "" || fragment == "top" {
		return true
	}
	for _, m := range idAttr.FindAllStringSubmatch(body, -1) {
		if m[1] == fragment {
			return true
		}
	}
	return false
}

// deployOutputs returns the paths the site deploy builds into site/ rather than the repository
// holding them. Each must be written by the deploy workflow, as a go build -o or a cp target, so an
// output the deploy stops building fails here instead of excusing a reference to nothing.
func deployOutputs(t *testing.T, workflow string) []string {
	t.Helper()
	body, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("read %s: %v", workflow, err)
	}
	var out []string
	for _, built := range []string{
		"/assess/assess.wasm", "/assess/wasm_exec.js", "/verify/loomseal.wasm", "/verify/wasm_exec.js",
	} {
		written := regexp.MustCompile(`(?m)(?:-o\s+|^\s*cp\s.*\s)` + regexp.QuoteMeta("site"+built) +
			`(?:\s|$)`)
		if !written.Match(body) {
			t.Errorf("%s does not build site%s, so a page loading it links to nothing", workflow, built)
			continue
		}
		out = append(out, built)
	}
	return out
}

// redirectSources reads the paths the host redirects, each of which is answered rather than
// missing.
func redirectSources(t *testing.T, file string) map[string]bool {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}
	defer f.Close()
	out := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Fields(line); len(fields) >= 2 {
			out[strings.TrimSuffix(fields[0], "/")] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	return out
}
