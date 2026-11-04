package marlin

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/sim"
)

func TestParseSDStatus(t *testing.T) {
	got, err := parseSDStatus([]string{"echo:busy: processing", "SD printing byte 12/345", "ok N3 P15 B15"})
	if err != nil || got != (SDStatus{Printing: true, Pos: 12, Size: 345}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	got, err = parseSDStatus([]string{"Not SD printing", "ok"})
	if err != nil || got.Printing {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := parseSDStatus([]string{"ok"}); err == nil {
		t.Fatal("no answer in the reply should be an error")
	}
}

// fakeBoard records exactly what's written, and answers each line with reply.
func fakeBoard(t *testing.T, reply func(line string, write func(string))) (*Link, func() []string) {
	t.Helper()
	host, device := net.Pipe()
	var mu sync.Mutex
	var got []string
	go func() {
		scanner := bufio.NewScanner(device)
		for scanner.Scan() {
			mu.Lock()
			got = append(got, scanner.Text())
			mu.Unlock()
			reply(scanner.Text(), func(s string) { device.Write([]byte(s + "\n")) })
		}
	}()
	link := NewLink(host, nil)
	t.Cleanup(func() { link.Close(); device.Close() })
	return link, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func okFor(line string, write func(string)) {
	n, _, _ := strings.Cut(line, " ")
	write("ok " + n + " P15 B15")
}

func TestEveryCommandIsNumberedOnceAsked(t *testing.T) {
	link, got := fakeBoard(t, okFor)
	link.wantNumbers = true
	ctx := context.Background()
	for _, cmd := range []string{"M140 S80", "M104 S150", "G0 X1"} {
		if _, err := link.Exchange(ctx, cmd, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{numbered(0, "M110 N0"), numbered(1, "M140 S80"), numbered(2, "M104 S150"), numbered(3, "G0 X1")}
	if lines := got(); strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q, want %q", lines, want)
	}
}

func TestResendRepeatsTheLineAndNotTheOthers(t *testing.T) {
	asked := false
	link, got := fakeBoard(t, func(line string, write func(string)) {
		if strings.Contains(line, "M104") && !asked {
			asked = true
			write("Error:checksum mismatch, Last Line: 1")
			write("Resend: 2")
			write("ok")
			return
		}
		okFor(line, write)
	})
	link.wantNumbers = true
	for _, cmd := range []string{"M140 S80", "M104 S150"} {
		if _, err := link.Exchange(context.Background(), cmd, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	lines := got()
	if len(lines) != 4 || lines[2] != lines[3] || lines[3] != numbered(2, "M104 S150") {
		t.Fatalf("got %q", lines)
	}
}

func TestAnOKForAnEarlierLineIsNotTheReply(t *testing.T) {
	link, _ := fakeBoard(t, func(line string, write func(string)) {
		if strings.Contains(line, "M105") {
			write("ok N1 P15 B15")
			write("the answer")
		}
		okFor(line, write)
	})
	link.wantNumbers = true
	if _, err := link.Exchange(context.Background(), "M140 S0", time.Second); err != nil {
		t.Fatal(err)
	}
	lines, err := link.Exchange(context.Background(), "M105", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(lines, "the answer") {
		t.Fatalf("got %q", lines)
	}
}

func TestLostLineIsResentAfterATimeout(t *testing.T) {
	opts := sim.Options{DropLine: 2}
	link := NewLink(sim.New(opts), nil)
	defer link.Close()
	link.wantNumbers = true
	ctx := context.Background()
	if _, err := link.Exchange(ctx, "M140 S0", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := link.Exchange(ctx, "M104 S0", 30*time.Millisecond); !errors.Is(err, ErrNoReply) {
		t.Fatalf("the lost line should time out, got %v", err)
	}
	if _, err := link.Exchange(ctx, "M105", time.Second); err != nil {
		t.Fatalf("the next command should recover: %v", err)
	}
}

func TestUrgentGetsPastACommandWaitingForItsReply(t *testing.T) {
	link, got := fakeBoard(t, func(line string, write func(string)) {
		if strings.Contains(line, "M524") {
			write("ok N99")
		}
	})
	link.wantNumbers = true
	link.setNumbering(true, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := link.Exchange(ctx, "M27", time.Minute)
		waiting <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if err := link.Urgent("M524"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	lines := got()
	if !contains(lines, numbered(5, "M27")) || !contains(lines, numbered(6, "M524")) {
		t.Fatalf("got %q", lines)
	}
	cancel()
	<-waiting
}

func TestM27WaitsThroughAHeaterWait(t *testing.T) {
	card := sim.NewCard()
	card.Put("A.GCO", "G1 X1\r\n")
	p := connectedShort(t, sim.Options{Card: card, PrintTime: time.Minute, PrintHold: 400 * time.Millisecond})
	ctx := context.Background()
	if err := p.StartSD(ctx, "A.GCO", 7); err != nil {
		t.Fatal(err)
	}
	// Silent for longer than a plain command may be, apart from temperatures.
	link, _ := p.currentLink()
	lines, err := link.exchangeAlive(ctx, "M27", 200*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := parseSDStatus(lines); err != nil || !st.Printing {
		t.Fatalf("got %+v, %v from %q", st, err, lines)
	}
}

func TestStartSDChecksTheSize(t *testing.T) {
	card := sim.NewCard()
	card.Put("A.GCO", "G1 X1\r\n")
	p := connectedShort(t, sim.Options{Card: card, PrintTime: time.Minute})
	if err := p.StartSD(context.Background(), "A.GCO", 8); err == nil || !strings.Contains(err.Error(), "should be 8") {
		t.Fatalf("got %v", err)
	}
	if err := p.StartSD(context.Background(), "MISSING.GCO", 8); err == nil || !strings.Contains(err.Error(), "didn't open") {
		t.Fatalf("got %v", err)
	}
}
