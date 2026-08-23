package engine

import (
	"sync"
	"time"
)

// Event is the stable live-update envelope consumed by the SSE endpoint.
type Event struct {
	Type        string         `json:"type"`
	At          time.Time      `json:"at"`
	SubjectType string         `json:"subject_type,omitempty"`
	SubjectID   int64          `json:"subject_id,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
}

// EventBus is a bounded in-process fan-out. Slow browser clients drop events;
// the database remains authoritative and a reconnect refreshes the view.
type EventBus struct {
	mu      sync.RWMutex
	nextID  int
	max     int
	clients map[int]chan Event
}

func NewEventBus(max int) *EventBus {
	if max <= 0 {
		max = 8
	}
	return &EventBus{max: max, clients: make(map[int]chan Event)}
}

func (b *EventBus) Publish(event Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (b *EventBus) Subscribe() (<-chan Event, func(), bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) >= b.max {
		return nil, func() {}, false
	}
	id := b.nextID
	b.nextID++
	ch := make(chan Event, 32)
	b.clients[id] = ch
	return ch, func() {
		b.mu.Lock()
		if current, ok := b.clients[id]; ok {
			delete(b.clients, id)
			close(current)
		}
		b.mu.Unlock()
	}, true
}
