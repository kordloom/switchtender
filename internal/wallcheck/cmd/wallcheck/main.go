// Command wallcheck generates the paid fingerprint list and checks the public tree against it.
//
//	wallcheck gen -paid DIR -public DIR [-out FILE]
//	wallcheck check [-root DIR] [-list FILE]
//
// gen needs both trees and runs where the paid module is present. check needs only the public tree
// and the committed list, so the public CI runs it on every push. Exit status is 0 when clean, 1
// when paid code is found, and 2 on a usage or read error.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kordloom/switchtender/internal/wallcheck"
)

// Exit codes.
const (
	// exitOK means the tree is clean.
	exitOK = 0
	// exitFound means paid code was found.
	exitFound = 1
	// exitUsage means bad arguments or an unreadable tree or list.
	exitUsage = 2
)

// main runs the command and exits with its status.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches the subcommand. Findings go to stdout, summaries and errors to stderr.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: wallcheck gen|check [flags]")
		return exitUsage
	}
	switch args[0] {
	case "gen":
		return runGen(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "wallcheck: unknown command %q: want gen or check\n", args[0])
		return exitUsage
	}
}

// runGen writes the list generated from the paid and public trees to -out, or stdout.
func runGen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	paid := fs.String("paid", "", "Root of the paid module checkout.")
	public := fs.String("public", "", "Root of the public repository checkout.")
	out := fs.String("out", "", "File to write the list to. Defaults to stdout.")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *paid == "" || *public == "" {
		fmt.Fprintln(stderr, "wallcheck gen: -paid and -public are required")
		return exitUsage
	}
	list, rep, err := wallcheck.Gen(*paid, *public, wallcheck.DefaultConfig())
	if errors.Is(err, wallcheck.ErrPresent) {
		fmt.Fprintf(stderr, "wallcheck gen: %v\n", err)
		return exitFound
	}
	if err != nil {
		fmt.Fprintf(stderr, "wallcheck gen: %v\n", err)
		return exitUsage
	}
	if err := writeList(list, *out, stdout); err != nil {
		fmt.Fprintf(stderr, "wallcheck gen: write list: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(stderr, "wallcheck gen: %d hashes listed, %d already-public test or short units "+
		"dropped, %d boilerplate shingles pruned\n", list.Len(), rep.Dropped, rep.Pruned)
	return exitOK
}

// writeList writes the list to the file at path, or to stdout when path is empty.
func writeList(list *wallcheck.List, path string, stdout io.Writer) error {
	if path == "" {
		_, err := list.WriteTo(stdout)
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := list.WriteTo(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// runCheck checks the public tree at -root against the list and prints every finding.
func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "Root of the public repository checkout.")
	listPath := fs.String("list", "", "Fingerprint list. Defaults to the committed list under -root.")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *listPath == "" {
		*listPath = filepath.Join(*root, filepath.FromSlash(wallcheck.ListPath))
	}
	f, err := os.Open(*listPath)
	if err != nil {
		fmt.Fprintf(stderr, "wallcheck check: %v\n", err)
		return exitUsage
	}
	list, err := wallcheck.ReadList(f)
	f.Close()
	if err != nil {
		fmt.Fprintf(stderr, "wallcheck check: %v\n", err)
		return exitUsage
	}
	if list.Len() == 0 {
		fmt.Fprintf(stderr, "wallcheck check: %s holds no fingerprints\n", *listPath)
		return exitUsage
	}
	res, err := wallcheck.Check(*root, list, wallcheck.DefaultConfig())
	if err != nil {
		fmt.Fprintf(stderr, "wallcheck check: %v\n", err)
		return exitUsage
	}
	for _, f := range res.Findings {
		fmt.Fprintln(stdout, f)
	}
	if len(res.Unparsed) > 0 {
		fmt.Fprintf(stderr, "wallcheck check: %d Go files did not parse and were checked by lines only\n",
			len(res.Unparsed))
	}
	if len(res.Findings) > 0 {
		fmt.Fprintf(stderr, "wallcheck check: %d places match paid code\n", len(res.Findings))
		return exitFound
	}
	fmt.Fprintf(stderr, "wallcheck check: clean against %d fingerprints\n", list.Len())
	return exitOK
}
