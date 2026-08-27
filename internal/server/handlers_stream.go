package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// defaultEventsPage is the page size when an events read names none, and maxEventsPage is the
// largest page a caller can request; the response's next_after cursor pages through the rest.
const (
	defaultEventsPage = 5000
	maxEventsPage     = 20000
)

// exportSentinel is the trailing line a streaming export writes when it stops short. The status
// line left with the first byte of the body, so a truncated transfer still reads as a clean 200 and
// the file it produced still parses. The sentinel is the only thing that tells whoever opens the
// file that entries are missing from it.
type exportSentinel struct {
	// Incomplete is always true. Its presence in the file is the whole signal.
	Incomplete bool `json:"export_incomplete"`
	// Reason says in plain words what stopped the export.
	Reason string `json:"reason"`
}

// writeExportSentinel appends the incomplete marker as its own line to a body already in flight. A
// write that fails here means the reader is already gone, so there is nobody left to warn.
func writeExportSentinel(w http.ResponseWriter, log *zap.Logger, reason string) {
	line, err := json.Marshal(exportSentinel{Incomplete: true, Reason: reason})
	if err != nil {
		log.Error("server: marshal export sentinel: " + err.Error())
		return
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		log.Error("server: write export sentinel: " + err.Error())
	}
}

// runEventsHandler returns a page of a run's structured events as JSON with a next_after cursor.
func runEventsHandler(store run.Store, authz *authorizer, log *zap.Logger) http.HandlerFunc {
	if store == nil {
		panic("server: runEventsHandler: Store required")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: get run events: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run events")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}
		// download=1 streams every event as a named NDJSON attachment, the export form tooling
		// consumes line by line; the paged JSON form below serves the UI.
		if r.URL.Query().Get("download") == "1" {
			// Paged rather than loaded whole. A run over a thousand hosts holds tens of thousands of
			// events, and reading them all before writing the first byte put the entire export in the
			// server's memory at once, for every concurrent download. The log export already streams;
			// this now matches it. NDJSON is written a line at a time, so a reader sees output
			// immediately and the server holds one page.
			var (
				enc     *json.Encoder
				flusher http.Flusher
				after   int64
				started bool
			)
			for {
				page, err := store.EventsAfter(r.Context(), id, after, maxEventsPage)
				if err != nil {
					log.Error("server: export run events: " + err.Error())
					if !started {
						// Nothing has been written, so this can still be an honest failure rather
						// than a file. A 500 with no attachment header cannot be mistaken for an
						// export the way a zero byte download can.
						respondError(w, log, http.StatusInternalServerError,
							"could not export run events")
						return
					}
					// The status line left with the first byte, so it cannot change now. A short
					// file that parses cleanly reads as a whole export, and this one is an audit
					// artifact, so its last line says it is incomplete.
					writeExportSentinel(w, log,
						"the event store failed part way through this export")
					return
				}
				if len(page) == 0 {
					return
				}
				if !started {
					started = true
					w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
					w.Header().Set("Content-Disposition",
						`attachment; filename="`+id+`-events.ndjson"`)
					enc = json.NewEncoder(w)
					flusher, _ = w.(http.Flusher)
				}
				for i := range page {
					if err := enc.Encode(&page[i]); err != nil {
						return
					}
				}
				after = page[len(page)-1].Seq
				if flusher != nil {
					flusher.Flush()
				}
				if len(page) < maxEventsPage {
					return
				}
			}
		}
		after := queryInt64(r, "after")
		limit := queryInt(r, "limit")
		if limit <= 0 {
			limit = defaultEventsPage
		}
		limit = min(limit, maxEventsPage)
		events, err := store.EventsAfter(r.Context(), id, after, limit)
		if err != nil {
			if errors.Is(err, run.ErrNotFound) {
				respondError(w, log, http.StatusNotFound, "run not found")
				return
			}
			log.Error("server: get run events: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not get run events")
			return
		}
		next := after
		if n := len(events); n > 0 {
			next = events[n-1].Seq
		}
		respondJSON(w, log, http.StatusOK,
			eventsResponse{Events: events, Count: len(events), NextAfter: next}, wantsPretty(r))
	}
}

// streamPollInterval is how often the stream handler drains new events from the store when no
// in-process signal arrives, which is how runs executing on other processes stream live.
const streamPollInterval = time.Second

// streamKeepaliveTicks is how many quiet poll ticks pass between SSE comment lines. A run that
// prints nothing writes nothing, and an intermediary with a read timeout cuts an idle stream; a
// comment is invisible to EventSource and keeps the connection warm.
const streamKeepaliveTicks = 20

// streamBatch caps how many events one drain reads from the store at a time, so a burst of
// output on a large run is emitted in bounded chunks rather than one unbounded read.
const streamBatch = 1000

// queryInt returns the named query parameter as a non-negative int, or zero when it is
// absent or not a positive number.
func queryInt(r *http.Request, name string) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// queryInt64 returns the named query parameter as a non-negative int64, or zero when it is
// absent or not a positive number.
func queryInt64(r *http.Request, name string) int64 {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// runStreamHandler streams a run's live events and log over Server Sent Events. The store is the
// source of truth: new rows beyond what the client has seen are emitted on a poll tick, and hub
// messages from a local executor only wake the drain early. Runs executing on any process in the
// fleet therefore stream the same way, and the stream ends when the stored run turns terminal.
//
// A stream also ends when shutdown is canceled. Graceful shutdown waits for handlers to return and
// does not cancel a request's context, so without this a draining process would sit out its whole
// shutdown timeout for every stream still open. The stream closes without an end event, so the
// browser reconnects and resumes from its cursor once the process is back.
func runStreamHandler(streamer Streamer, store run.Store, authz *authorizer, log *zap.Logger,
	shutdown context.Context) http.HandlerFunc {
	if store == nil {
		panic("server: runStreamHandler: Store required")
	}
	// A nil channel blocks forever in a select, which is what an unset shutdown context should do.
	var draining <-chan struct{}
	if shutdown != nil {
		draining = shutdown.Done()
	}
	openStreams := &streamCount{}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			respondError(w, log, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		// A stream is held open for as long as its run lasts, and each one keeps a goroutine, a
		// subscription, and a poll of the store every second. Nothing bounded how many one caller could
		// open, so a viewer, the lowest role that can read a run, could hold thousands open against
		// still-executing runs and drive the store at thousands of reads a second for as long as they
		// liked. The interface opens one per run page, so a real reader never approaches either limit.
		release, admitted := openStreams.admit(actorKeyFor(r))
		if !admitted {
			w.Header().Set("Retry-After", "5")
			respondError(w, log, http.StatusTooManyRequests,
				"too many live streams are open; close one before opening another")
			return
		}
		defer release()

		id := r.PathValue("id")
		rn, gerr := store.Get(r.Context(), id)
		if errors.Is(gerr, run.ErrNotFound) {
			respondError(w, log, http.StatusNotFound, "run not found")
			return
		}
		if gerr != nil {
			log.Error("server: stream run: " + gerr.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read run")
			return
		}
		if authorizeRunAccess(w, r, authz, log, rn) {
			return
		}

		// A split or pipeline parent runs no tool of its own, so its own event log stays empty and
		// re-reading the store for it emits nothing. Its children publish their events under the
		// parent topic as they run, so the payload a wake carries is the only copy the parent stream
		// ever sees. Such a run forwards the wake payload rather than draining its own log.
		coordinator := rn.Kind == run.KindSplit || rn.Kind == run.KindPipeline

		var wake <-chan live.Message
		if streamer != nil {
			ch, cancel := streamer.Subscribe(id)
			defer cancel()
			wake = ch
		}

		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// The browser passes the seq of the last event it already has as ?after=, so the
		// stream resumes from there and never replays history. Without it, the stream starts
		// from the current end. On an automatic reconnect the browser sends Last-Event-ID
		// carrying both cursors, so a dropped connection resumes events and log bytes without
		// a gap. Each drain is an indexed range scan from its cursor, never a re-read of what
		// was already sent.
		var lastSeq int64
		if _, ok := r.URL.Query()["after"]; ok {
			lastSeq = queryInt64(r, "after")
		} else if seq, err := store.LastEventSeq(r.Context(), id); err == nil {
			lastSeq = seq
		}
		var logSeq int64
		if seq, err := store.LastLogSeq(r.Context(), id); err == nil {
			logSeq = seq
		}
		if ev, lg, ok := parseStreamCursor(r.Header.Get("Last-Event-ID")); ok {
			lastSeq, logSeq = ev, lg
		}

		drain := func() bool {
			for {
				evs, err := store.EventsAfter(r.Context(), id, lastSeq, streamBatch)
				if err != nil || len(evs) == 0 {
					break
				}
				for _, e := range evs {
					lastSeq = e.Seq
					data, err := json.Marshal(e)
					if err != nil {
						continue
					}
					writeSSE(w, "event", streamCursor(lastSeq, logSeq), data)
				}
				if len(evs) < streamBatch {
					break
				}
			}
			for {
				chunks, err := store.LogAfter(r.Context(), id, logSeq, streamBatch)
				if err != nil || len(chunks) == 0 {
					break
				}
				var buf []byte
				for _, c := range chunks {
					logSeq = c.Seq
					buf = append(buf, c.Data...)
				}
				if data, err := json.Marshal(string(buf)); err == nil {
					writeSSE(w, "log", streamCursor(lastSeq, logSeq), data)
				}
				if len(chunks) < streamBatch {
					break
				}
			}
			flusher.Flush()
			rn, err := store.Get(r.Context(), id)
			return err == nil && rn.Status.Terminal()
		}

		ticker := time.NewTicker(streamPollInterval)
		defer ticker.Stop()
		quietTicks := 0
		for {
			select {
			case <-draining:
				return
			default:
			}
			if drain() {
				writeSSE(w, "end", streamCursor(lastSeq, logSeq), nil)
				flusher.Flush()
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-draining:
				return
			case msg, ok := <-wake:
				// A closed hub channel means the run ended and the topic is gone. Receiving
				// without checking made it always ready, so the loop stopped waiting and
				// re-queried the store as fast as it could: tens of thousands of statements a
				// second per connected stream. It fired exactly when the store was already
				// struggling, because that is when a run's stream is closed without the run
				// reaching a terminal state, so store trouble fed on itself.
				if !ok {
					// One last drain, so anything written between the previous read and the close
					// still reaches the browser, then end the stream the way a terminal run does.
					drain()
					writeSSE(w, "end", streamCursor(lastSeq, logSeq), nil)
					flusher.Flush()
					return
				}
				if coordinator && (msg.Type == "event" || msg.Type == "log") {
					// The parent's own log is empty, so the loop's drain has nothing to send for it.
					// Forward the child's event or log straight from the wake, which is the only
					// place a coordinator's live output exists. The cursor stays the parent's, which
					// carries no meaningful sequence, since the page merging shard histories folds
					// these by content and reconciles from the shards when the run ends.
					writeSSE(w, msg.Type, streamCursor(lastSeq, logSeq), msg.Data)
					flusher.Flush()
				}
			case <-ticker.C:
				quietTicks++
				if quietTicks >= streamKeepaliveTicks {
					quietTicks = 0
					if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
						return
					}
					flusher.Flush()
				}
			}
		}
	}
}

// streamCursor encodes the event and log positions as one SSE id, the value a browser echoes back
// as Last-Event-ID when it reconnects.
func streamCursor(eventSeq, logSeq int64) string {
	return strconv.FormatInt(eventSeq, 10) + ":" + strconv.FormatInt(logSeq, 10)
}

// parseStreamCursor decodes a Last-Event-ID header written by streamCursor. ok is false for an
// absent or malformed value, leaving the caller's defaults in place.
func parseStreamCursor(v string) (eventSeq, logSeq int64, ok bool) {
	evPart, lgPart, found := strings.Cut(v, ":")
	if !found {
		return 0, 0, false
	}
	ev, err := strconv.ParseInt(evPart, 10, 64)
	if err != nil || ev < 0 {
		return 0, 0, false
	}
	lg, err := strconv.ParseInt(lgPart, 10, 64)
	if err != nil || lg < 0 {
		return 0, 0, false
	}
	return ev, lg, true
}

// writeSSE writes one Server Sent Event with the given event name, resume id, and JSON data. A
// write failure means the client went away; the stream loop ends on the closed request context.
func writeSSE(w http.ResponseWriter, name, id string, data []byte) {
	if len(data) == 0 {
		data = []byte("null")
	}
	_, _ = fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", name, id, data)
}
