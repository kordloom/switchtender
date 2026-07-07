// Package main is the Yardmaster entry point.
package main

import (
	"embed"
	"io/fs"

	"github.com/dcadolph/yardmaster/cmd"
)

// docsDir holds the documentation the server renders inside the web UI, embedded so the single
// binary serves the same pages that live in the repository docs tree.
//
//go:embed docs
var docsDir embed.FS

// main runs the Yardmaster CLI.
func main() {
	if sub, err := fs.Sub(docsDir, "docs"); err == nil {
		cmd.DocsFS = sub
	}
	cmd.Execute()
}
