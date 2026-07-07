package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// docsFS is the embedded documentation tree the server renders inside the web UI, injected once at
// startup. It is nil in builds that do not provide it, which disables the in-app docs. It is
// unexported so no importer can mutate it.
var docsFS fs.FS

// Execute runs the Yardmaster root command with the embedded documentation and exits the process
// with the appropriate code.
func Execute(docs fs.FS) {
	docsFS = docs
	err := rootCmd.Execute()
	if err == nil {
		os.Exit(CodeOK)
	}
	fmt.Fprintln(os.Stderr, err)
	if errors.Is(err, ErrUsage) {
		os.Exit(CodeUsage)
	}
	os.Exit(CodeError)
}
