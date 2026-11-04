// Package server is Tramline's HTTPS API.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"tramline/internal/hue"
	"tramline/internal/marlin"
	"tramline/internal/secure"
)

// StatusResponse is returned by GET /v1/status.
type StatusResponse struct {
	Version   string        `json:"version"`
	Simulated bool          `json:"simulated,omitempty"`
	Printer   marlin.Status `json:"printer"`
	Plug      *PlugStatus   `json:"plug,omitempty"`
	// HeatersOffAt is when the idle timeout will turn the heaters off.
	HeatersOffAt *time.Time `json:"heaters_off_at,omitempty"`
	// HeatProblem says why the heaters couldn't be turned off when they should be.
	HeatProblem string `json:"heat_problem,omitempty"`
	// Tram is the running or most recent tramming run.
	Tram *TramJob `json:"tram,omitempty"`
	// Copy is the running or most recent copy to the SD card.
	Copy *CopyJob `json:"copy,omitempty"`
	// Print is the running or most recent print.
	Print *PrintJob `json:"print,omitempty"`
}

// PlugStatus is the plug's state, or why it couldn't be read.
type PlugStatus struct {
	Name  string `json:"name,omitempty"`
	On    bool   `json:"on"`
	Error string `json:"error,omitempty"`
}

// Plug is the printer's power switch.
type Plug interface {
	Pair(ctx context.Context) error
	Lights(ctx context.Context) ([]hue.Light, error)
	State(ctx context.Context) (hue.Light, error)
	Set(ctx context.Context, on bool) error
}

// CoolEnough is the nozzle temperature below which power may be cut. Above it,
// the hotend fan still needs power to keep cooling the nozzle.
const CoolEnough = 50.0

const plugTimeout = 15 * time.Second

// Server serves the API for one printer.
type Server struct {
	Version   string
	Simulated bool
	Printer   func() marlin.Status
	// Allowed reports whether a client certificate fingerprint may connect.
	Allowed func(fingerprint string) bool
	Plug    Plug
	// Heaters is nil if there's no printer to heat.
	Heaters Heaters
	// Prober is nil if there's no printer to tram.
	Prober Prober
	// Card is nil if there's no printer to copy files to.
	Card Card
	// Prints is nil if there's no printer to print on.
	Prints Prints
	// TramSoak is how long a print waits at temperature before its first tram check.
	TramSoak time.Duration
	// HeatHold is how long the heaters must stay at temperature before a tram check.
	// Zero means the normal 10 seconds. Tests shorten it.
	HeatHold time.Duration
	// DataDir is where uploads and tramming history are kept. Empty means they aren't.
	DataDir string
	// Now is the clock, replaced in tests.
	Now func() time.Time

	heat  heatState
	tram  tramState
	files fileState
	print printState
	web   webState
	ctx   context.Context
}

// baseCtx is what work that outlives a request runs under.
func (s *Server) baseCtx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// Handler returns the API's routes. Every route requires an allowed client certificate.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/power", s.handlePower)
	mux.HandleFunc("POST /v1/preheat", s.handlePreheat)
	mux.HandleFunc("POST /v1/cool", s.handleCool)
	mux.HandleFunc("POST /v1/tram", s.handleTram)
	mux.HandleFunc("GET /v1/tram/info", s.handleTramInfo)
	mux.HandleFunc("PUT /v1/files/{name}", s.handleFilePut)
	mux.HandleFunc("GET /v1/files/{name}", s.handleFileGet)
	mux.HandleFunc("DELETE /v1/files/{name}", s.handleFileDelete)
	mux.HandleFunc("POST /v1/files/{name}/copy", s.handleCopy)
	mux.HandleFunc("GET /v1/files", s.handleFiles)
	mux.HandleFunc("POST /v1/copy/cancel", s.handleCopyCancel)
	mux.HandleFunc("POST /v1/print", s.handlePrint)
	mux.HandleFunc("POST /v1/print/continue", s.handlePrintContinue)
	mux.HandleFunc("POST /v1/cancel", s.handleCancel)
	mux.HandleFunc("POST /v1/estop", s.handleEstop)
	mux.HandleFunc("GET /v1/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /v1/settings", s.handleSettingsPut)
	mux.HandleFunc("POST /v1/hue/pair", s.handleHuePair)
	mux.HandleFunc("GET /v1/hue/plugs", s.handleHuePlugs)
	return s.requireClient(mux)
}

// requireClient checks each request as well as each connection, so revoking a
// client also cuts off any connection it already has open.
func (s *Server) requireClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 ||
			!s.Allowed(secure.Fingerprint(r.TLS.PeerCertificates[0].Raw)) {
			writeError(w, http.StatusForbidden, "this client isn't allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	offAt, problem := s.heatInfo()
	// A running print keeps moving this on, so a time would only mislead.
	if s.printJob().running() {
		offAt = nil
	}
	writeJSON(w, http.StatusOK, StatusResponse{
		Version:      s.Version,
		Simulated:    s.Simulated,
		Printer:      s.Printer(),
		Plug:         s.plugStatus(r.Context()),
		HeatersOffAt: offAt,
		HeatProblem:  problem,
		Tram:         s.tramJob(),
		Copy:         s.copyJob(),
		Print:        s.printJob(),
	})
}

func (s *Server) plugStatus(ctx context.Context) *PlugStatus {
	if s.Plug == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	state, err := s.Plug.State(ctx)
	if err != nil {
		return &PlugStatus{Error: err.Error()}
	}
	return &PlugStatus{Name: state.Name, On: state.On}
}

// cutPowerBlocker says why the printer's power mustn't be cut right now. A
// connected printer must prove it's cold, so unknown temperatures also block.
func cutPowerBlocker(st marlin.Status) string {
	if !st.Connected {
		return ""
	}
	t := st.Temps
	switch {
	case t == nil:
		return "the printer is connected but its temperatures aren't known yet"
	case t.NozzleTarget > 0 || t.BedTarget > 0:
		return "the heaters are on"
	case t.Nozzle >= CoolEnough:
		return fmt.Sprintf("the nozzle is %.0f C, wait until it's below %.0f C", t.Nozzle, CoolEnough)
	}
	return ""
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	if s.Plug == nil {
		writeError(w, http.StatusServiceUnavailable, "no plug is configured")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	var in struct {
		On *bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.On == nil {
		writeError(w, http.StatusBadRequest, `body must be {"on":true} or {"on":false}`)
		return
	}
	if !*in.On && s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to cut power: a tram run is going, tramline estop stops it")
		return
	}
	if !*in.On && s.printRunning() {
		writeError(w, http.StatusConflict, "refusing to cut power: "+printBusy)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	if !*in.On {
		if s.tramRunning() {
			writeError(w, http.StatusConflict, "refusing to cut power: a tram run is going, tramline estop stops it")
			return
		}
		if s.printRunning() {
			writeError(w, http.StatusConflict, "refusing to cut power: "+printBusy)
			return
		}
		if why := cutPowerBlocker(s.Printer()); why != "" {
			writeError(w, http.StatusConflict, "refusing to cut power: "+why)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), plugTimeout)
	defer cancel()
	if err := s.Plug.Set(ctx, *in.On); err != nil {
		writeHueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"on": *in.On})
}

func (s *Server) handleHuePair(w http.ResponseWriter, r *http.Request) {
	if s.Plug == nil {
		writeError(w, http.StatusServiceUnavailable, "no bridge is configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), plugTimeout)
	defer cancel()
	if err := s.Plug.Pair(ctx); err != nil {
		writeHueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paired": true})
}

func (s *Server) handleHuePlugs(w http.ResponseWriter, r *http.Request) {
	if s.Plug == nil {
		writeError(w, http.StatusServiceUnavailable, "no bridge is configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), plugTimeout)
	defer cancel()
	lights, err := s.Plug.Lights(ctx)
	if err != nil {
		writeHueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lights)
}

func writeHueError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, hue.ErrLinkButton), errors.Is(err, hue.ErrAlreadyPaired):
		code = http.StatusConflict
	case errors.Is(err, hue.ErrNotPaired), errors.Is(err, hue.ErrNoPlug), errors.Is(err, hue.ErrNotConfigured):
		code = http.StatusPreconditionFailed
	}
	writeError(w, code, err.Error())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Serve runs until ctx ends. The port carries two servers: the API, over TLS with client
// certificates, and the read-only web page, over plain HTTP, which can't reach the API.
func (s *Server) Serve(ctx context.Context, ln net.Listener, tlsCfg *tls.Config) error {
	s.ctx = ctx
	s.loadPrintJob()
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	web := &http.Server{
		Handler:           s.WebHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       webTimeout,
		WriteTimeout:      webTimeout,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	if s.Heaters != nil {
		go s.watchHeaters(ctx)
	}
	tlsConns, plainConns := splitListener(ln)
	defer ln.Close()
	webDone := make(chan struct{})
	go func() {
		defer close(webDone)
		if err := web.Serve(plainConns); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("web page: %v", err)
		}
	}()
	stop := context.AfterFunc(ctx, func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
		web.Shutdown(shutdown)
	})
	defer stop()
	err := srv.ServeTLS(tlsConns, "", "")
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		err = nil
	}
	ln.Close()
	<-webDone
	return err
}
