package runtime

import (
	"sync"
)

const ringSize = 1000

// LogBuffer is a thread-safe ring buffer that captures the last N log lines
// and broadcasts new lines to all active subscribers.
type LogBuffer struct {
	mu   sync.Mutex
	lines []string
	subs  []chan string
}

func newLogBuffer() *LogBuffer {
	return &LogBuffer{}
}

// Write appends a line and broadcasts it to all subscribers.
func (b *LogBuffer) Write(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lines = append(b.lines, line)
	if len(b.lines) > ringSize {
		b.lines = b.lines[len(b.lines)-ringSize:]
	}

	// Broadcast non-blocking; drop lines for slow subscribers.
	for _, ch := range b.subs {
		select {
		case ch <- line:
		default:
		}
	}
}

// Lines returns a snapshot of all buffered lines.
func (b *LogBuffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// Subscribe returns a channel that receives new log lines.
// Call Unsubscribe(ch) when done.
func (b *LogBuffer) Subscribe() chan string {
	ch := make(chan string, 256)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return ch
}

// SubscribeWithHistory atomically returns a snapshot of all buffered lines and
// registers a subscriber for future lines. The lock is held across both
// operations so no lines can be written (and missed) between the snapshot and
// the subscription.
func (b *LogBuffer) SubscribeWithHistory() (history []string, sub chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	history = make([]string, len(b.lines))
	copy(history, b.lines)
	sub = make(chan string, 256)
	b.subs = append(b.subs, sub)
	return history, sub
}

// Unsubscribe removes a subscriber channel and closes it.
func (b *LogBuffer) Unsubscribe(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, s := range b.subs {
		if s == ch {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			close(ch)
			return
		}
	}
}
