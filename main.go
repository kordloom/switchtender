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

// main runs the Yardmaster CLI, handing it the embedded documentation to serve in the UI.
func main() {
	docs, err := fs.Sub(docsDir, "docs")
	if err != nil {
		docs = nil
	}
	cmd.Execute(docs)
}
