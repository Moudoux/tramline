package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/tram"
)

// fakeProber records what it was asked to do. If gate is set, each command waits
// for a receive on it, so a test can act while a command is running.
type fakeProber struct {
	mu      sync.Mutex
	z       [4]float64
	calls   []string
	failAt  string
	failErr error
	gate    chan struct{}
	entered chan string
	info    []string
	during  func(call string)
}

func (p *fakeProber) step(name string) error {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	gate, entered, during := p.gate, p.entered, p.during
	fail := p.failAt == name
	p.mu.Unlock()
	if entered != nil {
		entered <- name
	}
	if gate != nil {
		<-gate
	}
	if during != nil {
		during(name)
	}
	if fail {
		return p.failErr
	}
	return nil
}

func (p *fakeProber) Home(context.Context) error { return p.step("home") }
func (p *fakeProber) Park(context.Context) error { return p.step("park") }
func (p *fakeProber) Probe(_ context.Context, x, y int) (tram.Reading, error) {
	name := fmt.Sprintf("probe %d %d", x, y)
	if err := p.step(name); err != nil {
		return tram.Reading{}, err
	}
	var z float64
	switch {
	case x < 115 && y < 115:
		z = p.z[tram.FrontLeft]
	case y < 115:
		z = p.z[tram.FrontRight]
	case x >= 115:
		z = p.z[tram.BackRight]
	default:
		z = p.z[tram.BackLeft]
	}
	return tram.Reading{X: float64(x), Y: float64(y), Z: z}, nil
}
func (p *fakeProber) TramInfo(context.Context) ([]string, error) { return p.info, nil }

func (p *fakeProber) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func newTramRig() (*rig, *fakeProber) {
	r := newRig()
	p := &fakeProber{}
	r.s.Prober = p
	return r, p
}

func (r *rig) waitTram(t *testing.T) *TramJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.s.tramRunning() {
		if time.Now().After(deadline) {
			t.Fatal("the run didn't finish")
		}
		time.Sleep(time.Millisecond)
	}
	return r.s.tramJob()
}

func (r *rig) startTram(t *testing.T, body string) {
	t.Helper()
	if w := call(r.s.handleTram, body); w.Code != http.StatusOK {
		t.Fatalf("tram: %d %s", w.Code, w.Body)
	}
}

const bedClear = `{"bed_clear":true}`

func TestTramOrderAndPass(t *testing.T) {
	r, p := newTramRig()
	p.z = [4]float64{0.10, 0.11, 0.12, 0.10}
	r.startTram(t, bedClear)
	job := r.waitTram(t)
	want := "home|probe 25 25|probe 205 25|probe 205 205|probe 25 205|park"
	if got := strings.Join(p.got(), "|"); got != want {
		t.Fatalf("got %s", got)
	}
	if job.State != TramDone || !job.Parked || job.Result == nil || !job.Result.Pass || job.Error != "" {
		t.Fatalf("got %+v", job)
	}
	if got := job.Rounds[0].Readings[2]; got.Corner != "back-right" || got.Z != 0.12 {
		t.Fatalf("got %+v", got)
	}
}

func TestTramNamesTheCornerToChange(t *testing.T) {
	r, p := newTramRig()
	p.z = [4]float64{0.1, 0.1, 0.1, -0.2}
	r.startTram(t, bedClear)
	job := r.waitTram(t)
	if job.State != TramDone || job.Result.Pass || job.Result.Corner != "back-left" || job.Result.Action != "raise" {
		t.Fatalf("got %+v", job.Result)
	}
}

func TestTramRequestRefusals(t *testing.T) {
	for _, tt := range []struct{ body, want string }{
		{`{}`, "bed_clear"},
		{`{"bed_clear":false}`, "bed_clear"},
		{`{"bed_clear":"yes"}`, "body must be"},
		{`not json`, "body must be"},
		{`{"bed_clear":true,"inset":9}`, "inset must be"},
		{`{"bed_clear":true,"inset":61}`, "inset must be"},
		{`{"bed_clear":true,"repeat":0}`, "repeat must be"},
		{`{"bed_clear":true,"repeat":11}`, "repeat must be"},
	} {
		t.Run(tt.body, func(t *testing.T) {
			r, p := newTramRig()
			w := call(r.s.handleTram, tt.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tt.want) {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			if got := p.got(); len(got) != 0 {
				t.Fatalf("moved: %v", got)
			}
		})
	}
}

func TestTramPrinterRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*rig)
		code  int
		want  string
	}{
		{"plug off", func(r *rig) { r.plug.on = false }, http.StatusConflict, "plug is off"},
		{"plug unreadable", func(r *rig) { r.plug.stateErr = errors.New("bridge down") }, http.StatusBadGateway, "bridge down"},
		{"disconnected", func(r *rig) { r.printer.disconnect() }, http.StatusConflict, "isn't connected"},
		{"temperatures unknown", func(r *rig) { r.printer.st.Temps = nil }, http.StatusConflict, "aren't known"},
		{"printing", func(r *rig) { r.printer.printing = true }, http.StatusConflict, "SD print"},
		{"print state unknown", func(r *rig) { r.printer.printingErr = errNoReply }, http.StatusBadGateway, "whether a print"},
		{"no prober", func(r *rig) { r.s.Prober = nil }, http.StatusServiceUnavailable, "no printer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, p := newTramRig()
			tt.setup(r)
			w := call(r.s.handleTram, bedClear)
			if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.want) {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			if got := p.got(); len(got) != 0 {
				t.Fatalf("moved: %v", got)
			}
			if r.s.tramJob() != nil {
				t.Fatal("a job was recorded")
			}
		})
	}
}

func gated(t *testing.T) (*rig, *fakeProber, func()) {
	t.Helper()
	r, p := newTramRig()
	p.gate = make(chan struct{})
	p.entered = make(chan string, 16)
	var once sync.Once
	release := func() { once.Do(func() { close(p.gate) }) }
	t.Cleanup(func() {
		release()
		r.waitTram(t)
	})
	return r, p, release
}

// These must answer at once during a move, without waiting for the lock.
func TestRefusedStraightAwayDuringARun(t *testing.T) {
	r, p, _ := gated(t)
	r.startTram(t, bedClear)
	<-p.entered
	for _, tt := range []struct {
		name string
		h    http.HandlerFunc
		body string
		want string
	}{
		{"second tram", r.s.handleTram, bedClear, "already going"},
		{"preheat", r.s.handlePreheat, `{"bed":40,"nozzle":60}`, "tram run"},
		{"power off", r.s.handlePower, `{"on":false}`, "tram run"},
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if w := call(tt.h, tt.body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("%s: got %d %s", tt.name, w.Code, w.Body)
			}
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s waited for the move", tt.name)
		}
	}
	if r.plug.switches != 0 || len(r.printer.sets) != 0 {
		t.Fatalf("plug %d, heat %v", r.plug.switches, r.printer.sets)
	}
}

func TestStatusAnswersDuringAMove(t *testing.T) {
	r, p, _ := gated(t)
	r.s.Printer = r.printer.Status
	r.startTram(t, bedClear)
	<-p.entered
	done := make(chan *TramJob, 1)
	go func() { done <- r.s.tramJob() }()
	select {
	case job := <-done:
		if job.State != TramRunning || job.Step != "homing" {
			t.Fatalf("got %+v", job)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("status waited for the move")
	}
}

// The lock is held per step, so cool gets through between steps and doesn't
// have to wait for the whole run.
func TestCoolGetsInBetweenSteps(t *testing.T) {
	r, p, release := gated(t)
	r.startTram(t, bedClear)
	<-p.entered // homing
	cooled := make(chan int, 1)
	go func() { cooled <- call(r.s.handleCool, "").Code }()
	time.Sleep(20 * time.Millisecond)
	select {
	case code := <-cooled:
		t.Fatalf("cool didn't wait for the running command: %d", code)
	default:
	}
	release()
	select {
	case code := <-cooled:
		if code != http.StatusOK {
			t.Fatalf("cool: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cool never got its turn")
	}
	// With the heaters already off, cool changes nothing the readings depend on.
	if job := r.waitTram(t); job.State != TramDone {
		t.Fatalf("got %+v", job)
	}
}

func TestTargetsChangingSpoilsTheRunButStillParks(t *testing.T) {
	r, p := newTramRig()
	p.during = func(call string) {
		if call == "probe 205 25" {
			r.printer.setHeating(0, 150)
		}
	}
	r.startTram(t, bedClear)
	job := r.waitTram(t)
	if job.State != TramFailed || job.Result != nil || !strings.Contains(job.Error, "targets changed") {
		t.Fatalf("got %+v", job)
	}
	calls := p.got()
	if calls[len(calls)-1] != "park" || !job.Parked {
		t.Fatalf("calls %v, job %+v", calls, job)
	}
	if strings.Contains(strings.Join(calls, "|"), "probe 205 205") {
		t.Fatalf("carried on probing: %v", calls)
	}
}

func TestChangeAfterLastProbeHasNoVerdict(t *testing.T) {
	r, p := newTramRig()
	p.during = func(call string) {
		if call == "probe 25 205" {
			r.printer.setHeating(40, 0)
		}
	}
	r.startTram(t, bedClear)
	if job := r.waitTram(t); job.Result != nil || job.State != TramFailed {
		t.Fatalf("got %+v", job)
	}
}

// The check waits for the lock like any other command, so it needs a run long
// enough for it to get its turn, which it does between steps.
func TestIdleTimeoutFiringDuringARunEndsIt(t *testing.T) {
	r, p := newTramRig()
	r.preheat(t)
	expired := make(chan struct{})
	var once sync.Once
	p.during = func(string) {
		once.Do(func() {
			r.clock = r.clock.Add(HeatIdleLimit)
			close(expired)
		})
		time.Sleep(2 * time.Millisecond)
	}
	checked := make(chan struct{})
	go func() {
		defer close(checked)
		<-expired
		r.check()
	}()
	r.startTram(t, `{"bed_clear":true,"repeat":10}`)
	job := r.waitTram(t)
	<-checked
	if job.State != TramFailed || !strings.Contains(job.Error, "targets changed") {
		t.Fatalf("got %+v", job)
	}
	if st := r.printer.Status().Temps; st.BedTarget != 0 || st.NozzleTarget != 0 {
		t.Fatalf("heaters still on: %+v", st)
	}
}

func TestTramDoesNotExtendTheHeaterTimer(t *testing.T) {
	r, _ := newTramRig()
	r.preheat(t)
	before := r.s.offAt()
	r.advance(10 * time.Minute)
	r.startTram(t, bedClear)
	r.waitTram(t)
	if got := r.s.offAt(); !got.Equal(before) {
		t.Fatalf("timer moved from %v to %v", before, got)
	}
}

func TestFailedCommandGivesNoVerdictAndNoPark(t *testing.T) {
	for _, step := range []string{"home", "probe 25 25", "probe 25 205"} {
		t.Run(step, func(t *testing.T) {
			r, p := newTramRig()
			p.failAt, p.failErr = step, marlin.ErrClosed
			r.startTram(t, bedClear)
			job := r.waitTram(t)
			if job.State != TramFailed || job.Result != nil || job.Parked || !strings.Contains(job.Error, "disconnected") {
				t.Fatalf("got %+v", job)
			}
			if calls := p.got(); calls[len(calls)-1] != step {
				t.Fatalf("carried on after the failure: %v", calls)
			}
		})
	}
}

func TestFailureInALaterRoundDropsTheEarlierVerdict(t *testing.T) {
	r, p := newTramRig()
	p.during = func(call string) {
		if len(p.got()) == 8 { // home, 4 probes, then the 3rd probe of round 2
			p.mu.Lock()
			p.failAt, p.failErr = call, errNoReply
			p.mu.Unlock()
		}
	}
	r.startTram(t, `{"bed_clear":true,"repeat":3}`)
	job := r.waitTram(t)
	if job.State != TramFailed || job.Result != nil || job.Parked {
		t.Fatalf("got %+v", job)
	}
	if job.Rounds[0].Result == nil {
		t.Fatalf("finished round lost its data: %+v", job.Rounds)
	}
}

func TestFailedParkKeepsTheReadings(t *testing.T) {
	r, p := newTramRig()
	p.failAt, p.failErr = "park", errNoReply
	r.startTram(t, bedClear)
	job := r.waitTram(t)
	if job.State != TramFailed || job.Parked || job.Result == nil || !job.Result.Pass || !strings.Contains(job.Error, "parking") {
		t.Fatalf("got %+v", job)
	}
}

func TestRepeatReportsSpread(t *testing.T) {
	r, p := newTramRig()
	p.during = func(call string) {
		if call == "home" {
			return
		}
		// Rounds differ by 0.01 mm at every corner.
		p.mu.Lock()
		round := (len(p.calls) - 2) / 4
		p.z = [4]float64{float64(round) * 0.01, float64(round) * 0.01, float64(round) * 0.01, float64(round) * 0.01}
		p.mu.Unlock()
	}
	r.startTram(t, `{"bed_clear":true,"repeat":3}`)
	job := r.waitTram(t)
	if job.State != TramDone || len(job.Rounds) != 3 || job.Spread == nil {
		t.Fatalf("got %+v", job)
	}
	for c, spread := range job.Spread {
		if spread != 0.02 {
			t.Fatalf("corner %d: spread %v", c, spread)
		}
	}
	if calls := p.got(); strings.Count(strings.Join(calls, "|"), "home") != 1 {
		t.Fatalf("homed more than once: %v", calls)
	}
}

func TestHistoryRecordsEachRun(t *testing.T) {
	r, p := newTramRig()
	r.s.DataDir = t.TempDir()
	r.startTram(t, bedClear)
	r.waitTram(t)
	p.failAt, p.failErr = "home", errNoReply
	r.startTram(t, bedClear)
	r.waitTram(t)
	// The job is marked finished just before the history is written.
	path := filepath.Join(r.s.DataDir, tramHistoryFile)
	var lines []string
	deadline := time.Now().Add(5 * time.Second)
	for len(lines) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("history: %q", lines)
		}
		data, _ := os.ReadFile(path)
		lines = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	var first, second TramJob
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.State != TramDone || second.State != TramFailed || second.ID != first.ID+1 {
		t.Fatalf("got %+v, %+v", first, second)
	}
}

func TestUnwritableHistoryDoesNotFailTheRun(t *testing.T) {
	r, _ := newTramRig()
	r.s.DataDir = filepath.Join(t.TempDir(), "missing")
	r.startTram(t, bedClear)
	if job := r.waitTram(t); job.State != TramDone {
		t.Fatalf("got %+v", job)
	}
}

func TestTramInfo(t *testing.T) {
	r, p := newTramRig()
	p.info = []string{"> M851", "ok"}
	w := httptestGet(r.s.handleTramInfo)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "M851") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	r.plug.on = false
	if w := httptestGet(r.s.handleTramInfo); w.Code != http.StatusConflict {
		t.Fatalf("plug off: %d %s", w.Code, w.Body)
	}
}
