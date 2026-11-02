package web

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// eventStreamHeartbeat keeps proxies from closing an idle stream.
	eventStreamHeartbeat = 25 * time.Second
	// eventStreamLifetime ends each stream so the browser reconnects with a
	// freshly checked session; it stays under the proxy's route timeout.
	eventStreamLifetime = 10 * time.Minute
	// eventStreamCoalesce gathers a burst of changes into one event, and
	// eventStreamMinGap spaces events so clients refetch at most once per gap.
	eventStreamCoalesce = 250 * time.Millisecond
	eventStreamMinGap   = time.Second
	eventStreamWrite    = 10 * time.Second
	// Streams hold a connection each, so they have their own caps instead of
	// the per-request slots, which they would otherwise occupy for minutes.
	maxEventStreams     = 96
	maxEventStreamsUser = 4
)

// fleetEvents fans fleet-change notifications out to event-stream clients.
// Each subscriber has a one-slot buffer, so a burst collapses to one pending
// notification and a slow client never blocks the publisher.
type fleetEvents struct {
	mu     sync.Mutex
	subs   map[chan struct{}]struct{}
	users  map[string]int
	total  int
	closed bool
	done   chan struct{}
}

func newFleetEvents() *fleetEvents {
	return &fleetEvents{subs: make(map[chan struct{}]struct{}), users: make(map[string]int), done: make(chan struct{})}
}

func (e *fleetEvents) publish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for sub := range e.subs {
		select {
		case sub <- struct{}{}:
		default:
		}
	}
}

// subscribe registers a stream for user, or reports false when the stream caps
// are reached or the hub is closed.
func (e *fleetEvents) subscribe(user string) (<-chan struct{}, func(), bool) {
	user = strings.ToLower(strings.TrimSpace(user))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.total >= maxEventStreams || e.users[user] >= maxEventStreamsUser {
		return nil, nil, false
	}
	sub := make(chan struct{}, 1)
	e.subs[sub] = struct{}{}
	e.users[user]++
	e.total++
	var once sync.Once
	return sub, func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			delete(e.subs, sub)
			if e.users[user]--; e.users[user] <= 0 {
				delete(e.users, user)
			}
			e.total--
		})
	}, true
}

// close ends every stream; the server calls it when shutting down, since
// shutdown otherwise waits for long-lived handlers.
func (e *fleetEvents) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		close(e.done)
	}
}

// handleEvents streams a `fleet` event whenever node state the dashboard shows
// may have changed: after a gateway mutation, or when a node reports
// reservation, authorization or job events. Events carry no data; the client
// refetches the fleet snapshot it is allowed to see.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	session, ok := s.browserSessionUser(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "browser authentication required")
		return
	}
	updates, unsubscribe, ok := s.events.subscribe(session.User)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeJSONError(w, http.StatusTooManyRequests, "too many event streams")
		return
	}
	defer unsubscribe()

	controller := http.NewResponseController(w)
	// The server read deadline would cancel the request mid-stream; the stream
	// is bounded by eventStreamLifetime and per-write deadlines instead.
	_ = controller.SetReadDeadline(time.Time{})
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Accel-Buffering", "no")
	write := func(chunk string) bool {
		_ = controller.SetWriteDeadline(time.Now().Add(eventStreamWrite))
		if _, err := io.WriteString(w, chunk); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	// `ready` lets a reconnecting client refetch whatever it missed.
	if !write("retry: 3000\nevent: ready\ndata: {}\n\n") {
		return
	}
	heartbeat := time.NewTicker(eventStreamHeartbeat)
	defer heartbeat.Stop()
	lifetime := time.NewTimer(eventStreamLifetime)
	defer lifetime.Stop()
	var lastSent time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.events.done:
			return
		case <-lifetime.C:
			return
		case <-heartbeat.C:
			if !write(": keepalive\n\n") {
				return
			}
		case <-updates:
			delay := max(eventStreamCoalesce, eventStreamMinGap-time.Since(lastSent))
			timer := time.NewTimer(delay)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-s.events.done:
				timer.Stop()
				return
			case <-timer.C:
			}
			select {
			case <-updates:
			default:
			}
			if !write("event: fleet\ndata: {}\n\n") {
				return
			}
			lastSent = time.Now()
		}
	}
}
