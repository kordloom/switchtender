package server

import (
	"errors"
	"net/http"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// submitClass is the kind of error a submission ended in. Every path that submits work classes its
// error with classifySubmit, so one error is never the sender's to fix on one path and the server's
// failure on another, and each path decides only how its own sender is answered for each class.
type submitClass int

const (
	// submitFault is a failure of the server's own, which is anything no other class names.
	submitFault submitClass = iota
	// submitInput is an error in the request or in what it names, such as a missing project or an
	// inventory the product cannot run, that someone must change before the submission succeeds.
	submitInput
	// submitSecret is a credential whose secret this server cannot open, because nobody set it or
	// it was sealed under another key.
	submitSecret
	// submitPolicy is a submission a deny rule or the license refused.
	submitPolicy
	// submitUnresolved is an inventory that could not be resolved or sealed. Its errors also wrap
	// store faults, so a sender that retries a fault, or gives up on a refusal, must not be told the
	// error is its own.
	submitUnresolved
)

// submitClasses lists each class's errors in the order they are matched. The unresolved class is
// matched last because its errors wrap others: the composer wraps ErrNeedsAnsible, and an input's
// ErrNotFound, in ErrResolve, and a known error anywhere in the chain says better what went wrong.
var submitClasses = []struct {
	class submitClass
	errs  []error
}{
	{class: submitInput, errs: []error{
		credential.ErrNotFound, credential.ErrNoKey, project.ErrNotFound, inventory.ErrNotFound,
		inventory.ErrNeedsAnsible, inventory.ErrInvalidInventory, inventory.ErrTooManyHosts,
		inventory.ErrHostFilter, inventory.ErrSourceVars, dispatch.ErrNoPlaybook,
		dispatch.ErrNoCommand, dispatch.ErrUnknownTool, dispatch.ErrToolCredential,
		dispatch.ErrNoSteps, dispatch.ErrTooManySteps, dispatch.ErrStepInput, dispatch.ErrUnnamedStep,
		dispatch.ErrDuplicateStep, dispatch.ErrUnknownDependency, dispatch.ErrDependencyCycle,
		run.ErrApprovalStep, run.ErrDenyPath,
	}},
	{class: submitSecret, errs: []error{credential.ErrNoSecret, credential.ErrUnreadable}},
	{class: submitPolicy, errs: []error{dispatch.ErrPolicyDenied, dispatch.ErrQueueUnlicensed}},
	{class: submitUnresolved, errs: []error{
		inventory.ErrResolve, dispatch.ErrInventorySnapshot, inventory.ErrComposition,
	}},
}

// classifySubmit returns the class of a submission's error and the known error that placed it
// there, which is nil for a fault.
func classifySubmit(err error) (submitClass, error) {
	for _, c := range submitClasses {
		for _, known := range c.errs {
			if errors.Is(err, known) {
				return c.class, known
			}
		}
	}
	return submitFault, nil
}

// senderReason returns what a sender outside this server, such as a forge or a host, is told about
// err: the text of the known error it carries and nothing that error wraps, since the wrapped detail
// can quote an inventory's hosts and values to somebody who is not its owner.
func senderReason(err error) string {
	if _, known := classifySubmit(err); known != nil {
		return known.Error()
	}
	return "an error on the server's side"
}

// templateRefusal is what a foreign sender is told when the template it fired cannot launch for an
// error the template's owner must fix.
func templateRefusal(err error) string {
	return "this template cannot launch: " + senderReason(err)
}

// personSubmit answers a submission made by the person sending the request. Each error that person
// can act on is answered with its full reason, since they are the one who must fix it.
type personSubmit struct {
	// input is the status an error in the request, or an unresolved inventory, is answered with.
	input int
	// op names the submission in the server log, such as "submit run".
	op string
	// fault is the answer to a failure of the server's own, such as "could not submit run".
	fault string
}

// respond answers err and reports whether there was one to answer.
func (p personSubmit) respond(w http.ResponseWriter, log *zap.Logger, err error) bool {
	if err == nil {
		return false
	}
	switch class, _ := classifySubmit(err); class {
	case submitInput, submitUnresolved:
		respondError(w, log, p.input, err.Error())
	case submitSecret:
		respondError(w, log, http.StatusConflict, err.Error())
	case submitPolicy:
		respondError(w, log, http.StatusForbidden, err.Error())
	default:
		log.Error("server: " + p.op + ": " + err.Error())
		respondError(w, log, http.StatusInternalServerError, p.fault)
	}
	return true
}
