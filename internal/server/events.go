package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Status is also pushed as server-sent events, so a browser sees a print, a tram check
// or a mesh being probed move on within a fraction of a second instead of at its next
// poll. Each stream looks at what the server already holds in memory every eventTick,
// and sends it when something has changed, and at least every eventHeartbeat so clocks
// and estimates in the page keep going. Nothing here asks the printer or the Hue
// Bridge anything, so how many streams are open can't affect a print.
const (
	eventTick      = 200 * time.Millisecond
	eventHeartbeat = time.Second
	// eventWrite is how long one event may take to send before the stream is dropped.
	eventWrite = 10 * time.Second
	// eventRetry is how long a browser waits before reconnecting, in milliseconds.
	eventRetry = 2000
)

// snapshot gives the event to send, and the same with anything that changes on every
// read, such as a "seconds ago", left out, to tell whether there's news.
type snapshot func() (event, key any)

// streamEvents sends snapshots as "status" events until the client goes or the server
// stops.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, snap snapshot) {
	rc := http.NewResponseController(w)
	// Its read deadline would also end the request, as the server watches for the
	// connection closing by reading from it.
	rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(b []byte) bool {
		// The web page's server gives every response a minute, which a stream outlasts,
		// so each write gets its own deadline. A recorder in tests can't set one.
		rc.SetWriteDeadline(time.Now().Add(eventWrite))
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send([]byte("retry: " + strconv.Itoa(eventRetry) + "\n\n")) {
		return
	}

	ticker := time.NewTicker(eventTick)
	defer ticker.Stop()
	var lastKey []byte
	var lastSent time.Time
	for {
		event, key := snap()
		k, err := json.Marshal(key)
		if err != nil {
			return
		}
		if !bytes.Equal(k, lastKey) || time.Since(lastSent) >= eventHeartbeat {
			data, err := json.Marshal(event)
			if err != nil {
				return
			}
			// JSON from encoding/json has no newlines, so it's one data line.
			msg := make([]byte, 0, len(data)+24)
			msg = append(msg, "event: status\ndata: "...)
			msg = append(msg, data...)
			msg = append(msg, "\n\n"...)
			if !send(msg) {
				return
			}
			lastKey, lastSent = k, time.Now()
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.baseCtx().Done():
			return
		case <-ticker.C:
		}
	}
}

// handleEvents is GET /v1/events: the status, as GET /v1/status gives it, apart from the
// plug, as reading it means asking the Hue Bridge. The GUI asks for that separately and
// less often.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, func() (any, any) {
		st := s.status(nil)
		key := st
		// A new reading of the same temperatures isn't news, only different ones.
		key.Printer.TempsAt = nil
		if t := key.Tram; t != nil {
			steady := *t
			steady.StepSeconds = 0
			key.Tram = &steady
		}
		if p := key.Print; p != nil && p.Mesh != nil {
			steady, mesh := *p, *p.Mesh
			mesh.PointSeconds = 0
			steady.Mesh = &mesh
			key.Print = &steady
		}
		return st, key
	})
}

// handleWebEvents is GET /api/events on the web page: its status, pushed.
func (s *Server) handleWebEvents(w http.ResponseWriter, r *http.Request) {
	s.streamEvents(w, r, func() (any, any) {
		st := s.webStatus()
		key := st
		if p := key.Print; p != nil {
			steady := *p
			steady.MovedAgo, steady.RunSeconds = 0, 0
			key.Print = &steady
		}
		return st, key
	})
}
