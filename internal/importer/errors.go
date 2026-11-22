package importer

import "errors"

// ErrArchive marks a refusal an archive reader wrote for the person who built the archive: more
// members than it reads, a member larger than a definition, a member no export writes, or no job
// definition in it. The command line prints that sentence and the server answers with it as
// written, because it names the problem in the archive where a generic line about the format would
// call the export the wrong kind. Match it with errors.Is.
var ErrArchive = errors.New("the archive was refused")

// ErrExpansion marks an import refused because its YAML aliases expand past the import's budget.
// The sentence names the value that crossed it and is written for the person who built the export,
// so the command line and the server pass it on as written. Match it with errors.Is.
var ErrExpansion = errors.New("the export expands too far")
