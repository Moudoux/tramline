package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
)

const (
	// How often a stream checks whether it has stopped moving on.
	streamCheckEvery = 5 * time.Second
	// How long stopping may take, as it waits behind what's already queued, which could
	// be the rest of a G29.
	streamStopLimit = 5 * time.Minute
	// How often temperatures are logged during a print.
	tempLogEvery = time.Minute
)

// watchStream sends a file line by line and sees the job to its end. Unlike an SD print
// there's nothing to ask the printer: an answered line is what shows it's moving on.
func (s *Server) watchStream(ctx context.Context, id int, name string, wake <-chan struct{}) {
	defer func() {
		s.print.mu.Lock()
		defer s.print.mu.Unlock()
		s.print.watching = false
		if s.print.stopStream != nil {
			s.print.stopStream()
			s.print.stopStream = nil
		}
	}()
	done := make(chan error, 1)
	go func() { done <- s.sendStream(ctx, id, name) }()

	s.print.mu.Lock()
	every := s.print.every
	s.print.mu.Unlock()
	if every == 0 {
		every = streamCheckEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			s.streamEnded(id, err)
			return
		case <-wake:
		case <-tick.C:
		}
		s.print.mu.Lock()
		job, halted, stop := s.print.job, s.print.halted, s.print.stopStream
		stuck := job != nil && job.ID == id && job.State == PrintRunning && s.now().Sub(job.MovedAt) >= HeatIdleLimit
		s.print.mu.Unlock()
		switch {
		case halted != "":
			stop()
		case stuck:
			if err := s.cancelPrint(fmt.Sprintf("it hasn't moved on for %s", HeatIdleLimit)); err != nil {
				log.Printf("print %d: couldn't cancel a stuck stream: %v", id, err)
			}
		}
	}
}

func (s *Server) sendStream(ctx context.Context, id int, name string) error {
	dir, err := s.uploadsDir()
	if err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	s.print.mu.Lock()
	size := s.print.job.Size
	s.print.mu.Unlock()
	lines := gcode.NewProgress(gcode.NewReader(f), size)
	answered := 0
	return s.Prints.StreamFile(ctx, fmt.Sprintf("tramline-end-%d", id), lines.Next, func(p marlin.SDProgress) {
		now := s.now()
		s.print.mu.Lock()
		job := s.print.job
		moved := p.Answered != answered
		answered = p.Answered
		if job != nil && job.ID == id {
			job.Pos, job.Resends, job.Nudges = lines.Bytes(), p.Resends, p.Timeouts
			if moved {
				job.MovedAt = now
			}
		}
		s.print.mu.Unlock()
		if moved {
			s.setOffAt(now.Add(HeatIdleLimit))
		}
	})
}

// streamEnded decides what the end of sending means. A cancel, or a failure with the
// printer still reachable, needs the printer stopping: a halt doesn't, and an emergency
// stop has already closed the job.
func (s *Server) streamEnded(id int, err error) {
	ctx := s.baseCtx()
	s.print.mu.Lock()
	job := s.print.job
	if job == nil || job.ID != id || !job.running() {
		s.print.mu.Unlock()
		return
	}
	cancelling, why, halted := job.State == PrintCancelling, s.print.reason, s.print.halted
	s.print.mu.Unlock()

	switch {
	// The server is stopping, and the next one reads the saved job.
	case ctx.Err() != nil:
	case halted != "":
		s.endPrint(PrintFailed, "the firmware halted: "+halted)
	case errors.Is(err, marlin.ErrHalted):
		s.endPrint(PrintFailed, err.Error())
	case err == nil && !cancelling:
		s.finishStream(ctx, id)
	case marlin.IsLinkLoss(err) || !s.Printer().Connected:
		s.loseStream(ctx, err)
	case cancelling:
		s.stopStreamJob(ctx, PrintCancelled, why)
	default:
		log.Printf("print %d: stopping the printer, as the stream failed: %v", id, err)
		s.stopStreamJob(ctx, PrintFailed, "the stream failed: "+err.Error())
	}
}

// finishStream is the end of the file. M85 is what's left to clear, as the file's own
// end turned the heaters off, and if the link has just gone that waits for the next one.
func (s *Server) finishStream(ctx context.Context, id int) {
	s.heat.op.Lock()
	err := s.Prints.ClearKill(ctx)
	if err == nil {
		s.showLCD(ctx, "")
	}
	s.heat.op.Unlock()
	if err != nil {
		log.Printf("print %d: couldn't clear M85 at the end: %v", id, err)
		s.setStray()
	}
	s.endPrint(PrintFinished, "")
}

// loseStream ends a job whose printer can't be reached. The heater timer keeps running,
// so the plug cut still applies if the link doesn't come back. M85 should get there
// first, as nothing is being sent.
func (s *Server) loseStream(ctx context.Context, err error) {
	why := fmt.Sprintf("lost the link during a stream, which can't be picked up: %v", err)
	if plug := s.plugStatus(ctx); plug != nil && plug.Error == "" && !plug.On {
		s.endPrint(PrintFailed, "the plug is off, so the stream has stopped")
		return
	}
	s.setStray()
	s.setPrinting(false)
	s.setProblem("")
	s.closeJob(PrintFailed, why)
}

// stopStreamJob stops the printer and closes the job. Status says it's cancelling
// meanwhile, and the file isn't sending, so commands can't interleave with this.
func (s *Server) stopStreamJob(ctx context.Context, state, why string) {
	stop, cancel := context.WithTimeout(ctx, streamStopLimit)
	defer cancel()
	err := s.Prints.StopStream(stop)
	// Whatever that managed, the heaters are the part that matters.
	if t := s.Printer().Temps; s.Printer().Connected && s.Heaters != nil && (t == nil || t.NozzleTarget > 0 || t.BedTarget > 0) {
		s.heat.op.Lock()
		heatErr := s.Heaters.SetTargets(ctx, 0, 0)
		s.heat.op.Unlock()
		err = errors.Join(err, heatErr)
	}
	if err == nil {
		s.heat.op.Lock()
		s.showLCD(ctx, "")
		s.heat.op.Unlock()
		s.endPrint(state, why)
		return
	}
	log.Printf("print: stopping the stream: %v", err)
	if !s.Printer().Connected {
		s.loseStream(ctx, err)
		return
	}
	s.setStray()
	s.setPrinting(false)
	s.closeJob(PrintFailed, fmt.Sprintf("%s, but stopping the printer failed, and the heaters may be on: %v", why, err))
}

func (s *Server) setStray() {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	s.print.strayStream = true
}

func (s *Server) strayPending() bool {
	s.print.mu.Lock()
	defer s.print.mu.Unlock()
	return s.print.strayStream
}

// endStrayStream turns off the heaters and M85 after a stream was cut off, once the
// printer can be reached. Failing to is tried again next time.
func (s *Server) endStrayStream(ctx context.Context) {
	if !s.strayPending() || s.Prints == nil || !s.Printer().Connected {
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if err := s.Prints.EndLostStream(ctx); err != nil {
		log.Printf("print: turning things off after a stream was cut off: %v", err)
		return
	}
	s.print.mu.Lock()
	s.print.strayStream = false
	s.print.mu.Unlock()
	s.heatersOff()
	log.Print("print: heaters and M85 off, after a stream was cut off")
}

// logTemps records temperatures during a job about once a minute. A print is otherwise
// silent about them, and the log is all there is to go on afterwards.
func (s *Server) logTemps() {
	s.print.mu.Lock()
	job := s.print.job
	if !job.active() || time.Since(s.print.tempsLogged) < tempLogEvery {
		s.print.mu.Unlock()
		return
	}
	s.print.tempsLogged = time.Now()
	id, state, pos, size := job.ID, job.State, job.Pos, job.Size
	s.print.mu.Unlock()
	if t := s.Printer().Temps; t != nil {
		log.Printf("print %d: %s, nozzle %.1f/%.0f C, bed %.1f/%.0f C, %d of %d bytes", id, state, t.Nozzle, t.NozzleTarget, t.Bed, t.BedTarget, pos, size)
	}
}
