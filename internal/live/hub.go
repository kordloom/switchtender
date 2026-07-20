// Package live provides an in process publish and subscribe hub that fans run output to streaming
// clients. The dispatcher publishes events and log chunks as a run executes; the HTTP server
// subscribes per run and relays them to browsers over Server Sent Events.
package live

import (
	"encoding/json"
	"sync"

	"github.com/dcadolph/switchtender/internal/event"
)

// Message is one item delivered to a subscriber. Data is JSON ready for an SSE data field.
type Message struct {
	// Type is the SSE event name: event, log, or end.
	Type string
	// Data is the JSON payload, empty for end.
	Data []byte
}

// subscriberBuffer is the per subscriber channel capacity. A slow client drops messages rather
// than blocking the publisher; the store remains the source of truth for reconciliation.
const subscriberBuffer = 256

// Hub fans run messages out to per run subscribers.
type Hub struct {
	// mu guards topics.
	mu sync.Mutex
	// topics maps a run id to its set of subscriber channels.
	topics map[string]map[chan Message]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{topics: make(map[string]map[chan Message]struct{})}
}

// Subscribe registers a subscriber for a run and returns its channel and an unsubscribe func.
func (h *Hub) Subscribe(id string) (<-chan Message, func()) {
	ch := make(chan Message, subscriberBuffer)
	h.mu.Lock()
	subs, ok := h.topics[id]
	if !ok {
		subs = make(map[chan Message]struct{})
		h.topics[id] = subs
	}
	subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if set, ok := h.topics[id]; ok {
				if _, ok := set[ch]; ok {
					delete(set, ch)
					close(ch)
				}
				if len(set) == 0 {
					delete(h.topics, id)
				}
			}
		})
	}
	return ch, cancel
}

// PublishEvents delivers each event to the run's subscribers.
func (h *Hub) PublishEvents(id string, events []event.Event) {
	for _, e := range events {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		h.publish(id, Message{Type: "event", Data: data})
	}
}

// PublishLog delivers a log chunk to the run's subscribers as a JSON string.
func (h *Hub) PublishLog(id string, chunk []byte) {
	data, err := json.Marshal(string(chunk))
	if err != nil {
		return
	}
	h.publish(id, Message{Type: "log", Data: data})
}

// CloseRun signals end to the run's subscribers and drops the topic so late subscribers do not
// attach to a finished run.
func (h *Hub) CloseRun(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.topics[id] {
		select {
		case ch <- Message{Type: "end"}:
		default:
		}
	}
	delete(h.topics, id)
}

// publish performs a non blocking send to every subscriber of a run.
func (h *Hub) publish(id string, msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.topics[id] {
		select {
		case ch <- msg:
		default:
		}
	}
}
