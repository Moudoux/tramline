package marlin

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// printSilence is how long a board in a print may send nothing at all. It reports
	// temperatures every 2 seconds, even while waiting for a heater.
	printSilence   = 10 * time.Second
	printStartIdle = 5 * time.Second
)

// ErrStartUnknown means M24 was sent and its reply didn't arrive, so the print may
// or may not be running.
var ErrStartUnknown = errors.New("no reply to M24, the print may have started")

var (
	sdPrintingRE = regexp.MustCompile(`SD printing byte (\d+)/(\d+)`)
	sdOpenedRE   = regexp.MustCompile(`File opened: (\S+) Size: (\d+)`)
	sdFileRE     = regexp.MustCompile(`Current file: (\S+)`)
)

// SDStatus is what M27 reports. A file that's open and paused, or not yet started,
// also counts as printing, so only a change in Pos shows progress.
type SDStatus struct {
	Printing  bool
	Pos, Size int64
}

// isPrintStatus says whether a line is M27's own answer.
func isPrintStatus(line string) bool {
	return strings.Contains(line, "Not SD printing") || sdPrintingRE.MatchString(line) || sdFileRE.MatchString(line)
}

func parseSDStatus(lines []string) (SDStatus, error) {
	for _, line := range lines {
		if strings.Contains(line, "Not SD printing") {
			return SDStatus{}, nil
		}
		if m := sdPrintingRE.FindStringSubmatch(line); m != nil {
			pos, posErr := strconv.ParseInt(m[1], 10, 64)
			size, sizeErr := strconv.ParseInt(m[2], 10, 64)
			if posErr == nil && sizeErr == nil {
				return SDStatus{Printing: true, Pos: pos, Size: size}, nil
			}
		}
	}
	return SDStatus{}, fmt.Errorf("unrecognised reply to M27: %q", lines)
}

// SDStatus asks where a print has got to. The reply waits behind the print's queued
// commands, and through a whole heater wait, so ctx sets how long that may take.
func (p *Printer) SDStatus(ctx context.Context) (SDStatus, error) {
	link, err := p.currentLink()
	if err != nil {
		return SDStatus{}, err
	}
	lines, err := link.exchangePrint(ctx, "M27", printSilence)
	if err != nil {
		return SDStatus{}, err
	}
	return parseSDStatus(lines)
}

// SDFileName returns the name of the file open for printing, or "" if there's none.
func (p *Printer) SDFileName(ctx context.Context) (string, error) {
	link, err := p.currentLink()
	if err != nil {
		return "", err
	}
	lines, err := link.exchangePrint(ctx, "M27 C", printSilence)
	if err != nil {
		return "", err
	}
	for _, line := range lines {
		if m := sdFileRE.FindStringSubmatch(line); m != nil && m[1] != "(no" {
			return m[1], nil
		}
	}
	return "", nil
}

// StartSD starts printing a file on the card, whose size must be as given. M108
// closes the print-done screen of an earlier print first, as that otherwise leaves
// the firmware's own idle timeout for the hotend turned off during this print.
func (p *Printer) StartSD(ctx context.Context, name string, size int64) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	if _, err := link.Exchange(ctx, "M108", replyIdle); err != nil {
		return fmt.Errorf("M108: %w", err)
	}
	lines, err := link.Exchange(ctx, "M23 "+name, printStartIdle)
	if err != nil {
		return fmt.Errorf("M23: %w", err)
	}
	opened := false
	for _, line := range lines {
		m := sdOpenedRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		got, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || got != size {
			return fmt.Errorf("the printer opened %s at %s bytes, it should be %d", m[1], m[2], size)
		}
		opened = true
	}
	if !opened {
		return fmt.Errorf("the printer didn't open %s, it said %q", name, lines)
	}
	if _, err := link.Exchange(ctx, "M24", printStartIdle); err != nil {
		return fmt.Errorf("%w: %v", ErrStartUnknown, err)
	}
	return nil
}

// LCD shows a message on the status line, which stays until something replaces it.
// An empty one puts the line back to normal. It doesn't change the print screen's
// title, which always says "Printing".
func (p *Printer) LCD(ctx context.Context, text string) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	_, err = link.Exchange(ctx, strings.TrimSpace("M117 "+text), replyIdle)
	return err
}

// CloseDoneScreen closes the print-done screen of an earlier print. Without one up,
// M108 does nothing.
func (p *Printer) CloseDoneScreen(ctx context.Context) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	if _, err := link.Exchange(ctx, "M108", replyIdle); err != nil {
		return fmt.Errorf("M108: %w", err)
	}
	return nil
}

// CancelSD asks the firmware to abort a print. It acts on this straight away, ending
// any heater wait, then clears its queue, turns the heaters off and homes X and Y.
// Clearing the queue can lose the replies to this and to what was waiting, so
// confirm with SDStatus.
func (p *Printer) CancelSD() error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	return link.Urgent("M524")
}

// EmergencyStop sends M112, which the firmware acts on as it arrives: heaters and
// steppers off, then nothing until its power is cycled. It's ignored during a save.
func (p *Printer) EmergencyStop() error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	return link.Urgent("M112")
}

// FinishMoves returns once the printer has finished its moves.
func (p *Printer) FinishMoves(ctx context.Context) error {
	_, err := p.move(ctx, "M400", p.Motion.Home)
	return err
}
