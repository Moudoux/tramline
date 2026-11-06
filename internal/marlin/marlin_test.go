package marlin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/sim"
)

func TestParseTemps(t *testing.T) {
	for _, line := range []string{
		"ok T:21.59 /0.00 B:22.12 /0.00 @:0 B@:0",
		"T:21.59 /0.00 B:22.12 /0.00 @:0 B@:0",
		"T: 21.59 / 0.00 B: 22.12 / 0.00",
	} {
		got, ok := parseTemps(line)
		want := Temps{Nozzle: 21.59, Bed: 22.12}
		if !ok || got != want {
			t.Errorf("%q: got %+v, %v", line, got, ok)
		}
	}
	for _, line := range []string{"ok P15 B15", "Cap:Z_PROBE:1", ""} {
		if _, ok := parseTemps(line); ok {
			t.Errorf("%q parsed as temperatures", line)
		}
	}
}

func TestParseFirmware(t *testing.T) {
	got, ok := parseFirmware("FIRMWARE_NAME:Marlin 2.1.3 MRiscoC SOURCE_CODE_URL:github.com/x PROTOCOL_VERSION:1.0")
	if !ok || got != "Marlin 2.1.3 MRiscoC" {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestExchangeReturnsWholeReply(t *testing.T) {
	link := NewLink(sim.New(sim.Options{Corners: sim.Corners{FL: 0.125}}), nil)
	defer link.Close()
	lines, err := link.Exchange(context.Background(), "G30 X25 Y25", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Bed X:25.00 Y:25.00 Z:0.125") {
		t.Fatalf("got %q", lines)
	}
	if lines[len(lines)-1] != "ok P15 B15" {
		t.Fatalf("reply should end in ok, got %q", lines)
	}
}

func TestExchangeTimesOutOnSilence(t *testing.T) {
	link := NewLink(sim.New(sim.Options{Silent: true}), nil)
	defer link.Close()
	if _, err := link.Exchange(context.Background(), "M115", 50*time.Millisecond); err == nil {
		t.Fatal("expected a timeout")
	}
}

// A board restart must leave a trace, whether or not a command is waiting.
func TestLogsUnpromptedLinesAndBoots(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	host, device := net.Pipe()
	link := NewLink(host, nil)
	defer link.Close()
	go func() {
		io.WriteString(device, "ok P15 B15\necho:Unknown command\nT:21.59 /0.00 B:22.12 /0.00\nstart\n")
		device.Close()
	}()
	<-link.Done()

	got := buf.String()
	for _, want := range []string{`"echo:Unknown command"`, `"start"`} {
		if !strings.Contains(got, want) {
			t.Errorf("log is missing %s: %q", want, got)
		}
	}
	for _, unwanted := range []string{"P15", "T:21"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("log shouldn't have %s: %q", unwanted, got)
		}
	}
}

// A long command must survive as long as the printer keeps saying it's busy.
func TestExchangeBusyExtendsTimeout(t *testing.T) {
	host, device := net.Pipe()
	link := NewLink(host, nil)
	defer link.Close()
	go func() {
		buf := make([]byte, 64)
		device.Read(buf)
		for range 5 {
			time.Sleep(40 * time.Millisecond)
			io.WriteString(device, "echo:busy: processing\n")
		}
		io.WriteString(device, "ok\n")
	}()
	if _, err := link.Exchange(context.Background(), "G28", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestExchangeFailsOnDisconnect(t *testing.T) {
	host, device := net.Pipe()
	link := NewLink(host, nil)
	defer link.Close()
	go func() {
		buf := make([]byte, 64)
		device.Read(buf)
		device.Close()
	}()
	_, err := link.Exchange(context.Background(), "G28", 5*time.Second)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPrinterConnectsThroughGarbageAndBoot(t *testing.T) {
	p := NewPrinter(func() (io.ReadWriteCloser, error) {
		return sim.New(sim.Options{Garbage: true, BootDelay: 700 * time.Millisecond}), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	waitFor(t, "connection", func() bool { return p.Status().Connected })
	if got := p.Status().Firmware; !strings.Contains(got, "MRiscoC") {
		t.Fatalf("firmware %q", got)
	}
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	if got := p.Status().Temps.Bed; got != 22.12 {
		t.Fatalf("bed %v", got)
	}
}

// The device vanishes whenever the printer loses power, and must be picked up
// again when it returns, with no stale data shown in between.
func TestPrinterHandlesPowerCycle(t *testing.T) {
	var mu sync.Mutex
	var current io.ReadWriteCloser
	powered := true
	p := NewPrinter(func() (io.ReadWriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		if !powered {
			return nil, errors.New("no such device")
		}
		current = sim.New(sim.Options{})
		return current, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	waitFor(t, "first connection", func() bool { return p.Status().Temps != nil })

	mu.Lock()
	powered = false
	current.Close()
	mu.Unlock()
	waitFor(t, "disconnect", func() bool { return !p.Status().Connected })
	if s := p.Status(); s.Temps != nil || s.Firmware != "" {
		t.Fatalf("stale status after power loss: %+v", s)
	}

	mu.Lock()
	powered = true
	mu.Unlock()
	waitFor(t, "reconnect", func() bool { return p.Status().Temps != nil })
}

func connected(t *testing.T, opts sim.Options) *Printer {
	t.Helper()
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(opts), nil })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	return p
}

func TestSetTargetsIsReportedStraightAway(t *testing.T) {
	p := connected(t, sim.Options{})
	if err := p.SetTargets(context.Background(), 80, 150); err != nil {
		t.Fatal(err)
	}
	// M155 reports could take a while, so this proves the read-back updated it.
	got := p.Status().Temps
	if got.BedTarget != 80 || got.NozzleTarget != 150 {
		t.Fatalf("got %+v", got)
	}
	waitFor(t, "heating", func() bool { return p.Status().Temps.Bed > 30 })
	if err := p.SetTargets(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := p.Status().Temps; got.BedTarget != 0 || got.NozzleTarget != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSetTargetsFailsWhenPrinterIgnoresIt(t *testing.T) {
	p := connected(t, sim.Options{IgnoreHeat: true})
	err := p.SetTargets(context.Background(), 80, 150)
	if err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("got %v", err)
	}
}

func TestSetTargetsFailsWithoutReply(t *testing.T) {
	p := connected(t, sim.Options{NoHeatReply: true})
	if err := p.SetTargets(context.Background(), 80, 150); err == nil {
		t.Fatal("expected an error")
	}
}

// One failed command mustn't leave the other heater on.
var numberedLineRE = regexp.MustCompile(`^N\d+ (.*)\*\d+$`)

func TestSetTargetsTurnsBothOffAfterPartialFailure(t *testing.T) {
	host, device := net.Pipe()
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return host, nil })
	var mu sync.Mutex
	var seen []string
	go func() {
		scanner := bufio.NewScanner(device)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// What matters here is the command, whatever its number.
			if m := numberedLineRE.FindStringSubmatch(line); m != nil {
				line = m[1]
			}
			mu.Lock()
			seen = append(seen, line)
			mu.Unlock()
			switch {
			case line == "M115":
				io.WriteString(device, "FIRMWARE_NAME:Marlin SOURCE_CODE_URL:x\nok\n")
			case line == "M105":
				// Reports the bed as heating but the nozzle as not, then off.
				if contains(seen, "M140 S0") {
					io.WriteString(device, "ok T:22 /0 B:22 /0\n")
				} else {
					io.WriteString(device, "ok T:22 /0 B:22 /80\n")
				}
			case line != "":
				io.WriteString(device, "ok\n")
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	waitFor(t, "connection", func() bool { return p.Status().Connected })
	if err := p.SetTargets(ctx, 80, 150); err == nil {
		t.Fatal("expected a mismatch error")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"M140 S0", "M104 S0"} {
		if !contains(seen, want) {
			t.Errorf("never sent %s: %q", want, seen)
		}
	}
}

func contains(lines []string, want string) bool {
	return slices.Contains(lines, want)
}

func TestSetTargetsWithoutConnection(t *testing.T) {
	p := NewPrinter(nil)
	if err := p.SetTargets(context.Background(), 80, 150); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("got %v", err)
	}
	if _, err := p.SDPrinting(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("got %v", err)
	}
}

func TestSDPrinting(t *testing.T) {
	var mu sync.Mutex
	printing := false
	p := connected(t, sim.Options{Printing: func() bool {
		mu.Lock()
		defer mu.Unlock()
		return printing
	}})
	if got, err := p.SDPrinting(context.Background()); err != nil || got {
		t.Fatalf("idle: %v, %v", got, err)
	}
	mu.Lock()
	printing = true
	mu.Unlock()
	if got, err := p.SDPrinting(context.Background()); err != nil || !got {
		t.Fatalf("printing: %v, %v", got, err)
	}
}

// The board keeps what it's sent while it starts up, and answers in order: an "ok"
// for the garbage from opening the port, then M115. Only the second ends the wait,
// so the reply can't turn up later as noise or as a stale answer.
func TestSlowBootIsOneHandshakeWithNoStrayLines(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &buf})
	defer log.SetOutput(os.Stderr)

	p := connected(t, sim.Options{Garbage: true, GarbageIn: true, BootDelay: 4 * time.Second})
	if p.Status().Firmware == "" {
		t.Fatal("no firmware")
	}
	mu.Lock()
	defer mu.Unlock()
	// The garbage from opening the port is expected, a late M115 reply isn't.
	if got := buf.String(); strings.Contains(got, "Cap:") || strings.Contains(got, `said: "FIRMWARE`) {
		t.Fatalf("stray lines: %s", got)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func TestPrinterStopsOnCancel(t *testing.T) {
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(sim.Options{}), nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitFor(t, "connection", func() bool { return p.Status().Connected })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop")
	}
}

func shortMotion(p *Printer) {
	p.Motion = MotionLimits{Idle: 100 * time.Millisecond, Home: time.Second, Probe: time.Second, Park: time.Second}
}

func connectedShort(t *testing.T, opts sim.Options) *Printer {
	t.Helper()
	p := NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(opts), nil })
	shortMotion(p)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	return p
}

type seen struct {
	mu    sync.Mutex
	lines []string
}

func (s *seen) add(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *seen) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lines)
}

func TestHomeProbeAndPark(t *testing.T) {
	var cmds seen
	p := connectedShort(t, sim.Options{
		Corners: sim.Corners{FL: 0.1, FR: 0.2, BR: 0.3, BL: -0.4}, MoveDelay: 200 * time.Millisecond, Seen: cmds.add,
	})
	ctx := context.Background()
	if err := p.Home(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := p.Probe(ctx, 30, 200)
	if err != nil || r.X != 30 || r.Y != 200 || r.Z != -0.4 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if err := p.Park(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cmds.got(), "|")
	if !strings.Contains(got, "G28|G30 X30 Y200|G90|G0 Z30 F600|G0 X115 Y115 F3000|M400") {
		t.Fatalf("got %s", got)
	}
}

func TestProbeFailures(t *testing.T) {
	tests := []struct {
		name string
		opts sim.Options
		want string
	}{
		{"probing failed", sim.Options{FailProbe: 1}, "Probing Failed"},
		{"wrong position", sim.Options{ProbeShiftX: 5}, "reports probing X35"},
		{"link drop", sim.Options{DropProbe: 1}, "disconnected"},
		{"halted", sim.Options{HaltProbe: 1}, "halted"},
		{"silent", sim.Options{StallOn: "G30", Stall: 5 * time.Second}, "no reply"},
		{"busy past the limit", sim.Options{StallOn: "G30", Stall: 5 * time.Second, StallBusy: true}, "didn't finish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := connectedShort(t, tt.opts)
			_, err := p.Probe(context.Background(), 30, 30)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestHaltFailsWithoutWaitingForTheTimeout(t *testing.T) {
	p := connectedShort(t, sim.Options{HaltProbe: 1})
	p.Motion.Idle = time.Minute
	start := time.Now()
	_, err := p.Probe(context.Background(), 30, 30)
	if !errors.Is(err, ErrHalted) || time.Since(start) > 5*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
}

// A move that's given up on still finishes, and its reply must not be taken for
// the next command's. Here that would make M27 fail, or the next probe wrong.
func TestLateReplyIsNotCreditedToTheNextCommand(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(lockedWriter{&mu, &buf})
	defer log.SetOutput(os.Stderr)

	p := connectedShort(t, sim.Options{
		Corners: sim.Corners{FL: 0.5, FR: 0.9}, StallOn: "G30", Stall: 500 * time.Millisecond,
	})
	ctx := context.Background()
	if _, err := p.Probe(ctx, 30, 30); err == nil {
		t.Fatal("expected a timeout")
	}
	// Waits out the old move, then answers for itself.
	printing, err := p.SDPrinting(ctx)
	if err != nil || printing {
		t.Fatalf("M27 got %v, %v", printing, err)
	}
	mu.Lock()
	log := buf.String()
	mu.Unlock()
	if !strings.Contains(log, "answered something earlier") || !strings.Contains(log, "Z:0.500") {
		t.Fatalf("the late reply wasn't discarded: %s", log)
	}
}

// While the old move is still running, nothing else can get through, and saying
// so is better than waiting or pretending.
func TestStuckMoveBlocksLaterCommandsWithAClearError(t *testing.T) {
	p := connectedShort(t, sim.Options{StallOn: "G30", Stall: time.Minute})
	ctx := context.Background()
	if _, err := p.Probe(ctx, 30, 30); err == nil {
		t.Fatal("expected a timeout")
	}
	_, err := p.SDPrinting(ctx)
	if err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("got %v", err)
	}
}

// The link stays usable after the reply has passed, for a probe as well.
func TestProbeWorksAfterResync(t *testing.T) {
	p := NewPrinter(func() (io.ReadWriteCloser, error) {
		return sim.New(sim.Options{Corners: sim.Corners{FL: 0.5, FR: 0.9}, StallOn: "G28", Stall: 300 * time.Millisecond}), nil
	})
	p.Motion = MotionLimits{Idle: 50 * time.Millisecond, Home: time.Second, Probe: time.Second, Park: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	if err := p.Home(ctx); err == nil {
		t.Fatal("expected a timeout")
	}
	r, err := p.Probe(ctx, 200, 30)
	if err != nil || r.Z != 0.9 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

// A link that drops mid-probe comes back by itself, ready for the next command.
func TestReconnectsAfterDropMidProbe(t *testing.T) {
	var mu sync.Mutex
	opens := 0
	p := NewPrinter(func() (io.ReadWriteCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		if opens == 1 {
			return sim.New(sim.Options{DropProbe: 1}), nil
		}
		return sim.New(sim.Options{Corners: sim.Corners{FL: 0.5}}), nil
	})
	shortMotion(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	waitFor(t, "temperatures", func() bool { return p.Status().Temps != nil })
	if _, err := p.Probe(ctx, 30, 30); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	waitFor(t, "disconnect", func() bool { return !p.Status().Connected })
	waitFor(t, "reconnect", func() bool { return p.Status().Temps != nil })
	if r, err := p.Probe(ctx, 30, 30); err != nil || r.Z != 0.5 {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestMeshInfoOnlyAsksForTheMap(t *testing.T) {
	var cmds seen
	p := connectedShort(t, sim.Options{Seen: cmds.add})
	lines, err := p.MeshInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParseMeshReport(lines)
	if err != nil {
		t.Fatalf("%v: %q", err, lines)
	}
	if !r.Valid || len(r.Z) != 5 || r.Z[0][0] == nil {
		t.Errorf("got %+v", r)
	}
	for _, cmd := range cmds.got() {
		if strings.HasPrefix(cmd, "G") || (strings.HasPrefix(cmd, "M420") && cmd != MeshCommand) {
			t.Errorf("sent %s", cmd)
		}
	}
}

func TestTramInfoMovesNothing(t *testing.T) {
	var cmds seen
	p := connectedShort(t, sim.Options{Seen: cmds.add})
	lines, err := p.TramInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"> M851", "M851 X-40", "tramline-check", "MESH_MIN_X"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q: %s", want, got)
		}
	}
	for _, cmd := range cmds.got() {
		if strings.HasPrefix(cmd, "G0") || strings.HasPrefix(cmd, "G28") || strings.HasPrefix(cmd, "G30") {
			t.Errorf("sent %s", cmd)
		}
	}
}
