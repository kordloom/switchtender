package main

import (
	"fmt"
	"strings"
	"time"
)

// engineOutcome is what a deployed install is expected to answer for one advertised engine.
type engineOutcome int

const (
	// engineExecutes means the image carries the tool and a command alone is enough to run it, so
	// the run has to reach succeeded.
	engineExecutes engineOutcome = iota
	// engineNeedsProject means the image carries the tool and it runs inside a checkout, so a run
	// with no project has to be refused for the directory rather than for the tool.
	engineNeedsProject
	// engineOperatorProvided means the image does not carry the tool on purpose, so the run has to
	// be refused by name, with the binary named, and never silently.
	engineOperatorProvided
)

// advertisedEngine is one of the seven tools the product is sold as running, with the answer a
// deployed install owes for it.
type advertisedEngine struct {
	// Tool is the value a run carries.
	Tool string
	// Binary is the executable the runner reaches for, which a refusal has to name.
	Binary string
	// Command is the smallest thing worth asking the tool to do.
	Command string
	// Want is the answer this install is expected to give.
	Want engineOutcome
}

// advertisedEngines is the whole list the pricing page names, each with what the shipped image is
// expected to answer.
//
// It is written down rather than discovered, which is the point. A table that asked the install
// what it could do and then agreed would pass whatever the image happened to contain, including an
// image that had quietly lost a tool. Adding a tool to the Dockerfile without moving it here fails,
// and so does removing one.
var advertisedEngines = []advertisedEngine{
	{Tool: "bash", Binary: "bash", Command: "echo supertest", Want: engineExecutes},
	{Tool: "python", Binary: "python3", Command: "print('supertest')", Want: engineExecutes},
	{Tool: "terraform", Binary: "terraform", Command: "plan", Want: engineNeedsProject},
	{Tool: "opentofu", Binary: "tofu", Command: "plan", Want: engineNeedsProject},
	{Tool: "powershell", Binary: "pwsh", Command: "Write-Output supertest",
		Want: engineOperatorProvided},
	{Tool: "go", Binary: "go", Command: "vet", Want: engineOperatorProvided},
}

// checkEveryAdvertisedEngineAnswers holds the deployed install to a known answer for all seven
// tools the product is sold as running.
//
// Ansible is proved by the fleet run above rather than here, because proving it properly needs
// three real machines and this check is about the other six.
//
// Until now the suite executed three of the seven end to end and said nothing about the rest. The
// engine contract exercises each tool's argv and exit codes against real binaries in CI, which is
// worth having and is not this: it never deploys anything, and the defect that shipped this month
// was one only a deployment could show. An image missing a tool it advertises, or refusing one it
// carries, is invisible to every test that does not pull the image and ask.
//
// Each engine is asked for the smallest thing worth asking, and the answer has to be the one
// written down for it. A silent success is a failure here: a tool that is not installed must not
// report that it did the work.
func (h *harness) checkEveryAdvertisedEngineAnswers(phase string) {
	const claim = "every advertised engine answers the way this install is built to answer"
	type submitted struct {
		engine advertisedEngine
		id     string
	}
	var runs []submitted
	for _, engine := range advertisedEngines {
		var created map[string]any
		if err := h.apiCall("POST", "/v1/runs", &h.human, map[string]any{
			"tool": engine.Tool, "command": engine.Command,
		}, &created); err != nil {
			h.fail(phase, claim, fmt.Errorf("submit a %s run: %w", engine.Tool, err))
			return
		}
		id, _ := created["id"].(string)
		if id == "" {
			h.fail(phase, claim, fmt.Errorf("%w: the %s run", ErrNoID, engine.Tool))
			return
		}
		runs = append(runs, submitted{engine: engine, id: id})
	}

	var wrong []string
	for _, r := range runs {
		doc, err := h.awaitRunTerminal(r.id, 120*time.Second)
		if err != nil {
			wrong = append(wrong, fmt.Sprintf("%s never settled: %v", r.engine.Tool, err))
			continue
		}
		status, _ := doc["status"].(string)
		failure, _ := doc["error"].(string)
		if note := judgeEngineAnswer(r.engine, status, failure); note != "" {
			wrong = append(wrong, note)
		}
	}
	if len(wrong) > 0 {
		h.fail(phase, claim, fmt.Errorf("%s", strings.Join(wrong, "; ")))
		return
	}
	h.pass(phase, claim, fmt.Sprintf("%d engines asked: each ran, or was refused naming the tool "+
		"it lacks, or was refused naming the project it needs", len(runs)))
}

// judgeEngineAnswer returns why an engine's answer is wrong, or empty when it is the expected one.
//
// The refusals have to name the thing that is missing. "Not found" against a binary that is present
// is what this exists to catch: it sends a reader to check an install that is fine while the real
// gap, a project for the tool to run in, goes unmentioned.
func judgeEngineAnswer(engine advertisedEngine, status, failure string) string {
	switch engine.Want {
	case engineExecutes:
		if status != "succeeded" {
			return fmt.Sprintf("%s is carried by this image and reached %q: %s",
				engine.Tool, status, oneLine(failure))
		}
	case engineNeedsProject:
		if status == "succeeded" {
			return fmt.Sprintf("%s ran to success with no project, so it worked somewhere nobody "+
				"chose", engine.Tool)
		}
		if !strings.Contains(failure, "working directory does not exist") {
			return fmt.Sprintf("%s is carried by this image and was refused for something other "+
				"than the project it needs: %s", engine.Tool, oneLine(failure))
		}
	case engineOperatorProvided:
		if status == "succeeded" {
			return fmt.Sprintf("%s is not in this image and reported that it did the work",
				engine.Tool)
		}
		if !strings.Contains(failure, engine.Binary) {
			return fmt.Sprintf("%s is not in this image and its refusal does not name %q, so "+
				"nobody is told what to install: %s", engine.Tool, engine.Binary, oneLine(failure))
		}
	}
	return ""
}
