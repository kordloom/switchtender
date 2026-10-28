// Package docs embeds the documentation the server renders inside the web UI, so every binary
// built from this tree, Community or paid, serves the same pages that live in the repository.
package docs

import "embed"

// FS holds every documentation page, rooted at this directory.
//
//go:embed *.md
var FS embed.FS
