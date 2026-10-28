// Package main is the SwitchTender Community entry point. The paid build has its own entry point in
// the private module, which registers the paid capabilities and runs this same command tree.
package main

import (
	"github.com/kordloom/switchtender/cmd"
	"github.com/kordloom/switchtender/docs"
)

// main runs the SwitchTender CLI, handing it the embedded documentation to serve in the UI.
func main() {
	cmd.Execute(docs.FS)
}
