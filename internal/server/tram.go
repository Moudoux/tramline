package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/tram"
)

const (
	// MaxTramRepeat limits how many rounds one request can run.
	MaxTramRepeat = 10

	TramRunning = "running"
	TramDone    = "done"
	TramFailed  = "failed"

	// tramHistoryFile is kept because the Pi's journal doesn't survive a reboot.
	tramHistoryFile = "tram-history.jsonl"
)

// Prober is the part of the printer that tramming needs.
type Prober interface {
	Home(ctx context.Context) error
	Probe(ctx context.Context, x, y int) (tram.Reading, error)
	Park(ctx context.Context) error
	TramInfo(ctx context.Context) ([]string, error)
}

// TramRequest is the body of POST /v1/tram.
type TramRequest struct {
	// BedClear confirms nothing is on the bed, as the head crosses it low down.
	BedClear bool `json:"bed_clear"`
	Inset    *int `json:"inset,omitempty"`
	Repeat   *int `json:"repeat,omitempty"`
}

// TramRound is one pass over the four corners. Result is set once all four are in.
type TramRound struct {
	Readings []tram.Reading `json:"readings"`
	Result   *tram.Result   `json:"result,omitempty"`
}

// TramJob is one tramming run, and is what status reports.
type TramJob struct {
	ID         int          `json:"id"`
	State      string       `json:"state"`
	Step       string       `json:"step,omitempty"`
	Inset      int          `json:"inset"`
	Repeat     int          `json:"repeat"`
	Temps      marlin.Temps `json:"temps"`
	Rounds     []TramRound  `json:"rounds"`
	Result     *tram.Result `json:"result,omitempty"`
	Spread     *[4]float64  `json:"spread_mm,omitempty"`
	Parked     bool         `json:"parked"`
	Error      string       `json:"error,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
}

// clone copies everything status could see change while it's being sent.
func (j *TramJob) clone() *TramJob {
	out := *j
	out.Rounds = slices.Clone(j.Rounds)
	for i := range out.Rounds {
		out.Rounds[i].Readings = slices.Clone(out.Rounds[i].Readings)
	}
	return &out
}

type tramState struct {
	mu  sync.Mutex
	job *TramJob
	ids int
}

func (s *Server) tramJob() *TramJob {
	s.tram.mu.Lock()
	defer s.tram.mu.Unlock()
	if s.tram.job == nil {
		return nil
	}
	return s.tram.job.clone()
}

func (s *Server) tramRunning() bool {
	s.tram.mu.Lock()
	defer s.tram.mu.Unlock()
	return s.tram.job != nil && s.tram.job.State == TramRunning
}

func (s *Server) updateTram(change func(*TramJob)) {
	s.tram.mu.Lock()
	defer s.tram.mu.Unlock()
	change(s.tram.job)
}

// handleTram starts a run and returns straight away. It carries on if the client
// goes away, and the result is in status.
func (s *Server) handleTram(w http.ResponseWriter, r *http.Request) {
	in := TramRequest{}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"bed_clear":true}, with optional "inset" and "repeat"`)
		return
	}
	inset, repeat := tram.DefaultInset, 1
	if in.Inset != nil {
		inset = *in.Inset
	}
	if in.Repeat != nil {
		repeat = *in.Repeat
	}
	switch {
	case !in.BedClear:
		writeError(w, http.StatusBadRequest, "refusing to tram: the head crosses the bed, so confirm it's clear with bed_clear")
		return
	case inset < tram.MinInset || inset > tram.MaxInset:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("inset must be %d to %d mm, got %d", tram.MinInset, tram.MaxInset, inset))
		return
	case repeat < 1 || repeat > MaxTramRepeat:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("repeat must be 1 to %d, got %d", MaxTramRepeat, repeat))
		return
	}
	if s.Prober == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to tram")
		return
	}
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to tram: a run is already going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}

	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to tram: a run is already going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	if code, why := s.printerBlocker(r.Context()); why != "" {
		writeError(w, code, "refusing to tram: "+why)
		return
	}
	// The link can drop at any moment, so this is read once and checked.
	st := s.Printer()
	if st.Temps == nil {
		writeError(w, http.StatusConflict, "refusing to tram: the printer's temperatures aren't known yet")
		return
	}
	temps := *st.Temps
	out := s.newTramJob(inset, repeat, temps)
	go s.runTram(inset, repeat, temps)
	writeJSON(w, http.StatusOK, out)
}

// newTramJob records a run that's about to start, so it counts as running at once.
func (s *Server) newTramJob(inset, repeat int, temps marlin.Temps) *TramJob {
	s.tram.mu.Lock()
	s.tram.ids++
	job := &TramJob{
		ID: s.tram.ids, State: TramRunning, Step: "starting",
		Inset: inset, Repeat: repeat, Temps: temps, StartedAt: s.now(),
	}
	s.tram.job = job
	out := job.clone()
	s.tram.mu.Unlock()
	log.Printf("tram %d: starting, inset %d mm, %d round(s), bed %.1f/%.0f C, nozzle %.1f/%.0f C",
		job.ID, inset, repeat, temps.Bed, temps.BedTarget, temps.Nozzle, temps.NozzleTarget)
	return out
}

var errTargetsChanged = errors.New("the heater targets changed during the run, so the readings can't be compared")

// tramGuard fails if the heaters were changed, by the idle timeout, cool or the LCD,
// because the bed would then be changing shape between readings.
func (s *Server) tramGuard(start marlin.Temps) error {
	// A check that's part of a print stops between steps if that print is cancelled.
	s.print.mu.Lock()
	stop := s.print.leg != 0 && (!s.print.job.preparing() || s.print.job.Stopping)
	s.print.mu.Unlock()
	if stop {
		return errPrepStopped
	}
	t := s.Printer().Temps
	if t != nil && (t.BedTarget != start.BedTarget || t.NozzleTarget != start.NozzleTarget) {
		return errTargetsChanged
	}
	return nil
}

// tramStep runs one printer command with the operation lock held only for that
// command, so cool and the idle check get their turn between steps. The firmware
// couldn't act on them sooner anyway, as it runs commands in order.
func (s *Server) tramStep(name string, start *marlin.Temps, step func() error) error {
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if start != nil {
		if err := s.tramGuard(*start); err != nil {
			return err
		}
	}
	s.updateTram(func(j *TramJob) { j.Step = name })
	return step()
}

func (s *Server) tramSequence(ctx context.Context, inset, repeat int, start marlin.Temps) error {
	if err := s.tramStep("homing", &start, func() error { return s.Prober.Home(ctx) }); err != nil {
		return err
	}
	points := tram.Points(inset)
	var rounds [][4]float64
	for round := 1; round <= repeat; round++ {
		s.updateTram(func(j *TramJob) { j.Rounds = append(j.Rounds, TramRound{}) })
		var z [4]float64
		for _, pt := range points {
			var reading tram.Reading
			err := s.tramStep("probing "+pt.Corner.String(), &start, func() (err error) {
				reading, err = s.Prober.Probe(ctx, pt.X, pt.Y)
				return err
			})
			if err != nil {
				return fmt.Errorf("%s: %w", pt.Corner, err)
			}
			reading.Corner = pt.Corner.String()
			z[pt.Corner] = reading.Z
			log.Printf("tram: round %d, %s, X%.2f Y%.2f Z%.3f", round, reading.Corner, reading.X, reading.Y, reading.Z)
			s.updateTram(func(j *TramJob) {
				cur := &j.Rounds[len(j.Rounds)-1]
				cur.Readings = append(cur.Readings, reading)
			})
		}
		if err := s.tramGuard(start); err != nil {
			return err
		}
		result := tram.Evaluate(z)
		rounds = append(rounds, z)
		log.Printf("tram: round %d, range %.3f mm, pass %t, %s %s", round, result.Range, result.Pass, result.Action, result.Corner)
		s.updateTram(func(j *TramJob) {
			j.Rounds[len(j.Rounds)-1].Result = &result
			j.Result = &result
		})
	}
	if repeat > 1 {
		spread := tram.Spread(rounds)
		log.Printf("tram: spread across rounds, by corner: %v", spread)
		s.updateTram(func(j *TramJob) { j.Spread = &spread })
	}
	return nil
}

// runTram never parks after a failed command, because the head's state isn't known
// then. If the heaters were changed it's only the readings that are in doubt, so it
// still moves a possibly hot nozzle away from the bed.
func (s *Server) runTram(inset, repeat int, start marlin.Temps) error {
	ctx := s.baseCtx()
	seqErr := s.tramSequence(ctx, inset, repeat, start)
	err, parked := seqErr, false
	if err == nil || errors.Is(err, errTargetsChanged) || errors.Is(err, errPrepStopped) {
		parkErr := s.tramStep("parking", nil, func() error { return s.Prober.Park(ctx) })
		parked = parkErr == nil
		if parkErr != nil {
			err = errors.Join(err, fmt.Errorf("parking: %w", parkErr))
		}
	}
	now := s.now()
	s.updateTram(func(j *TramJob) {
		j.Step = ""
		j.Parked = parked
		j.FinishedAt = &now
		j.State = TramDone
		if err != nil {
			j.State = TramFailed
			j.Error = err.Error()
		}
		// An unfinished run has no verdict, even if earlier rounds had one. A failure
		// to park doesn't change what the readings said.
		if seqErr != nil {
			j.Result = nil
		}
	})
	if err != nil {
		log.Printf("tram: failed: %v", err)
	} else {
		log.Print("tram: finished")
	}
	s.saveTramHistory()
	return err
}

// saveTramHistory adds the finished run to the log of real readings. A failure to
// write it mustn't change the result of the run.
func (s *Server) saveTramHistory() {
	if s.DataDir == "" {
		return
	}
	line, err := json.Marshal(s.tramJob())
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(filepath.Join(s.DataDir, tramHistoryFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err == nil {
			_, err = f.Write(append(line, '\n'))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		log.Printf("tram: couldn't save the history: %v", err)
	}
}

// handleTramInfo returns the firmware's raw answers about probing. It's a fixed
// list of commands that don't move anything, and not a way to send arbitrary ones.
func (s *Server) handleTramInfo(w http.ResponseWriter, r *http.Request) {
	if s.Prober == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to ask")
		return
	}
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	if code, why := s.printerBlocker(r.Context()); why != "" {
		writeError(w, code, why)
		return
	}
	lines, err := s.Prober.TramInfo(context.WithoutCancel(r.Context()))
	if err != nil {
		writePrinterError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lines)
}
