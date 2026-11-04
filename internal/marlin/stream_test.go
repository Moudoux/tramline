package marlin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/sim"
)

const testNudge = 40 * time.Millisecond

func streamPrinter(t *testing.T, opts sim.Options) (*Printer, *seen) {
	t.Helper()
	cmds := &seen{}
	opts.Seen = cmds.add
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(opts), nil })
	shortMotion(p)
	p.StreamNudge = testNudge
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	return p, cmds
}

// nextNumber is what the first line of a stream will be numbered, once it's the only
// thing using the link.
func nextNumber(t *testing.T, p *Printer) int {
	t.Helper()
	link, err := p.currentLink()
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "numbering", link.isNumbering)
	return link.lineCount()
}

func moves(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("G1 X%d Y%d", i, i)
	}
	return lines
}

// onlyFile leaves out what ran before the file.
func onlyFile(cmds []string, first string) []string {
	if i := slices.Index(cmds, first); i >= 0 {
		return cmds[i:]
	}
	return nil
}

func TestStreamSendsEveryLineInOrderThenTheMarker(t *testing.T) {
	card := sim.NewCard()
	p, cmds := streamPrinter(t, sim.Options{Card: card, MoveTime: time.Millisecond})
	lines := moves(30)
	var last SDProgress
	if err := p.StreamFile(context.Background(), "end-1", source(lines), func(s SDProgress) { last = s }); err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(lines), "M400", "M118 end-1")
	if got := onlyFile(cmds.got(), lines[0]); !slices.Equal(got[:len(want)], want) {
		t.Fatalf("got %q", got)
	}
	var size int64
	for _, line := range lines {
		size += int64(len(line)) + 2
	}
	if last.Lines != len(lines) || last.Bytes != size || last.Resends != 0 {
		t.Errorf("progress: %+v", last)
	}
	// One line at a time, so no more than one can wait at the board.
	if most := card.MaxQueued(); most > 30 {
		t.Errorf("up to %d bytes waited at the board", most)
	}
}

// A full planner holds a move's "ok", and M400 has to wait for what's planned.
func TestStreamWaitsForAFullPlannerAndM400(t *testing.T) {
	const moveTime = 5 * time.Millisecond
	p, _ := streamPrinter(t, sim.Options{MoveTime: moveTime})
	start := time.Now()
	const n = 25
	if err := p.StreamFile(context.Background(), "end-1", source(moves(n)), nil); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < n*moveTime {
		t.Errorf("the marker came back after %s, before the moves could finish", took)
	}
}

// The firmware doesn't read during a heater wait, so what's sent meanwhile must fit.
func TestStreamNudgesStayWithinLimitsDuringAHeaterWait(t *testing.T) {
	card := sim.NewCard()
	p, cmds := streamPrinter(t, sim.Options{Card: card, HeatWait: 12 * testNudge})
	lines := append([]string{"M190 S80"}, moves(20)...)
	var last SDProgress
	var mu sync.Mutex
	if err := p.StreamFile(context.Background(), "end-1", source(lines), func(s SDProgress) {
		mu.Lock()
		defer mu.Unlock()
		last = s
	}); err != nil {
		t.Fatal(err)
	}
	if got := onlyFile(cmds.got(), lines[0]); !slices.Equal(got[:len(lines)], lines) {
		t.Fatalf("got %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if last.Timeouts != streamMaxWaiting-1 {
		t.Errorf("%d lines went while another was waiting, want %d", last.Timeouts, streamMaxWaiting-1)
	}
	if most := card.MaxQueued(); most > DefaultSDWindow {
		t.Errorf("up to %d bytes waited at the board", most)
	}
}

// A long line would overflow the buffer before the count ran out.
func TestStreamNudgeStopsAtTheWindow(t *testing.T) {
	card := sim.NewCard()
	p, _ := streamPrinter(t, sim.Options{Card: card, HeatWait: 12 * testNudge})
	lines := []string{"M190 S80"}
	for i := range 6 {
		lines = append(lines, fmt.Sprintf("G1 X%d ;%s", i, strings.Repeat("x", 60)))
	}
	long := make([]string, len(lines))
	for i, line := range lines {
		long[i] = strings.ReplaceAll(line, ";", "E")
	}
	if err := p.StreamFile(context.Background(), "end-1", source(long), nil); err != nil {
		t.Fatal(err)
	}
	if most := card.MaxQueued(); most > DefaultSDWindow {
		t.Errorf("up to %d bytes waited at the board", most)
	}
}

func TestStreamRecoversFromALostLineWithNothingAfterIt(t *testing.T) {
	const n = 5
	for _, c := range []struct {
		what string
		// from the first line of the file
		at int
	}{
		{"the first line", 0},
		{"a middle line", 2},
		{"the file's last line", n - 1},
		{"M400", n},
		{"the marker", n + 1},
	} {
		t.Run(c.what, func(t *testing.T) {
			lines := moves(n)
			// The numbering is the same each run, so it's found on a run with no fault.
			clean, _ := streamPrinter(t, sim.Options{})
			first := nextNumber(t, clean)
			p, cmds := streamPrinter(t, sim.Options{DropLine: first + c.at})
			var last SDProgress
			if err := p.StreamFile(context.Background(), "end-1", source(lines), func(s SDProgress) { last = s }); err != nil {
				t.Fatal(err)
			}
			got := onlyFile(cmds.got(), lines[0])
			want := append(slices.Clone(lines), "M400", "M118 end-1")
			// A repeated marker may run after the first one has come back.
			if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			if last.Resends == 0 {
				t.Errorf("progress: %+v", last)
			}
		})
	}
}

func TestStreamRecoversFromADamagedLine(t *testing.T) {
	lines := moves(10)
	clean, _ := streamPrinter(t, sim.Options{})
	first := nextNumber(t, clean)
	p, cmds := streamPrinter(t, sim.Options{CorruptLine: first + 4})
	if err := p.StreamFile(context.Background(), "end-1", source(lines), nil); err != nil {
		t.Fatal(err)
	}
	if got := onlyFile(cmds.got(), lines[0]); !slices.Equal(got[:len(lines)], lines) {
		t.Fatalf("got %q", got)
	}
}

// streamOn runs a stream on a scripted board, which sees exactly what's written.
func streamOn(t *testing.T, reply func(line string, write func(string)), lines []string) (*Link, func() []string, chan error) {
	t.Helper()
	link, got := fakeBoard(t, reply)
	link.setNumbering(true, 10)
	done := make(chan error, 1)
	go func() {
		done <- link.sendLines(context.Background(), sendPlan{
			window: DefaultSDWindow, first: 10, ending: []string{"M400", "M118 end-1"}, nudge: testNudge, marker: "end-1",
		}, source(lines), nil)
	}()
	return link, got, done
}

func endOK(line string, write func(string)) {
	if strings.Contains(line, "M118") {
		write("end-1")
	}
	okFor(line, write)
}

// No 200 ms settle: on short moves, the planner could run out in that time.
func TestStreamResendsAsSoonAsItsOKArrives(t *testing.T) {
	var mu sync.Mutex
	var asked, again time.Time
	_, _, done := streamOn(t, func(line string, write func(string)) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(line, "N12 ") {
			if asked.IsZero() {
				asked = time.Now()
				write("Error:checksum mismatch, Last Line: 11")
				write("Resend: 12")
				write("ok")
				return
			}
			again = time.Now()
		}
		endOK(line, write)
	}, moves(5))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gap := again.Sub(asked); again.IsZero() || gap >= sdSettle/2 {
		t.Errorf("sent again after %s", gap)
	}
}

// The real firmware checks a line as it reads it, ahead of running what's queued, so a
// rejection can come before the "ok" of a line it already has, which stays unanswered.
func TestStreamRejectionBeforeAnEarlierLinesOK(t *testing.T) {
	var mu sync.Mutex
	rejected := false
	release := make(chan struct{})
	stopTemps := make(chan struct{})
	defer close(stopTemps)
	_, got, done := streamOn(t, func(line string, write func(string)) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(line, "N10 "):
			// Held, like a heater wait, which sends only temperatures. Its "ok" comes
			// after the rejection.
			go func() {
				for {
					select {
					case <-time.After(testNudge / 4):
						write("T:80.00 /80.00 B:80.00 /80.00 @:0 B@:0")
					case <-stopTemps:
						return
					}
				}
			}()
			go func() {
				<-release
				write("ok N10 P15 B15")
			}()
		case strings.HasPrefix(line, "N11 ") && !rejected:
			// Taken to be lost on the way.
		case strings.HasPrefix(line, "N12 ") && !rejected:
			rejected = true
			write("Error:Line Number is not Last Line Number+1, Last Line: 10")
			write("Resend: 11")
			write("ok")
			close(release)
		default:
			endOK(line, write)
		}
	}, moves(4))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := got()
	var numbers []string
	for _, line := range lines {
		n, _, _ := strings.Cut(line, " ")
		numbers = append(numbers, n)
	}
	want := []string{"N10", "N11", "N12", "N11", "N12", "N13", "N14", "N15"}
	if len(numbers) < len(want) || !slices.Equal(numbers[:len(want)], want) {
		t.Fatalf("got %q", lines)
	}
}

func TestUrgentDuringAStreamKeepsTheNumbersInStep(t *testing.T) {
	release := make(chan struct{})
	link, got := fakeBoard(t, func(line string, write func(string)) {
		// Not held here, as that would hold up reading the next line too.
		if strings.HasPrefix(line, "N12 ") {
			go func() {
				<-release
				endOK(line, write)
			}()
			return
		}
		endOK(line, write)
	})
	link.setNumbering(true, 10)
	done := make(chan error, 1)
	go func() {
		done <- link.sendLines(context.Background(), sendPlan{
			window: DefaultSDWindow, first: 10, ending: []string{"M400", "M118 end-1"}, nudge: time.Minute, marker: "end-1",
		}, source(moves(4)), nil)
	}()
	waitFor(t, "line 12", func() bool { return len(got()) == 3 })
	if err := link.Urgent("M108"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := got()
	for i, line := range lines {
		if !strings.HasPrefix(line, fmt.Sprintf("N%d ", 10+i)) {
			t.Fatalf("line %d: got %q", i, lines)
		}
	}
	if !contains(lines, numbered(13, "M108")) {
		t.Fatalf("got %q", lines)
	}
}

// A board that has restarted asks for line 1, and what's before the file mustn't go
// out again.
func TestStreamFailsIfAskedForALineBeforeTheFile(t *testing.T) {
	_, got, done := streamOn(t, func(line string, write func(string)) {
		write("Resend: 1")
		write("ok")
	}, moves(3))
	if err := <-done; err == nil || !strings.Contains(err.Error(), "asked for line 1") {
		t.Fatalf("got %v", err)
	}
	if lines := got(); len(lines) != 1 {
		t.Fatalf("got %q", lines)
	}
}

func TestStreamFailsIfTheBoardGoesCompletelySilent(t *testing.T) {
	_, _, done := streamOn(t, func(string, func(string)) {}, moves(3))
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoReply) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it should have noticed")
	}
}

// Temperatures still arriving could be a long heater wait, so it keeps waiting.
func TestStreamWaitsWhileTemperaturesArrive(t *testing.T) {
	clean, _ := streamPrinter(t, sim.Options{})
	first := nextNumber(t, clean)
	p, _ := streamPrinter(t, sim.Options{SilentFromLine: first + 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.StreamFile(ctx, "end-1", source(moves(20)), nil) }()
	select {
	case err := <-done:
		t.Fatalf("it gave up: %v", err)
	case <-time.After(10 * testNudge):
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// M85 fires with nothing moving, even with lines still being answered.
func TestStreamEndsOnAHalt(t *testing.T) {
	p, _ := streamPrinter(t, sim.Options{KillAfter: 50 * time.Millisecond})
	if _, err := mustLink(t, p).Exchange(context.Background(), "M85 S300", time.Second); err != nil {
		t.Fatal(err)
	}
	err := p.StreamFile(context.Background(), "end-1", func() (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "M117 waiting", nil
	}, nil)
	if !errors.Is(err, ErrHalted) {
		t.Fatalf("got %v", err)
	}
}

func mustLink(t *testing.T, p *Printer) *Link {
	t.Helper()
	link, err := p.currentLink()
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func TestStopStreamEndsAHeaterWaitAndLeavesThePrinterSafe(t *testing.T) {
	p, cmds := streamPrinter(t, sim.Options{HeatWait: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	lines := append([]string{"M190 S80", "M109 S230"}, moves(5)...)
	done := make(chan error, 1)
	go func() { done <- p.StreamFile(ctx, "end-1", source(lines), nil) }()
	// Long enough for lines to build up behind the wait, including a second one.
	time.Sleep(5 * testNudge)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := p.StopStream(stop); err != nil {
		t.Fatal(err)
	}
	got := cmds.got()
	i := slices.Index(got, "M104 S0")
	if i < 0 {
		t.Fatalf("got %q", got)
	}
	want := []string{"M104 S0", "M140 S0", "M107", "G91", "G0 Z5 F600", "G90", "M85 S0", "M84"}
	if !slices.Equal(got[i:i+len(want)], want) {
		t.Fatalf("got %q", got[i:])
	}
	for _, line := range got {
		if strings.HasPrefix(line, "G28") || strings.HasPrefix(line, "M524") {
			t.Errorf("it shouldn't send %q", line)
		}
	}
	// The link has to be usable afterwards.
	if _, err := mustLink(t, p).Exchange(context.Background(), "M105", time.Second); err != nil {
		t.Fatal(err)
	}
}

// Lines of the file mustn't go out again, when the firmware rejects the stop's numbers.
func TestStopStreamDoesNotResendTheFile(t *testing.T) {
	lines := moves(30)
	clean, _ := streamPrinter(t, sim.Options{})
	first := nextNumber(t, clean)
	p, cmds := streamPrinter(t, sim.Options{DropLine: first + 3, MoveTime: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- p.StreamFile(ctx, "end-1", func() func() (string, error) {
			next := source(lines)
			sent := 0
			return func() (string, error) {
				if sent == 4 {
					cancel()
				}
				sent++
				return next()
			}
		}(), nil)
	}()
	<-done
	stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := p.StopStream(stop); err != nil {
		t.Fatal(err)
	}
	got := cmds.got()
	i := slices.Index(got, "M104 S0")
	if i < 0 {
		t.Fatalf("got %q", got)
	}
	for _, line := range got[i:] {
		if strings.HasPrefix(line, "G1 ") {
			t.Errorf("a line of the file ran after the stop: %q", got[i:])
		}
	}
}

// On the real printer, a restart during a stream lost M115's first line, then waited a
// whole minute to ask again, with the heaters on.
func TestHandshakeAsksAgainIfTheFirmwareLineWasDamaged(t *testing.T) {
	asked := 0
	link, _ := fakeBoard(t, func(line string, write func(string)) {
		if !strings.Contains(line, "M115") {
			return
		}
		if asked++; asked == 1 {
			write("Cap:AUTOREPORT_TEMP:1")
			write("ok")
			return
		}
		write("FIRMWARE_NAME:Marlin 2.1.3 SOURCE_CODE_URL:x")
		write("Cap:AUTOREPORT_TEMP:1")
		write("ok")
	})
	start := time.Now()
	name, ok := handshake(context.Background(), link)
	if !ok || name != "Marlin 2.1.3" {
		t.Fatalf("got %q, %t", name, ok)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s", took)
	}
}

func TestOKTimesAreLoggedAtTheEnd(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &logged})
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	p, _ := streamPrinter(t, sim.Options{MoveTime: time.Millisecond})
	if err := p.StreamFile(context.Background(), "end-1", source(moves(10)), nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := logged.String(); !strings.Contains(got, "ok times, whole stream: 11 lines") || !strings.Contains(got, "planner free") {
		t.Errorf("logged: %s", got)
	}
}

func TestTheLCDsStopReachesTheServerDuringAStream(t *testing.T) {
	host, device := net.Pipe()
	told := make(chan string, 1)
	link := newLink(host, nil, func(line string) { told <- line }, time.Second)
	defer link.Close()
	go device.Write([]byte(ActionCancel + "\n"))
	select {
	case line := <-told:
		if line != ActionCancel {
			t.Fatalf("got %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("it wasn't passed on")
	}
}

func TestStartStreamOrderAndM86(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &logged})
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	p, cmds := streamPrinter(t, sim.Options{M86: "M86 B0 E0 S300 T170"})
	if err := p.StartStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmds.got(), "|")
	if !strings.Contains(got, "M108|M86|M85 S300") {
		t.Fatalf("got %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "isn't the expected S600 T170") {
		t.Errorf("logged: %s", logged.String())
	}
}

// Reading the print's position used to throw away anything else that arrived.
func TestLinesDuringAnM27WaitAreLogged(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &logged})
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	link, _ := fakeBoard(t, func(line string, write func(string)) {
		write("X:1.00 Y:2.00 Z:3.00 E:0.00 Count X:0 Y:0 Z:0")
		write("echo:busy: processing")
		write("SD printing byte 1/2")
		okFor(line, write)
	})
	if _, err := link.exchangePrint(context.Background(), "M27", time.Second); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	got := logged.String()
	if !strings.Contains(got, "X:1.00") || strings.Contains(got, "busy") || strings.Contains(got, "SD printing") {
		t.Errorf("logged: %s", got)
	}
}
