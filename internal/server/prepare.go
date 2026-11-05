package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/tram"
)

// A print starts with these states, as every print needs a passing tram check first.
const (
	PrintHeating   = "heating"
	PrintSoaking   = "soaking"
	PrintChecking  = "checking"
	PrintAdjusting = "adjusting"
	PrintStarting  = "starting"
	// PrintScheduled ends a tram check made when scheduling, which passed.
	PrintScheduled = "scheduled"

	// TramBed and TramNozzle match the start G-code, so PETG doesn't ooze while probing.
	TramBed    = 80
	TramNozzle = 150

	// How close to its target each heater must stay, for how long, before a check.
	heatWindow = 1.0
	heatHold   = 10 * time.Second
	// The real printer took 257 seconds from cold.
	heatLimit = 15 * time.Minute
	// How often a heating or soaking job looks at the printer.
	prepEvery = time.Second

	restartNote = "the server restarted before the check finished, so nothing will move or heat until tramline print --continue"
	idleNote    = "the idle timer turned the heaters off, tramline print --continue heats them again"
	changedNote = "the heater targets changed, tramline print --continue heats them again and checks"
)

var (
	errPrepStopped = errors.New("stopped on request")
	errPrepGone    = errors.New("the job has ended")
)

func (j *PrintJob) preparing() bool {
	if j == nil {
		return false
	}
	switch j.State {
	case PrintHeating, PrintSoaking, PrintChecking, PrintAdjusting, PrintStarting:
		return true
	}
	return false
}

func (j *PrintJob) active() bool {
	return j.running() || j.preparing()
}

// showLCD is called with the operation lock held. The message is only a courtesy, so
// failing to show it doesn't stop anything. An empty one clears the line.
func (s *Server) showLCD(ctx context.Context, text string) {
	if err := s.Prints.LCD(ctx, text); err != nil {
		log.Printf("print: couldn't show %q on the LCD: %v", text, err)
	}
}

// startHeating is called with the operation lock held. The message comes last, as
// M140 and M104 each write their own, and then reset the line once at temperature,
// which showed "Nozzle Heating" then "Ready" on the real LCD. M117 cancels that reset.
func (s *Server) startHeating(ctx context.Context) error {
	if err := s.Heaters.SetTargets(ctx, TramBed, TramNozzle); err != nil {
		return err
	}
	s.restartHeatTimer()
	s.showLCD(ctx, "Heating bed")
	return nil
}

// beginPrep records a new job that's heating for its first check, and runs it.
func (s *Server) beginPrep(job *PrintJob) PrintJob {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	s.print.ids++
	job.ID = s.print.ids
	job.SoakPending = s.TramSoak > 0
	s.print.job = job
	s.print.sawDone, s.print.halted, s.print.reason = false, "", ""
	s.print.linkChecked = true
	s.savePrintJob()
	if job.ScheduleID != 0 {
		s.linkScheduleJob(job.ScheduleID, job.ID)
	}
	s.startLeg(job.ID)
	return *job
}

// startLeg is called with print.mu held. A leg runs from heating until the job is
// waiting for the user, printing or over. Nothing runs while the user adjusts.
func (s *Server) startLeg(id int) {
	s.print.legs++
	s.print.leg = s.print.legs
	s.print.wake = make(chan struct{}, 1)
	go s.prepare(id, s.print.leg, s.print.wake)
}

func (s *Server) prepare(id, leg int, wake <-chan struct{}) {
	defer func() {
		s.print.mu.Lock()
		defer s.print.mu.Unlock()
		if s.print.leg == leg {
			s.print.leg = 0
		}
	}()
	ctx := s.baseCtx()
	err := s.prepareSteps(ctx, id, wake)
	if errors.Is(err, errTargetsChanged) {
		err = s.toAdjusting(ctx, id, nil, false, changedNote)
	}
	s.print.mu.Lock()
	why := s.print.reason
	s.print.mu.Unlock()
	switch {
	case err == nil, errors.Is(err, errPrepGone):
	case ctx.Err() != nil:
		// The server is stopping, and the next one reads the saved job.
	case errors.Is(err, errPrepStopped):
		s.endPrep(PrintCancelled, why)
	default:
		s.endPrep(PrintFailed, err.Error())
	}
}

func (s *Server) prepareSteps(ctx context.Context, id int, wake <-chan struct{}) error {
	if err := s.waitAtTemp(ctx, id, wake); err != nil {
		return err
	}
	if err := s.soak(ctx, id, wake); err != nil {
		return err
	}
	result, parked, err := s.checkTram(ctx, id)
	if errors.Is(err, errTargetsChanged) {
		return s.toAdjusting(ctx, id, nil, parked, changedNote)
	}
	if err != nil {
		return err
	}
	if !result.Pass {
		return s.toAdjusting(ctx, id, result, parked, "")
	}
	job := s.printJob()
	if job == nil || job.ID != id {
		return errPrepGone
	}
	if job.PreCheck {
		return s.finishPreCheck(id, result)
	}
	if job.ScheduleID != 0 && !s.scheduleWanted(job.ScheduleID) {
		s.stopFor("the scheduled print was cancelled")
		return errPrepStopped
	}
	return s.startFile(ctx, id)
}

// finishPreCheck ends the check made when scheduling. The heaters go off with the job,
// then closeJob's report to the schedule starts the wait to cut the plug.
func (s *Server) finishPreCheck(id int, result *tram.Result) error {
	if err := s.prepCheck(id); err != nil {
		return err
	}
	s.endPrep(PrintScheduled, fmt.Sprintf("tram passed at %.3f mm, turning the printer off until its scheduled time", result.Range))
	return nil
}

// stopFor records why a job's stopping before its file starts, for prepare to end it.
func (s *Server) stopFor(why string) {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if s.print.job != nil {
		s.print.job.Stopping = true
	}
	s.print.reason = why
}

// prepCheck says whether the job should carry on.
func (s *Server) prepCheck(id int) error {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	return s.prepCheckLocked(id)
}

func (s *Server) prepCheckLocked(id int) error {
	job := s.print.job
	switch {
	case job == nil || job.ID != id || !job.preparing():
		return errPrepGone
	case job.Stopping:
		return errPrepStopped
	case s.print.halted != "":
		return fmt.Errorf("the firmware halted: %s", s.print.halted)
	}
	return nil
}

// setPrepState moves the job on, unless it has been cancelled, which is decided under
// the same lock, so a cancel can't go unnoticed.
func (s *Server) setPrepState(id int, state string, change func(*PrintJob)) error {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if err := s.prepCheckLocked(id); err != nil {
		return err
	}
	job := s.print.job
	job.State = state
	if change != nil {
		change(job)
	}
	s.savePrintJob()
	log.Printf("print %d: %s", id, state)
	return nil
}

// prepSleep waits for the next look at the printer, or until something changes.
func (s *Server) prepSleep(ctx context.Context, wake <-chan struct{}) {
	s.print.mu.Lock()
	every := s.print.every
	s.print.mu.Unlock()
	if every == 0 {
		every = prepEvery
	}
	select {
	case <-wake:
	case <-time.After(every):
	case <-ctx.Done():
	}
}

// heatGuard fails if the printer has gone, or its heaters were changed.
func (s *Server) heatGuard(ctx context.Context, id int) (*marlin.Temps, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.prepCheck(id); err != nil {
		return nil, err
	}
	st := s.Printer()
	if !st.Connected {
		return nil, errors.New("lost the link to the printer before the tram check")
	}
	if st.Temps != nil && (st.Temps.BedTarget != TramBed || st.Temps.NozzleTarget != TramNozzle) {
		return nil, errTargetsChanged
	}
	return st.Temps, nil
}

// waitAtTemp follows the temperature reports instead of sending M190 and M109, so the
// link stays free and cancel and cool act at once.
func (s *Server) waitAtTemp(ctx context.Context, id int, wake <-chan struct{}) error {
	hold := s.HeatHold
	if hold == 0 {
		hold = heatHold
	}
	start := time.Now()
	var since time.Time
	for {
		t, err := s.heatGuard(ctx, id)
		if err != nil {
			return err
		}
		if t != nil && math.Abs(t.Bed-TramBed) <= heatWindow && math.Abs(t.Nozzle-TramNozzle) <= heatWindow {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= hold {
				return nil
			}
		} else {
			since = time.Time{}
		}
		if time.Since(start) >= heatLimit {
			return fmt.Errorf("the heaters didn't reach bed %d C and nozzle %d C within %s", TramBed, TramNozzle, heatLimit)
		}
		s.prepSleep(ctx, wake)
	}
}

// soak waits at temperature before a first check, or a check after the bed has cooled.
func (s *Server) soak(ctx context.Context, id int, wake <-chan struct{}) error {
	s.print.mu.Lock()
	pending := s.print.job != nil && s.print.job.SoakPending
	s.print.mu.Unlock()
	if pending && s.TramSoak > 0 {
		until := s.now().Add(s.TramSoak)
		err := s.setPrepState(id, PrintSoaking, func(j *PrintJob) { j.SoakUntil = &until })
		if err != nil {
			return err
		}
		end := time.Now().Add(s.TramSoak)
		for time.Now().Before(end) {
			if _, err := s.heatGuard(ctx, id); err != nil {
				return err
			}
			s.prepSleep(ctx, wake)
		}
	}
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if s.print.job != nil && s.print.job.ID == id {
		s.print.job.SoakPending, s.print.job.SoakUntil = false, nil
	}
	return nil
}

// checkTram runs one tram check, and is a normal tram run in status and the history.
func (s *Server) checkTram(ctx context.Context, id int) (*tram.Result, bool, error) {
	if err := s.setPrepState(id, PrintChecking, nil); err != nil {
		return nil, false, err
	}
	s.heat.op.Lock()
	st := s.Printer()
	if !st.Connected || st.Temps == nil {
		s.heat.op.Unlock()
		return nil, false, errors.New("lost the link to the printer before the tram check")
	}
	temps := *st.Temps
	s.showLCD(ctx, "Checking tram")
	s.newTramJob(tram.DefaultInset, 1, temps)
	s.heat.op.Unlock()

	err := s.runTram(tram.DefaultInset, 1, temps)
	if stop := s.prepCheck(id); stop != nil {
		return nil, false, stop
	}
	job := s.tramJob()
	if err != nil {
		// Only a change of targets leaves a parked head that can be checked again.
		if errors.Is(err, errTargetsChanged) && job.Parked {
			return nil, true, errTargetsChanged
		}
		return nil, false, fmt.Errorf("the tram check failed: %w", err)
	}
	s.updatePrint(func(j *PrintJob) {
		j.Check = job.Result
		j.Checks++
	})
	return job.Result, job.Parked, nil
}

// lcdAdjust says what to turn, in what fits on the LCD's status line.
func lcdAdjust(r *tram.Result) string {
	way := "CW"
	if r.Action == "lower" {
		way = "CCW"
	}
	return strings.ToUpper(r.Action[:1]) + r.Action[1:] + " " + r.Corner + " " + way
}

// toAdjusting leaves the job waiting for tramline print --continue. Nothing runs while
// it waits, except the idle timer, which still turns the heaters off.
func (s *Server) toAdjusting(ctx context.Context, id int, result *tram.Result, parked bool, note string) error {
	s.print.mu.Lock()
	if err := s.prepCheckLocked(id); err != nil {
		s.print.mu.Unlock()
		return err
	}
	job := s.print.job
	// Nobody is there for a scheduled print to wait for, so it's cancelled, with the
	// reason recorded.
	if job.ScheduleID != 0 && !job.PreCheck {
		job.Stopping = true
		if result != nil {
			s.print.reason = fmt.Sprintf("the tram check failed at its scheduled time: range %.3f mm, limit %.2f mm, %s %s", result.Range, tram.Tolerance, result.Action, result.Corner)
		} else {
			s.print.reason = "the heater targets changed during its tram check"
		}
		s.print.mu.Unlock()
		return errPrepStopped
	}
	job.State, job.Parked, job.Note = PrintAdjusting, parked, note
	// A cancel from here on ends the job by itself.
	s.print.leg = 0
	s.savePrintJob()
	s.print.mu.Unlock()
	if result != nil {
		log.Printf("print %d: waiting for the user to %s %s, range %.3f mm", id, result.Action, result.Corner, result.Range)
	} else {
		log.Printf("print %d: waiting for the user: %s", id, note)
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if result != nil {
		s.showLCD(ctx, lcdAdjust(result))
	} else {
		s.showLCD(ctx, "Waiting, see status")
	}
	return nil
}

// startFile starts the print. The job then belongs to the watcher.
func (s *Server) startFile(ctx context.Context, id int) error {
	var sdName, mode string
	var size int64
	err := s.setPrepState(id, PrintStarting, func(j *PrintJob) { sdName, mode, size = j.SDName, j.Mode, j.Size })
	if err != nil {
		return err
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if err := s.prepCheck(id); err != nil {
		return err
	}
	// M23 would abort a print started from the LCD while this job was going.
	printing, err := s.Heaters.SDPrinting(ctx)
	if err != nil {
		return fmt.Errorf("can't tell whether a print is running: %w", err)
	}
	if printing {
		s.closeJobPower(PrintUnknown, "another print is running now, so this one didn't start", false)
		s.printStartedElsewhere()
		return errPrepGone
	}
	s.showLCD(ctx, "Starting print")
	var startErr error
	if mode == ModeStream {
		if err := s.Prints.StartStream(ctx); err != nil {
			return fmt.Errorf("the stream didn't start: %w", err)
		}
	} else {
		startErr = s.Prints.StartSD(ctx, sdName, size)
		if startErr != nil && !errors.Is(startErr, marlin.ErrStartUnknown) {
			return fmt.Errorf("the print didn't start: %w", startErr)
		}
	}
	// If M24's reply went missing it may be running, so it's followed either way.
	now := s.now()
	s.setOffAt(now.Add(HeatIdleLimit))
	// Only an SD print carries on without the server, so only that keeps the plug on.
	s.heat.printing = mode != ModeStream
	s.heat.printCheckedAt = now

	s.print.mu.Lock()
	job := s.print.job
	if job == nil || job.ID != id || !job.preparing() {
		s.print.mu.Unlock()
		return errPrepGone
	}
	stopping, why, name := job.Stopping, s.print.reason, job.Name
	job.State, job.Stopping, job.MovedAt = PrintRunning, false, now
	job.Parked, job.Note = false, ""
	if startErr != nil {
		job.Note = startErr.Error()
	}
	s.print.leg = 0
	s.print.sawDone, s.print.halted = false, ""
	s.print.linkChecked = true
	s.savePrintJob()
	s.startWatcher()
	s.print.mu.Unlock()
	s.noteBedUsed()
	if mode == ModeStream {
		log.Printf("print %d: tram passed, streaming %s, %d bytes", id, name, size)
	} else {
		log.Printf("print %d: tram passed, %s from the card as %s, %d bytes", id, name, sdName, size)
	}
	if stopping {
		if err := s.cancelPrint(why); err != nil {
			log.Printf("print %d: couldn't cancel: %v", id, err)
		}
	}
	return nil
}

// endPrep ends a job before its print started, so all there is to do is turn the
// heaters off and clear the LCD. If the link is down, the idle timer and the plug cut
// still look after the heaters.
func (s *Server) endPrep(state, why string) {
	ctx := s.baseCtx()
	var err error
	s.heat.op.Lock()
	if s.Printer().Connected && s.Heaters != nil {
		if err = s.Heaters.SetTargets(ctx, 0, 0); err == nil {
			s.heatersOff()
		}
		// After M104, which writes its own message.
		s.showLCD(ctx, "")
	}
	s.heat.op.Unlock()
	if err != nil {
		s.closeJob(PrintFailed, fmt.Sprintf("%s, but the heaters may be on: %v", why, err))
		return
	}
	s.closeJob(state, why)
}

// handlePrintContinue is the user saying the knobs are done. It restarts the heater
// timer, as preheat does, so a timer about to run out can't cut a check short.
func (s *Server) handlePrintContinue(w http.ResponseWriter, r *http.Request) {
	if s.Prints == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to print on")
		return
	}
	waiting := func() string {
		s.print.mu.Lock()
		defer s.print.mu.Unlock()
		switch job := s.print.job; {
		case job == nil || job.State != PrintAdjusting:
			return "no print is waiting for an adjustment"
		case job.Stopping:
			return "the print is being cancelled"
		}
		return ""
	}
	if why := waiting(); why != "" {
		writeError(w, http.StatusConflict, why)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if code, why := s.printerReady(r.Context()); why != "" {
		writeError(w, code, "refusing to continue: "+why)
		return
	}
	// A bed that has cooled needs the soak again, as it isn't really a recheck.
	cooled := true
	if t := s.Printer().Temps; t != nil {
		cooled = t.BedTarget != TramBed || t.NozzleTarget != TramNozzle
	}
	ctx := context.WithoutCancel(r.Context())
	if err := s.startHeating(ctx); err != nil {
		writePrinterError(w, err)
		return
	}
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	job := s.print.job
	if job == nil || job.State != PrintAdjusting || job.Stopping {
		writeError(w, http.StatusConflict, "the print has been cancelled")
		return
	}
	job.State, job.Note, job.Parked = PrintHeating, "", false
	job.SoakPending = job.SoakPending || (cooled && s.TramSoak > 0)
	s.savePrintJob()
	log.Printf("print %d: adjusted, checking again", job.ID)
	s.startLeg(job.ID)
	writeJSON(w, http.StatusOK, *job)
}
