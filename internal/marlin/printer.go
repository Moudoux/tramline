package marlin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	retryDelay   = time.Second
	handshakeFor = 30 * time.Second
	settleDelay  = 500 * time.Millisecond
	replyIdle    = 3 * time.Second
	// A freshly powered board stays silent for about 6 seconds, then answers.
	bootIdle = 10 * time.Second
	// How long one M115 may wait for a board that's alive but busy, such as during a
	// print waiting for its heaters, before it's sent again.
	handshakeRetry = time.Minute
	// How recently temperatures must have arrived to keep trying to connect.
	aliveWithin = 15 * time.Second
	// How long the first numbered command may take, as it can wait behind a move.
	startLinesIdle = 10 * time.Second
	// How far a reported target may be from the one that was set.
	targetTolerance = 0.5
)

// ErrNotConnected means no command could be sent, because the link is down.
var ErrNotConnected = errors.New("the printer isn't connected")

// Status is a snapshot of the printer's connection and temperatures.
type Status struct {
	Connected bool       `json:"connected"`
	Firmware  string     `json:"firmware,omitempty"`
	Temps     *Temps     `json:"temps,omitempty"`
	TempsAt   *time.Time `json:"temps_at,omitempty"`
}

// Printer keeps a connection open whenever the printer is on. The USB device
// only exists while the printer has power, so it comes and goes as normal.
type Printer struct {
	open func() (io.ReadWriteCloser, error)
	// Motion is how long moves may take. Set it before Run.
	Motion MotionLimits
	// SDLineIdle is how long a line sent to the card may take. Set it before Run.
	SDLineIdle time.Duration
	// StreamNudge is how long a stream waits for an "ok" before sending the next line
	// anyway. Zero means DefaultStreamNudge. Set it before Run.
	StreamNudge time.Duration
	// OnLine is told when a print finishes, the LCD's stop is pressed, the firmware
	// halts or G29 starts a point of the mesh, whether or not a command is waiting. It
	// runs on the read loop, so it must return quickly and not use the printer. Set it
	// before Run.
	OnLine func(string)

	mu     sync.Mutex
	status Status
	link   *Link
}

func NewPrinter(open func() (io.ReadWriteCloser, error)) *Printer {
	return &Printer{open: open, Motion: DefaultMotionLimits, SDLineIdle: DefaultSDLineIdle}
}

func (p *Printer) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Run reconnects until ctx ends.
func (p *Printer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := p.open()
		if err == nil {
			p.session(ctx, conn)
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryDelay):
		}
	}
}

func (p *Printer) session(ctx context.Context, conn io.ReadWriteCloser) {
	link := newLink(conn, p.setTemps, p.OnLine, p.SDLineIdle)
	stop := context.AfterFunc(ctx, func() { link.Close() })
	defer stop()
	defer link.Close()
	defer p.setDisconnected()

	firmware, ok := handshake(ctx, link)
	if !ok {
		return
	}
	link.sendMu.Lock()
	link.wantNumbers = true
	link.sendMu.Unlock()
	p.mu.Lock()
	p.status.Connected = true
	p.status.Firmware = firmware
	p.link = link
	p.mu.Unlock()
	log.Printf("printer connected: %s", firmware)
	defer log.Print("printer disconnected")

	if _, err := link.Exchange(ctx, "M155 S2", startLinesIdle); err != nil {
		log.Printf("couldn't start temperature reports: %v", err)
		return
	}
	<-link.Done()
}

// handshake waits for the firmware to answer M115. A freshly powered board is
// still starting up, and answers what it was sent earlier first, such as garbage
// from opening the port, so only an "ok" after the firmware line ends the wait.
func handshake(ctx context.Context, link *Link) (string, bool) {
	deadline := time.Now().Add(handshakeFor)
	// A board sending temperatures is alive, and may be in a print waiting for its
	// heaters, which it answers nothing during. Giving up would gain nothing.
	for (time.Now().Before(deadline) || link.tempsWithin(aliveWithin)) && ctx.Err() == nil {
		if link.Flush() != nil {
			return "", false
		}
		select {
		case <-time.After(settleDelay):
		case <-link.Done():
			return "", false
		case <-ctx.Done():
			return "", false
		}
		attempt, cancel := context.WithTimeout(ctx, handshakeRetry)
		lines, err := link.exchangeAlive(attempt, "M115", bootIdle, func(lines []string) bool {
			// The rest of M115's reply shows its first line was damaged, as can happen just
			// after opening the port, so it can be asked again without waiting.
			return hasFirmware(lines) || savingToCard(lines) || anyContains(lines, "Cap:")
		})
		cancel()
		if err != nil {
			select {
			case <-link.Done():
				return "", false
			default:
				continue
			}
		}
		// The link dropped during a transfer while the board kept its power, so it's
		// still saving and refuses everything else until told to stop.
		if savingToCard(lines) {
			log.Print("the board is still saving to its card, ending that")
			if _, err := link.exchange(ctx, "M29", replyIdle, func([]string) bool { return true }); err != nil {
				log.Printf("couldn't end the save: %v", err)
			}
			continue
		}
		for _, line := range lines {
			if name, ok := parseFirmware(line); ok {
				return name, true
			}
		}
	}
	return "", false
}

func hasFirmware(lines []string) bool {
	return slices.ContainsFunc(lines, func(line string) bool {
		_, ok := parseFirmware(line)
		return ok
	})
}

func parseFirmware(line string) (string, bool) {
	const prefix = "FIRMWARE_NAME:"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(line, prefix)
	if i := strings.Index(name, " SOURCE_CODE_URL:"); i >= 0 {
		name = name[:i]
	}
	return name, true
}

func (p *Printer) currentLink() (*Link, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.link == nil {
		return nil, ErrNotConnected
	}
	return p.link, nil
}

// SetTargets sets both heaters and returns without waiting for them, once the
// printer has reported the new targets back. If that fails while heating, it
// tries to turn both off, so a partial failure doesn't leave one heater on.
func (p *Printer) SetTargets(ctx context.Context, bed, nozzle int) error {
	err := p.setTargets(ctx, bed, nozzle)
	if err != nil && (bed > 0 || nozzle > 0) && !errors.Is(err, ErrNotConnected) {
		if off := p.setTargets(context.WithoutCancel(ctx), 0, 0); off != nil {
			log.Printf("couldn't turn the heaters off after a failed heat command: %v", off)
		}
	}
	return err
}

func (p *Printer) setTargets(ctx context.Context, bed, nozzle int) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	// Both are always sent, because turning one off mustn't depend on the other.
	_, err = link.Exchange(ctx, fmt.Sprintf("M140 S%d", bed), replyIdle)
	if _, nozzleErr := link.Exchange(ctx, fmt.Sprintf("M104 S%d", nozzle), replyIdle); err == nil {
		err = nozzleErr
	}
	if err != nil {
		return err
	}
	lines, err := link.Exchange(ctx, "M105", replyIdle)
	if err != nil {
		return err
	}
	for _, line := range lines {
		t, ok := parseTemps(line)
		if !ok {
			continue
		}
		if math.Abs(t.BedTarget-float64(bed)) > targetTolerance || math.Abs(t.NozzleTarget-float64(nozzle)) > targetTolerance {
			return fmt.Errorf("asked for bed %d C and nozzle %d C, but the printer reports %.0f C and %.0f C", bed, nozzle, t.BedTarget, t.NozzleTarget)
		}
		return nil
	}
	return fmt.Errorf("no temperatures in the reply to M105: %q", lines)
}

// SDPrinting asks the firmware whether an SD print is running, which also finds
// prints started from the LCD. An unrecognised reply is an error, not a "no".
func (p *Printer) SDPrinting(ctx context.Context) (bool, error) {
	link, err := p.currentLink()
	if err != nil {
		return false, err
	}
	lines, err := link.Exchange(ctx, "M27", replyIdle)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		switch {
		case strings.Contains(line, "Not SD printing"):
			return false, nil
		case strings.Contains(line, "SD printing byte"):
			return true, nil
		}
	}
	return false, fmt.Errorf("unrecognised reply to M27: %q", lines)
}

func (p *Printer) setTemps(t Temps) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status.Temps = &t
	p.status.TempsAt = &now
}

func (p *Printer) setDisconnected() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = Status{}
	p.link = nil
}
