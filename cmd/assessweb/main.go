//go:build js && wasm

// Command assessweb runs the migration assessment inside the visitor's browser.
//
// It is the same reader and the same renderer the assess command uses, compiled for the browser so
// that an export never leaves the machine it is on. That is not only a privacy line on a page: an
// AWX export names every project, inventory, credential and schedule an organization runs, and
// asking a stranger to upload one is asking for the map of their estate. Nothing here posts
// anything, and the page it is served from has no endpoint to post to.
//
// The global scope is given two functions. switchtenderFormats lists what can be read, and
// switchtenderAssess reads one export and returns the report as text alongside the sentence and the
// few numbers the page puts in front of it. The page runs this in a worker, so the global scope is
// the worker's.
package main

import (
	"bytes"
	"fmt"
	"syscall/js"
	"time"

	"github.com/kordloom/switchtender/internal/importer"
)

// main publishes the two functions and then blocks, because a Go wasm module whose main returns is
// torn down and every exported function with it.
func main() {
	js.Global().Set("switchtenderFormats", js.FuncOf(formats))
	js.Global().Set("switchtenderAssess", js.FuncOf(assess))
	select {}
}

// formats returns the readable format names, so the page offers exactly what the command does
// rather than a list somebody typed into the HTML.
func formats(js.Value, []js.Value) any {
	names := importer.Formats()
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, n)
	}
	return js.ValueOf(out)
}

// assess reads one export and returns the rendered report with the headline numbers beside it.
//
// It takes the format name and the export as the file's bytes, or as text, and always returns an
// object carrying ok. The page hands over bytes so the reader decodes them exactly as the command
// does, byte order mark and UTF-16 included, rather than trusting the browser's decoding to agree.
// A malformed export is an ordinary outcome on a page anyone can drop a file onto, so a failure is
// reported rather than thrown, and a panic in a reader is recovered into the same shape.
func assess(_ js.Value, args []js.Value) (result any) {
	defer func() {
		if r := recover(); r != nil {
			result = fail(fmt.Sprintf("this export could not be read: %v", r))
		}
	}()
	if len(args) < 2 || args[0].Type() != js.TypeString {
		return fail("assess takes a format name and the export")
	}
	format := args[0].String()
	var data []byte
	switch export := args[1]; {
	case export.Type() == js.TypeString:
		data = []byte(export.String())
	case export.InstanceOf(js.Global().Get("Uint8Array")):
		data = make([]byte, export.Get("length").Int())
		js.CopyBytesToGo(data, export)
	default:
		return fail("assess takes the export as bytes or as text")
	}
	read, ok := importer.Readers[format]
	if !ok {
		return fail(fmt.Sprintf("unknown format %q", format))
	}
	plan, err := read(data, time.Now())
	if err != nil {
		return fail(err.Error())
	}
	a := plan.Assess()
	var report bytes.Buffer
	importer.Render(&report, format, "the file you chose", a)
	headline := importer.HeadlineOf(a)
	return js.ValueOf(map[string]any{
		"ok":     true,
		"report": report.String(),
		// The sentence and the numbers the page leads with, handed over already decided so the
		// page never concludes or counts anything itself. A headline written in JavaScript is a
		// second implementation of the report's conclusion, and it was the one that disagreed.
		"headline":     headline.Text,
		"headlineKind": headline.Kind,
		"templates":    a.Governance.Templates,
		"irreversible": len(a.Governance.Irreversible),
		"highRisk":     len(a.Governance.HighRisk),
		"objects":      a.Report.CreatedTotal,
		"leftOut":      len(a.Report.LeftOut),
	})
}

// fail returns the shape the page reads for an export it could not use.
func fail(reason string) any {
	return js.ValueOf(map[string]any{"ok": false, "error": reason})
}
