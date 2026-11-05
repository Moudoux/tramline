package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/sim"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type schedRig struct {
	*uploadRig
	clock *fakeClock
	name  string
	at    time.Time
	knobs *knobs
	ctx   context.Context
}

func tuneSchedule(s *Server) {
	s.sched.settle = time.Millisecond
	s.sched.connectLimit = 500 * time.Millisecond
	s.sched.plugWait = time.Millisecond
	s.sched.offEvery = 5 * time.Millisecond
}

// schedulingRig is a server with a file on the card and a clock under the test's control.
// The simulated printer stays connected when the fake plug goes off, so a test that wants
// the printer to appear to be off says so itself.
func schedulingRig(t *testing.T, bed sim.Corners) *schedRig {
	t.Helper()
	return schedulingRigFor(t, bed, 200*time.Millisecond)
}

func schedulingRigFor(t *testing.T, bed sim.Corners, printTime time.Duration) *schedRig {
	t.Helper()
	k := &knobs{bed: bed}
	r, name := printRig(t, sim.Options{PrintTime: printTime, Bed: k.get})
	clock := &fakeClock{t: time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)}
	r.s.Now = clock.now
	tuneSchedule(r.s)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &schedRig{uploadRig: r, clock: clock, name: name, at: clock.now().Add(10 * time.Hour), knobs: k, ctx: ctx}
}

func waitAutoOff(s *Server) {
	// One that can't finish, such as with a plug that's failing, saves nothing meanwhile.
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		s.sched.mu.Lock()
		pending := s.sched.data.OffPending
		s.sched.mu.Unlock()
		if !pending {
			return
		}
	}
}

func (r *schedRig) schedule(t *testing.T, at time.Time) int {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"from":"sd","at":%q,"bed_clear":true}`, r.name, at.Format(time.RFC3339))
	w := call(r.s.handleSchedule, body)
	if w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body)
	}
	var p ScheduledPrint
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func (r *schedRig) get(id int) ScheduledPrint {
	for _, p := range r.s.schedulesCopy() {
		if p.ID == id {
			return p
		}
	}
	return ScheduledPrint{}
}

func (r *schedRig) waitState(t *testing.T, id int, state string) ScheduledPrint {
	t.Helper()
	r.waitFor(t, "scheduled print state "+state, func() bool { return r.get(id).State == state })
	return r.get(id)
}

func (r *schedRig) plugOn() bool {
	st := r.s.plugStatus(context.Background())
	return st != nil && st.On
}

func (r *schedRig) waitPlugOff(t *testing.T) {
	t.Helper()
	r.waitFor(t, "the plug to go off", func() bool { return !r.plugOn() })
}

// scheduled is a print past its check, with the printer turned off, waiting for its time.
func (r *schedRig) scheduled(t *testing.T) int {
	t.Helper()
	id := r.schedule(t, r.at)
	r.waitState(t, id, SchedWaiting)
	r.waitPlugOff(t)
	return id
}

func (r *schedRig) fire(t *testing.T) {
	t.Helper()
	r.clock.set(r.at)
	r.s.checkSchedules(r.ctx)
}

func cancelSchedule(s *Server, id int) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("id", fmt.Sprint(id))
	s.handleScheduleCancel(w, req)
	return w
}

func TestScheduleRefusals(t *testing.T) {
	r := schedulingRig(t, inTram)
	soon := r.clock.now().Add(ScheduleMinLead - time.Minute).Format(time.RFC3339)
	far := r.clock.now().Add(ScheduleMaxLead + time.Hour).Format(time.RFC3339)
	good := r.at.Format(time.RFC3339)
	for _, c := range []struct {
		what, body string
		code       int
	}{
		{"no bed clear", fmt.Sprintf(`{"name":%q,"from":"sd","at":%q}`, r.name, good), http.StatusBadRequest},
		{"too soon", fmt.Sprintf(`{"name":%q,"from":"sd","at":%q,"bed_clear":true}`, r.name, soon), http.StatusBadRequest},
		{"too far", fmt.Sprintf(`{"name":%q,"from":"sd","at":%q,"bed_clear":true}`, r.name, far), http.StatusBadRequest},
		{"no from", fmt.Sprintf(`{"name":%q,"at":%q,"bed_clear":true}`, r.name, good), http.StatusBadRequest},
		{"no time", fmt.Sprintf(`{"name":%q,"from":"sd","bed_clear":true}`, r.name), http.StatusBadRequest},
		{"not a time", fmt.Sprintf(`{"name":%q,"from":"sd","at":"6am","bed_clear":true}`, r.name), http.StatusBadRequest},
		{"no such file", fmt.Sprintf(`{"name":"other.gcode","from":"sd","at":%q,"bed_clear":true}`, good), http.StatusNotFound},
	} {
		if w := call(r.s.handleSchedule, c.body); w.Code != c.code {
			t.Errorf("%s: got %d, %s", c.what, w.Code, w.Body)
		}
	}
	if got := r.s.schedulesCopy(); len(got) != 0 {
		t.Errorf("a refusal left a record: %+v", got)
	}
	if r.sawCommand("M140") != 0 {
		t.Error("a refusal heated the printer")
	}
}

func TestScheduleChecksTramTurnsOffThenPrintsAndTurnsOff(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.schedule(t, r.at)
	p := r.waitState(t, id, SchedWaiting)
	if p.Check == nil || !p.Check.Pass {
		t.Errorf("check: %+v", p.Check)
	}
	r.waitPlugOff(t)
	if job := r.s.printJob(); job.State != PrintScheduled || !job.PreCheck {
		t.Errorf("job: %+v", job)
	}
	if r.sawCommand("M23") != 0 || r.sawCommand("M24") != 0 {
		t.Fatal("the file started at the first check")
	}
	if bed, nozzle := r.targets(); bed != 0 || nozzle != 0 {
		t.Errorf("heaters should be off: %v %v", bed, nozzle)
	}
	if r.s.Plug == nil || r.sawCommand("G30") != 4 {
		t.Errorf("G30s: %d", r.sawCommand("G30"))
	}

	// Not due yet: nothing happens.
	r.clock.set(r.at.Add(-time.Second))
	r.s.checkSchedules(r.ctx)
	if r.plugOn() || r.get(id).State != SchedWaiting {
		t.Fatal("it started early")
	}

	r.fire(t)
	r.waitFor(t, "the print to run or end", func() bool { return r.sawCommand("M24") == 1 })
	if !r.plugOn() && r.s.printJob().State == PrintHeating {
		t.Error("the plug should be on for the print")
	}
	if r.sawCommand("G30") != 8 {
		t.Errorf("it should check again: %d G30s", r.sawCommand("G30"))
	}
	end := r.waitState(t, id, PrintFinished)
	if end.FinalCheck == nil || !end.FinalCheck.Pass || end.EndedAt == nil {
		t.Errorf("end: %+v", end)
	}
	r.waitPlugOff(t)
}

func TestScheduleWaitsForAdjustingWhenScheduling(t *testing.T) {
	r := schedulingRig(t, outOfTram)
	id := r.schedule(t, r.at)
	r.waitPrint(t, PrintAdjusting)
	if r.get(id).State != SchedChecking || !r.plugOn() {
		t.Fatalf("schedule: %+v, plug on %t", r.get(id), r.plugOn())
	}
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	r.waitFor(t, "the second check", func() bool {
		job := r.s.printJob()
		return job.State == PrintAdjusting && job.Checks == 2
	})
	r.knobs.turn(inTram)
	if w := call(r.s.handlePrintContinue, ""); w.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", w.Code, w.Body)
	}
	r.waitState(t, id, SchedWaiting)
	r.waitPlugOff(t)
	if r.sawCommand("M24") != 0 {
		t.Fatal("the file started")
	}
}

func TestFailedCheckAtItsTimeCancelsAndPowersOff(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	r.knobs.turn(outOfTram)
	r.fire(t)
	p := r.waitState(t, id, PrintCancelled)
	if p.FinalCheck == nil || p.FinalCheck.Pass || !strings.Contains(p.Reason, "tram check failed") {
		t.Errorf("schedule: %+v", p)
	}
	if r.sawCommand("M23") != 0 || r.sawCommand("M24") != 0 {
		t.Fatal("the file started on a failed check")
	}
	r.waitTargets(t, 0, 0)
	r.waitPlugOff(t)
	if job := r.s.printJob(); job.State != PrintCancelled {
		t.Errorf("it should not wait for an adjustment: %+v", job)
	}
}

func TestAPrintSinceTheBedWasClearedCancelsWithoutTouchingThePrinter(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	call(r.s.handlePower, `{"on":true}`)
	r.clock.set(r.clock.now().Add(time.Hour))
	r.startPrint(t, r.name)
	r.waitPrint(t, PrintFinished)
	r.waitPlugOff(t)
	waitAutoOff(r.s)
	switches := r.plug.switches
	heats := r.sawCommand("M140")

	r.fire(t)
	p := r.waitState(t, id, PrintCancelled)
	if !strings.Contains(p.Reason, "part may be on it") {
		t.Errorf("reason: %q", p.Reason)
	}
	if r.plug.switches != switches || r.sawCommand("M140") != heats {
		t.Error("it should not touch the printer")
	}
}

func TestSeveralCanWaitAndTheSecondGivesWayToTheFirstsPart(t *testing.T) {
	r := schedulingRig(t, inTram)
	first := r.scheduled(t)
	r.clock.set(r.clock.now().Add(time.Minute))
	second := r.schedule(t, r.at.Add(8*time.Hour))
	r.waitState(t, second, SchedWaiting)
	r.waitPlugOff(t)

	r.fire(t)
	r.waitState(t, first, PrintFinished)
	r.waitPlugOff(t)

	r.clock.set(r.at.Add(8 * time.Hour))
	r.s.checkSchedules(r.ctx)
	p := r.waitState(t, second, PrintCancelled)
	if !strings.Contains(p.Reason, "part may be on it") {
		t.Errorf("reason: %q", p.Reason)
	}
}

func TestABusyPrinterCancelsIt(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	call(r.s.handlePower, `{"on":true}`)
	r.knobs.turn(outOfTram)
	r.startPrint(t, r.name)
	r.waitPrint(t, PrintAdjusting)
	r.fire(t)
	p := r.waitState(t, id, PrintCancelled)
	if !strings.Contains(p.Reason, "busy") {
		t.Errorf("reason: %q", p.Reason)
	}
	if job := r.s.printJob(); job.State != PrintAdjusting {
		t.Errorf("the other job should be left alone: %+v", job)
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestCancelWaiting(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	if w := cancelSchedule(r.s, id); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	r.fire(t)
	time.Sleep(20 * time.Millisecond)
	if r.plugOn() || r.get(id).State != PrintCancelled || r.sawCommand("M24") != 0 {
		t.Errorf("it ran anyway: %+v", r.get(id))
	}
	if w := cancelSchedule(r.s, id); w.Code != http.StatusConflict {
		t.Errorf("again: %d", w.Code)
	}
	if w := cancelSchedule(r.s, 99); w.Code != http.StatusNotFound {
		t.Errorf("no such one: %d", w.Code)
	}
}

func TestCancelDuringTheFirstCheckEndsIt(t *testing.T) {
	r := schedulingRig(t, outOfTram)
	id := r.schedule(t, r.at)
	r.waitPrint(t, PrintAdjusting)
	if w := cancelSchedule(r.s, id); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
	r.waitTargets(t, 0, 0)
	if r.get(id).State != PrintCancelled {
		t.Errorf("schedule: %+v", r.get(id))
	}
	if !r.plugOn() {
		t.Error("the user is at the printer, so it should stay on")
	}
}

func TestPlainCancelOfTheFirstCheckCancelsTheSchedule(t *testing.T) {
	r := schedulingRig(t, outOfTram)
	id := r.schedule(t, r.at)
	r.waitPrint(t, PrintAdjusting)
	call(r.s.handleCancel, "")
	r.waitState(t, id, PrintCancelled)
}

func TestCancelWhilePrintingIsRefusedAndPointsAtCancel(t *testing.T) {
	r := schedulingRigFor(t, inTram, time.Minute)
	id := r.scheduled(t)
	r.fire(t)
	r.waitPrint(t, PrintRunning)
	r.s.checkSchedules(r.ctx)
	r.waitState(t, id, SchedPrinting)
	if w := cancelSchedule(r.s, id); w.Code != http.StatusConflict {
		t.Errorf("cancel: %d", w.Code)
	}
	call(r.s.handleCancel, "")
	r.waitState(t, id, PrintCancelled)
	r.waitPlugOff(t)
}

func TestItsFileIsProtected(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	content := gcodeOf(6)
	w := named(r.s.handleFilePut, http.MethodPut, r.name, content, map[string]string{HashHeader: hashOf(content)})
	if w.Code != http.StatusConflict {
		t.Errorf("overwrite: %d %s", w.Code, w.Body)
	}
	if w := named(r.s.handleFileDelete, http.MethodDelete, r.name, "", nil); w.Code != http.StatusConflict {
		t.Errorf("delete: %d %s", w.Code, w.Body)
	}
	if w := cancelSchedule(r.s, id); w.Code != http.StatusOK {
		t.Fatalf("cancel: %s", w.Body)
	}
	if w := named(r.s.handleFilePut, http.MethodPut, r.name, content, map[string]string{HashHeader: hashOf(content)}); w.Code != http.StatusOK {
		t.Errorf("overwrite after cancelling: %d %s", w.Code, w.Body)
	}
}

func TestMissedWhenTheServerWasDownTooLong(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	r.clock.set(r.at.Add(ScheduleGrace + time.Minute))
	r.s.checkSchedules(r.ctx)
	p := r.get(id)
	if p.State != SchedMissed || r.plugOn() {
		t.Errorf("schedule: %+v, plug on %t", p, r.plugOn())
	}
}

func TestStillStartsWithinTheGrace(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	r.clock.set(r.at.Add(ScheduleGrace - time.Minute))
	r.s.checkSchedules(r.ctx)
	r.waitState(t, id, PrintFinished)
}

func TestFirstCheckNotPassedByItsTimeIsCancelled(t *testing.T) {
	r := schedulingRig(t, outOfTram)
	id := r.schedule(t, r.at)
	r.waitPrint(t, PrintAdjusting)
	r.fire(t)
	p := r.waitState(t, id, PrintCancelled)
	if !strings.Contains(p.Reason, "hadn't passed") {
		t.Errorf("reason: %q", p.Reason)
	}
	if r.sawCommand("M24") != 0 {
		t.Fatal("the file started")
	}
}

func TestPlugFailureAtItsTime(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	r.plug.setErr = errors.New("the bridge is gone")
	r.fire(t)
	p := r.waitState(t, id, PrintFailed)
	if !strings.Contains(p.Reason, "plug couldn't be switched on") {
		t.Errorf("reason: %q", p.Reason)
	}
	if r.sawCommand("M24") != 0 {
		t.Fatal("the file started")
	}
}

func TestSurvivesARestartWhileWaiting(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	old := r.s
	s := restarted(r.uploadRig, r.ctx)
	s.Now = old.Now
	tuneSchedule(s)
	s.loadSchedules()
	if got := r.get(id); got.State != SchedWaiting || !got.At.Equal(r.at.UTC()) {
		t.Fatalf("after a restart: %+v", got)
	}
	r.fire(t)
	r.waitState(t, id, PrintFinished)
}

func TestRestartDuringItsCheckAtItsTimeCancelsRatherThanWaiting(t *testing.T) {
	r := schedulingRig(t, inTram)
	id := r.scheduled(t)
	// As if it had reached its check when the server stopped.
	r.s.sched.mu.Lock()
	r.s.sched.data.Prints[0].State = SchedStarting
	r.s.sched.data.Prints[0].JobID = 50
	r.s.keepSchedulesLocked()
	r.s.sched.mu.Unlock()
	job := PrintJob{ID: 50, Name: r.name, Mode: ModeSD, State: PrintChecking, ScheduleID: id, StartedAt: r.clock.now(), MovedAt: r.clock.now()}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.s.DataDir, printFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	old := r.s
	s := restarted(r.uploadRig, r.ctx)
	s.Now = old.Now
	tuneSchedule(s)
	s.loadSchedules()
	if got := s.printJob(); got.State != PrintAdjusting {
		t.Fatalf("job: %+v", got)
	}
	// The printer hasn't yet said whether its file started.
	s.checkSchedules(r.ctx)
	if got := s.printJob(); got.State != PrintAdjusting {
		t.Fatalf("it should wait to hear from the printer: %+v", got)
	}
	s.adoptPrint(r.ctx)
	s.checkSchedules(r.ctx)
	r.waitState(t, id, PrintCancelled)
	r.waitPlugOff(t)
}

func TestRestartDuringItsPrintIsFollowed(t *testing.T) {
	r := schedulingRigFor(t, inTram, 300*time.Millisecond)
	id := r.scheduled(t)
	r.fire(t)
	r.waitPrint(t, PrintRunning)
	r.s.checkSchedules(r.ctx)
	r.waitState(t, id, SchedPrinting)
	old := r.s
	s := restarted(r.uploadRig, r.ctx)
	s.Now = old.Now
	tuneSchedule(s)
	s.loadSchedules()
	s.adoptPrint(r.ctx)
	r.waitState(t, id, PrintFinished)
	r.waitPlugOff(t)
}

// The rule for cutting the plug is stricter than power off's, which lets a printer it
// can't reach through, as it can't tell how hot that nozzle is.
func TestAutoOffOnlyCutsAConnectedPrinterKnownToBeCool(t *testing.T) {
	for _, c := range []struct {
		what     string
		status   marlin.Status
		finished bool
		off      bool
	}{
		{"hot", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 120}}, false, false},
		{"at the limit", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: CoolEnough}}, false, false},
		{"link down", marlin.Status{}, false, false},
		{"temperatures unknown", marlin.Status{Connected: true}, false, false},
		{"someone heating it", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 30, BedTarget: 80}}, true, false},
		{"cool", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: CoolEnough - 1}}, true, true},
	} {
		plug := &fakePlug{on: true}
		s := &Server{Plug: plug, Printer: func() marlin.Status { return c.status }}
		finished, why := s.tryAutoOff(context.Background(), func() bool { return true })
		if finished != c.finished || plug.on == c.off {
			t.Errorf("%s: finished %t, plug on %t: %s", c.what, finished, plug.on, why)
		}
	}
	// Failing to switch it is tried again.
	plug := &fakePlug{on: true, setErr: errors.New("no bridge")}
	s := &Server{Plug: plug, Printer: func() marlin.Status {
		return marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 30}}
	}}
	if finished, _ := s.tryAutoOff(context.Background(), func() bool { return true }); finished {
		t.Error("it should try again")
	}
	// A newer auto-off, or someone wanting power, stops this one.
	plug = &fakePlug{on: true}
	s = &Server{Plug: plug, Printer: func() marlin.Status {
		return marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 30}}
	}}
	if s.tryAutoOff(context.Background(), func() bool { return false }); !plug.on {
		t.Error("a stale auto-off cut the plug")
	}
}

// A printer that really goes away when the plug is off, unlike the other tests', from
// the first check to the end of a print streamed from the Pi.
func TestWholeFlowWithThePrinterReallyPoweredOff(t *testing.T) {
	seen := &uploadRig{}
	plug := &sim.Plug{Options: sim.Options{HeatRate: 4000, Corners: inTram, Seen: seen.saw, Card: sim.NewCard()}}
	printer := marlin.NewPrinter(plug.Open)
	printer.StreamNudge = 40 * time.Millisecond
	clock := &fakeClock{t: time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{
		Plug: plug, Heaters: printer, Card: printer, Prober: printer, Prints: printer,
		Printer: printer.Status, DataDir: t.TempDir(), ctx: ctx, HeatHold: time.Millisecond, Now: clock.now,
	}
	t.Cleanup(func() { waitAutoOff(s) })
	printer.OnLine = s.PrinterLine
	s.print.every = 10 * time.Millisecond
	tuneSchedule(s)
	s.sched.connectLimit = 8 * time.Second
	go printer.Run(ctx)
	r := &schedRig{uploadRig: &uploadRig{s: s, plug: nil, printer: printer}, clock: clock, name: "tile.gcode", at: clock.now().Add(8 * time.Hour), ctx: ctx}
	r.uploadRig.put(t, r.name, gcodeOf(5))

	// The printer is off to begin with, so scheduling switches it on.
	body := fmt.Sprintf(`{"name":%q,"from":"pi","at":%q,"bed_clear":true}`, r.name, r.at.Format(time.RFC3339))
	w := call(s.handleSchedule, body)
	if w.Code != http.StatusOK {
		t.Fatalf("schedule: %d %s", w.Code, w.Body)
	}
	const id = 1
	r.waitState(t, id, SchedWaiting)
	r.waitPlugOff(t)
	r.waitFor(t, "the printer to go away", func() bool { return !printer.Status().Connected })

	r.fire(t)
	r.waitFor(t, "it to come back and print", func() bool { return r.get(id).State == PrintFinished })
	if job := s.printJob(); job.State != PrintFinished || job.Mode != ModeStream {
		t.Errorf("job: %+v", job)
	}
	r.waitPlugOff(t)
	r.waitFor(t, "the printer to go away again", func() bool { return !printer.Status().Connected })
	if seen.sawCommand("G30") != 8 {
		t.Errorf("G30s: %d", seen.sawCommand("G30"))
	}
}

func TestPowerOnStopsAnAutoOff(t *testing.T) {
	r := schedulingRig(t, inTram)
	r.s.sched.offEvery = time.Hour
	id := r.schedule(t, r.at)
	r.waitState(t, id, SchedWaiting)
	// Either it has already been cut once, or the wait is stopped by this.
	if w := call(r.s.handlePower, `{"on":true}`); w.Code != http.StatusOK {
		t.Fatalf("power: %s", w.Body)
	}
	time.Sleep(30 * time.Millisecond)
	if !r.plugOn() {
		t.Error("asking for power should stop the plug being cut")
	}
	r.s.sched.mu.Lock()
	pending := r.s.sched.data.OffPending
	r.s.sched.mu.Unlock()
	if pending {
		t.Error("the wait should be forgotten")
	}
}

func TestAPendingAutoOffCarriesOnAfterARestart(t *testing.T) {
	r := schedulingRig(t, inTram)
	r.s.sched.mu.Lock()
	r.s.sched.data.OffPending = true
	r.s.keepSchedulesLocked()
	r.s.sched.mu.Unlock()
	s := restarted(r.uploadRig, r.ctx)
	tuneSchedule(s)
	s.loadSchedules()
	r.waitPlugOff(t)
	r.waitFor(t, "the wait to finish", func() bool {
		s.sched.mu.Lock()
		defer s.sched.mu.Unlock()
		return !s.sched.data.OffPending
	})
}

func TestUnreadableFileRefusesRatherThanOverwriting(t *testing.T) {
	r := schedulingRig(t, inTram)
	path := filepath.Join(r.s.DataDir, scheduleFile)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := restarted(r.uploadRig, r.ctx)
	s.loadSchedules()
	if w := httptestGet(s.handleScheduleList); w.Code != http.StatusInternalServerError {
		t.Errorf("list: %d", w.Code)
	}
	if w := call(s.handleSchedule, `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("schedule: %d", w.Code)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "not json" {
		t.Errorf("the file changed: %q %v", data, err)
	}
}

func TestOnlyTheNewestEndedAreKept(t *testing.T) {
	r := schedulingRig(t, inTram)
	r.s.sched.mu.Lock()
	for i := range schedKeepEnded + 5 {
		p := &ScheduledPrint{ID: i + 1, Name: "x", At: r.at, State: SchedWaiting}
		r.s.sched.data.Prints = append(r.s.sched.data.Prints, p)
		r.s.endScheduleLocked(p, PrintCancelled, "test")
		r.clock.set(r.clock.now().Add(time.Second))
	}
	waiting := &ScheduledPrint{ID: 100, Name: "y", At: r.at, State: SchedWaiting}
	r.s.sched.data.Prints = append(r.s.sched.data.Prints, waiting)
	r.s.sched.mu.Unlock()
	got := r.s.schedulesCopy()
	if len(got) != schedKeepEnded+1 {
		t.Errorf("kept %d", len(got))
	}
	if r.get(1).ID != 0 || r.get(100).ID != 100 {
		t.Error("it should drop the oldest ended one, and never a waiting one")
	}
}

func TestAPrintFromTheCardNeedsACopy(t *testing.T) {
	r := schedulingRig(t, inTram)
	r.put(t, "nocopy.gcode", gcodeOf(3))
	body := fmt.Sprintf(`{"name":"nocopy.gcode","from":"sd","at":%q,"bed_clear":true}`, r.at.Format(time.RFC3339))
	if w := call(r.s.handleSchedule, body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no verified copy") {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
	if r.sawCommand("M140") != 0 {
		t.Error("it heated")
	}
}

func TestWebShowsScheduledPrints(t *testing.T) {
	r := schedulingRig(t, inTram)
	r.s.sched.mu.Lock()
	r.s.sched.data.Prints = []*ScheduledPrint{{ID: 1, Name: "later.gcode", From: FromPi, At: r.at.Add(time.Hour), State: SchedWaiting}}
	for i := range webEndedSchedules + 3 {
		p := &ScheduledPrint{ID: 10 + i, Name: "old.gcode", From: FromSD, At: r.at.Add(time.Duration(-i) * time.Hour), State: PrintCancelled, Reason: "why"}
		now := r.clock.now()
		p.EndedAt = &now
		r.s.sched.data.Prints = append(r.s.sched.data.Prints, p)
	}
	r.s.sched.data.Prints = append(r.s.sched.data.Prints, &ScheduledPrint{ID: 2, Name: "soon.gcode", From: FromSD, At: r.at, State: SchedChecking})
	r.s.sched.mu.Unlock()
	w := httptestGet(r.s.handleWebStatus)
	var out WebStatus
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Scheduled) != 2+webEndedSchedules {
		t.Fatalf("got %d: %+v", len(out.Scheduled), out.Scheduled)
	}
	if got := out.Scheduled[0]; got.ID != 2 || got.From != FromSD || got.Ended {
		t.Errorf("soonest first: %+v", got)
	}
	if got := out.Scheduled[1]; got.ID != 1 {
		t.Errorf("then the later one: %+v", got)
	}
	if got := out.Scheduled[2]; got.ID != 10 || !got.Ended || got.Reason != "why" {
		t.Errorf("then the latest ended: %+v", got)
	}
}
