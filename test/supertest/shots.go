package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// phaseShots captures the deployed UI through the live port-forward, so the report carries what a
// person would actually see, not only what the API said. It borrows playwright from the
// repository's own e2e suite rather than installing a second copy.
func (h *harness) phaseShots(dir string) error {
	const phase = "screenshots"
	script := filepath.Join(h.work, "shots.mjs")
	if err := os.WriteFile(script, []byte(mustManifest("shots.mjs")), 0o644); err != nil {
		return err
	}
	args := []string{script, h.repo, h.api, dir}
	if runID, ok := h.ids["team-destroy-run"]; ok {
		args = append(args, runID)
	}
	if out, err := h.run("node", args...); err != nil {
		h.fail(phase, "the deployed UI photographs", fmt.Errorf("%w\n%s", err, out))
		return nil
	}
	h.pass(phase, "the deployed UI photographs", "runs, estate, policies, and the governed run")
	return nil
}
