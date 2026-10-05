package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// DefaultHookAnswerWithin bounds how long a webhook delivery waits for the work it starts before
// the sender is answered. GitHub stops waiting for an answer after ten seconds, as GitLab does by
// default, and the approval gate may download the modules a Terraform or OpenTofu configuration
// calls before it records a run, which can take longer than that.
const DefaultHookAnswerWithin = 5 * time.Second

// WithHookAnswerWithin sets how long a webhook delivery waits for the work it starts before the
// sender is answered that the delivery was accepted and the work goes on. Zero keeps the default.
func WithHookAnswerWithin(d time.Duration) Option {
	return func(srv *Server) {
		if d > 0 {
			srv.hooks.within = d
		}
	}
}

// hookAnswer is what a webhook's work says to its sender: a status with a JSON body, or with an
// error message, and the receipt of the audit entry the work recorded.
type hookAnswer struct {
	// status is the HTTP status.
	status int
	// body is the JSON body of an answer that is not an error.
	body any
	// message is the error an error answer carries, empty for any other answer.
	message string
	// receipt is the receipt of the audit entry the answer rests on, empty for none.
	receipt string
}

// failed reports whether the answer tells the sender the work did not happen.
func (a hookAnswer) failed() bool {
	return a.message != "" || a.status >= http.StatusBadRequest
}

// write sends the answer to the sender.
func (a hookAnswer) write(w http.ResponseWriter, r *http.Request, log *zap.Logger) {
	if a.receipt != "" {
		w.Header().Set(AuditReceiptHeader, a.receipt)
	}
	if a.message != "" {
		respondError(w, log, a.status, a.message)
		return
	}
	respondJSON(w, log, a.status, a.body, wantsPretty(r))
}

// hookFlights runs the work webhook deliveries start apart from the requests that carried them, one
// flight per delivery. A slow module download then neither holds a sender past its patience nor
// runs twice for a redelivery that arrives while the first is still going: the redelivery joins the
// flight already running for the same delivery.
type hookFlights struct {
	// within is how long a delivery waits for its work before the sender is answered anyway.
	within time.Duration
	// mu guards flights and every flight's deferred flag and answer.
	mu sync.Mutex
	// flights holds the work in progress, by delivery key.
	flights map[string]*hookFlight
	// wg counts the work in progress, so a shutdown or a test can wait for it.
	wg sync.WaitGroup
}

// hookFlight is the work one delivery started.
type hookFlight struct {
	// done is closed when the work has finished and answer is set.
	done chan struct{}
	// answer is what the work said.
	answer hookAnswer
	// deferred records that a sender was answered before the work finished, so whatever the work
	// says next reaches no sender and has to be recorded instead.
	deferred bool
}

// newHookFlights returns flights that wait DefaultHookAnswerWithin.
func newHookFlights() *hookFlights {
	return &hookFlights{within: DefaultHookAnswerWithin, flights: map[string]*hookFlight{}}
}

// run starts work for the delivery key, or joins the work already running for it, and waits up to
// the bound for its answer. It returns the answer and true when the work finished in time, and
// false when the sender has to be answered before it does. When a sender was answered first, unsent
// is called with what the work finally says, so a failure nobody was told about is recorded.
func (f *hookFlights) run(key string, log *zap.Logger, work func() hookAnswer,
	unsent func(hookAnswer)) (hookAnswer, bool) {
	f.mu.Lock()
	fl, running := f.flights[key]
	if !running {
		fl = &hookFlight{done: make(chan struct{})}
		f.flights[key] = fl
		f.wg.Add(1)
		go f.fly(key, fl, log, work, unsent)
	}
	f.mu.Unlock()

	timer := time.NewTimer(f.within)
	defer timer.Stop()
	select {
	case <-fl.done:
		return fl.answer, true
	case <-timer.C:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-fl.done:
		return fl.answer, true
	default:
		fl.deferred = true
		return hookAnswer{}, false
	}
}

// fly runs one delivery's work to its end and settles the flight. A panic in the work becomes a
// failed answer, since it runs where no request handler would recover it.
func (f *hookFlights) fly(key string, fl *hookFlight, log *zap.Logger, work func() hookAnswer,
	unsent func(hookAnswer)) {
	defer f.wg.Done()
	answer := func() (a hookAnswer) {
		defer func() {
			if p := recover(); p != nil {
				log.Error(fmt.Sprintf("server: webhook work panicked: %v", p))
				a = hookAnswer{status: http.StatusInternalServerError,
					message: "the delivery could not be handled"}
			}
		}()
		return work()
	}()
	f.mu.Lock()
	fl.answer = answer
	deferred := fl.deferred
	close(fl.done)
	delete(f.flights, key)
	f.mu.Unlock()
	if deferred && unsent != nil {
		unsent(answer)
	}
}

// wait blocks until every delivery's work has finished or ctx ends, and reports whether it all
// finished.
func (f *hookFlights) wait(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// WaitForHooks blocks until the work webhook deliveries started has finished or ctx ends, and
// reports whether it all finished. A server draining calls it after it stops taking requests, so a
// delivery it already answered is carried through rather than cut off.
func (s *Server) WaitForHooks(ctx context.Context) bool {
	return s.hooks.wait(ctx)
}
