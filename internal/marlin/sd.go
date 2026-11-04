package marlin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultSDLineIdle is how long the board may take over one line. Writing to the
	// card can stall it.
	DefaultSDLineIdle = 5 * time.Second
	sdListIdle        = 10 * time.Second
	// A dead board fails after this many timeouts in a row, about 15 seconds.
	sdMaxSilent = 3
	// How many resends without getting further give up on the transfer.
	sdMaxStuck = 10
	// How many sent lines are kept, to resend them. It has to cover the largest
	// window, of the shortest lines.
	sdHistory = 512
	// DefaultSDWindow is how many bytes may be unanswered. The firmware's build
	// script gives this board a 128-byte receive buffer, which this always fits.
	DefaultSDWindow = 120
	// MaxSDWindow is the most a test may ask for. Past the buffer's size this relies
	// on the firmware's line queue having room, and on resends if it doesn't.
	MaxSDWindow = 2048
	// How long replies must stop for after a request to resend.
	sdSettle = 200 * time.Millisecond
	// M29 is sent up to this many times to end a save.
	sdCloseTries = 3
	// The first line of the file. N0 resets the count, and N1 opens the file.
	sdFirstLine = 2

	sdSavedReply  = "Done saving file."
	sdOpenedReply = "Writing to file:"
)

// ErrNoMedia means the printer says there's no SD card in it.
var ErrNoMedia = errors.New("the printer has no SD card mounted")

var (
	resendRE = regexp.MustCompile(`^(?:Resend:|rs)\s*(\d+)`)
	okLineRE = regexp.MustCompile(`^ok N(\d+)`)
)

// SDFile is one entry in the card's listing.
type SDFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// SDProgress is how far a transfer has got.
type SDProgress struct {
	// Lines and Bytes count what has been sent. Bytes is the size on the card.
	Lines             int
	Bytes             int64
	Resends, Timeouts int
	// Answered is how many lines have had their "ok".
	Answered int
}

// numbered adds a line number and checksum, which the firmware checks. A line it
// misses or finds damaged is asked for again.
func numbered(n int, text string) string {
	line := fmt.Sprintf("N%d %s", n, text)
	var sum byte
	for i := range len(line) {
		sum ^= line[i]
	}
	return fmt.Sprintf("%s*%d", line, sum)
}

func resendLine(lines []string) (int, bool) {
	for _, line := range lines {
		if m := resendRE.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			return n, err == nil
		}
	}
	return 0, false
}

func anyContains(lines []string, text string) bool {
	return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, text) })
}

// savingToCard reports the firmware's answer to an unnumbered command during a
// save. It didn't run the command.
func savingToCard(lines []string) bool {
	return anyContains(lines, "No Checksum with line number")
}

// closeSave ends a save with an unnumbered M29, the one command the firmware takes
// without a number then, and which is harmless if no save is open. An "ok" could be
// for a line sent earlier, so what proves it is the echo of a marker: during a save
// the firmware would refuse that too.
func (l *Link) closeSave(ctx context.Context) error {
	// The firmware's line count isn't known here, and neither of these needs one.
	l.setNumbering(false, 0)
	var err error
	for range sdCloseTries {
		if err = l.writeRaw("M29\n"); err != nil {
			return fmt.Errorf("%w: %v", ErrClosed, err)
		}
		err = l.resyncWithin(ctx, l.sdIdle)
		if err == nil {
			l.saving = false
			return nil
		}
		if errors.Is(err, ErrClosed) || errors.Is(err, ErrHalted) {
			return err
		}
	}
	return err
}

// WriteFile saves lines to the card as name, with up to window bytes unanswered, or
// DefaultSDWindow if that's 0. It holds the link throughout. next returns io.EOF
// after the last line.
//
// If it fails it ends the save if it can, as the board otherwise refuses every
// other command, but what was written stays on the card. If the save couldn't be
// ended, the next command tries again first.
func (l *Link) WriteFile(ctx context.Context, name string, window int, next func() (string, error), progress func(SDProgress)) (err error) {
	if window <= 0 {
		window = DefaultSDWindow
	}
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	if l.saving {
		if err := l.closeSave(ctx); err != nil {
			return fmt.Errorf("an earlier save is still open: %w", err)
		}
	}
	if l.stale {
		if err := l.resync(ctx); err != nil {
			return fmt.Errorf("the printer is still busy with an earlier command: %w", err)
		}
	}
	// A transfer numbers its own lines. The next command starts the count again.
	l.setNumbering(false, 0)
	if _, _, err := l.send(ctx, numbered(0, "M110 N0"), replyIdle, nil); err != nil {
		return fmt.Errorf("resetting the line count: %w", err)
	}

	// From here the board may be saving, even if the reply to M28 goes missing.
	l.saving = true
	defer func() {
		if !l.saving {
			return
		}
		// Not ctx: this has to happen after a cancel as well.
		if closeErr := l.closeSave(context.WithoutCancel(ctx)); closeErr != nil {
			log.Printf("couldn't end the save on the card: %v", closeErr)
		}
	}()
	lines, _, err := l.send(ctx, numbered(1, "M28 "+name), l.sdIdle, nil)
	if err != nil {
		return fmt.Errorf("opening %s on the card: %w", name, err)
	}
	// With no card mounted, the firmware answers M28 with only "ok".
	if !anyContains(lines, sdOpenedReply) {
		return fmt.Errorf("the printer didn't open %s on the card, it said %q", name, lines)
	}
	// M29 is numbered like the file's lines. The next command starts the count again.
	l.setNumbering(true, sdFirstLine)
	defer l.setNumbering(false, 0)
	return l.sendLines(ctx, sendPlan{window: window, first: sdFirstLine, ending: []string{"M29"}}, next, progress)
}

// sendPlan is how a copy and a stream differ.
type sendPlan struct {
	// window is how many bytes may be unanswered.
	window int
	// first is the earliest line the firmware may ask for again.
	first int
	// ending is sent after the file's last line.
	ending []string
	// nudge is how long a stream waits for an "ok" before sending the next line anyway.
	// A copy leaves it 0.
	nudge time.Duration
	// marker is the echo that ends a stream.
	marker string
}

// sendLines sends the file, on the link's own count and history, so a command sent
// with Urgent meanwhile can't disagree with it about numbers. The caller has started
// the count, and holds sendMu.
//
// A copy keeps up to window bytes unanswered, and each "ok" frees its line's share. As
// long as the window fits the board's receive buffer, nothing can overflow it, even if
// a write to the card stalls the board.
//
// A stream keeps one line unanswered, as lines were only ever lost when they
// overlapped. A lost line with nothing after it gives no error, and silence can't show
// it, as a heater wait or a full planner holds an "ok" for minutes. So with no "ok" for
// the nudge time the next line goes anyway: the firmware asks for the lost one when
// it gets it, and otherwise it just waits its turn. The window still applies to those,
// as nothing reads the receive buffer during a heater wait.
func (l *Link) sendLines(ctx context.Context, plan sendPlan, next func() (string, error), progress func(SDProgress)) (err error) {
	stream := plan.nudge > 0
	replies := make(chan string, 1024)
	l.mu.Lock()
	l.tap = replies
	l.mu.Unlock()
	stopTap := func() {
		l.mu.Lock()
		l.tap = nil
		l.mu.Unlock()
	}
	defer stopTap()
	// A reply can still be on its way after a failure, so the next command resyncs.
	defer func() {
		if err != nil {
			l.stale = true
		}
	}()

	var state SDProgress
	report := func() {
		if progress != nil {
			progress(state)
		}
	}
	// The last timeouts and resends come after the last line read.
	defer report()

	type sentLine struct {
		n, size int
		at      time.Time
	}
	var pending []sentLine
	// sent is the line to send next, read the first number no line of this file has
	// had, and furthest the first not yet answered. endLine holds M29, once it's sent.
	read := l.lineCount()
	sent, furthest := read, read
	ending := plan.ending
	var upcoming, last string
	var haveUpcoming, fromFile, fileDone, nudged, awaitResendOK bool
	var endLine, inFlight, silent, stuck, repeats int
	seenOK := false
	lastMove, lastHeard := time.Now(), time.Now()
	timer := time.NewTimer(l.sdIdle)
	defer timer.Stop()

	var total, recent okTimes
	recentFrom := time.Now()
	if stream {
		defer func() { total.log("whole stream") }()
	}
	answered := func(n int, line string) {
		for len(pending) > 0 && pending[0].n <= n {
			inFlight -= pending[0].size
			if pending[0].n >= furthest {
				furthest = pending[0].n + 1
				stuck = 0
				state.Answered++
				if stream {
					took := time.Since(pending[0].at)
					total.add(took, pending[0].n, line)
					recent.add(took, pending[0].n, line)
				}
			}
			pending = pending[1:]
		}
		if stream && time.Since(recentFrom) >= okTimesEvery {
			recent.log("last minute")
			recent, recentFrom = okTimes{}, time.Now()
		}
		report()
	}
	// pickUp finds the next new line, and returns false if there's none for now.
	pickUp := func() (bool, error) {
		for !haveUpcoming {
			switch {
			case !fileDone:
				text, err := next()
				switch {
				case errors.Is(err, io.EOF):
					fileDone = true
				case err != nil:
					return false, err
				default:
					upcoming, fromFile, haveUpcoming = text, true, true
				}
			case len(ending) > 0:
				upcoming, ending, fromFile, haveUpcoming = ending[0], ending[1:], false, true
				last = upcoming
			case stream && nudged:
				// The last line could have been lost, with nothing after it to show that.
				if repeats++; repeats > sdMaxStuck {
					return false, fmt.Errorf("sent the end of the file %d more times with no answer: %w", repeats-1, ErrNoReply)
				}
				upcoming, fromFile, haveUpcoming = last, false, true
			default:
				return false, nil
			}
		}
		return true, nil
	}
	// fill sends as much as the plan allows. A lone line is always sent, however long.
	fill := func() error {
		for !awaitResendOK {
			again := sent < read
			var text string
			if again {
				text = l.lineAt(sent)
			} else {
				more, err := pickUp()
				if err != nil || !more {
					return err
				}
				text = upcoming
			}
			size := len(numbered(sent, text)) + 1
			if len(pending) > 0 {
				room := inFlight+size <= plan.window
				if stream {
					room = room && nudged && len(pending) < streamMaxWaiting
				}
				if !room {
					return nil
				}
			}
			if stream && len(pending) > 0 {
				state.Timeouts++
				log.Printf("stream: no ok for %s with %d lines unanswered, up to line %d, so line %d goes anyway", plan.nudge, len(pending), pending[len(pending)-1].n, sent)
			}
			n := sent
			if again {
				if err := l.writeAgain(n, text); err != nil {
					return err
				}
				sent++
			} else {
				var err error
				if n, err = l.writeLine(text); err != nil {
					return fmt.Errorf("%w: %v", ErrClosed, err)
				}
				sent, read = n+1, n+1
				haveUpcoming = false
				if fromFile {
					state.Lines++
					state.Bytes += int64(len(text)) + 2
				} else if !stream && len(ending) == 0 {
					endLine = n
				}
				report()
			}
			now := time.Now()
			pending = append(pending, sentLine{n, size, now})
			inFlight += size
			nudged = false
			if stream {
				lastMove = now
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fill(); err != nil {
			return err
		}
		nudged = false

		wait := l.sdIdle
		if stream {
			wait = max(time.Until(lastMove.Add(plan.nudge)), 0)
		}
		timer.Reset(wait)
		var line string
		select {
		case line = <-replies:
		case <-timer.C:
			if stream {
				// Temperatures arrive through any wait, so nothing at all is a dead board.
				if time.Since(lastHeard) >= 2*plan.nudge && !l.tempsWithin(2*plan.nudge) {
					return fmt.Errorf("the printer stopped answering at line %d: %w", sent-1, ErrNoReply)
				}
				awaitResendOK, nudged = false, true
				lastMove = time.Now()
				continue
			}
			state.Timeouts++
			silent++
			log.Printf("sd: no reply with %d lines unanswered, up to line %d, %d in a row", len(pending), sent-1, silent)
			if silent >= sdMaxSilent {
				return fmt.Errorf("the printer stopped answering at line %d: %w", sent-1, ErrNoReply)
			}
			if endLine != 0 && sent > endLine {
				stopTap()
				l.stale = true
				if err := l.closeSave(ctx); err != nil {
					return fmt.Errorf("ending the save: %w", err)
				}
				return nil
			}
			// The firmware ignores a line it already has, without a word, so sending it
			// again could wait forever. Moving on is safe either way: if a line was
			// lost, the firmware rejects the next one and asks for it.
			pending, inFlight = nil, 0
			continue
		case <-l.done:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
		silent = 0
		lastHeard = time.Now()
		want, isResend := resendLine([]string{line})
		isOK := strings.HasPrefix(line, "ok")
		switch {
		// The reason for a resend, which comes just before it.
		case strings.HasPrefix(line, "Error:"):
			log.Printf("sd: printer said: %q", line)
		// Nothing else would ever see these.
		case stream && !isOK && !isResend && !strings.HasPrefix(line, "echo:busy:") && !strings.Contains(line, plan.marker):
			log.Printf("stream: printer said: %q", line)
		}
		switch {
		case strings.HasPrefix(line, "Error:") && strings.Contains(line, "halted"):
			return fmt.Errorf("%w: %s", ErrHalted, line)
		case stream && strings.Contains(line, plan.marker):
			// Its "ok" is still to come.
			l.stale = true
			return nil
		case strings.Contains(line, "error writing to file"):
			return fmt.Errorf("the card reported a write error near line %d", sent-1)
		case strings.Contains(line, sdSavedReply):
			if err := l.awaitOK(ctx, replies); err != nil {
				return err
			}
			l.saving = false
			return nil
		}
		if isResend {
			state.Resends++
			stuck++
			var oldest time.Duration
			if len(pending) > 0 {
				oldest = time.Since(pending[0].at).Round(time.Millisecond)
			}
			log.Printf("sd: printer asked for line %d, with %d lines and %d bytes unanswered, up to line %d, the oldest sent %s ago", want, len(pending), inFlight, sent-1, oldest)
			if stuck > sdMaxStuck {
				return fmt.Errorf("gave up after %d resends in a row at line %d", stuck, want)
			}
			if !stream {
				var done bool
				var err error
				want, done, err = l.settle(ctx, replies, want)
				if err != nil {
					return err
				}
				if done {
					l.saving = false
					return nil
				}
			}
			top := l.lineCount()
			if want < plan.first || want > top || top-want > sdHistory || (endLine != 0 && want > endLine) {
				return fmt.Errorf("the printer asked for line %d, while sending up to line %d", want, sent-1)
			}
			sent, read = want, max(read, want)
			if stream {
				// Lines before it did arrive, and may still be waiting their turn. Writing
				// now could lose the line, as the firmware empties its receive buffer before
				// the "ok" that follows.
				pending = slices.DeleteFunc(pending, func(s sentLine) bool {
					if s.n >= want {
						inFlight -= s.size
						return true
					}
					return false
				})
				awaitResendOK = true
				lastMove = time.Now()
			} else {
				pending, inFlight = nil, 0
			}
			report()
			continue
		}
		if !isOK {
			continue
		}
		if !seenOK {
			seenOK = true
			log.Printf("sd: first reply to a line of the file: %q", line)
		}
		if stream {
			lastMove = time.Now()
		}
		if m := okLineRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			answered(n, line)
		} else if stream && awaitResendOK {
			awaitResendOK = false
		} else if len(pending) > 0 {
			// Without a number, each "ok" answers the oldest line.
			answered(pending[0].n, line)
		}
	}
}

// settle waits for the replies to lines already sent to stop, after a request to
// resend. Those lines are rejected or were thrown away, and sending more before
// they've all gone would only cause more errors. The last line asked for wins.
// done means the file was closed after all.
func (l *Link) settle(ctx context.Context, replies <-chan string, want int) (int, bool, error) {
	timer := time.NewTimer(sdSettle)
	defer timer.Stop()
	for {
		select {
		case line := <-replies:
			if n, ok := resendLine([]string{line}); ok {
				want = n
			}
			if strings.Contains(line, sdSavedReply) {
				return want, true, l.awaitOK(ctx, replies)
			}
			timer.Reset(sdSettle)
		case <-timer.C:
			return want, false, nil
		case <-l.done:
			return 0, false, ErrClosed
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
}

// awaitOK consumes the "ok" after the file closes, so it can't answer the next command.
func (l *Link) awaitOK(ctx context.Context, replies <-chan string) error {
	timer := time.NewTimer(l.sdIdle)
	defer timer.Stop()
	for {
		select {
		case line := <-replies:
			if strings.HasPrefix(line, "ok") {
				return nil
			}
		case <-timer.C:
			// The file is closed. A late "ok" is what the resync is for.
			l.stale = true
			return nil
		case <-l.done:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WriteSD saves a file to the card. See Link.WriteFile.
func (p *Printer) WriteSD(ctx context.Context, name string, window int, next func() (string, error), progress func(SDProgress)) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	return link.WriteFile(ctx, name, window, next, progress)
}

// ListSD returns what M20 reports. Names are DOS 8.3, and files in folders start
// with the folder's name.
func (p *Printer) ListSD(ctx context.Context) ([]SDFile, error) {
	link, err := p.currentLink()
	if err != nil {
		return nil, err
	}
	lines, err := link.Exchange(ctx, "M20", sdListIdle)
	if err != nil {
		return nil, err
	}
	return parseListing(lines)
}

func parseListing(lines []string) ([]SDFile, error) {
	if anyContains(lines, "No media") {
		return nil, ErrNoMedia
	}
	begin := slices.Index(lines, "Begin file list")
	end := slices.Index(lines, "End file list")
	if begin < 0 || end < begin {
		return nil, fmt.Errorf("unrecognised reply to M20: %q", lines)
	}
	files := []SDFile{}
	for _, line := range lines[begin+1 : end] {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("unrecognised line from M20: %q", line)
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unrecognised line from M20: %q", line)
		}
		files = append(files, SDFile{Name: fields[0], Size: size})
	}
	return files, nil
}

// DeleteSD removes a file from the card.
func (p *Printer) DeleteSD(ctx context.Context, name string) error {
	link, err := p.currentLink()
	if err != nil {
		return err
	}
	lines, err := link.Exchange(ctx, "M30 "+name, sdListIdle)
	if err != nil {
		return err
	}
	if !anyContains(lines, "File deleted") {
		return fmt.Errorf("the printer didn't delete %s, it said %q", name, lines)
	}
	return nil
}
