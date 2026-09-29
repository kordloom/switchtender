package server

import (
	"context"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/user"
	"github.com/kordloom/switchtender/internal/util"
)

// The scrubbers below are the field-level halves of the object scrubbers in handlers_runsread.go.
// An update restores through the same function the read masked with, so the two cannot drift: a
// field whose masking changes changes what the update recognizes in the same edit.
//
// They take the request context because scrubbing is per caller. An admin reads the original, so an
// admin's edit carries no marker and is taken as given, which is what should happen.

// isAdmin reports whether the caller in ctx reads originals rather than scrubbed values. An absent
// actor is not an admin, which is the safe direction: an unauthenticated read is scrubbed.
func isAdmin(ctx context.Context) bool {
	actor, ok := actorFrom(ctx)
	return ok && actor.Role == user.RoleAdmin
}

// templateCommandScrubber masks a template's command for the caller in ctx.
func templateCommandScrubber(ctx context.Context) scrub.Scrubber[string] {
	return scrub.ScrubberFunc[string](func(command string) string {
		if isAdmin(ctx) || command == "" {
			return command
		}
		masked, _ := util.RedactAssignments(command, scrub.Marker)
		return masked
	})
}

// templateVarsScrubber masks a template's launch variables for the caller in ctx.
func templateVarsScrubber(ctx context.Context) scrub.Scrubber[map[string]any] {
	return scrub.ScrubberFunc[map[string]any](func(vars map[string]any) map[string]any {
		if isAdmin(ctx) {
			return vars
		}
		if redacted := redactVars(vars); redacted != nil {
			return redacted
		}
		return vars
	})
}

// stepsScrubber masks each pipeline step's script for the caller in ctx. Templates and schedules
// both carry steps and both scrub them the same way, so they share one scrubber.
func stepsScrubber(ctx context.Context) scrub.Scrubber[[]run.PipelineStep] {
	return scrub.ScrubberFunc[[]run.PipelineStep](func(steps []run.PipelineStep) []run.PipelineStep {
		if isAdmin(ctx) {
			return steps
		}
		if redacted := redactSteps(steps); redacted != nil {
			return redacted
		}
		return steps
	})
}
