package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/sim"
)

func printRig(t *testing.T, opts sim.Options) (*uploadRig, string) {
	t.Helper()
	r := newUploadRig(t, opts)
	const name = "tile.gcode"
	r.put(t, name, gcodeOf(5))
	if w := named(r.s.handleCopy, http.MethodPost, name, "", nil); w.Code != http.StatusOK {
		t.Fatalf("copy: %s", w.Body)
	}
	r.waitFor(t, "the copy", func() bool { return !r.s.copyRunning() })
	return r, name
}

func (r *uploadRig) startPrint(t *testing.T, name string) {
	t.Helper()
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("print: %d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the print to get past its tram check", func() bool {
		job := r.s.printJob()
		return !job.preparing() || job.State == PrintAdjusting
	})
}

func (r *uploadRig) waitPrint(t *testing.T, state string) *PrintJob {
	t.Helper()
	r.waitFor(t, "print state "+state, func() bool {
		job := r.s.printJob()
		return job != nil && job.State == state
	})
	return r.s.printJob()
}

func TestPrintRefusals(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{`{"name":"` + name + `"}`, http.StatusBadRequest, "bed is clear"},
		{`{"name":"nope.gcode","bed_clear":true}`, http.StatusNotFound, "isn't on the server"},
		{`{"name":"../x.gcode","bed_clear":true}`, http.StatusBadRequest, "file name"},
	} {
		if w := call(r.s.handlePrint, c.body); w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.body, w.Code, w.Body)
		}
	}
	if r.sawCommand("M23") != 0 || r.sawCommand("M24") != 0 {
		t.Error("nothing should have been started")
	}
}

func TestPrintRefusedWhenTheCardDiffers(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	rec, err := r.s.loadRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	r.card.Put(rec.SDName, "short")
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "it should be") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestPrintRunsToTheEnd(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 100 * time.Millisecond})
	r.startPrint(t, name)
	if r.sawCommand("M108") == 0 || r.sawCommand("M23") != 1 || r.sawCommand("M24") != 1 {
		t.Errorf("start commands: M108 %d, M23 %d, M24 %d", r.sawCommand("M108"), r.sawCommand("M23"), r.sawCommand("M24"))
	}
	job := r.waitPrint(t, PrintFinished)
	if job.FinishedAt == nil || job.Mode != ModeSD || job.Error != "" {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M524") != 0 {
		t.Error("a finished print shouldn't be cancelled")
	}
}

func TestOtherCommandsAreRefusedDuringAPrint(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	for _, c := range []struct {
		what string
		code int
	}{
		{"print", call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`).Code},
		{"preheat", call(r.s.handlePreheat, `{"bed":80,"nozzle":150}`).Code},
		{"tram", call(r.s.handleTram, `{"bed_clear":true}`).Code},
		{"power off", call(r.s.handlePower, `{"on":false}`).Code},
		{"copy", named(r.s.handleCopy, http.MethodPost, name, "", nil).Code},
		{"delete", named(r.s.handleFileDelete, http.MethodDelete, name, "", nil).Code},
	} {
		if c.code != http.StatusConflict {
			t.Errorf("%s: got %d", c.what, c.code)
		}
	}
	if r.plug.on != true {
		t.Error("the plug was cut")
	}
	if r.sawCommand("M28") != 1 {
		t.Error("a copy started during a print")
	}
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
}

func TestCancel(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
	if r.card.Printing() {
		t.Error("the board is still printing")
	}
	if r.sawCommand("M400") == 0 {
		t.Error("it should wait for the head to stop")
	}
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusConflict {
		t.Errorf("no print to cancel: %d", w.Code)
	}
	// The link must be in step afterwards, whatever M524's clearing lost.
	r.startPrint(t, name)
	if job := r.waitPrint(t, PrintRunning); job.ID != 2 {
		t.Errorf("job: %+v", job)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestCancelEndsAHeaterWait(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, PrintHold: time.Minute})
	r.startPrint(t, name)
	// An M27 is now stuck behind the wait, and answers nothing but temperatures.
	r.waitFor(t, "an M27 to be waiting", func() bool { return r.s.printJob() != nil })
	time.Sleep(50 * time.Millisecond)
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
}

func TestCoolDuringAPrintCancelsIt(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	w := call(r.s.handleCool, "")
	var out CoolResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || !out.Cancelling {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := r.waitPrint(t, PrintCancelled)
	if !strings.Contains(job.Note, "cool") {
		t.Errorf("job: %+v", job)
	}
}

func TestEstopDuringAPrint(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	w := call(r.s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.M112 || !out.PlugOff {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.sawCommand("M112") != 1 {
		t.Error("M112 wasn't sent")
	}
	if r.plug.on {
		t.Error("the plug is on")
	}
	if job := r.waitPrint(t, PrintFailed); !strings.Contains(job.Error, "emergency") {
		t.Errorf("job: %+v", job)
	}
}

func TestEstopWithFailedPlugStillStops(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	r.plug.setErr = io.ErrUnexpectedEOF
	w := call(r.s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.M112 || out.PlugOff || len(out.Problems) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestEstopDuringACopyCutsThePlugWithoutM112(t *testing.T) {
	r := newUploadRig(t, sim.Options{LineDelay: time.Millisecond})
	const name = "big.gcode"
	r.put(t, name, gcodeOf(2000))
	if w := named(r.s.handleCopy, http.MethodPost, name, "", nil); w.Code != http.StatusOK {
		t.Fatalf("copy: %s", w.Body)
	}
	r.waitFor(t, "copy started", r.s.copyRunning)
	w := call(r.s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.M112 || !out.PlugOff {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.sawCommand("M112") != 0 {
		t.Error("M112 is ignored during a save")
	}
	r.waitFor(t, "the copy to stop", func() bool { return !r.s.copyRunning() })
}

func TestEstopDuringACopyWithFailedPlugEndsTheSaveThenM112(t *testing.T) {
	r := newUploadRig(t, sim.Options{LineDelay: time.Millisecond})
	const name = "big.gcode"
	r.put(t, name, gcodeOf(2000))
	if w := named(r.s.handleCopy, http.MethodPost, name, "", nil); w.Code != http.StatusOK {
		t.Fatalf("copy: %s", w.Body)
	}
	r.waitFor(t, "copy started", r.s.copyRunning)
	r.plug.setErr = io.ErrUnexpectedEOF
	w := call(r.s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.M112 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.sawCommand("M112") != 1 {
		t.Error("M112 should follow the end of the save")
	}
}

func TestEstopWhenIdleWithNoLink(t *testing.T) {
	plug := &fakePlug{on: true}
	s := &Server{Plug: plug, Printer: func() marlin.Status { return marlin.Status{} }}
	w := call(s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.M112 || !out.PlugOff || plug.on {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestStuckPrintIsCancelled(t *testing.T) {
	// This long a print of this small a file doesn't move on in the time the test takes.
	r, name := printRig(t, sim.Options{PrintTime: 100 * time.Hour})
	var mu sync.Mutex
	clock := time.Now()
	r.s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	time.Sleep(50 * time.Millisecond)
	if job := r.s.printJob(); job.State != PrintRunning {
		t.Fatalf("it shouldn't be cancelled yet: %+v", job)
	}
	mu.Lock()
	clock = clock.Add(HeatIdleLimit)
	mu.Unlock()
	if job := r.waitPrint(t, PrintCancelled); !strings.Contains(job.Note, "hasn't moved") {
		t.Errorf("job: %+v", job)
	}
}

// Starting a second server is what a restart looks like to the printer.
func restarted(r *uploadRig, ctx context.Context) *Server {
	s := &Server{
		Plug: r.plug, Heaters: r.printer, Card: r.printer, Prober: r.printer, Prints: r.printer,
		Printer: r.printer.Status, DataDir: r.s.DataDir, ctx: ctx, HeatHold: time.Millisecond,
	}
	s.print.every = 10 * time.Millisecond
	s.print.grace = 50 * time.Millisecond
	s.loadPrintJob()
	r.s = s
	return s
}

func TestRestartPicksUpARunningPrint(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 300 * time.Millisecond})
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	if job := s.printJob(); job == nil || job.State != PrintRunning {
		t.Fatalf("job: %+v", job)
	}
	if !s.adoptPrint(ctx) {
		t.Fatal("it should be followed")
	}
	if job := s.printJob(); job.Name != name || !strings.Contains(job.Note, "restart") {
		t.Errorf("job: %+v", job)
	}
	r.waitPrint(t, PrintFinished)
}

// The real board took 16 seconds to answer behind its queue, and much longer is
// possible, so asking must neither give up nor hold anything else up meanwhile.
func TestRestartWaitsForASlowAnswer(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, PrintHold: 500 * time.Millisecond})
	r.startPrint(t, name)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	start := time.Now()
	if !s.checkAdopt(ctx) {
		t.Fatal("a running print should stop the heater check")
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("it held the heater check up for %s", took)
	}
	r.waitFor(t, "the print to be picked up", func() bool {
		s.print.mu.Lock()
		defer s.print.mu.Unlock()
		return s.print.watching && strings.Contains(s.print.job.Note, "restart")
	})
	if w := call(s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
}

func TestRestartAfterThePrintEnded(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.card.StopPrint()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	if s.adoptPrint(ctx) {
		t.Fatal("nothing is running")
	}
	if job := s.printJob(); job.State != PrintUnknown {
		t.Errorf("job: %+v", job)
	}
}

func TestPrintFromTheLCDIsFollowed(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.card.Put("LCD.GCO", "G1 X1\r\n")
	r.card.StartPrint("LCD.GCO", 200*time.Millisecond)
	if !r.s.adoptPrint(context.Background()) {
		t.Fatal("it should be followed")
	}
	if job := r.s.printJob(); !job.Elsewhere || job.SDName != "LCD.GCO" {
		t.Errorf("job: %+v", job)
	}
	r.waitPrint(t, PrintFinished)
}

func TestPrintStoppedFromTheLCDIsNoticed(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.card.StopPrint()
	if job := r.waitPrint(t, PrintUnknown); !strings.Contains(job.Note, "without finishing") {
		t.Errorf("job: %+v", job)
	}
}

func TestPlugOffEndsAPrint(t *testing.T) {
	plug := &fakePlug{on: false}
	st := marlin.Status{}
	s := &Server{Plug: plug, Printer: func() marlin.Status { return st }, Prints: nil, ctx: context.Background()}
	s.print.job = &PrintJob{ID: 1, State: PrintRunning}
	var ended time.Time
	if !s.printStep(context.Background(), 1, &ended) {
		t.Fatal("it should be over")
	}
	if job := s.printJob(); job.State != PrintFailed || !strings.Contains(job.Error, "plug") {
		t.Errorf("job: %+v", job)
	}
}

func TestLostLinkKeepsThePrintAndPower(t *testing.T) {
	plug := &fakePlug{on: true}
	s := &Server{Plug: plug, Printer: func() marlin.Status { return marlin.Status{} }, ctx: context.Background()}
	s.print.job = &PrintJob{ID: 1, State: PrintRunning}
	var ended time.Time
	if s.printStep(context.Background(), 1, &ended) {
		t.Fatal("it should carry on")
	}
	job := s.printJob()
	if job.State != PrintRunning || !job.LinkLost {
		t.Errorf("job: %+v", job)
	}
	if _, problem := s.heatInfo(); !strings.Contains(problem, "power was left on") {
		t.Errorf("problem: %q", problem)
	}
	if !plug.on {
		t.Error("the plug was cut")
	}
	s.checkAdopt(context.Background())
	if !s.printRunning() {
		t.Error("still running")
	}
}

func TestHaltEndsAPrint(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.s.PrinterLine("Error:Printer halted. kill() called!")
	if job := r.waitPrint(t, PrintFailed); !strings.Contains(job.Error, "halted") {
		t.Errorf("job: %+v", job)
	}
}
