package marlin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"tramline/internal/sim"
)

const testIdle = 40 * time.Millisecond

func testLines(n int) []string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("G1 X%d.5 Y%d E0.1", i, i*2)
	}
	return lines
}

func source(lines []string) func() (string, error) {
	i := 0
	return func() (string, error) {
		if i == len(lines) {
			return "", io.EOF
		}
		i++
		return lines[i-1], nil
	}
}

func onCard(lines []string) string {
	return strings.Join(lines, "\r\n") + "\r\n"
}

func sdLink(t *testing.T, opts sim.Options) (*Link, *sim.Card) {
	t.Helper()
	if opts.Card == nil {
		opts.Card = sim.NewCard()
	}
	link := NewLink(sim.New(opts), nil)
	link.sdIdle = testIdle
	t.Cleanup(func() { link.Close() })
	return link, opts.Card
}

func writeTest(t *testing.T, link *Link, lines []string) (SDProgress, error) {
	t.Helper()
	return writeWindow(t, link, DefaultSDWindow, lines)
}

func writeWindow(t *testing.T, link *Link, window int, lines []string) (SDProgress, error) {
	t.Helper()
	var last SDProgress
	err := link.WriteFile(context.Background(), "TEST.GCO", window, source(lines), func(p SDProgress) { last = p })
	return last, err
}

func TestNumberedMatchesKnownChecksum(t *testing.T) {
	// The example from the RepRap G-code documentation.
	if got := numbered(3, "T0"); got != "N3 T0*57" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteFile(t *testing.T) {
	lines := testLines(5000)
	link, card := sdLink(t, sim.Options{})
	progress, err := writeTest(t, link, lines)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := card.File("TEST.GCO")
	if !ok || got != onCard(lines) {
		t.Fatalf("the card has %d bytes, want %d", len(got), len(onCard(lines)))
	}
	if card.Saving() != "" || link.saving {
		t.Fatal("the save is still open")
	}
	if progress.Lines != 5000 || progress.Bytes != int64(len(got)) || progress.Resends != 0 || progress.Timeouts != 0 {
		t.Fatalf("got %+v", progress)
	}
}

// Each fault must leave the card exactly right, with no line lost or repeated,
// however many lines are unanswered when it happens. The counts are for one line
// at a time, as with more than that, how a fault shows depends on timing.
func TestWriteFileSurvivesLineFaults(t *testing.T) {
	lines := testLines(200)
	const m29Line = sdFirstLine + 200
	for _, tt := range []struct {
		name              string
		opts              sim.Options
		resends, timeouts int
	}{
		{"corrupted line", sim.Options{CorruptLine: 50}, 1, 0},
		{"first line corrupted", sim.Options{CorruptLine: sdFirstLine}, 1, 0},
		{"last line corrupted", sim.Options{CorruptLine: m29Line - 1}, 1, 0},
		{"M29 corrupted", sim.Options{CorruptLine: m29Line}, 1, 0},
		{"line lost", sim.Options{DropLine: 50}, 1, 1},
		{"reply lost", sim.Options{LoseOKLine: 50}, 0, 1},
		{"last reply lost", sim.Options{LoseOKLine: m29Line - 1}, 0, 1},
		{"M29 lost", sim.Options{DropLine: m29Line}, 0, 1},
	} {
		for _, window := range []int{1, 60, DefaultSDWindow, 600, MaxSDWindow} {
			t.Run(fmt.Sprintf("%s, window %d", tt.name, window), func(t *testing.T) {
				link, card := sdLink(t, tt.opts)
				progress, err := writeWindow(t, link, window, lines)
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := card.File("TEST.GCO"); got != onCard(lines) {
					t.Fatalf("the card has %d bytes, want %d", len(got), len(onCard(lines)))
				}
				if card.Saving() != "" {
					t.Fatal("the save is still open")
				}
				if window == 1 && (progress.Resends != tt.resends || progress.Timeouts != tt.timeouts) {
					t.Fatalf("got %+v", progress)
				}
				// Whatever happened, the link must be usable straight away.
				if _, err := link.Exchange(context.Background(), "M105", time.Second); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// The window is what stops the board's receive buffer overflowing, even with the
// board slow to take lines up. It must also really keep several lines going.
func TestWindowLimitsWhatsWaitingAtTheBoard(t *testing.T) {
	for _, window := range []int{1, 60, DefaultSDWindow} {
		link, card := sdLink(t, sim.Options{LineDelay: time.Millisecond})
		lines := testLines(300)
		if _, err := writeWindow(t, link, window, lines); err != nil {
			t.Fatal(err)
		}
		if got, _ := card.File("TEST.GCO"); got != onCard(lines) {
			t.Fatalf("window %d: the card has %d bytes", window, len(got))
		}
		limit := max(window, 40)
		if got := card.MaxQueued(); got > limit {
			t.Errorf("window %d: %d bytes were waiting", window, got)
		}
		if window == DefaultSDWindow && card.MaxQueued() < 60 {
			t.Errorf("only %d bytes were ever waiting, so lines aren't overlapping", card.MaxQueued())
		}
	}
}

// With ADVANCED_OK missing, an "ok" has no line number and answers the oldest line.
func TestWindowCopesWithoutNumberedOKs(t *testing.T) {
	link, card := sdLink(t, sim.Options{PlainOK: true})
	lines := testLines(500)
	if _, err := writeTest(t, link, lines); err != nil {
		t.Fatal(err)
	}
	if got, _ := card.File("TEST.GCO"); got != onCard(lines) {
		t.Fatalf("the card has %d bytes", len(got))
	}
}

func TestWriteFileCardWriteError(t *testing.T) {
	link, card := sdLink(t, sim.Options{WriteErrorLine: 20})
	_, err := writeTest(t, link, testLines(100))
	if err == nil || !strings.Contains(err.Error(), "write error") {
		t.Fatalf("got %v", err)
	}
	if card.Saving() != "" || link.saving {
		t.Fatal("the save wasn't ended")
	}
}

func TestWriteFileNoCard(t *testing.T) {
	card := sim.NewCard()
	card.Missing = true
	link, _ := sdLink(t, sim.Options{Card: card})
	_, err := writeTest(t, link, testLines(10))
	if err == nil || !strings.Contains(err.Error(), "didn't open") {
		t.Fatalf("got %v", err)
	}
}

// A board that stops answering must fail the copy, and later commands must keep
// trying to end the save before anything else.
func TestWriteFileBoardGoesSilent(t *testing.T) {
	link, card := sdLink(t, sim.Options{SilentFromLine: 20})
	start := time.Now()
	progress, err := writeTest(t, link, testLines(100))
	if err == nil || !errors.Is(err, ErrNoReply) {
		t.Fatalf("got %v", err)
	}
	if progress.Timeouts != sdMaxSilent {
		t.Fatalf("got %+v", progress)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %s", took)
	}
	if card.Saving() == "" || !link.saving {
		t.Fatal("the save should be known to be open")
	}
	if _, err := link.Exchange(context.Background(), "M105", testIdle); err == nil || !strings.Contains(err.Error(), "end a save") {
		t.Fatalf("got %v", err)
	}
}

func TestWriteFileLinkDrop(t *testing.T) {
	link, card := sdLink(t, sim.Options{DropLinkLine: 20})
	_, err := writeTest(t, link, testLines(100))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	if card.Saving() == "" {
		t.Fatal("a board that kept its power should still be saving")
	}
}

func TestWriteFileCancelEndsTheSave(t *testing.T) {
	link, card := sdLink(t, sim.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	lines := testLines(100)
	i := 0
	next := func() (string, error) {
		if i == 10 {
			cancel()
		}
		i++
		return lines[i-1], nil
	}
	err := link.WriteFile(ctx, "TEST.GCO", 0, next, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if card.Saving() != "" || link.saving {
		t.Fatal("the save wasn't ended")
	}
	// Other commands work straight away.
	if _, err := link.Exchange(context.Background(), "M105", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFileSourceErrorEndsTheSave(t *testing.T) {
	link, card := sdLink(t, sim.Options{})
	boom := errors.New("disk trouble")
	err := link.WriteFile(context.Background(), "TEST.GCO", 0, func() (string, error) { return "", boom }, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if card.Saving() != "" {
		t.Fatal("the save wasn't ended")
	}
}

// After a timeout, the guard against late replies mustn't send its marker into a
// save, and has to work again once the save is over.
func TestCommandsWorkAfterAFaultyTransfer(t *testing.T) {
	link, _ := sdLink(t, sim.Options{LoseOKLine: 5})
	if _, err := writeTest(t, link, testLines(20)); err != nil {
		t.Fatal(err)
	}
	lines, err := link.Exchange(context.Background(), "M105", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !anyContains(lines, "T:") {
		t.Fatalf("got %q", lines)
	}
}

func TestRewriteReplacesTheFile(t *testing.T) {
	link, card := sdLink(t, sim.Options{})
	if _, err := writeTest(t, link, testLines(50)); err != nil {
		t.Fatal(err)
	}
	short := testLines(5)
	if _, err := writeTest(t, link, short); err != nil {
		t.Fatal(err)
	}
	if got, _ := card.File("TEST.GCO"); got != onCard(short) {
		t.Fatalf("got %q", got)
	}
}

func onlinePrinter(t *testing.T, card *sim.Card) (*Printer, context.CancelFunc) {
	t.Helper()
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(sim.Options{Card: card}), nil })
	p.SDLineIdle = testIdle
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !p.Status().Connected {
		if time.Now().After(deadline) {
			t.Fatal("didn't connect")
		}
		time.Sleep(time.Millisecond)
	}
	return p, cancel
}

// A server restart mid-transfer leaves the board refusing everything but M29.
func TestHandshakeEndsASaveLeftOpen(t *testing.T) {
	card := sim.NewCard()
	link, _ := sdLink(t, sim.Options{Card: card, DropLinkLine: 20})
	if _, err := writeTest(t, link, testLines(100)); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	if card.Saving() == "" {
		t.Fatal("the save should still be open")
	}
	start := time.Now()
	onlinePrinter(t, card)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s to reconnect", took)
	}
	if card.Saving() != "" {
		t.Fatal("the save is still open")
	}
}

func TestPrinterSDCommands(t *testing.T) {
	card := sim.NewCard()
	card.Put("OTHER.GCO", "G28\r\n")
	p, _ := onlinePrinter(t, card)
	ctx := context.Background()
	lines := testLines(10)
	if err := p.WriteSD(ctx, "TEST.GCO", 0, source(lines), nil); err != nil {
		t.Fatal(err)
	}
	files, err := p.ListSD(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []SDFile{{"OTHER.GCO", 5}, {"TEST.GCO", int64(len(onCard(lines)))}}
	if fmt.Sprint(files) != fmt.Sprint(want) {
		t.Fatalf("got %+v", files)
	}
	if err := p.DeleteSD(ctx, "TEST.GCO"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSD(ctx, "TEST.GCO"); err == nil {
		t.Fatal("deleting a missing file should fail")
	}
	if got := card.Names(); len(got) != 1 || got[0] != "OTHER.GCO" {
		t.Fatalf("got %q", got)
	}
}

func TestListSDNoCard(t *testing.T) {
	card := sim.NewCard()
	card.Missing = true
	p, _ := onlinePrinter(t, card)
	if _, err := p.ListSD(context.Background()); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("got %v", err)
	}
}

func TestParseListing(t *testing.T) {
	files, err := parseListing([]string{"Begin file list", "A.GCO 12", "DIR/B.GCO 34 0x5A21 LONG NAME.gcode", "End file list", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(files) != fmt.Sprint([]SDFile{{"A.GCO", 12}, {"DIR/B.GCO", 34}}) {
		t.Fatalf("got %+v", files)
	}
	for _, bad := range [][]string{{"ok"}, {"Begin file list", "ok"}, {"Begin file list", "A.GCO x", "End file list"}, {"Begin file list", "A.GCO", "End file list"}} {
		if _, err := parseListing(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
