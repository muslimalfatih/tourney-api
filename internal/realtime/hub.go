// Package realtime implements the Server-Sent Events (SSE) fan-out used for
// live match and tournament updates. It is deliberately in-process and
// dependency-free: no Redis, no message broker. When the service outgrows a
// single instance, the Publish path is the seam where a Postgres LISTEN/NOTIFY
// or an external pub/sub can be introduced without touching handlers.
package realtime

import (
	"encoding/json"
	"sync"
)

// Event is a single SSE message. Name maps to the SSE "event:" field; Data is
// JSON-encoded into the "data:" field.
type Event struct {
	Name string
	Data any
}

// Encode returns the JSON payload for the event's data field.
func (e Event) Encode() ([]byte, error) { return json.Marshal(e.Data) }

// subscriber is one connected client, keyed by topic.
type subscriber struct {
	ch chan Event
}

// Hub routes events to subscribers grouped by topic. A topic is typically a
// tournament id or slug, so a client only receives updates for what it is
// watching.
type Hub struct {
	mu     sync.RWMutex
	topics map[string]map[*subscriber]struct{}
}

func NewHub() *Hub {
	return &Hub{topics: make(map[string]map[*subscriber]struct{})}
}

// Subscribe registers a client for a topic and returns its event channel plus
// an unsubscribe function the caller MUST defer.
func (h *Hub) Subscribe(topic string) (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 16)}

	h.mu.Lock()
	if h.topics[topic] == nil {
		h.topics[topic] = make(map[*subscriber]struct{})
	}
	h.topics[topic][sub] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if subs, ok := h.topics[topic]; ok {
			delete(subs, sub)
			if len(subs) == 0 {
				delete(h.topics, topic)
			}
		}
		h.mu.Unlock()
		close(sub.ch)
	}
	return sub.ch, unsubscribe
}

// Publish delivers an event to every subscriber of a topic. Delivery is
// non-blocking: if a slow client's buffer is full the event is dropped for that
// client rather than stalling the publisher.
func (h *Hub) Publish(topic string, event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range h.topics[topic] {
		select {
		case sub.ch <- event:
		default:
		}
	}
}
