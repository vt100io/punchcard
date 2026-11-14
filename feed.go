package main

import (
	"bytes"
	"sync"
)

// Kinds of feed events.
const (
	eventQueued   = "queued"   // Build has no ID or result yet
	eventDropped  = "dropped"  // a queued build was superseded
	eventStarted  = "started"  // Build has its ID; the log exists
	eventLine     = "line"     // one line of a running build's log
	eventFinished = "finished" // Build has its result
)

// Event is something that happened to a build.
type Event struct {
	Kind  string `json:"kind"`
	Build Build  `json:"build"`          // the build as it stands; for eventLine only Build.ID is set
	Line  string `json:"line,omitempty"` // eventLine only
	N     int    `json:"n,omitempty"`    // eventLine only: the line's index in the build log
}

// Feed broadcasts build events to live viewers such as a TUI. It never
// blocks the worker: a subscriber that falls behind misses events and should
// resync from History and the log file.
type Feed struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

var feed = &Feed{subs: map[chan Event]struct{}{}}

// Subscribe returns a channel of events and a function that ends the
// subscription and closes the channel.
func (f *Feed) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, ch)
			close(ch)
			f.mu.Unlock()
		})
	}
}

// Publish sends e to every subscriber that has room for it.
func (f *Feed) Publish(e Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		select {
		case ch <- e:
		default: // subscriber is behind; drop rather than stall the build
		}
	}
}

// lineWriter publishes each complete line written to it as an eventLine,
// numbered so a viewer that also read the log file can line the two up.
type lineWriter struct {
	id  string
	buf []byte
	n   int
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		feed.Publish(Event{Kind: eventLine, Build: Build{ID: w.id}, Line: string(w.buf[:i]), N: w.n})
		w.buf = w.buf[i+1:]
		w.n++
	}
	return len(p), nil
}
