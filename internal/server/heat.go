package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
)

const (
	MaxBed    = gcode.MaxBed
	MaxNozzle = gcode.MaxNozzle

	// HeatIdleLimit is how long heaters stay on after the last preheat.
	HeatIdleLimit = 30 * time.Minute

	heatCheckEvery  = 5 * time.Second
	printCheckEvery = 30 * time.Second
	// cutGrace is how long past the limit the printer may stay out of reach
	// before the plug is cut, so one failed command doesn't end in a power cut.
	cutGrace = time.Minute
)

// Heaters is the part of the printer that heat commands need.
type Heaters interface {
	SetTargets(ctx context.Context, bed, nozzle int) error
	SDPrinting(ctx context.Context) (bool, error)
}

// PreheatRequest is the body of POST /v1/preheat.
type PreheatRequest struct {
	Bed    *int `json:"bed"`
	Nozzle *int `json:"nozzle"`
}

// PreheatResponse is returned by POST /v1/preheat.
type PreheatResponse struct {
	Bed          int       `json:"bed"`
	Nozzle       int       `json:"nozzle"`
	HeatersOffAt time.Time `json:"heaters_off_at"`
}

type heatState struct {
	// op keeps preheat, cool, power and the idle check from interleaving. It's
	// held across printer commands, so status doesn't take it.
	op sync.Mutex
	// printing and printCheckedAt are guarded by op.
	printing       bool
	printCheckedAt time.Time

	mu      sync.Mutex
	offAt   time.Time
	problem string
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) heatInfo() (*time.Time, string) {
	s.heat.mu.Lock()
	defer s.heat.mu.Unlock()
	if s.heat.offAt.IsZero() {
		return nil, s.heat.problem
	}
	offAt := s.heat.offAt
	return &offAt, s.heat.problem
}

func (s *Server) setOffAt(t time.Time) {
	s.heat.mu.Lock()
	defer s.heat.mu.Unlock()
	s.heat.offAt = t
}

func (s *Server) offAt() time.Time {
	s.heat.mu.Lock()
	defer s.heat.mu.Unlock()
	return s.heat.offAt
}

// setProblem logs only changes, because the idle check repeats every few seconds.
func (s *Server) setProblem(problem string) {
	s.heat.mu.Lock()
	defer s.heat.mu.Unlock()
	if problem != "" && problem != s.heat.problem {
		log.Print(problem)
		s.notifyProblem("Printer problem", problem)
	}
	s.heat.problem = problem
}

// heatersOff records that the heaters are known to be off.
func (s *Server) heatersOff() {
	s.setOffAt(time.Time{})
	s.setProblem("")
	s.heat.printing = false
}

func (s *Server) handlePreheat(w http.ResponseWriter, r *http.Request) {
	var in PreheatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.Bed == nil || in.Nozzle == nil {
		writeError(w, http.StatusBadRequest, `body must be {"bed":<whole C>,"nozzle":<whole C>}`)
		return
	}
	bed, nozzle := *in.Bed, *in.Nozzle
	switch {
	case bed < 0 || bed > MaxBed:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("bed must be 0 to %d C, got %d", MaxBed, bed))
		return
	case nozzle < 0 || nozzle > MaxNozzle:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("nozzle must be 0 to %d C, got %d", MaxNozzle, nozzle))
		return
	case bed == 0 && nozzle == 0:
		writeError(w, http.StatusBadRequest, "nothing to heat, use cool to turn the heaters off")
		return
	}

	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to heat: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to heat: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	if code, why := s.printerBlocker(r.Context()); why != "" {
		writeError(w, code, "refusing to heat: "+why)
		return
	}
	// Not the request's context: a client dropping out mustn't stop a command
	// halfway, leaving one heater on and the other not set.
	ctx := context.WithoutCancel(r.Context())
	if err := s.Heaters.SetTargets(ctx, bed, nozzle); err != nil {
		writePrinterError(w, err)
		return
	}
	offAt := s.restartHeatTimer()
	log.Printf("heating: bed %d C, nozzle %d C, off at %s", bed, nozzle, offAt.Format(time.RFC3339))
	writeJSON(w, http.StatusOK, PreheatResponse{Bed: bed, Nozzle: nozzle, HeatersOffAt: offAt})
}

// restartHeatTimer records that heaters were just turned on. The operation lock is held.
func (s *Server) restartHeatTimer() time.Time {
	now := s.now()
	offAt := now.Add(HeatIdleLimit)
	s.setOffAt(offAt)
	s.setProblem("")
	s.heat.printing = false
	s.heat.printCheckedAt = now
	return offAt
}

// printerBlocker says why heating or moving mustn't start. Anything unknown blocks it.
func (s *Server) printerBlocker(ctx context.Context) (int, string) {
	if s.Heaters == nil {
		return http.StatusServiceUnavailable, "there's no printer"
	}
	// M27 would wait behind the print's commands, and the answer is known.
	if s.printRunning() {
		return http.StatusConflict, printBusy
	}
	// That would turn off whatever was started now.
	if s.strayPending() {
		return http.StatusConflict, "a stream was cut off, and its heaters and M85 haven't been turned off yet"
	}
	return s.printerReady(ctx)
}

// printerReady is the part of printerBlocker that asks the plug and the printer.
func (s *Server) printerReady(ctx context.Context) (int, string) {
	plug := s.plugStatus(ctx)
	switch {
	case plug == nil:
		return http.StatusConflict, "no plug is configured, so power can't be confirmed"
	case plug.Error != "":
		return http.StatusBadGateway, "the plug's state can't be read: " + plug.Error
	case !plug.On:
		return http.StatusConflict, "the plug is off"
	}
	st := s.Printer()
	switch {
	case !st.Connected:
		return http.StatusConflict, "the printer isn't connected"
	case st.Temps == nil:
		return http.StatusConflict, "the printer's temperatures aren't known yet"
	}
	printing, err := s.Heaters.SDPrinting(ctx)
	switch {
	case err != nil:
		return http.StatusBadGateway, "can't tell whether a print is running: " + err.Error()
	case printing:
		return http.StatusConflict, "an SD print is running"
	}
	return 0, ""
}

// CoolResponse is returned by POST /v1/cool. During a print the heaters go off with
// the cancel, which status follows.
type CoolResponse struct {
	Off        bool `json:"off"`
	Cancelling bool `json:"cancelling,omitempty"`
}

// handleCool is never refused because of a print, as it's a safety command.
func (s *Server) handleCool(w http.ResponseWriter, r *http.Request) {
	// Heater commands would wait behind the print's own, and it would then carry on
	// moving with the nozzle cooling. Cancelling also turns the heaters off.
	if s.printRunning() {
		if err := s.cancelPrint("cool was requested"); err != nil {
			writePrinterError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, CoolResponse{Cancelling: true})
		return
	}
	// The firmware won't take heater commands during a copy, so the copy gives way.
	s.stopCopy("cool was requested")
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if !s.Printer().Connected {
		if plug := s.plugStatus(r.Context()); plug != nil && plug.Error == "" && !plug.On {
			s.heatersOff()
			writeJSON(w, http.StatusOK, CoolResponse{Off: true})
			return
		}
		writeError(w, http.StatusConflict, "the printer isn't connected and its plug isn't confirmed off, so the heaters can't be reached")
		return
	}
	if s.Heaters == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to cool")
		return
	}
	if err := s.Heaters.SetTargets(context.WithoutCancel(r.Context()), 0, 0); err != nil {
		writePrinterError(w, err)
		return
	}
	s.heatersOff()
	log.Print("heaters off, on request")
	writeJSON(w, http.StatusOK, CoolResponse{Off: true})
}

func writePrinterError(w http.ResponseWriter, err error) {
	writeError(w, printerErrorCode(err), err.Error())
}

func printerErrorCode(err error) int {
	if errors.Is(err, marlin.ErrNotConnected) {
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func (s *Server) watchHeaters(ctx context.Context) {
	tick := time.NewTicker(heatCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			s.checkHeaters(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// checkHeaters applies the idle timeout. It goes by the targets the printer
// reports, not by what was asked for here, so it also covers heaters set from
// the LCD, a server restart, and a client that dropped out mid-request.
func (s *Server) checkHeaters(ctx context.Context) {
	s.logTemps()
	s.endStrayStream(ctx)
	if s.Prints != nil && s.checkAdopt(ctx) {
		return
	}
	if s.copyRunning() && !s.copyMustYield() {
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()

	st := s.Printer()
	if !st.Connected || st.Temps == nil {
		s.checkUnreachable(ctx)
		return
	}
	if st.Temps.NozzleTarget <= 0 && st.Temps.BedTarget <= 0 {
		s.heatersOff()
		return
	}

	now := s.now()
	offAt := s.offAt()
	if offAt.IsZero() {
		offAt = now.Add(HeatIdleLimit)
		s.setOffAt(offAt)
		log.Printf("heaters are on without a preheat from this server, off at %s", offAt.Format(time.RFC3339))
	}
	expired := !now.Before(offAt)
	if expired || now.Sub(s.heat.printCheckedAt) >= printCheckEvery {
		printing, err := s.Heaters.SDPrinting(ctx)
		if err != nil {
			log.Printf("couldn't check for a print: %v", err)
		} else {
			s.heat.printing = printing
			s.heat.printCheckedAt = now
			if printing {
				s.setOffAt(now.Add(HeatIdleLimit))
				s.setProblem("")
				s.printStartedElsewhere()
				return
			}
		}
	}
	if !expired {
		return
	}
	// If M27 failed, the heaters still go off: a spoiled print is the lesser risk.
	if err := s.Heaters.SetTargets(ctx, 0, 0); err != nil {
		s.setProblem(fmt.Sprintf("the idle timeout couldn't turn the heaters off: %v", err))
		s.cutIfOverdue(ctx, now, offAt)
		return
	}
	s.heatersOff()
	log.Print("heaters off, idle timeout")
	s.updatePrint(func(j *PrintJob) {
		if j.State == PrintAdjusting {
			j.Note = idleNote
		}
	})
}

// copyMustYield is the idle check during a copy, which holds the operation lock.
// Temperatures still arrive then. A copy doesn't extend the timer, so once that's
// up the copy stops and the heaters go off. A lost link fails the copy by itself.
func (s *Server) copyMustYield() bool {
	st := s.Printer()
	if !st.Connected || st.Temps == nil || (st.Temps.NozzleTarget <= 0 && st.Temps.BedTarget <= 0) {
		return false
	}
	now := s.now()
	offAt := s.offAt()
	if offAt.IsZero() {
		offAt = now.Add(HeatIdleLimit)
		s.setOffAt(offAt)
		log.Printf("heaters are on without a preheat from this server, off at %s", offAt.Format(time.RFC3339))
	}
	if now.Before(offAt) {
		return false
	}
	s.stopCopy("the heaters' idle timeout came first")
	return true
}

// checkUnreachable handles a link that's down while heaters were last known on.
func (s *Server) checkUnreachable(ctx context.Context) {
	offAt := s.offAt()
	if offAt.IsZero() {
		return
	}
	s.heat.printCheckedAt = time.Time{}
	if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
		s.heatersOff()
		log.Print("the plug is off, so the heaters are too")
		return
	}
	now := s.now()
	if now.Before(offAt) {
		return
	}
	s.setProblem("the idle timeout can't reach the printer, and its heaters were last known on")
	s.cutIfOverdue(ctx, now, offAt)
}

// cutIfOverdue is the last resort for heaters nobody can control. The hotend fan
// stops too, but that's better than leaving them on. An SD print survives losing
// the link, so one that was running is left alone.
func (s *Server) cutIfOverdue(ctx context.Context, now, offAt time.Time) {
	if now.Before(offAt.Add(cutGrace)) {
		return
	}
	if s.heat.printing {
		s.setProblem("lost control of the printer during an SD print, so its power was left on")
		return
	}
	if s.Plug == nil {
		s.setProblem("can't reach the printer to turn its heaters off, and no plug is configured")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, plugTimeout)
	defer cancel()
	if err := s.Plug.Set(ctx, false); err != nil {
		s.setProblem(fmt.Sprintf("can't reach the printer to turn its heaters off, and cutting the plug failed: %v", err))
		return
	}
	s.setOffAt(time.Time{})
	s.setProblem("the plug was cut, because the printer's heaters couldn't be turned off")
}
