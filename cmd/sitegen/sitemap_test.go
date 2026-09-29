package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// notLanding names files under site/ that are not landing pages, so their absence from landingURLs
// is correct rather than an omission. 404.html is served on error and must never be crawled.
var notLanding = map[string]bool{"404.html": true}

// TestEveryLandingPageIsListed is the guard for the defect that motivated it: terms.html and
// refund.html were written and linked from every footer, while landingURLs still held neither, so
// the next sitegen run would have silently dropped both from the sitemap. Nothing failed, no build
// broke, and the only symptom would have been two pages quietly missing from search.
//
// The list cannot be derived, because sitegen generates the docs pages and does not own the
// hand-written ones. So the list stays hand-maintained and this test makes forgetting it loud.
func TestEveryLandingPageIsListed(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join("..", "..", "site"))
	if err != nil {
		t.Fatalf("ReadDir(site) error = %v", err)
	}

	listed := make(map[string]bool, len(landingURLs))
	for _, u := range landingURLs {
		listed[strings.TrimPrefix(strings.TrimPrefix(u, "https://switchtender.com/"), "/")] = true
	}

	var missing []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") || notLanding[name] {
			continue
		}
		// The sitemap names a page by its extensionless path, and the home page by the empty slug.
		slug := strings.TrimSuffix(name, ".html")
		if slug == "index" {
			slug = ""
		}
		if !listed[slug] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if diff := cmp.Diff([]string(nil), missing, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("landing pages under site/ are missing from landingURLs, so the next sitegen run "+
			"drops them from sitemap.xml (-want +got):\n%s", diff)
	}
}

// TestListedPagesExist is the other direction. A URL left in landingURLs after its page is deleted
// publishes a sitemap entry that returns 404, which search engines treat as a quality signal
// against the whole site.
func TestListedPagesExist(t *testing.T) {
	t.Parallel()
	for i, u := range landingURLs {
		t.Run(fmt.Sprintf("test %d %s", i, u), func(t *testing.T) {
			t.Parallel()
			slug := strings.TrimPrefix(strings.TrimPrefix(u, "https://switchtender.com/"), "/")
			if slug == "" {
				slug = "index"
			}
			// A slug may be served either as a file or as a directory holding index.html.
			file := filepath.Join("..", "..", "site", slug+".html")
			dir := filepath.Join("..", "..", "site", slug, "index.html")
			if _, err := os.Stat(file); err == nil {
				return
			}
			if _, err := os.Stat(dir); err == nil {
				return
			}
			t.Errorf("sitemap lists %s but neither %s nor %s exists", u, file, dir)
		})
	}
}
