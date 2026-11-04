package server

import (
	"context"
	"encoding/json"
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

// A bed that fails naming back-left, until the test turns the knob.
var (
	outOfTram = sim.Corners{FL: 0.05, FR: 0.05, BR: 0.05, BL: 0.15}
	inTram    = sim.Corners{FL: 0.05, FR: 0.05, BR: 0.05, BL: 0.06}
)

type knobs struct {
	mu  sync.Mutex
	bed sim.Corners
}

func (k *knobs) get() sim.Corners {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bed
}

func (k *knobs) turn(bed sim.Corners) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bed = bed
}

// adjustingRig is a print waiting for back-left to be adjusted.
func adjustingRig(t *testing.T, opts sim.Options) (*uploadRig, string, *knobs) {
	t.Helper()
	k := &knobs{bed: outOfTram}
	opts.Bed = k.get
	if opts.PrintTime == 0 {
		opts.PrintTime = time.Minute
	}
	r, name := printRig(t, opts)
	r.startPrint(t, name)
	r.waitPrint(t, PrintAdjusting)
	return r, name, k
}

func (r *uploadRig) targets() (bed, nozzle float64) {
	t := r.printer.Status().Temps
	return t.BedTarget, t.NozzleTarget
}

func (r *uploadRig) waitTargets(t *testing.T, bed, nozzle float64) {
	t.Helper()
	r.waitFor(t, "the heater targets", func() bool {
		b, n := r.targets()
		return b == bed && n == nozzle
	})
}

func (r *uploadRig) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

func TestPrintHeatsAndChecksTramBeforeStarting(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Corners: inTram})
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintRunning)
	if job.Checks != 1 || job.Check == nil || !job.Check.Pass {
		t.Errorf("job: %+v", job)
	}
	var order []string
	for _, want := range []string{"M108", "M140 S80", "M104 S150", "M117 Heating bed", "M117 Checking tram", "G28", "G30", "M400", "M117 Starting print", "M23", "M24"} {
		for i, line := range r.lines() {
			if strings.HasPrefix(line, want) {
				order = append(order, want)
				if len(order) > 1 && i < r.firstLine(order[len(order)-2]) {
					t.Errorf("%s came before %s", want, order[len(order)-2])
				}
				break
			}
		}
	}
	if len(order) != 11 {
		t.Errorf("only saw %v in %q", order, r.lines())
	}
	if r.sawCommand("G30") != 4 {
		t.Errorf("G30s: %d", r.sawCommand("G30"))
	}
	if st := r.s.tramJob(); st == nil || st.Result == nil || !st.Result.Pass {
		t.Errorf("tram: %+v", st)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func (r *uploadRig) firstLine(prefix string) int {
	return slices.IndexFunc(r.lines(), func(line string) bool { return strings.HasPrefix(line, prefix) })
}

func TestFailedCheckWaitsThenRechecksUntilItPasses(t *testing.T) {
	r, name, k := adjustingRig(t, sim.Options{})
	job := r.s.printJob()
	if job.Check == nil || job.Check.Pass || job.Check.Corner != "back-left" || job.Check.Action != "lower" || !job.Parked {
		t.Fatalf("job: %+v", job)
	}
	// The job goes to adjusting just before the message.
	r.waitFor(t, "the LCD to be told", func() bool { return r.firstLine("M117 Lower back-left CCW") >= 0 })
	if r.firstLine("G0 Z30") < 0 || r.firstLine("M400") < 0 {
		t.Error("the head should be parked")
	}
	if bed, nozzle := r.targets(); bed != TramBed || nozzle != TramNozzle {
		t.Errorf("the heaters should stay on: %v %v", bed, nozzle)
	}
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
	if r.sawCommand("M23") != 0 {
		t.Fatal("the file started without a pass")
	}

	// Not adjusted enough: it asks again.
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the second check", func() bool {
		job := r.s.printJob()
		return job.State == PrintAdjusting && job.Checks == 2
	})
	if r.sawCommand("M23") != 0 {
		t.Fatal("the file started without a pass")
	}

	k.turn(inTram)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	job = r.waitPrint(t, PrintRunning)
	if job.Checks != 3 || !job.Check.Pass || job.Parked {
		t.Errorf("job: %+v", job)
	}
	// Each check homes, as the steppers will have timed out during the wait.
	if r.sawCommand("G28") != 3 || r.sawCommand("G30") != 12 || r.sawCommand("M23") != 1 {
		t.Errorf("G28 %d, G30 %d, M23 %d", r.sawCommand("G28"), r.sawCommand("G30"), r.sawCommand("M23"))
	}
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusConflict {
		t.Errorf("nothing is waiting: %d", w.Code)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestContinueIsRefusedWithoutAPrintOrPrinter(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	r.plug.on = false
	w := call(r.s.handlePrintContinue, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "plug") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if job := r.s.printJob(); job.State != PrintAdjusting {
		t.Errorf("job: %+v", job)
	}
	r.plug.on = true
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusConflict {
		t.Errorf("after a cancel: %d", w.Code)
	}
}

func TestCancelWhileAdjusting(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	moves := r.sawCommand("G0")
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := r.s.printJob()
	if job.State != PrintCancelled || job.Stopping || job.FinishedAt == nil {
		t.Errorf("job: %+v", job)
	}
	r.waitTargets(t, 0, 0)
	if r.sawCommand("M524") != 0 || r.sawCommand("M23") != 0 || r.sawCommand("G0") != moves {
		t.Error("there's no print to abort, and the head should stay parked")
	}
	if i := slices.Index(r.lines(), "M117"); i < r.firstLine("M104 S0") {
		t.Errorf("the LCD should be cleared after M104's own message: %q", r.lines())
	}
	if offAt, _ := r.s.heatInfo(); offAt != nil {
		t.Errorf("timer still set: %v", offAt)
	}
}

func TestCoolWhileAdjustingCancels(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	w := call(r.s.handleCool, "")
	var out CoolResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Cancelling {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.waitPrint(t, PrintCancelled); !strings.Contains(job.Note, "cool") {
		t.Errorf("job: %+v", job)
	}
	r.waitTargets(t, 0, 0)
}

func TestCancelWhileHeatingIsImmediate(t *testing.T) {
	r, name := printRig(t, sim.Options{HeatRate: 1})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.s.printJob(); job.State != PrintHeating {
		t.Fatalf("job: %+v", job)
	}
	r.waitTargets(t, TramBed, TramNozzle)
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
	r.waitTargets(t, 0, 0)
	if r.sawCommand("G28") != 0 {
		t.Error("nothing should have moved")
	}
}

func TestCancelDuringACheckFinishesTheStepAndParks(t *testing.T) {
	r, name := printRig(t, sim.Options{MoveDelay: 150 * time.Millisecond})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the first probe", func() bool { return r.sawCommand("G30") == 1 })
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.s.printJob(); !job.Stopping || job.State != PrintChecking {
		t.Errorf("it should wait for the step: %+v", job)
	}
	r.waitPrint(t, PrintCancelled)
	r.waitTargets(t, 0, 0)
	if n := r.sawCommand("G30"); n >= 4 {
		t.Errorf("it carried on probing: %d", n)
	}
	if r.firstLine("G0 Z30") < 0 {
		t.Error("it should park")
	}
	if r.sawCommand("M23") != 0 || r.sawCommand("M524") != 0 {
		t.Error("nothing should have started or been aborted")
	}
}

func TestEstopWhileAdjusting(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	w := call(r.s.handleEstop, "")
	var out EstopResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.M112 || !out.PlugOff {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if job := r.s.printJob(); job.State != PrintFailed || !strings.Contains(job.Error, "emergency") {
		t.Errorf("job: %+v", job)
	}
}

func TestEstopDuringACheck(t *testing.T) {
	r, name := printRig(t, sim.Options{MoveDelay: 100 * time.Millisecond})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the first probe", func() bool { return r.sawCommand("G30") == 1 })
	call(r.s.handleEstop, "")
	if job := r.s.printJob(); job.State != PrintFailed {
		t.Errorf("job: %+v", job)
	}
	r.waitFor(t, "the check to stop", func() bool { return !r.s.tramRunning() })
	if r.sawCommand("M23") != 0 {
		t.Error("the file started")
	}
}

func TestFailedProbeEndsTheJobWithHeatersOffAndNoParking(t *testing.T) {
	r, name := printRig(t, sim.Options{FailProbe: 2})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := r.waitPrint(t, PrintFailed)
	if !strings.Contains(job.Error, "tram check failed") {
		t.Errorf("job: %+v", job)
	}
	r.waitTargets(t, 0, 0)
	if r.firstLine("G0 Z30") >= 0 || r.sawCommand("M23") != 0 {
		t.Error("it shouldn't move on after a failed command")
	}
}

func TestHeatersChangedWhileHeatingWaitsForTheUser(t *testing.T) {
	r, name := printRig(t, sim.Options{HeatRate: 1})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitTargets(t, TramBed, TramNozzle)
	if err := r.printer.SetTargets(context.Background(), 60, 150); err != nil {
		t.Fatal(err)
	}
	job := r.waitPrint(t, PrintAdjusting)
	if job.Note != changedNote || job.Parked {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("G28") != 0 {
		t.Error("nothing should have moved")
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestHaltWhileAdjustingEndsTheJob(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	r.s.PrinterLine("Error:Printer halted. kill() called!")
	if job := r.waitPrint(t, PrintFailed); !strings.Contains(job.Error, "halted") {
		t.Errorf("job: %+v", job)
	}
}

func TestIdleTimerStillRunsWhileAdjustingAndContinueReheats(t *testing.T) {
	r, _, k := adjustingRig(t, sim.Options{})
	var mu sync.Mutex
	clock := time.Now()
	r.s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	ctx := context.Background()
	r.s.setOffAt(clock.Add(HeatIdleLimit))
	r.s.checkHeaters(ctx)
	if bed, _ := r.targets(); bed != TramBed {
		t.Fatalf("it's too soon for the timer: %v", bed)
	}
	mu.Lock()
	clock = clock.Add(HeatIdleLimit)
	mu.Unlock()
	r.s.checkHeaters(ctx)
	r.waitTargets(t, 0, 0)
	job := r.s.printJob()
	if job.State != PrintAdjusting || job.Note != idleNote {
		t.Fatalf("it should still wait: %+v", job)
	}
	k.turn(inTram)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	offAt, _ := r.s.heatInfo()
	if offAt == nil || !offAt.Equal(clock.Add(HeatIdleLimit)) {
		t.Errorf("continue should restart the timer: %v", offAt)
	}
	r.waitTargets(t, TramBed, TramNozzle)
	r.waitPrint(t, PrintRunning)
}

func TestContinueRestartsTheTimer(t *testing.T) {
	r, _, k := adjustingRig(t, sim.Options{})
	clock := time.Now().Add(time.Hour)
	r.s.Now = func() time.Time { return clock }
	k.turn(inTram)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	if offAt, _ := r.s.heatInfo(); offAt == nil || !offAt.Equal(clock.Add(HeatIdleLimit)) {
		t.Errorf("timer: %v", offAt)
	}
	r.waitPrint(t, PrintRunning)
}

func TestSoakOnlyBeforeAFirstCheckOrAfterCooling(t *testing.T) {
	k := &knobs{bed: outOfTram}
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Bed: k.get})
	r.s.TramSoak = 200 * time.Millisecond
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	job := r.waitPrint(t, PrintSoaking)
	if job.SoakUntil == nil || r.sawCommand("G28") != 0 {
		t.Errorf("it should wait before moving: %+v", job)
	}
	job = r.waitPrint(t, PrintAdjusting)
	if job.SoakPending || job.SoakUntil != nil {
		t.Errorf("job: %+v", job)
	}

	// A recheck with the bed still hot doesn't soak.
	var out PrintJob
	w := call(r.s.handlePrintContinue, "")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.SoakPending {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the second check", func() bool {
		job := r.s.printJob()
		return job.State == PrintAdjusting && job.Checks == 2
	})

	// One after the heaters went off does.
	if err := r.printer.SetTargets(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	r.waitTargets(t, 0, 0)
	w = call(r.s.handlePrintContinue, "")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.SoakPending {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintSoaking)
	r.waitFor(t, "the third check", func() bool {
		job := r.s.printJob()
		return job.State == PrintAdjusting && job.Checks == 3
	})
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestCancelDuringASoakIsImmediate(t *testing.T) {
	r, name := printRig(t, sim.Options{})
	r.s.TramSoak = time.Hour
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintSoaking)
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
	r.waitTargets(t, 0, 0)
}

func savedJob(t *testing.T, job PrintJob) *Server {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, printFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{DataDir: dir}
	s.loadPrintJob()
	return s
}

func TestRestartDuringPreparingWaitsForTheUser(t *testing.T) {
	for _, state := range []string{PrintHeating, PrintSoaking, PrintChecking, PrintStarting} {
		s := savedJob(t, PrintJob{ID: 3, Name: "a.gcode", State: state, Stopping: true, Parked: true})
		job := s.printJob()
		if job.State != PrintAdjusting || job.Parked || job.Stopping || job.Note != restartNote {
			t.Errorf("%s: %+v", state, job)
		}
	}
	// One already waiting keeps what it knew, except a cancel nothing is finishing.
	s := savedJob(t, PrintJob{ID: 3, State: PrintAdjusting, Parked: true, Stopping: true})
	if job := s.printJob(); job.State != PrintAdjusting || !job.Parked || job.Stopping || job.Note != "" {
		t.Errorf("job: %+v", job)
	}
}

func TestRestartWhileAdjustingThenContinue(t *testing.T) {
	r, _, k := adjustingRig(t, sim.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	if job := s.printJob(); job.State != PrintAdjusting || !job.Parked {
		t.Fatalf("job: %+v", job)
	}
	if s.adoptPrint(ctx) {
		t.Fatal("nothing is printing")
	}
	if job := s.printJob(); job.State != PrintAdjusting {
		t.Fatalf("job: %+v", job)
	}
	k.turn(inTram)
	if w := call(s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintRunning)
}

func TestRestartJustAfterStartingAdoptsThePrint(t *testing.T) {
	r, name, _ := adjustingRig(t, sim.Options{})
	sdName := r.s.printJob().SDName
	r.card.StartPrint(sdName, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := restarted(r, ctx)
	if !s.adoptPrint(ctx) {
		t.Fatal("it should be followed")
	}
	if job := s.printJob(); job.State != PrintRunning || job.Name != name || job.Elsewhere {
		t.Errorf("job: %+v", job)
	}
	r.waitPrint(t, PrintFinished)
}

func TestAnotherPrintFromTheLCDEndsAWaitingJob(t *testing.T) {
	r, _, _ := adjustingRig(t, sim.Options{})
	first := r.s.printJob().ID
	r.card.Put("LCD.GCO", "G1 X1\r\n")
	r.card.StartPrint("LCD.GCO", 200*time.Millisecond)
	if !r.s.adoptPrint(context.Background()) {
		t.Fatal("it should be followed")
	}
	job := r.s.printJob()
	if job.ID == first || !job.Elsewhere || job.State != PrintRunning {
		t.Errorf("job: %+v", job)
	}
	r.waitPrint(t, PrintFinished)
}

func TestTemperatureWaitFailsAfterTheLinkDrops(t *testing.T) {
	r, name := printRig(t, sim.Options{HeatRate: 1})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.s.Printer = func() marlin.Status { return marlin.Status{} }
	job := r.waitPrint(t, PrintFailed)
	if !strings.Contains(job.Error, "lost the link") {
		t.Errorf("job: %+v", job)
	}
}
