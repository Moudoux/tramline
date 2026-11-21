package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/sim"
)

// streamRig has a file on the Pi with no copy on the card, so it streams.
func streamRig(t *testing.T, opts sim.Options, lines int) (*uploadRig, string) {
	t.Helper()
	r := newUploadRig(t, opts)
	const name = "stream.gcode"
	r.put(t, name, gcodeOf(lines))
	return r, name
}

func (r *uploadRig) fileLines() []string {
	var out []string
	for _, line := range r.lines() {
		if strings.HasPrefix(line, "G1 ") {
			out = append(out, line)
		}
	}
	return out
}

func TestStreamRunsAfterTheTramCheckToTheEnd(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: time.Millisecond}, 50)
	r.startPrint(t, name)
	r.waitFor(t, "some of the file", func() bool { return r.s.printJob().Pos > 0 })
	if job := r.s.printJob(); job.Pos > job.Size {
		t.Errorf("M73 lines were counted: %+v", job)
	}
	job := r.waitPrint(t, PrintFinished)
	rec := r.record(t, name)
	if job.Mode != ModeStream || job.Pos != rec.Info.SDSize || job.Size != rec.Info.SDSize || job.Error != "" || job.Resends != 0 {
		t.Errorf("job: %+v", job)
	}
	lines := r.lines()
	firstMove := slices.Index(lines, "G1 X0 Y0")
	order := []string{"G28", "M108", "M86", "M85 S300"}
	at := 0
	for _, want := range order {
		i := slices.IndexFunc(lines[at:], func(l string) bool { return strings.HasPrefix(l, want) })
		if i < 0 {
			t.Fatalf("%s is missing or out of order: %q", want, lines[:firstMove+1])
		}
		at += i
	}
	if at >= firstMove {
		t.Errorf("the file started before it was ready: %q", lines[:firstMove+1])
	}
	after := lines[firstMove:]
	for _, want := range []string{"M400", "M118 tramline-end-1", "M85 S0"} {
		if !slices.Contains(after, want) {
			t.Errorf("%s is missing after the file: %q", want, after[len(after)-6:])
		}
	}
	for _, word := range []string{"M23", "M24", "M28", "M27"} {
		for _, line := range after {
			if strings.HasPrefix(line, word) {
				t.Errorf("%s shouldn't be sent during a stream", word)
			}
		}
	}
	if len(r.fileLines()) != 50 {
		t.Errorf("%d lines ran", len(r.fileLines()))
	}
	if len(r.card.Names()) != 0 {
		t.Errorf("the card has %q", r.card.Names())
	}
}

func TestVerifiedCopyStillPrintsFromTheCard(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 100 * time.Millisecond})
	r.startPrint(t, name)
	if job := r.waitPrint(t, PrintFinished); job.Mode != ModeSD {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M85") != 0 {
		t.Error("M85 is only for a stream")
	}
}

func TestAFailedTramCheckHoldsTheStreamBack(t *testing.T) {
	k := &knobs{bed: outOfTram}
	r, name := streamRig(t, sim.Options{Bed: k.get}, 5)
	r.startPrint(t, name)
	r.waitPrint(t, PrintAdjusting)
	if r.sawCommand("M85") != 0 || len(r.fileLines()) != 0 {
		t.Fatalf("it went ahead: %q", r.lines())
	}
	k.turn(inTram)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintFinished)
	if len(r.fileLines()) != 5 {
		t.Errorf("%d lines ran", len(r.fileLines()))
	}
}

func TestStreamCancelStopsTheHeadWithoutHoming(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	r.waitFor(t, "some lines", func() bool { return len(r.fileLines()) > 3 })
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := r.waitPrint(t, PrintCancelled)
	if job.Note != "cancelled on request" {
		t.Errorf("job: %+v", job)
	}
	lines := r.lines()
	i := slices.Index(lines, "M104 S0")
	// The first M104 S0 could only be from this.
	want := []string{"M104 S0", "M140 S0", "M107", "G91", "G0 Z5 F600", "G90", "M85 S0", "M84"}
	if i < 0 || !slices.Equal(lines[i:i+len(want)], want) {
		t.Fatalf("got %q", lines)
	}
	if r.sawCommand("M524") != 0 {
		t.Error("M524 only acts on the card")
	}
	first := slices.Index(lines, "G1 X0 Y0")
	if slices.Contains(lines[first:], "G28") {
		t.Error("it homed, so the head crossed the part")
	}
	if len(r.fileLines()) >= 5000 {
		t.Error("the file kept going")
	}
	r.waitTargets(t, 0, 0)
	if !r.plug.on {
		t.Error("the plug was cut")
	}
	// The link has to be in step for what comes next.
	r.startPrint(t, name)
	if job := r.waitPrint(t, PrintRunning); job.ID != 2 {
		t.Errorf("job: %+v", job)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func heatFile(n int) string {
	return "M190 S80\nM109 S230\n" + gcodeOf(n)
}

func TestStreamCancelEndsAHeaterWait(t *testing.T) {
	r := newUploadRig(t, sim.Options{HeatWait: time.Hour})
	const name = "heat.gcode"
	r.put(t, name, heatFile(5))
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.waitFor(t, "the heater wait", func() bool { return r.sawCommand("M190") == 1 })
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s", took)
	}
	r.waitTargets(t, 0, 0)
}

func TestCoolDuringAStreamCancelsIt(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	if w := call(r.s.handleCool, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "cancelling") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.waitPrint(t, PrintCancelled); !strings.Contains(job.Note, "cool") {
		t.Errorf("job: %+v", job)
	}
	r.waitTargets(t, 0, 0)
}

func TestEstopDuringAStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	r.waitFor(t, "some lines", func() bool { return len(r.fileLines()) > 3 })
	if w := call(r.s.handleEstop, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.waitPrint(t, PrintFailed); !strings.Contains(job.Error, "emergency") {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M112") != 1 || r.plug.on {
		t.Errorf("M112 %d, plug %t", r.sawCommand("M112"), r.plug.on)
	}
	r.waitFor(t, "the stream to let go", func() bool {
		r.s.print.mu.Lock()
		defer r.s.print.mu.Unlock()
		return !r.s.print.watching
	})
	if r.sawCommand("M84") != 0 {
		t.Error("nothing should be sent after an emergency stop")
	}
}

func TestStuckStreamIsCancelled(t *testing.T) {
	r := newUploadRig(t, sim.Options{HeatWait: time.Hour})
	const name = "heat.gcode"
	r.put(t, name, heatFile(5))
	var mu sync.Mutex
	clock := time.Now()
	r.s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	time.Sleep(100 * time.Millisecond)
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

func TestAnsweredLinesKeepTheStreamMovingAndTheTimerOn(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 10 * time.Millisecond}, 5000)
	var mu sync.Mutex
	clock := time.Now()
	r.s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	start := len(r.fileLines())
	mu.Lock()
	clock = clock.Add(HeatIdleLimit - time.Minute)
	mu.Unlock()
	r.waitFor(t, "more lines answered", func() bool {
		job := r.s.printJob()
		return len(r.fileLines()) > start+2 && job.MovedAt.Equal(r.s.Now())
	})
	if r.s.offAt().Sub(r.s.Now()) != HeatIdleLimit {
		t.Errorf("the timer wasn't moved on: %s", r.s.offAt().Sub(r.s.Now()))
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestLostLinkFailsTheStreamAndTheNextConnectionCleansUp(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: time.Millisecond, DropLinkLine: 150}, 5000)
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintFailed)
	if !strings.Contains(job.Error, "lost the link") {
		t.Errorf("job: %+v", job)
	}
	if r.s.offAt().IsZero() {
		t.Error("the heater timer has to keep running, for the plug cut")
	}
	if !r.plug.on {
		t.Error("the plug was cut")
	}
	r.s.setPrinting(false)
	before := r.sawCommand("M85")
	// Nothing heats until the old stream has been dealt with.
	if w := call(r.s.handlePreheat, `{"bed":50,"nozzle":0}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cut off") {
		t.Errorf("preheat: %d %s", w.Code, w.Body)
	}
	r.waitFor(t, "reconnected", func() bool {
		st := r.printer.Status()
		return st.Connected && st.Temps != nil
	})
	r.s.checkHeaters(context.Background())
	if r.s.strayPending() {
		t.Fatal("it should be dealt with")
	}
	if r.sawCommand("M85") == before || r.sawCommand("M104") == 0 {
		t.Errorf("M85 %d, M104 %d", r.sawCommand("M85"), r.sawCommand("M104"))
	}
	r.waitTargets(t, 0, 0)
	if job := r.s.printJob(); job.State != PrintFailed {
		t.Errorf("no resume: %+v", job)
	}
}

func TestPlugOffDuringAStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: time.Millisecond, DropLinkLine: 150}, 5000)
	r.plug.on = true
	r.startPrint(t, name)
	r.waitFor(t, "the print to start", func() bool { return r.s.printJob().State == PrintRunning })
	r.plug.on = false
	if job := r.waitPrint(t, PrintFailed); !strings.Contains(job.Error, "plug") {
		t.Errorf("job: %+v", job)
	}
	if r.s.strayPending() {
		t.Error("there's nothing to clean up with no power")
	}
}

func TestRestartFailsARunningStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	// The first server stopping, while the printer carries on.
	first, stop := context.WithCancel(context.Background())
	defer stop()
	r.s.ctx = first
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.waitFor(t, "some lines", func() bool { return len(r.fileLines()) > 3 })
	old := r.s
	stop()
	r.waitFor(t, "the first server to let go", func() bool {
		old.print.mu.Lock()
		defer old.print.mu.Unlock()
		return !old.print.watching
	})
	if r.sawCommand("M84") != 0 {
		t.Error("a stopping server only leaves the saved job")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	job := s.printJob()
	if job.State != PrintFailed || !strings.Contains(job.Error, "restarted") || job.FinishedAt == nil {
		t.Fatalf("job: %+v", job)
	}
	if !s.strayPending() || s.offAt().IsZero() {
		t.Errorf("stray %t, off at %s", s.strayPending(), s.offAt())
	}
	sent := len(r.fileLines())
	s.checkHeaters(ctx)
	if s.strayPending() {
		t.Fatal("the heaters should be off by now")
	}
	r.waitTargets(t, 0, 0)
	if !slices.Contains(r.lines(), "M85 S0") {
		t.Error("M85 wasn't cleared")
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(r.fileLines()); got > sent+streamQueuedMax {
		t.Errorf("%d more lines ran after the restart", got-sent)
	}
}

// What could still be in the firmware's queue when sending stops.
const streamQueuedMax = 4

func TestLCDStopCancelsAStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.s.PrinterLine(marlin.ActionCancel)
	if job := r.waitPrint(t, PrintCancelled); !strings.Contains(job.Note, "LCD") {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M84") != 1 {
		t.Error("it should stop the printer as a cancel does")
	}
}

func TestLCDStopOnAnSDPrintIsLeftToTheBoard(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 300 * time.Millisecond})
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	r.s.PrinterLine(marlin.ActionCancel)
	if job := r.s.printJob(); job.State != PrintRunning || r.s.print.halted != "" {
		t.Errorf("job: %+v, halted %q", job, r.s.print.halted)
	}
	r.waitPrint(t, PrintFinished)
}

// The bar has to move on the LCD, and M73 mustn't count as part of the file.
func TestStreamSendsProgressForTheLCD(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	const name = "timed.gcode"
	r.put(t, name, gcodeOf(100))
	r.startPrint(t, name)
	r.waitPrint(t, PrintFinished)
	var seen []string
	for _, line := range r.lines() {
		if strings.HasPrefix(line, "M73") {
			seen = append(seen, line)
		}
	}
	if len(seen) < 50 || !strings.HasPrefix(seen[len(seen)-1], "M73 P100") {
		t.Errorf("got %q", seen)
	}
	if !strings.Contains(seen[0], " R2") {
		t.Errorf("the file says 100 seconds, so 2 minutes left: %q", seen[0])
	}
}

func TestDeleteAndReplaceAreRefusedDuringAStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	if w := named(r.s.handleFileDelete, http.MethodDelete, name, "", nil); w.Code != http.StatusConflict {
		t.Errorf("delete: %d %s", w.Code, w.Body)
	}
	body := gcodeOf(3)
	w := named(r.s.handleFilePut, http.MethodPut, name, body, map[string]string{HashHeader: hashOf(body)})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "streamed") {
		t.Errorf("replace: %d %s", w.Code, w.Body)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestFileTroubleMidStreamStopsThePrinter(t *testing.T) {
	r, name := streamRig(t, sim.Options{}, 5)
	dir, err := r.s.uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	bad := gcodeOf(5) + "M28 NOPE\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(bad), 0o640); err != nil {
		t.Fatal(err)
	}
	// The record says it's this file, so it gets past the check at the start, as a file
	// changing after that would.
	rec := r.record(t, name)
	rec.SHA256 = hashOf(bad)
	if err := r.s.saveRecord(rec); err != nil {
		t.Fatal(err)
	}
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintFailed)
	if !strings.Contains(job.Error, "can't be sent") {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M84") != 1 || !slices.Contains(r.lines(), "M85 S0") {
		t.Errorf("it should have stopped the printer: %q", r.lines())
	}
	r.waitTargets(t, 0, 0)
}

func TestResendsDuringAStreamAreCountedAndRecovered(t *testing.T) {
	r, name := streamRig(t, sim.Options{DropLine: 120, CorruptLine: 130}, 200)
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintFinished)
	if job.Resends < 2 {
		t.Errorf("job: %+v", job)
	}
	got := r.fileLines()
	var want []string
	for _, line := range strings.Split(strings.TrimSpace(gcodeOf(200)), "\n")[1:] {
		want = append(want, strings.TrimSpace(strings.SplitN(line, ";", 2)[0]))
	}
	if !slices.Equal(got, want) {
		t.Errorf("%d lines ran, in the wrong order or repeated", len(got))
	}
}

// M85 firing is a real outcome: the firmware halts, and no commands can follow.
func TestM85KillEndsTheStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{SilentFromLine: 150, KillAfter: 150 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintFailed)
	if !strings.Contains(job.Error, "halted") {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M84") != 0 {
		t.Error("nothing can be sent to a halted printer")
	}
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(b)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	logged := &lockedLog{}
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	return logged
}

func TestTemperaturesAreLoggedAboutOnceAMinuteDuringAJob(t *testing.T) {
	logged := captureLog(t)
	st := marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 229.6, NozzleTarget: 230, Bed: 75, BedTarget: 75}}
	s := &Server{Printer: func() marlin.Status { return st }}
	s.logTemps()
	if strings.Contains(logged.String(), "229.6") {
		t.Error("there's no job")
	}
	s.print.job = &PrintJob{ID: 3, State: PrintRunning, Pos: 10, Size: 20}
	s.logTemps()
	s.logTemps()
	if got := strings.Count(logged.String(), "nozzle 229.6/230 C, bed 75.0/75 C"); got != 1 {
		t.Errorf("logged %d times: %s", got, logged.String())
	}
	s.print.tempsLogged = time.Now().Add(-tempLogEvery)
	s.logTemps()
	if got := strings.Count(logged.String(), "nozzle 229.6"); got != 2 {
		t.Errorf("logged %d times", got)
	}
}

func TestEachM27WaitIsLogged(t *testing.T) {
	logged := captureLog(t)
	r, name := printRig(t, sim.Options{PrintTime: 200 * time.Millisecond})
	r.startPrint(t, name)
	r.waitPrint(t, PrintFinished)
	if !strings.Contains(logged.String(), "M27 took") {
		t.Errorf("logged: %s", logged.String())
	}
}
