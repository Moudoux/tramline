package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
	"tramline/internal/tram"
	"tramline/internal/webui"
)

// The web page is the one thing served without a client certificate, as plain HTTP on
// the same port. It's read-only by construction: it has its own routes on its own
// http.Server, so nothing here can reach the API, and it only reads what the server
// already holds in memory. It never sends the printer or the Hue Bridge anything, so
// however many browsers are open, a print can't be affected by them.

// How long a connection may say nothing before it's dropped, while finding out whether
// it's TLS, and how long the web page's requests may take.
const (
	sniffTimeout = 10 * time.Second
	webTimeout   = time.Minute
)

// webEndedSchedules is how many scheduled prints that have ended the page shows.
const webEndedSchedules = 5

// WebStatus is GET /api/status on the web page. It's a list of its own rather than a
// StatusResponse, so a field added to the API isn't made public by accident.
type WebStatus struct {
	Version   string        `json:"version"`
	Simulated bool          `json:"simulated,omitempty"`
	Connected bool          `json:"connected"`
	Temps     *marlin.Temps `json:"temps,omitempty"`
	Print     *WebPrint     `json:"print,omitempty"`
	Tramming  bool          `json:"tramming,omitempty"`
	Copy      *WebCopy      `json:"copy,omitempty"`
	// Scheduled is the prints still to come, soonest first, then the latest to end.
	Scheduled []WebSchedule `json:"scheduled,omitempty"`
}

// WebSchedule is one scheduled print.
type WebSchedule struct {
	ID     int       `json:"id"`
	Name   string    `json:"name"`
	SHA256 string    `json:"sha256,omitempty"`
	From   string    `json:"from"`
	At     time.Time `json:"at"`
	State  string    `json:"state"`
	Ended  bool      `json:"ended,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// WebPrint is the running or most recent print.
type WebPrint struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	SHA256 string `json:"sha256,omitempty"`
	Mode   string `json:"mode"`
	State  string `json:"state"`
	Pos    int64  `json:"pos"`
	Size   int64  `json:"size"`
	// MovedAgo is how many seconds ago Pos last moved on, by the server's clock.
	MovedAgo float64 `json:"moved_ago"`
	// RunSeconds is how long the job has been going, up to its end.
	RunSeconds float64      `json:"run_seconds"`
	Elsewhere  bool         `json:"elsewhere,omitempty"`
	LinkLost   bool         `json:"link_lost,omitempty"`
	Parked     bool         `json:"parked,omitempty"`
	Stopping   bool         `json:"stopping,omitempty"`
	Check      *tram.Result `json:"check,omitempty"`
	// Turn is which way the knob goes for a failed check.
	Turn  string `json:"turn,omitempty"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// WebCopy is a running copy to the card.
type WebCopy struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256,omitempty"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
}

// webState keeps the last model, so reloading the page doesn't mean reading the file
// again on the Pi during a print.
type webState struct {
	mu      sync.Mutex
	name    string
	modTime time.Time
	size    int64
	model   []byte
}

// WebHandler returns the web page's routes. Anything else is a 404, including the API.
func (s *Server) WebHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(webui.Page()))
	mux.Handle("GET /shared/", http.StripPrefix("/shared", http.FileServerFS(webui.Shared())))
	mux.HandleFunc("GET /api/status", s.handleWebStatus)
	mux.HandleFunc("GET /api/events", s.handleWebEvents)
	mux.HandleFunc("GET /api/model/{id}", s.handleWebModel)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleWebStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.webStatus())
}

func (s *Server) webStatus() WebStatus {
	st := s.Printer()
	out := WebStatus{
		Version:   s.Version,
		Simulated: s.Simulated,
		Connected: st.Connected,
		Temps:     st.Temps,
		Tramming:  s.tramRunning(),
	}
	now := s.now()
	if job := s.printJob(); job != nil {
		end := now
		if job.FinishedAt != nil {
			end = *job.FinishedAt
		}
		out.Print = &WebPrint{
			ID: job.ID, Name: job.Name, SHA256: job.SHA256, Mode: job.Mode, State: job.State,
			Pos: job.Pos, Size: job.Size,
			MovedAgo:   now.Sub(job.MovedAt).Seconds(),
			RunSeconds: end.Sub(job.StartedAt).Seconds(),
			Elsewhere:  job.Elsewhere, LinkLost: job.LinkLost, Parked: job.Parked, Stopping: job.Stopping,
			Check: job.Check, Note: job.Note, Error: job.Error,
		}
		if c := job.Check; c != nil && !c.Pass {
			out.Print.Turn = tram.Turn(c.Action)
		}
	}
	if job := s.copyJob(); job != nil && job.State == CopyRunning {
		out.Copy = &WebCopy{Name: job.Name, SHA256: job.SHA256, Bytes: job.Bytes, TotalBytes: job.TotalBytes}
	}
	out.Scheduled = s.webSchedules()
	return out
}

// webSchedules keeps the page short: all that are to come, and a few that have ended.
func (s *Server) webSchedules() []WebSchedule {
	var coming, ended []WebSchedule
	for _, p := range s.schedulesCopy() {
		item := WebSchedule{ID: p.ID, Name: p.Name, SHA256: p.SHA256, From: p.From, At: p.At, State: p.State, Ended: p.ended(), Reason: p.Reason}
		if item.Ended {
			ended = append(ended, item)
		} else {
			coming = append(coming, item)
		}
	}
	slices.Reverse(ended)
	if len(ended) > webEndedSchedules {
		ended = ended[:webEndedSchedules]
	}
	return append(coming, ended...)
}

// handleWebModel gives the moves of the current job's file. Only that one can be asked
// for, by job number, so nobody can list or fetch the other files.
func (s *Server) handleWebModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	job := s.printJob()
	if err != nil || job == nil || job.ID != id {
		writeError(w, http.StatusNotFound, "that isn't the current job")
		return
	}
	model, err := s.webModel(job.Name)
	if err != nil {
		// A print from the LCD, or a file deleted since, has nothing here to read.
		writeError(w, http.StatusNotFound, "the file isn't on the server")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(model)))
	w.Write(model)
}

// webModel reads a file's moves. The lock also means several browsers asking at once
// read it only once.
func (s *Server) webModel(name string) ([]byte, error) {
	// A job's name can come from the printer, so it's checked like an upload's.
	if err := validFileName(name); err != nil {
		return nil, err
	}
	dir, err := s.uploadsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	s.web.mu.Lock()
	defer s.web.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if s.web.model != nil && s.web.name == name && s.web.modTime.Equal(info.ModTime()) && s.web.size == info.Size() {
		return s.web.model, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	parsed, err := gcode.ParsePath(f)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	s.web.name, s.web.modTime, s.web.size, s.web.model = name, info.ModTime(), info.Size(), parsed.Encode()
	return s.web.model, nil
}

// tlsRecordHandshake is the first byte of a TLS connection. No HTTP request starts with
// it, so one look tells the two apart.
const tlsRecordHandshake = 0x16

// splitListener sends TLS connections to one listener and everything else to the other.
// Closing either closes ln, and so does ln failing.
func splitListener(ln net.Listener) (tlsConns, plainConns *connListener) {
	done := make(chan struct{})
	var once sync.Once
	closeAll := func() error {
		once.Do(func() { close(done) })
		return ln.Close()
	}
	tlsConns = &connListener{addr: ln.Addr(), conns: make(chan net.Conn), done: done, close: closeAll}
	plainConns = &connListener{addr: ln.Addr(), conns: make(chan net.Conn), done: done, close: closeAll}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				closeAll()
				return
			}
			go sniff(conn, tlsConns, plainConns)
		}
	}()
	return tlsConns, plainConns
}

// sniff waits for the first byte in its own goroutine, so a silent client holds up
// nobody else. Either way the bytes are left for whichever server gets the connection.
func sniff(conn net.Conn, tlsConns, plainConns *connListener) {
	conn.SetReadDeadline(time.Now().Add(sniffTimeout))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	to := plainConns
	if first[0] == tlsRecordHandshake {
		to = tlsConns
	}
	select {
	case to.conns <- &peekedConn{Conn: conn, reader: reader}:
	case <-to.done:
		conn.Close()
	}
}

// peekedConn reads what was already looked at first.
type peekedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// connListener hands out connections that splitListener has sorted.
type connListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	close func() error
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *connListener) Close() error {
	err := l.close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (l *connListener) Addr() net.Addr { return l.addr }
