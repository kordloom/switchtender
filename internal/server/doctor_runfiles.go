package server

import (
	"github.com/kordloom/switchtender/internal/runfiles"
)

// runFilesState is where a server stages run files and what it proved about that place at startup.
type runFilesState struct {
	// source says which rule picked the root.
	source runfiles.Source
	// report is what runfiles.Prepare proved about the root.
	report runfiles.Report
}

// WithRunFiles tells the doctor where this server stages the secret material its runs write to disk
// and what is true of that place, so it can warn when the material sits on persistent disk.
func WithRunFiles(source runfiles.Source, report runfiles.Report) Option {
	return func(srv *Server) { srv.runFiles = &runFilesState{source: source, report: report} }
}

// runFilesFindings returns the doctor's findings about where run files are staged: a warning when
// the root is the temporary directory fallback, which a systemd runtime directory or a private XDG
// runtime directory would have beaten, and a warning whenever the root is not memory-backed, since
// then every key and token a run writes reaches a disk. Nil state, a server nobody told, finds
// nothing.
func runFilesFindings(rf *runFilesState) []doctorFinding {
	if rf == nil {
		return nil
	}
	finding := func(problem string) []doctorFinding {
		return []doctorFinding{{
			Severity: "warning", ObjectType: "install", ObjectID: "runfiles", ObjectName: "run files",
			Problem: problem, FixPath: "/ui/docs/run-files",
		}}
	}
	root, fs := rf.report.Root, rf.report.Filesystem
	switch {
	case rf.source == runfiles.SourceTemp && !rf.report.MemoryBacked:
		return finding("Runs stage keys, tokens, and secret answers in " + root + " in the " +
			"temporary directory, which is on " + fs + ", so they reach persistent disk while a run " +
			"holds them, and a crash leaves them there until a sweep removes them. Run the server " +
			"under the shipped systemd unit, whose runtime directory is memory-backed and removed " +
			"when the service stops, or set --runfiles-dir to a private tmpfs.")
	case rf.source == runfiles.SourceTemp:
		return finding("Runs stage keys, tokens, and secret answers in " + root + " in the " +
			"temporary directory. It is memory-backed here, but nothing removes it when the service " +
			"stops. Run the server under the shipped systemd unit, whose runtime directory systemd " +
			"removes when the service stops, or set --runfiles-dir to a private tmpfs.")
	case !rf.report.MemoryBacked:
		return finding("Runs stage keys, tokens, and secret answers in " + root + ", which is on " +
			fs + ", so they reach persistent disk while a run holds them. Point --runfiles-dir at a " +
			"private tmpfs, or run under the shipped systemd unit.")
	}
	return nil
}
