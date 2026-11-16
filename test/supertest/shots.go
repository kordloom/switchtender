package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// shotsTokenEnv names the environment variable that carries the sign-in token to the capture
// script, which reads the same name.
const shotsTokenEnv = "SUPERTEST_SHOTS_TOKEN"

// phaseShots captures the deployed UI through the live port-forward, so the report carries what a
// person would actually see, not only what the API said. It borrows playwright from the
// repository's own e2e suite rather than installing a second copy.
func (h *harness) phaseShots(dir string) error {
	const phase = "screenshots"
	const claim = "the deployed UI photographs"
	script := filepath.Join(h.work, "shots.mjs")
	if err := os.WriteFile(script, []byte(mustManifest("shots.mjs")), 0o644); err != nil {
		return err
	}
	detail, err := h.captureUI(script, dir)
	if err != nil {
		h.fail(phase, claim, err)
		return nil
	}
	h.pass(phase, claim, detail)
	return nil
}

// captureUI runs the capture script at script with node, writing into dir, and returns the evidence
// the report carries. The browser signs in as the person the phases act as, with that person's
// token carried in the environment. The script exiting zero is not the claim: the folder must then
// hold a capture, and no two captures may be the same picture.
func (h *harness) captureUI(script, dir string) (string, error) {
	args := []string{script, h.repo, h.api, dir}
	if runID, ok := h.ids["team-destroy-run"]; ok {
		args = append(args, runID)
	}
	env := []string{shotsTokenEnv + "=" + h.human.Token}
	if out, err := h.runEnv(env, "node", args...); err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	names, err := distinctCaptures(dir)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("signed in as %s, no two captures alike: %s", h.human.Name,
		strings.Join(names, ", ")), nil
}

// distinctCaptures returns the names of the captures in dir, in name order, once it has checked
// that there is at least one and that no two hold the same bytes. A capture is a .png file directly
// in dir. The error names every capture that matches another, with the SHA-256 they share.
func distinctCaptures(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the screenshot folder: %w", err)
	}
	var names []string
	bySum := map[string][]string{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".png" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read capture %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])
		bySum[digest] = append(bySum[digest], entry.Name())
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: %s holds no capture", ErrNothingRead, dir)
	}
	var same []string
	for digest, group := range bySum {
		if len(group) > 1 {
			same = append(same, fmt.Sprintf("%s share sha256 %s", strings.Join(group, ", "), digest))
		}
	}
	if len(same) > 0 {
		sort.Strings(same)
		return nil, fmt.Errorf("%w: %s", ErrSameCapture, strings.Join(same, "; "))
	}
	return names, nil
}
