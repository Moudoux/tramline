// Package marlin speaks Marlin's line protocol over a serial connection.
package marlin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrClosed = errors.New("printer disconnected")

// ErrHalted means the firmware has stopped itself, and won't answer until it's
// powered off and on again.
var ErrHalted = errors.New("the printer has halted")

// ActionCancel is what the firmware tells a host when the LCD's stop is pressed.
const ActionCancel = "//action:cancel"

// ErrNoReply means the printer said nothing for too long.
var ErrNoReply = errors.New("no reply")

// Temps is one temperature report.
type Temps struct {
	Nozzle       float64 `json:"nozzle"`
	NozzleTarget float64 `json:"nozzle_target"`
	Bed          float64 `json:"bed"`
	BedTarget    float64 `json:"bed_target"`
}

var tempsRE = regexp.MustCompile(`T:\s*(-?[\d.]+)\s*/\s*(-?[\d.]+)\s+B:\s*(-?[\d.]+)\s*/\s*(-?[\d.]+)`)

func parseTemps(line string) (Temps, bool) {
	m := tempsRE.FindStringSubmatch(line)
	if m == nil {
		return Temps{}, false
	}
	var v [4]float64
	for i := range v {
		f, err := strconv.ParseFloat(m[i+1], 64)
		if err != nil {
			return Temps{}, false
		}
		v[i] = f
	}
	return Temps{Nozzle: v[0], NozzleTarget: v[1], Bed: v[2], BedTarget: v[3]}, true
}

type waiter struct {
	lines []string
	// accept says whether an "ok" ends this reply. Nil accepts any.
	accept func(lines []string) bool
	// line is the command's number, or -1 if it has none. An "ok" naming an earlier
	// line isn't its reply.
	line int
	// resendWant is the line the firmware asked for, until its "ok" arrives, or -1.
	resendWant int
	// tempsAlive counts temperature reports as signs of life. The firmware sends
	// them, and nothing else, while it waits for a heater.
	tempsAlive bool
	// tell logs what arrives during the wait that nothing would otherwise look at.
	tell     bool
	resend   chan int
	activity chan struct{}
	result   chan []string
	halted   chan string
}

// Link runs one command at a time over a connection. Unsolicited temperature
// reports are passed to onTemps, and never appear in a command's reply.
type Link struct {
	conn    io.ReadWriteCloser
	onTemps func(Temps)
	// onLine is told about lines that matter whether or not a command is waiting. It
	// runs on the read loop, so it mustn't wait for anything that needs the link.
	onLine func(string)

	// writeMu makes each write whole, and is never held while waiting for a reply,
	// so Urgent can always get through.
	writeMu sync.Mutex
	// numbering means commands get a line number and checksum. Guarded by writeMu.
	numbering bool
	nextLine  int
	sent      [sdHistory]string
	lastTemps atomic.Int64

	sendMu sync.Mutex
	// wantNumbers means commands should be numbered, and the count is started again
	// whenever a transfer has stopped it. Guarded by sendMu.
	wantNumbers bool
	tempsAlive  bool
	tellLines   bool
	// stale means a command was given up on, so its reply may still arrive and
	// would be mistaken for the next command's. Guarded by sendMu.
	stale bool
	syncs int
	// saving means the board may be saving to its card, when it refuses every
	// command but M29. Guarded by sendMu.
	saving bool
	sdIdle time.Duration

	mu  sync.Mutex
	cur *waiter
	// tap gets every reply instead of cur, during a transfer to the card, when
	// several lines are unanswered at once.
	tap chan<- string

	done chan struct{}
}

func NewLink(conn io.ReadWriteCloser, onTemps func(Temps)) *Link {
	return newLink(conn, onTemps, nil, DefaultSDLineIdle)
}

func newLink(conn io.ReadWriteCloser, onTemps func(Temps), onLine func(string), sdIdle time.Duration) *Link {
	l := &Link{conn: conn, onTemps: onTemps, onLine: onLine, sdIdle: sdIdle, done: make(chan struct{})}
	go l.readLoop()
	return l
}

// writeLine sends a command, numbered if commands are being numbered, and returns
// its number or -1.
func (l *Link) writeLine(cmd string) (int, error) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	n := -1
	out := cmd
	if l.numbering {
		n = l.nextLine
		l.nextLine++
		l.sent[n%sdHistory] = cmd
		out = numbered(n, cmd)
	}
	_, err := io.WriteString(l.conn, out+"\n")
	return n, err
}

// writeRaw sends text exactly as it is.
func (l *Link) writeRaw(text string) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_, err := io.WriteString(l.conn, text)
	return err
}

func (l *Link) setNumbering(on bool, next int) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	l.numbering, l.nextLine = on, next
}

func (l *Link) isNumbering() bool {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.numbering
}

// lineCount is the number the next new line will get.
func (l *Link) lineCount() int {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.nextLine
}

// lineAt returns an earlier line, as it was sent.
func (l *Link) lineAt(n int) string {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.sent[n%sdHistory]
}

// writeAgain sends an earlier line again, with its own number.
func (l *Link) writeAgain(n int, text string) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if _, err := io.WriteString(l.conn, numbered(n, text)+"\n"); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return nil
}

// rewrite sends lines again from the one the firmware asked for. A line it hasn't
// been sent yet can't be missing, so asking for that needs nothing.
func (l *Link) rewrite(from int) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if from >= l.nextLine {
		return nil
	}
	if from < 1 || l.nextLine-from > sdHistory {
		return fmt.Errorf("the printer asked for line %d, while sending up to line %d", from, l.nextLine-1)
	}
	for n := from; n < l.nextLine; n++ {
		if _, err := io.WriteString(l.conn, numbered(n, l.sent[n%sdHistory])+"\n"); err != nil {
			return fmt.Errorf("%w: %v", ErrClosed, err)
		}
	}
	return nil
}

// Urgent sends a command the firmware acts on as it arrives, ahead of whatever is
// waiting, even while another command is waiting for its reply. Its number doesn't
// have to be right for that, but it keeps the count correct. Its "ok" can get lost,
// so it doesn't wait for one.
func (l *Link) Urgent(cmd string) error {
	if _, err := l.writeLine(cmd); err != nil {
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return nil
}

// tempsWithin reports whether temperatures arrived that recently.
func (l *Link) tempsWithin(d time.Duration) bool {
	last := l.lastTemps.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < d
}

// Done is closed when the connection is lost.
func (l *Link) Done() <-chan struct{} { return l.done }

func (l *Link) Close() error { return l.conn.Close() }

func (l *Link) readLoop() {
	defer close(l.done)
	scanner := bufio.NewScanner(l.conn)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	for scanner.Scan() {
		l.handle(strings.TrimRight(scanner.Text(), "\r"))
	}
}

func (l *Link) handle(line string) {
	trimmed := strings.TrimSpace(line)
	if temps, ok := parseTemps(trimmed); ok && l.onTemps != nil {
		l.onTemps(temps)
	}
	if l.onLine != nil && (strings.Contains(trimmed, "Done printing file") || strings.Contains(trimmed, ActionCancel) || isHalt(trimmed)) {
		l.onLine(trimmed)
	}
	isOK := trimmed == "ok" || strings.HasPrefix(trimmed, "ok ")
	if strings.HasPrefix(trimmed, "T:") {
		l.lastTemps.Store(time.Now().UnixNano())
		l.mu.Lock()
		defer l.mu.Unlock()
		if w := l.cur; w != nil && w.tempsAlive {
			select {
			case w.activity <- struct{}{}:
			default:
			}
		}
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tap != nil {
		if trimmed != "" {
			select {
			case l.tap <- trimmed:
			case <-time.After(time.Second):
				log.Printf("sd: dropped a reply nothing was reading: %q", trimmed)
			}
		}
		return
	}
	w := l.cur
	// Nothing asked for these lines, and "start" means the board has just booted.
	if trimmed == "start" || (w == nil && trimmed != "" && !isOK && !strings.HasPrefix(trimmed, "echo:busy:")) {
		log.Printf("printer said: %q", trimmed)
	}
	if w == nil {
		return
	}
	// Any line, including Marlin's busy: keepalive, shows the printer is alive.
	select {
	case w.activity <- struct{}{}:
	default:
	}
	w.lines = append(w.lines, trimmed)
	if w.tell && trimmed != "" && !isOK && !strings.HasPrefix(trimmed, "echo:busy:") && !isPrintStatus(trimmed) {
		log.Printf("printer said during a wait: %q", trimmed)
	}
	if isHalt(trimmed) {
		select {
		case w.halted <- trimmed:
		default:
		}
		return
	}
	if want, ok := resendLine([]string{trimmed}); ok && w.line >= 0 {
		w.resendWant = want
		return
	}
	if !isOK {
		return
	}
	// The "ok" after a request to resend says nothing about the command.
	if w.resendWant >= 0 {
		select {
		case w.resend <- w.resendWant:
		default:
		}
		w.resendWant = -1
		w.lines = nil
		return
	}
	if m := okLineRE.FindStringSubmatch(trimmed); m != nil && w.line >= 0 {
		if n, err := strconv.Atoi(m[1]); err == nil && n < w.line {
			log.Printf("printer answered something earlier: %q", w.lines)
			w.lines = nil
			return
		}
	}
	if w.accept != nil && !w.accept(w.lines) {
		log.Printf("printer answered something earlier: %q", w.lines)
		w.lines = nil
		return
	}
	l.cur = nil
	w.result <- w.lines
}

func isHalt(line string) bool {
	return strings.HasPrefix(line, "Error:") && strings.Contains(line, "halted")
}

// Exchange sends one command and returns every line up to and including its
// "ok". It fails if the printer goes silent for longer than idle.
func (l *Link) Exchange(ctx context.Context, cmd string, idle time.Duration) ([]string, error) {
	return l.exchange(ctx, cmd, idle, nil)
}

func (l *Link) exchange(ctx context.Context, cmd string, idle time.Duration, accept func([]string) bool) ([]string, error) {
	lines, _, err := l.timed(ctx, cmd, idle, accept)
	return lines, err
}

// timed is exchange, and also returns the longest gap between lines of the reply.
func (l *Link) timed(ctx context.Context, cmd string, idle time.Duration, accept func([]string) bool) ([]string, time.Duration, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.timedLocked(ctx, cmd, idle, accept)
}

// exchangeAlive is exchange for a command that may wait behind a heater, when the
// firmware stays silent apart from temperatures. Only ctx limits the whole wait.
func (l *Link) exchangeAlive(ctx context.Context, cmd string, silence time.Duration, accept func([]string) bool) ([]string, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	l.tempsAlive = true
	defer func() { l.tempsAlive = false }()
	lines, _, err := l.timedLocked(ctx, cmd, silence, accept)
	return lines, err
}

// exchangePrint is exchangeAlive during a print, when other lines can arrive that
// nothing else would ever see, so they're logged.
func (l *Link) exchangePrint(ctx context.Context, cmd string, silence time.Duration) ([]string, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	l.tempsAlive, l.tellLines = true, true
	defer func() { l.tempsAlive, l.tellLines = false, false }()
	lines, _, err := l.timedLocked(ctx, cmd, silence, nil)
	return lines, err
}

func (l *Link) timedLocked(ctx context.Context, cmd string, idle time.Duration, accept func([]string) bool) ([]string, time.Duration, error) {
	if l.saving {
		if err := l.closeSave(ctx); err != nil {
			return nil, 0, fmt.Errorf("couldn't end a save on the card: %w", err)
		}
	}
	switch {
	case l.wantNumbers && !l.isNumbering():
		if err := l.startLines(ctx); err != nil {
			return nil, 0, err
		}
	// A reply that names what it's waiting for can't be fooled by an old one.
	case accept == nil && l.stale:
		if err := l.resync(ctx); err != nil {
			return nil, 0, fmt.Errorf("the printer is still busy with an earlier command: %w", err)
		}
	}
	return l.send(ctx, cmd, idle, accept)
}

// startLines starts the line count again, which the firmware accepts whatever count
// it had. The first command to run after connecting or after a transfer does this.
func (l *Link) startLines(ctx context.Context) error {
	if l.stale {
		if err := l.resync(ctx); err != nil {
			return fmt.Errorf("the printer is still busy with an earlier command: %w", err)
		}
	}
	if _, _, err := l.send(ctx, numbered(0, "M110 N0"), startLinesIdle, nil); err != nil {
		return fmt.Errorf("starting the line count: %w", err)
	}
	l.setNumbering(true, 1)
	return nil
}

// resync discards replies to commands that were given up on. The firmware runs
// commands in order, so everything before the echo of a new marker is old. It
// can't jump the queue, so this fails while the old command is still running.
func (l *Link) resync(ctx context.Context) error {
	// While temperatures count as signs of life, the real gap between them is the limit.
	if l.tempsAlive {
		return l.resyncWithin(ctx, printSilence)
	}
	return l.resyncWithin(ctx, replyIdle)
}

func (l *Link) resyncWithin(ctx context.Context, idle time.Duration) error {
	l.syncs++
	marker := fmt.Sprintf("sync-%d", l.syncs)
	_, _, err := l.send(ctx, "M118 "+marker, idle, func(lines []string) bool {
		return slices.Contains(lines, marker)
	})
	if err == nil {
		l.stale = false
	}
	return err
}

func (l *Link) send(ctx context.Context, cmd string, idle time.Duration, accept func([]string) bool) ([]string, time.Duration, error) {
	w := &waiter{
		accept: accept, line: -1, resendWant: -1, tempsAlive: l.tempsAlive, tell: l.tellLines,
		resend: make(chan int, 1), activity: make(chan struct{}, 1), result: make(chan []string, 1), halted: make(chan string, 1),
	}
	// The number has to be known before a reply can arrive, so it's taken here.
	l.writeMu.Lock()
	if l.numbering {
		w.line = l.nextLine
	}
	l.writeMu.Unlock()
	l.mu.Lock()
	l.cur = w
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.cur == w {
			l.cur = nil
		}
		l.mu.Unlock()
	}()

	n, err := l.writeLine(cmd)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrClosed, err)
	}
	// Urgent may have taken the number looked at above.
	l.mu.Lock()
	w.line = n
	l.mu.Unlock()

	var resends int
	var longest time.Duration
	last := time.Now()
	gap := func() {
		now := time.Now()
		longest = max(longest, now.Sub(last))
		last = now
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		select {
		case lines := <-w.result:
			gap()
			return lines, longest, nil
		case <-w.activity:
			gap()
			timer.Reset(idle)
		case want := <-w.resend:
			gap()
			timer.Reset(idle)
			resends++
			log.Printf("printer asked for line %d again, for %q", want, cmd)
			if resends > sdMaxStuck {
				l.stale = true
				return nil, longest, fmt.Errorf("gave up after %d resends of %q", resends, cmd)
			}
			if err := l.rewrite(want); err != nil {
				l.stale = true
				return nil, longest, err
			}
		case line := <-w.halted:
			l.stale = true
			return nil, longest, fmt.Errorf("%w: %s", ErrHalted, line)
		case <-timer.C:
			l.stale = true
			return nil, longest, fmt.Errorf("%w to %q within %s", ErrNoReply, cmd, idle)
		case <-l.done:
			return nil, longest, ErrClosed
		case <-ctx.Done():
			l.stale = true
			return nil, longest, ctx.Err()
		}
	}
}

// Flush clears garbage left in the firmware's input from opening the port. The
// replies are ignored, because no command is waiting on them.
func (l *Link) Flush() error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	return l.writeRaw("\n")
}
