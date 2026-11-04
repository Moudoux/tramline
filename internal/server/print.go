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
	"strings"
	"sync"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/tram"
)

const (
	PrintRunning    = "running"
	PrintCancelling = "cancelling"
	PrintFinished   = "finished"
	PrintCancelled  = "cancelled"
	PrintFailed     = "failed"
	// PrintUnknown means it ended where it couldn't be seen, such as during a lost
	// link, so whether it finished properly isn't known.
	PrintUnknown = "unknown"

	printFile = "print.json"
	printBusy = "a print is running, tramline cancel or estop stops it"

	// How often a print's position is read.
	printEvery = 30 * time.Second
	// How long the firmware may take to say it's done, once M27 says it's not printing.
	// It closes the file with commands still to run.
	printEndGrace = time.Minute
	// One M27 can wait through a whole heater wait, but not for ever.
	printPollLimit = 15 * time.Minute
	// How long a cancel may take to be confirmed, and how often M524 is repeated.
	cancelLimit = 2 * time.Minute
	cancelRetry = 10 * time.Second
	// How long estop waits for a copy to let go when the plug has failed.
	estopCopyWait  = 20 * time.Second
	estopTimeout   = time.Second
	estopHeadStart = 50 * time.Millisecond
)

// Prints is the part of the printer that printing needs.
type Prints interface {
	StartSD(ctx context.Context, name string, size int64) error
	SDStatus(ctx context.Context) (marlin.SDStatus, error)
	SDFileName(ctx context.Context) (string, error)
	CancelSD() error
	LCD(ctx context.Context, text string) error
	CloseDoneScreen(ctx context.Context) error
	EmergencyStop() error
	FinishMoves(ctx context.Context) error
	StartStream(ctx context.Context) error
	StreamFile(ctx context.Context, marker string, next func() (string, error), progress func(marlin.SDProgress)) error
	StopStream(ctx context.Context) error
	ClearKill(ctx context.Context) error
	EndLostStream(ctx context.Context) error
}

// PrintRequest is the body of POST /v1/print.
type PrintRequest struct {
	Name string `json:"name"`
	// BedClear confirms nothing is on the bed.
	BedClear bool `json:"bed_clear"`
}

// PrintJob is one print, and is what status reports. It's saved, so it survives a
// restart of the server, which an SD print does too.
type PrintJob struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	SDName string `json:"sd_name"`
	Mode   string `json:"mode"`
	State  string `json:"state"`
	// Elsewhere means it wasn't started here, such as from the LCD.
	Elsewhere bool `json:"elsewhere,omitempty"`
	// Pos is the firmware's position in the file, or in a stream the bytes sent, counted
	// as they would be on the card.
	Pos  int64 `json:"pos"`
	Size int64 `json:"size"`
	// Resends and Nudges are for a stream: how often the firmware asked for a line again,
	// and how many lines went with an earlier one still unanswered.
	Resends int `json:"resends,omitempty"`
	Nudges  int `json:"nudges,omitempty"`
	// LinkLost means the printer can't be reached, and the print is presumed running.
	LinkLost bool `json:"link_lost,omitempty"`
	// SoakPending means the next check waits at temperature first, until SoakUntil.
	SoakPending bool       `json:"soak_pending,omitempty"`
	SoakUntil   *time.Time `json:"soak_until,omitempty"`
	// Check is the latest tram check, and Checks is how many have finished.
	Check  *tram.Result `json:"check,omitempty"`
	Checks int          `json:"checks,omitempty"`
	// Parked means the head is parked for adjusting.
	Parked bool `json:"parked,omitempty"`
	// Stopping means a cancel is waiting for the current step to finish.
	Stopping   bool       `json:"stopping,omitempty"`
	Note       string     `json:"note,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	MovedAt    time.Time  `json:"moved_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type printState struct {
	mu  sync.Mutex
	job *PrintJob
	ids int
	// leg is non-zero while a goroutine is taking the job towards its start, and legs
	// counts them.
	leg, legs int
	// watching means a goroutine is following the job.
	watching bool
	wake     chan struct{}
	// stopPoll ends an M27 that's waiting for its reply.
	stopPoll context.CancelFunc
	// stopStream stops sending a stream.
	stopStream context.CancelFunc
	// strayStream means a stream was cut off, so the next connection turns off the
	// heaters and M85.
	strayStream bool
	// tempsLogged is when temperatures were last logged during a job.
	tempsLogged time.Time
	sawDone     bool
	halted      string
	reason      string
	// linkChecked means this connection has been asked whether a print is running.
	linkChecked bool
	// adopting means that question is waiting for its answer.
	adopting bool
	// every is printEvery, and grace printEndGrace, unless a test changes them.
	every, grace time.Duration
}

func (j *PrintJob) running() bool {
	return j != nil && (j.State == PrintRunning || j.State == PrintCancelling)
}

func (s *Server) printJob() *PrintJob {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if s.print.job == nil {
		return nil
	}
	job := *s.print.job
	return &job
}

// printRunning includes a job that hasn't started its file yet, as anything else
// touching the printer would upset it just the same.
func (s *Server) printRunning() bool {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	return s.print.job.active()
}

func (s *Server) wakeWatcher() {
	if s.print.wake == nil {
		return
	}
	select {
	case s.print.wake <- struct{}{}:
	default:
	}
}

// PrinterLine is told when a print finishes or the firmware halts. It's on the
// link's read loop, so it only leaves a note for the watcher.
func (s *Server) PrinterLine(line string) {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if !s.print.job.active() {
		return
	}
	// The firmware says this for an SD print's own cancel too, which stops by itself. A
	// stream only stops if it's told to, as nothing on the board can stop it.
	if strings.Contains(line, marlin.ActionCancel) {
		if job := s.print.job; job.Mode == ModeStream && job.State == PrintRunning {
			log.Printf("print %d: stopped from the LCD", job.ID)
			go func() {
				if err := s.cancelPrint("stopped from the LCD"); err != nil {
					log.Printf("print: cancelling after the LCD's stop: %v", err)
				}
			}()
		}
		return
	}
	if s.print.job.preparing() {
		if strings.Contains(line, "Done printing file") {
			return
		}
		s.print.halted = line
		if s.print.leg == 0 {
			go s.closeJob(PrintFailed, "the firmware halted: "+line)
		}
		log.Printf("print %d: printer said: %q", s.print.job.ID, line)
		s.wakeWatcher()
		return
	}
	if strings.Contains(line, "Done printing file") {
		s.print.sawDone = true
	} else {
		s.print.halted = line
	}
	log.Printf("print %d: printer said: %q", s.print.job.ID, line)
	s.wakeWatcher()
}

// savePrintJob is called with print.mu held. Failing to save it only matters after
// a restart, so it mustn't stop the print.
func (s *Server) savePrintJob() {
	if s.DataDir == "" || s.print.job == nil {
		return
	}
	data, err := json.MarshalIndent(s.print.job, "", "  ")
	if err == nil {
		err = writeAtomic(filepath.Join(s.DataDir, printFile), append(data, '\n'))
	}
	if err != nil {
		log.Printf("print: couldn't save the job: %v", err)
	}
}

// loadPrintJob picks up where the server left off. Whether a print is really still
// running is found out from the printer once it's connected, see adoptPrint.
func (s *Server) loadPrintJob() {
	if s.DataDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(s.DataDir, printFile))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var job PrintJob
	if err == nil {
		err = json.Unmarshal(data, &job)
	}
	if err != nil {
		log.Printf("print: couldn't read the saved job: %v", err)
		return
	}
	// Nothing moves or heats after a restart until the user says so. A file that did
	// get started is found by adoptPrint, as the name matches.
	if job.preparing() {
		job.Stopping, job.SoakUntil = false, nil
		if job.State != PrintAdjusting {
			job.State, job.Parked, job.Note = PrintAdjusting, false, restartNote
		}
	}
	// Unlike an SD print, nothing carries on sending it.
	stray := job.Mode == ModeStream && job.running()
	if stray {
		now := s.now()
		job.State, job.FinishedAt, job.Error = PrintFailed, &now, "the server restarted during a stream, which can't be picked up"
		log.Printf("print %d: %s", job.ID, job.Error)
		s.setOffAt(now.Add(HeatIdleLimit))
	}
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	s.print.strayStream = stray
	s.print.job, s.print.ids = &job, job.ID
	s.savePrintJob()
}

// handlePrint starts a print and returns straight away. It heats and checks tram
// first, and the file only starts on a pass, with no way around that. It needs nothing
// more from the client unless the bed needs adjusting, and status follows it.
func (s *Server) handlePrint(w http.ResponseWriter, r *http.Request) {
	var in PrintRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, `body must be {"name":"<file>","bed_clear":true}`)
		return
	}
	if err := validFileName(in.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !in.BedClear {
		writeError(w, http.StatusBadRequest, "refusing to print: confirm the bed is clear with bed_clear")
		return
	}
	if s.Prints == nil || s.Card == nil || s.Prober == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to print on")
		return
	}
	busy := func() string {
		switch job := s.printJob(); {
		case job != nil && job.State == PrintAdjusting:
			return "a print is waiting for the bed to be adjusted, tramline print --continue or tramline cancel"
		case s.printRunning():
			return "another print is running"
		case s.tramRunning():
			return "a tram run is going"
		case s.copyRunning():
			return "a copy to the SD card is going"
		}
		return ""
	}
	if why := busy(); why != "" {
		writeError(w, http.StatusConflict, "refusing to print: "+why)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if why := busy(); why != "" {
		writeError(w, http.StatusConflict, "refusing to print: "+why)
		return
	}
	s.files.mu.Lock()
	rec, err := s.loadRecord(in.Name)
	s.files.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, in.Name+" isn't on the server")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// What isn't verified on the card goes from here, line by line.
	stream := rec.SDState != CopyDone
	// M23 would abort a print that's running, so anything unknown refuses.
	if code, why := s.printerBlocker(r.Context()); why != "" {
		writeError(w, code, "refusing to print: "+why)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	mode, sdName := ModeStream, ""
	if !stream {
		mode, sdName = ModeSD, rec.SDName
		listed, err := s.Card.ListSD(ctx)
		if err != nil {
			writePrinterError(w, fmt.Errorf("refusing to print: can't read the card: %w", err))
			return
		}
		found, exists := findOnCard(listed, rec.SDName)
		switch {
		case !exists:
			writeError(w, http.StatusConflict, fmt.Sprintf("refusing to print: %s isn't on the card, tramline copy %s makes one", rec.SDName, in.Name))
			return
		case found.Size != rec.Info.SDSize:
			writeError(w, http.StatusConflict, fmt.Sprintf("refusing to print: %s is %d bytes on the card, it should be %d", rec.SDName, found.Size, rec.Info.SDSize))
			return
		}
	}
	// The print-done screen would hide the LCD's messages.
	if err := s.Prints.CloseDoneScreen(ctx); err != nil {
		writePrinterError(w, err)
		return
	}
	if err := s.startHeating(ctx); err != nil {
		writePrinterError(w, err)
		return
	}
	now := s.now()
	job := s.beginPrep(&PrintJob{
		Name: in.Name, SDName: sdName, Mode: mode, State: PrintHeating,
		Size: rec.Info.SDSize, StartedAt: now, MovedAt: now,
	})
	log.Printf("print %d: %s, heating for its tram check", job.ID, in.Name)
	writeJSON(w, http.StatusOK, job)
}

// beginJob records a new job and starts following it.
func (s *Server) beginJob(job *PrintJob) PrintJob {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	s.print.ids++
	job.ID = s.print.ids
	s.print.job = job
	s.print.sawDone, s.print.halted, s.print.reason = false, "", ""
	s.print.linkChecked = true
	s.savePrintJob()
	s.startWatcher()
	return *job
}

// startWatcher is called with print.mu held.
func (s *Server) startWatcher() {
	if s.print.watching {
		return
	}
	s.print.watching = true
	s.print.wake = make(chan struct{}, 1)
	if s.print.job.Mode == ModeStream {
		ctx, stop := context.WithCancel(s.baseCtx())
		s.print.stopStream = stop
		go s.watchStream(ctx, s.print.job.ID, s.print.job.Name, s.print.wake)
		return
	}
	go s.watchPrint(s.print.job.ID, s.print.wake)
}

// watchPrint follows one print until it ends. It's a separate goroutine because an
// M27 can wait for minutes behind a heater, which nothing else should have to wait
// for. It doesn't hold the operation lock, as commands are refused during a print.
func (s *Server) watchPrint(id int, wake <-chan struct{}) {
	ctx := s.baseCtx()
	defer func() {
		s.print.mu.Lock()
		defer s.print.mu.Unlock()
		s.print.watching = false
	}()
	var endedAt time.Time
	for ctx.Err() == nil {
		if s.printStep(ctx, id, &endedAt) {
			return
		}
		s.print.mu.Lock()
		every := s.print.every
		s.print.mu.Unlock()
		if every == 0 {
			every = printEvery
		}
		select {
		case <-wake:
		case <-time.After(every):
		case <-ctx.Done():
		}
	}
}

// printStep looks at the print once, and returns true once it's over.
func (s *Server) printStep(ctx context.Context, id int, endedAt *time.Time) bool {
	s.print.mu.Lock()
	job := s.print.job
	if job == nil || job.ID != id || !job.running() {
		s.print.mu.Unlock()
		return true
	}
	state, done, halted, wasLost, moved := job.State, s.print.sawDone, s.print.halted, job.LinkLost, job.MovedAt
	grace := s.print.grace
	s.print.mu.Unlock()
	if grace == 0 {
		grace = printEndGrace
	}

	switch {
	case halted != "":
		s.endPrint(PrintFailed, "the firmware halted: "+halted)
		return true
	case state == PrintCancelling:
		s.finishCancel(ctx)
		return true
	case done:
		s.endPrint(PrintFinished, "")
		return true
	}

	if !s.Printer().Connected {
		if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
			s.endPrint(PrintFailed, "the plug is off, so the print has stopped")
			return true
		}
		s.updatePrint(func(j *PrintJob) { j.LinkLost, j.Note = true, "link lost, print presumed running" })
		s.setProblem("lost the link during an SD print, so its power was left on")
		return false
	}

	poll, stop := context.WithTimeout(ctx, printPollLimit)
	s.print.mu.Lock()
	s.print.stopPoll = stop
	s.print.mu.Unlock()
	asked := time.Now()
	st, err := s.Prints.SDStatus(poll)
	stop()
	took := time.Since(asked).Round(time.Millisecond)
	// Nothing stops a long wait from going unnoticed but this.
	if err == nil {
		log.Printf("print %d: M27 took %s, at %d of %d bytes", id, took, st.Pos, st.Size)
	}

	now := s.now()
	switch {
	case err != nil:
		if ctx.Err() == nil && poll.Err() == nil {
			log.Printf("print %d: couldn't read its position after %s: %v", id, took, err)
		}
	case !st.Printing:
		if wasLost {
			s.endPrint(PrintUnknown, "it ended while the link was lost, so whether it finished isn't known")
			return true
		}
		// The firmware closes the file with commands still to run, then says it's done.
		if endedAt.IsZero() {
			*endedAt = now
		} else if now.Sub(*endedAt) >= grace {
			s.endPrint(PrintUnknown, "it stopped without finishing, perhaps from the LCD")
			return true
		}
		return false
	default:
		*endedAt = time.Time{}
		s.setProblem("")
		s.updatePrint(func(j *PrintJob) {
			j.LinkLost = false
			if j.Note == "link lost, print presumed running" {
				j.Note = ""
			}
			j.Size = st.Size
			if st.Pos != j.Pos {
				j.Pos, j.MovedAt = st.Pos, now
				moved = now
				s.setOffAt(now.Add(HeatIdleLimit))
			}
		})
	}
	// A paused or stuck print still counts as printing, so this is what stops it
	// holding the heaters on for ever.
	if now.Sub(moved) >= HeatIdleLimit {
		if err := s.cancelPrint(fmt.Sprintf("it hasn't moved on for %s", HeatIdleLimit)); err != nil {
			log.Printf("print %d: couldn't cancel a stuck print: %v", id, err)
		}
	}
	return false
}

func (s *Server) updatePrint(change func(*PrintJob)) {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	if s.print.job == nil {
		return
	}
	before := *s.print.job
	change(s.print.job)
	// Position changes are read back from the printer after a restart anyway.
	if before.State != s.print.job.State || before.LinkLost != s.print.job.LinkLost || before.Note != s.print.job.Note {
		s.savePrintJob()
	}
}

// closeJob records how a print ended. It doesn't take the operation lock.
func (s *Server) closeJob(state, why string) {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	job := s.print.job
	if !job.active() {
		return
	}
	now := s.now()
	job.State, job.FinishedAt, job.LinkLost, job.Stopping = state, &now, false, false
	if state == PrintFinished {
		job.Pos = job.Size
	}
	if state == PrintFailed {
		job.Error = why
	} else {
		job.Note = why
	}
	s.savePrintJob()
	log.Printf("print %d: %s. %s", job.ID, state, why)
	s.wakeWatcher()
}

func (s *Server) setPrinting(printing bool) {
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	s.heat.printing = printing
}

// endPrint hands the heaters back to the idle timer.
func (s *Server) endPrint(state, why string) {
	s.setPrinting(false)
	s.setOffAt(time.Time{})
	s.setProblem("")
	s.closeJob(state, why)
}

// cancelPrint starts cancelling a running print. The watcher confirms it.
func (s *Server) cancelPrint(why string) error {
	s.print.mu.Lock()
	if job := s.print.job; job.preparing() {
		// A command that's running can't be stopped safely, so what's running finishes
		// the job. M524 only acts on a print, and M410 would lose the position.
		waiting, already := job.State == PrintAdjusting && s.print.leg == 0, job.Stopping
		job.Stopping = true
		if !already {
			s.print.reason = why
			s.savePrintJob()
			log.Printf("print %d: cancelling, %s", job.ID, why)
		}
		s.wakeWatcher()
		s.print.mu.Unlock()
		if waiting && !already {
			s.endPrep(PrintCancelled, why)
		}
		return nil
	}
	s.print.mu.Unlock()
	if !s.Printer().Connected {
		return marlin.ErrNotConnected
	}
	s.print.mu.Lock()
	job := s.print.job
	if !job.running() {
		s.print.mu.Unlock()
		return errors.New("no print is running")
	}
	if job.State == PrintRunning {
		job.State = PrintCancelling
		s.print.reason = why
		s.savePrintJob()
		log.Printf("print %d: cancelling, %s", job.ID, why)
	}
	// It would wait for a reply that clearing the queue can lose.
	if s.print.stopPoll != nil {
		s.print.stopPoll()
	}
	// The watcher stops a stream once it has stopped sending.
	stream := job.Mode == ModeStream
	if stream && s.print.stopStream != nil {
		s.print.stopStream()
	}
	s.wakeWatcher()
	s.print.mu.Unlock()
	if stream {
		return nil
	}
	return s.Prints.CancelSD()
}

// finishCancel waits for the firmware to confirm, repeating M524 in case it was
// damaged on the way, then checks the heaters and that the head has stopped.
func (s *Server) finishCancel(ctx context.Context) {
	deadline := time.Now().Add(cancelLimit)
	lastSent := time.Now()
	for {
		poll, stop := context.WithTimeout(ctx, cancelRetry)
		st, err := s.Prints.SDStatus(poll)
		stop()
		if ctx.Err() != nil {
			return
		}
		if err == nil && !st.Printing {
			break
		}
		s.print.mu.Lock()
		halted := s.print.halted
		s.print.mu.Unlock()
		switch {
		case halted != "":
			s.endPrint(PrintFailed, "the firmware halted: "+halted)
			return
		case time.Now().After(deadline):
			s.endPrint(PrintFailed, "the printer hasn't confirmed the cancel, tramline estop stops it")
			return
		}
		if !s.Printer().Connected {
			if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
				s.endPrint(PrintCancelled, "the plug is off")
				return
			}
		}
		if time.Since(lastSent) >= cancelRetry {
			lastSent = time.Now()
			if err := s.Prints.CancelSD(); err != nil {
				log.Printf("print: repeating the cancel: %v", err)
			}
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
	// The firmware then homes X and Y, so the head may still be moving.
	if err := s.Prints.FinishMoves(ctx); err != nil {
		log.Printf("print: waiting for the head after a cancel: %v", err)
	}
	s.print.mu.Lock()
	why := s.print.reason
	s.print.mu.Unlock()
	if t := s.Printer().Temps; s.Heaters != nil && (t == nil || t.NozzleTarget > 0 || t.BedTarget > 0) {
		s.heat.op.Lock()
		err := s.Heaters.SetTargets(ctx, 0, 0)
		s.heat.op.Unlock()
		if err != nil {
			s.endPrint(PrintFailed, fmt.Sprintf("cancelled, but the heaters may be on: %v", err))
			return
		}
	}
	s.endPrint(PrintCancelled, why)
}

// handleCancel asks the firmware to stop, and the head goes home. Status follows it.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !s.printRunning() {
		writeError(w, http.StatusConflict, "no print is running")
		return
	}
	if err := s.cancelPrint("cancelled on request"); err != nil {
		writePrinterError(w, fmt.Errorf("can't reach the printer to cancel, tramline estop cuts its power: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
}

// printStartedElsewhere asks at the next check whether a print should be followed.
func (s *Server) printStartedElsewhere() {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	s.print.linkChecked = false
}

// checkAdopt asks the printer about a print once per connection, so one that outlived
// a server restart, or was started from the LCD, gets followed like any other. The
// answer waits behind whatever the print has queued, 16 seconds in one real test and
// far longer behind a heater, so it's a goroutine: giving up and asking again would
// only queue more. It returns true while a print is running or may be, when the
// heaters are the watcher's job.
func (s *Server) checkAdopt(ctx context.Context) bool {
	s.print.mu.Lock()
	job := s.print.job
	running, watching, checked, adopting := job.running(), s.print.watching, s.print.linkChecked, s.print.adopting
	s.print.mu.Unlock()

	if !s.Printer().Connected {
		s.print.mu.Lock()
		s.print.linkChecked = false
		s.print.mu.Unlock()
		if running && !watching {
			if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
				s.endPrint(PrintUnknown, "the plug is off, so it ended while the server was down")
				return false
			}
		}
		return running
	}
	if adopting {
		return true
	}
	s.print.mu.Lock()
	starting := s.print.leg != 0 && job != nil && job.State == PrintStarting
	s.print.mu.Unlock()
	if checked || (running && watching) || starting || s.tramRunning() || s.copyRunning() {
		return running
	}
	s.print.mu.Lock()
	s.print.adopting = true
	s.print.mu.Unlock()
	go func() {
		defer func() {
			s.print.mu.Lock()
			defer s.print.mu.Unlock()
			s.print.adopting = false
		}()
		s.adoptPrint(ctx)
	}()
	return true
}

// adoptPrint asks whether a print is running and starts following it, or closes a job
// that has ended. It returns true if one is running. It doesn't take the operation
// lock, as this can wait for minutes and cool mustn't have to.
func (s *Server) adoptPrint(ctx context.Context) bool {
	s.print.mu.Lock()
	job := s.print.job
	running, watching := job.running(), s.print.watching
	// A job with nothing going on may have started its file before a restart.
	ours := running || (job.preparing() && s.print.leg == 0)
	s.print.mu.Unlock()

	poll, stop := context.WithTimeout(ctx, printPollLimit)
	defer stop()
	asked := time.Now()
	st, err := s.Prints.SDStatus(poll)
	log.Printf("print: M27 asking whether one is running took %s", time.Since(asked).Round(time.Millisecond))
	var name string
	if err == nil && st.Printing {
		name, err = s.Prints.SDFileName(poll)
	}
	if err != nil {
		log.Printf("print: couldn't ask whether one is running: %v", err)
		return running
	}

	s.print.mu.Lock()
	s.print.linkChecked = true
	reason := s.print.reason
	s.print.mu.Unlock()
	switch {
	case !st.Printing && running && !watching && job.State == PrintCancelling:
		s.endPrint(PrintCancelled, reason)
		return false
	case !st.Printing && running && !watching:
		s.endPrint(PrintUnknown, "it ended while the server was down, so whether it finished isn't known")
		return false
	case !st.Printing:
		return running
	case running && watching:
		return true
	}
	now := s.now()
	if ours && strings.EqualFold(job.SDName, name) {
		s.print.mu.Lock()
		job.State, job.Parked, job.Pos, job.MovedAt = PrintRunning, false, st.Pos, now
		job.Note = "picked up after a restart of the server"
		s.print.sawDone, s.print.halted = false, ""
		s.savePrintJob()
		s.startWatcher()
		s.print.mu.Unlock()
		log.Printf("print %d: still running, at %d of %d bytes", job.ID, st.Pos, st.Size)
	} else {
		s.closeJob(PrintUnknown, "another print is running now")
		adopted := s.beginJob(&PrintJob{
			Name: name, SDName: name, Mode: ModeSD, State: PrintRunning, Elsewhere: true,
			Pos: st.Pos, Size: st.Size, StartedAt: now, MovedAt: now,
		})
		log.Printf("print %d: %s is running, not started from here, at %d of %d bytes", adopted.ID, name, st.Pos, st.Size)
	}
	s.setPrinting(true)
	s.setOffAt(now.Add(HeatIdleLimit))
	return true
}

// EstopResponse says what an emergency stop did.
type EstopResponse struct {
	// M112 means it was sent to the printer.
	M112 bool `json:"m112"`
	// PlugOff means the plug has been switched off and read back as off.
	PlugOff bool `json:"plug_off"`
	// Problems says what couldn't be done.
	Problems []string `json:"problems,omitempty"`
}

// handleEstop takes no lock, so nothing can hold it up. The firmware ignores M112
// during a save, so a copy relies on the plug, which is tried in every case without
// waiting for M112. The hotend fan stops too, which is accepted in an emergency.
func (s *Server) handleEstop(w http.ResponseWriter, r *http.Request) {
	log.Print("EMERGENCY STOP requested")
	copying := s.copyRunning()
	s.abandonPrint("emergency stop")

	var out EstopResponse
	m112 := make(chan error, 1)
	if s.Prints != nil && s.Printer().Connected && !copying {
		out.M112 = true
		go func() { m112 <- s.Prints.EmergencyStop() }()
		// A moment for it to get out before the power goes, but a stuck port mustn't
		// hold up the plug.
		select {
		case err := <-m112:
			m112 <- err
		case <-time.After(estopHeadStart):
		}
	} else {
		m112 <- nil
	}

	if s.Plug == nil {
		out.Problems = append(out.Problems, "no plug is configured")
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), plugTimeout)
		if err := s.Plug.Set(ctx, false); err != nil {
			out.Problems = append(out.Problems, "the plug couldn't be cut: "+err.Error())
		} else {
			out.PlugOff = true
		}
		cancel()
	}

	if copying {
		s.stopCopy("emergency stop")
		if !out.PlugOff {
			// All that's left is to end the save, then M112 will work.
			if err := s.estopAfterCopy(); err != nil {
				out.Problems = append(out.Problems, err.Error())
			} else {
				out.M112 = true
			}
		}
	}
	select {
	case err := <-m112:
		if err != nil {
			out.M112 = false
			out.Problems = append(out.Problems, "M112 couldn't be sent: "+err.Error())
		}
	case <-time.After(estopTimeout):
		out.M112 = false
		out.Problems = append(out.Problems, "M112 is stuck on its way to the printer")
	}
	log.Printf("emergency stop: M112 sent %t, plug off %t, problems: %q", out.M112, out.PlugOff, out.Problems)
	writeJSON(w, http.StatusOK, out)
}

// estopAfterCopy waits for a stopped copy to end its save.
func (s *Server) estopAfterCopy() error {
	deadline := time.Now().Add(estopCopyWait)
	for !s.heat.op.TryLock() {
		if time.Now().After(deadline) {
			return errors.New("the copy didn't stop in time for M112")
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.heat.op.Unlock()
	if s.Prints == nil {
		return errors.New("there's no printer")
	}
	if err := s.Prints.EmergencyStop(); err != nil {
		return fmt.Errorf("M112 couldn't be sent: %w", err)
	}
	return nil
}

// abandonPrint marks a print as over without asking the printer. The heater state
// follows from the firmware halting or the plug being off.
func (s *Server) abandonPrint(why string) {
	s.print.mu.Lock()
	running := s.print.job.active()
	if running && s.print.stopPoll != nil {
		s.print.stopPoll()
	}
	if running && s.print.stopStream != nil {
		s.print.stopStream()
	}
	s.print.mu.Unlock()
	if running {
		s.closeJob(PrintFailed, why)
		s.setOffAt(time.Time{})
		// The operation lock may be held by whatever the emergency is about.
		go s.setPrinting(false)
	}
}
