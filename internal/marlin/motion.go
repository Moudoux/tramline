package marlin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"tramline/internal/tram"
)

// MotionLimits are how long commands that move the printer may take. Moves are
// slow and the firmware runs them in order, so nothing else gets a turn until
// they're done: a limit is also the longest the heater checks can be held up.
type MotionLimits struct {
	// Idle is how long the printer may say nothing. The firmware should say
	// "busy" every 2 seconds, but this hasn't been seen on the real printer yet.
	Idle time.Duration
	// Home, Probe and Park each cap one command, even if it keeps saying "busy".
	Home, Probe, Park time.Duration
}

// DefaultMotionLimits are generous on purpose until there's real timing data.
// At the firmware's default speeds, homing Z from the top takes about 25 seconds.
var DefaultMotionLimits = MotionLimits{
	Idle:  60 * time.Second,
	Home:  3 * time.Minute,
	Probe: 90 * time.Second,
	Park:  time.Minute,
}

const (
	// How far a reported probe position may be from the one asked for.
	probeTolerance = 0.1
	infoIdle       = 10 * time.Second
	parkZ          = 30
	parkXY         = 115
	parkZFeed      = 600
	parkXYFeed     = 3000
)

// move sends one command that can take a while, and fails on any "Error:" line.
func (p *Printer) move(ctx context.Context, cmd string, limit time.Duration) ([]string, error) {
	link, err := p.currentLink()
	if err != nil {
		return nil, err
	}
	capped, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	start := time.Now()
	lines, silence, err := link.timed(capped, cmd, p.Motion.Idle, nil)
	log.Printf("%s: took %s, longest silence %s", cmd, time.Since(start).Round(10*time.Millisecond), silence.Round(10*time.Millisecond))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s didn't finish within %s", cmd, limit)
		}
		return nil, fmt.Errorf("%s: %w", cmd, err)
	}
	reply := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "Error:") {
			return nil, fmt.Errorf("%s: printer said %q", cmd, line)
		}
		if !strings.HasPrefix(line, "echo:busy:") {
			reply = append(reply, line)
		}
	}
	return reply, nil
}

// Home homes all axes, and returns once they've finished.
func (p *Printer) Home(ctx context.Context) error {
	_, err := p.move(ctx, "G28", p.Motion.Home)
	return err
}

// Probe takes one reading with G30, and checks it came from the point asked for.
func (p *Printer) Probe(ctx context.Context, x, y int) (tram.Reading, error) {
	cmd := fmt.Sprintf("G30 X%d Y%d", x, y)
	lines, err := p.move(ctx, cmd, p.Motion.Probe)
	if err != nil {
		return tram.Reading{}, err
	}
	r, err := tram.ParseProbe(lines)
	if err != nil {
		return tram.Reading{}, fmt.Errorf("%s: %w", cmd, err)
	}
	if math.Abs(r.X-float64(x)) > probeTolerance || math.Abs(r.Y-float64(y)) > probeTolerance {
		return tram.Reading{}, fmt.Errorf("%s: the printer reports probing X%.2f Y%.2f", cmd, r.X, r.Y)
	}
	return r, nil
}

// Park raises the head and moves it to the middle, clear of the knobs, and
// returns only once it has stopped, because someone may then reach under it.
func (p *Printer) Park(ctx context.Context) error {
	for _, cmd := range []string{
		"G90",
		fmt.Sprintf("G0 Z%d F%d", parkZ, parkZFeed),
		fmt.Sprintf("G0 X%d Y%d F%d", parkXY, parkXY, parkXYFeed),
		"M400",
	} {
		if _, err := p.move(ctx, cmd, p.Motion.Park); err != nil {
			return err
		}
	}
	return nil
}

// TramInfo asks for what's needed to work out the probe points. None of these
// commands move anything. M118 is here to show that resyncing works on this board.
func (p *Printer) TramInfo(ctx context.Context) ([]string, error) {
	link, err := p.currentLink()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, cmd := range []string{"M118 tramline-check", "M114", "M851", "M503", "G29 W", "M413", "M86"} {
		lines, err := link.Exchange(ctx, cmd, infoIdle)
		if err != nil {
			return out, fmt.Errorf("%s: %w", cmd, err)
		}
		out = append(out, "> "+cmd)
		out = append(out, lines...)
	}
	return out, nil
}
