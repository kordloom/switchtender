package main

import (
	"fmt"
	"strings"
)

// checkTheChangeRegisterRenders proves the deployed install can produce the document Team is bought
// for.
//
// The period change register is the SOC 2 CC8.1 and ISO 27001 A.8.32 export, assembled from real
// run history, and it is the reason the paid tier costs what it costs. It had the thinnest test
// coverage of any headline feature and none of it ran against a deployed install, so the one
// artifact a customer buys was the one this suite never asked an install to produce.
//
// It is rendered in the pod, by the shipped binary, against the PostgreSQL database the install
// actually runs on, because that is the path an operator takes. Rendering it anywhere else would
// prove the renderer works and leave the interesting part, whether a real install can reach its own
// history and its own license, unasked.
func (h *harness) checkTheChangeRegisterRenders(phase, namespace, dsn, runID string) {
	const claim = "the deployed install renders the period change register"
	pod, err := h.serverPod(namespace)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	report, err := h.kubectl("exec", "-n", namespace, pod, "--",
		"switchtender", "audit", "report", "--db", dsn)
	if err != nil {
		h.fail(phase, claim, fmt.Errorf("render the register in the pod: %w\n%s", err,
			oneLine(report)))
		return
	}
	// A register that renders empty is the failure this is written against. The phase above it made
	// a change, held it, approved it and executed it, so a document that does not name that change
	// is one an auditor would read as a period in which nothing happened.
	switch {
	case strings.TrimSpace(report) == "":
		h.fail(phase, claim, fmt.Errorf("%w: the register rendered nothing at all", ErrNothingRead))
	case !strings.Contains(report, "<html") && !strings.Contains(report, "<!DOCTYPE"):
		h.fail(phase, claim, fmt.Errorf("the register is not the self-contained HTML document it "+
			"is documented to be: %s", oneLine(report)))
	case !strings.Contains(report, runID):
		// The run, not the change label. The register's change column describes what a run did,
		// its tool and its playbook, and the label that groups runs into one arc lives on the
		// changes view instead. Asserting the label here failed against a register that was
		// rendering correctly, which is the check being wrong rather than the product.
		h.fail(phase, claim, fmt.Errorf("%w: the register does not name run %s, though this phase "+
			"held it, approved it and executed it", ErrNothingRead, runID))
	default:
		h.pass(phase, claim, fmt.Sprintf("%d bytes of self-contained HTML naming run %s, rendered "+
			"by the shipped binary against the install's own PostgreSQL", len(report), runID))
	}
}
