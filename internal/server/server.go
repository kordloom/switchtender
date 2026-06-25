// Package server exposes the Yardmaster HTTP API over the run store and dispatcher.
package server

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/ui"
)

// Submitter accepts a run request and returns the created run. The dispatcher satisfies it.
type Submitter interface {
	Submit(ctx context.Context, playbook, inventory string) (*run.Run, error)
}

// Server wires the run store and submitter into an HTTP handler.
type Server struct {
	// store reads runs and their logs for the query endpoints.
	store run.Store
	// submitter accepts new runs.
	submitter Submitter
	// log records request handling activity.
	log *zap.Logger
	// web serves the embedded user interface.
	web *ui.UI
}

// New returns a Server. It panics if store or submitter is nil; a nil logger becomes a no-op.
func New(store run.Store, submitter Submitter, log *zap.Logger) *Server {
	if store == nil {
		panic("server: Store required")
	}
	if submitter == nil {
		panic("server: Submitter required")
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Server{store: store, submitter: submitter, log: log, web: ui.New(log)}
}

// Handler returns the HTTP handler serving the Yardmaster API and web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthHandler())
	mux.Handle("POST /runs", createRunHandler(s.submitter, s.log))
	mux.Handle("GET /runs", listRunsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}", getRunHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/logs", runLogsHandler(s.store, s.log))
	mux.Handle("GET /runs/{id}/events", runEventsHandler(s.store, s.log))
	mux.Handle("/ui/", s.web.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	return mux
}
