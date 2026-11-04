package marlin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"time"
)

const (
	// DefaultStreamNudge is how long a stream waits for an "ok" before sending the next
	// line anyway.
	DefaultStreamNudge = 5 * time.Second
	// The free moves the real board reports with its planner empty.
	plannerEmpty = 15
	// How many lines may be unanswered while nudging.
	streamMaxWaiting = 4
	// StreamKillSeconds is M85's limit during a stream: with no moves queued for this
	// long, such as when the Pi hangs, the firmware kills the printer. It also means an
	// LCD pause of over 5 minutes kills the print.
	StreamKillSeconds = 300
	// What the firmware's hotend idle timeout was read as on the real printer.
	expectedM86 = "S600 T170"
	// How often the urgent commands are repeated while a stop waits for the queue, in
	// case one was damaged, or a second heater wait was waiting behind the first.
	streamStopRepeat = 5 * time.Second
)

// okTimesEvery is how often a stream logs how long each "ok" took.
const okTimesEvery = time.Minute

var plannerFreeRE = regexp.MustCompile(` P(\d+)`)

// okTimes adds up how long lines waited for their "ok", and how full the planner was
// by the firmware's count of free moves, as an empty one during moves means it ran out.
type okTimes struct {
	lines                             int
	sum, longest                      time.Duration
	longestLine                       int
	under50ms, under500ms, under5s    int
	over5s                            int
	plannerReports, plannerFree, idle int
}

func (o *okTimes) add(took time.Duration, n int, reply string) {
	o.lines++
	o.sum += took
	if took > o.longest {
		o.longest, o.longestLine = took, n
	}
	switch {
	case took < 50*time.Millisecond:
		o.under50ms++
	case took < 500*time.Millisecond:
		o.under500ms++
	case took < 5*time.Second:
		o.under5s++
	default:
		o.over5s++
	}
	if m := plannerFreeRE.FindStringSubmatch(reply); m != nil {
		free, _ := strconv.Atoi(m[1])
		o.plannerReports++
		o.plannerFree += free
		if free >= plannerEmpty {
			o.idle++
		}
	}
}

func (o *okTimes) log(what string) {
	if o.lines == 0 {
		return
	}
	var free float64
	if o.plannerReports > 0 {
		free = float64(o.plannerFree) / float64(o.plannerReports)
	}
	log.Printf("stream: ok times, %s: %d lines, mean %s, longest %s at line %d, under 50 ms %d, under 500 ms %d, under 5 s %d, over %d, planner free %.1f on average, empty at %d",
		what, o.lines, (o.sum / time.Duration(o.lines)).Round(100*time.Microsecond), o.longest.Round(time.Millisecond), o.longestLine,
		o.under50ms, o.under500ms, o.under5s, o.over5s, free, o.idle)
}

// StartStream gets the firmware ready to take a file by streaming. M108 closes the
// print-done screen, which would leave the hotend idle timeout turned off, and that's
// read and logged, as it's what looks after the heaters if M85 somehow doesn't.
func (p *Printer) StartStream(ctx context.Context) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	if _, err := link.Exchange(ctx, "M108", replyIdle); err != nil {
		return fmt.Errorf("M108: %w", err)
	}
	lines, err := link.Exchange(ctx, "M86", replyIdle)
	switch {
	case err != nil:
		return fmt.Errorf("M86: %w", err)
	case anyContains(lines, expectedM86):
		log.Printf("stream: hotend idle timeout as expected: %q", lines)
	default:
		log.Printf("stream: hotend idle timeout isn't the expected %s: %q", expectedM86, lines)
	}
	if _, err := link.Exchange(ctx, fmt.Sprintf("M85 S%d", StreamKillSeconds), replyIdle); err != nil {
		// It may have been taken, even with the reply missing.
		if _, clearErr := link.Exchange(context.WithoutCancel(ctx), "M85 S0", replyIdle); clearErr != nil {
			log.Printf("stream: couldn't clear M85: %v", clearErr)
		}
		return fmt.Errorf("M85: %w", err)
	}
	return nil
}

// StreamFile sends a file's lines one at a time, then M400 and an M118 of marker, and
// returns once the marker comes back, or when ctx ends. It holds the link throughout,
// so nothing but Urgent can be sent meanwhile.
func (p *Printer) StreamFile(ctx context.Context, marker string, next func() (string, error), progress func(SDProgress)) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	link.sendMu.Lock()
	defer link.sendMu.Unlock()
	switch {
	case link.saving:
		return errors.New("a save on the card is still open")
	case !link.isNumbering():
		if err := link.startLines(ctx); err != nil {
			return err
		}
	case link.stale:
		if err := link.resync(ctx); err != nil {
			return fmt.Errorf("the printer is still busy with an earlier command: %w", err)
		}
	}
	nudge := p.StreamNudge
	if nudge <= 0 {
		nudge = DefaultStreamNudge
	}
	return link.sendLines(ctx, sendPlan{
		window: DefaultSDWindow,
		// A board asking for anything earlier has restarted, or lost its place.
		first:  link.lineCount(),
		ending: []string{"M400", "M118 " + marker},
		nudge:  nudge,
		marker: marker,
	}, next, progress)
}

// stopNow sends the commands the firmware acts on as they arrive: M108 ends a heater
// wait, and M410 drops the planned moves. Whatever was already queued still runs, which
// one line at a time keeps to a few lines.
func stopNow(link *Link) error {
	return errors.Join(link.Urgent("M108"), link.Urgent("M410"))
}

// StopStream stops a stream that has stopped sending, and leaves the printer safe: no
// heat, no fan, the head 5 mm up and the steppers off, with no homing, so it doesn't
// cross the part. M524 only acts on the card, so it isn't used. ctx limits the whole
// wait, as a queued G29 can take minutes to end. M85 is cleared, so a cancelled print
// doesn't kill the printer later.
func (p *Printer) StopStream(ctx context.Context) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	if err := stopNow(link); err != nil {
		return err
	}
	// The firmware may reject those numbers, if a line was lost before the stop. Lines
	// from the file mustn't go out again to answer that, so the count starts again, which
	// also forgets them.
	link.setNumbering(false, 0)

	first := make(chan struct{})
	repeated := make(chan struct{})
	go func() {
		defer close(repeated)
		tick := time.NewTicker(p.stopRepeat())
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if err := stopNow(link); err != nil {
					return
				}
			case <-first:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var errs []error
	for i, cmd := range []string{"M104 S0", "M140 S0", "M107", "G91", "G0 Z5 F600", "G90", "M85 S0", "M84"} {
		_, err := link.exchangeAlive(ctx, cmd, printSilence, nil)
		if i == 0 {
			close(first)
			<-repeated
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cmd, err))
			if errors.Is(err, ErrClosed) || errors.Is(err, ErrHalted) {
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (p *Printer) stopRepeat() time.Duration {
	if p.StreamNudge > 0 && p.StreamNudge < streamStopRepeat {
		return p.StreamNudge
	}
	return streamStopRepeat
}

// ClearKill turns M85 off at the end of a stream.
func (p *Printer) ClearKill(ctx context.Context) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	_, err = link.Exchange(ctx, "M85 S0", replyIdle)
	return err
}

// EndLostStream turns the heaters and M85 off after a stream was cut off, at the next
// connection. It leaves the head where it is, as what happened to it isn't known.
func (p *Printer) EndLostStream(ctx context.Context) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	if err := link.Urgent("M108"); err != nil {
		return err
	}
	return errors.Join(p.setTargets(ctx, 0, 0), p.ClearKill(ctx))
}

// IsLinkLoss reports whether an error means the printer can't be reached.
func IsLinkLoss(err error) bool {
	return errors.Is(err, ErrClosed) || errors.Is(err, ErrNotConnected)
}
