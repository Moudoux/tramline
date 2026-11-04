package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/hue"
	"tramline/internal/marlin"
)

var errNoReply = errors.New("no reply")

// fakePrinter records commands, so tests can prove a refused one was never sent.
type fakePrinter struct {
	mu          sync.Mutex
	st          marlin.Status
	printing    bool
	printingErr error
	setErr      error
	sets        [][2]int
	checks      int
}

func (p *fakePrinter) Status() marlin.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st
}

func (p *fakePrinter) SetTargets(_ context.Context, bed, nozzle int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sets = append(p.sets, [2]int{bed, nozzle})
	if p.setErr != nil {
		return p.setErr
	}
	t := *p.st.Temps
	t.BedTarget, t.NozzleTarget = float64(bed), float64(nozzle)
	p.st.Temps = &t
	return nil
}

func (p *fakePrinter) SDPrinting(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	return p.printing, p.printingErr
}

// setHeating sets the targets directly, like someone using the LCD.
func (p *fakePrinter) setHeating(bed, nozzle float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st.Temps = &marlin.Temps{Nozzle: 22, Bed: 22, BedTarget: bed, NozzleTarget: nozzle}
}

func (p *fakePrinter) disconnect() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st = marlin.Status{}
}

type fakePlug struct {
	on       bool
	stateErr error
	setErr   error
	switches int
}

func (p *fakePlug) Pair(context.Context) error                  { return nil }
func (p *fakePlug) Lights(context.Context) ([]hue.Light, error) { return nil, nil }
func (p *fakePlug) State(context.Context) (hue.Light, error) {
	return hue.Light{Name: "plug", On: p.on}, p.stateErr
}
func (p *fakePlug) Set(_ context.Context, on bool) error {
	if p.setErr != nil {
		return p.setErr
	}
	p.on = on
	p.switches++
	return nil
}

type rig struct {
	s       *Server
	printer *fakePrinter
	plug    *fakePlug
	clock   time.Time
}

func (r *rig) advance(d time.Duration) { r.clock = r.clock.Add(d) }
func (r *rig) check()                  { r.s.checkHeaters(context.Background()) }

func newRig() *rig {
	r := &rig{
		printer: &fakePrinter{st: marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 22, Bed: 22}}},
		plug:    &fakePlug{on: true},
		clock:   time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
	r.s = &Server{Plug: r.plug, Heaters: r.printer, Printer: r.printer.Status, Now: func() time.Time { return r.clock }}
	return r
}

func call(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return w
}

func httptestGet(h http.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/", nil))
	return w
}

func (r *rig) preheat(t *testing.T) {
	t.Helper()
	if w := call(r.s.handlePreheat, `{"bed":80,"nozzle":150}`); w.Code != http.StatusOK {
		t.Fatalf("preheat: %d %s", w.Code, w.Body)
	}
}

func (r *rig) wantSets(t *testing.T, want ...[2]int) {
	t.Helper()
	if len(r.printer.sets) != len(want) {
		t.Fatalf("commands %v, want %v", r.printer.sets, want)
	}
	for i := range want {
		if r.printer.sets[i] != want[i] {
			t.Fatalf("commands %v, want %v", r.printer.sets, want)
		}
	}
}

func TestPreheatLimits(t *testing.T) {
	tests := []struct {
		body string
		code int
	}{
		{`{"bed":100,"nozzle":260}`, http.StatusOK},
		{`{"bed":60,"nozzle":0}`, http.StatusOK},
		{`{"bed":0,"nozzle":200}`, http.StatusOK},
		{`{"bed":101,"nozzle":150}`, http.StatusBadRequest},
		{`{"bed":80,"nozzle":261}`, http.StatusBadRequest},
		{`{"bed":-1,"nozzle":150}`, http.StatusBadRequest},
		{`{"bed":80,"nozzle":-1}`, http.StatusBadRequest},
		{`{"bed":0,"nozzle":0}`, http.StatusBadRequest},
		{`{"bed":80.5,"nozzle":150}`, http.StatusBadRequest},
		{`{"bed":"80 S1","nozzle":150}`, http.StatusBadRequest},
		{`{"bed":80}`, http.StatusBadRequest},
		{`{"nozzle":150}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			r := newRig()
			w := call(r.s.handlePreheat, tt.body)
			if w.Code != tt.code {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			if sent := len(r.printer.sets) > 0; sent != (tt.code == http.StatusOK) {
				t.Fatalf("commands %v after %d", r.printer.sets, w.Code)
			}
		})
	}
}

func TestPreheatRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*rig)
		code  int
		want  string
	}{
		{"plug off", func(r *rig) { r.plug.on = false }, http.StatusConflict, "plug is off"},
		{"plug unreadable", func(r *rig) { r.plug.stateErr = errors.New("bridge down") }, http.StatusBadGateway, "bridge down"},
		{"no plug", func(r *rig) { r.s.Plug = nil }, http.StatusConflict, "no plug"},
		{"disconnected", func(r *rig) { r.printer.disconnect() }, http.StatusConflict, "isn't connected"},
		{"temperatures unknown", func(r *rig) { r.printer.st.Temps = nil }, http.StatusConflict, "aren't known"},
		{"printing", func(r *rig) { r.printer.printing = true }, http.StatusConflict, "SD print"},
		{"print state unknown", func(r *rig) { r.printer.printingErr = errNoReply }, http.StatusBadGateway, "whether a print"},
		{"no printer", func(r *rig) { r.s.Heaters = nil }, http.StatusServiceUnavailable, "no printer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig()
			tt.setup(r)
			w := call(r.s.handlePreheat, `{"bed":80,"nozzle":150}`)
			if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.want) {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			r.wantSets(t)
			if at, _ := r.s.heatInfo(); at != nil {
				t.Fatalf("timer started: %v", at)
			}
		})
	}
}

func TestPreheatReportsWhenHeatersTurnOff(t *testing.T) {
	r := newRig()
	w := call(r.s.handlePreheat, `{"bed":80,"nozzle":150}`)
	var out PreheatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := r.clock.Add(30 * time.Minute)
	if out.Bed != 80 || out.Nozzle != 150 || !out.HeatersOffAt.Equal(want) {
		t.Fatalf("got %+v", out)
	}
	if at, _ := r.s.heatInfo(); at == nil || !at.Equal(want) {
		t.Fatalf("status has %v", at)
	}
}

func TestFailedPreheatReportsTheError(t *testing.T) {
	r := newRig()
	r.printer.setErr = errNoReply
	if w := call(r.s.handlePreheat, `{"bed":80,"nozzle":150}`); w.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if at, _ := r.s.heatInfo(); at != nil {
		t.Fatalf("timer started: %v", at)
	}
}

func TestIdleTimeoutTurnsHeatersOff(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.advance(30*time.Minute - time.Second)
	r.check()
	r.wantSets(t, [2]int{80, 150})

	r.advance(time.Second)
	r.check()
	r.wantSets(t, [2]int{80, 150}, [2]int{0, 0})
	if at, problem := r.s.heatInfo(); at != nil || problem != "" {
		t.Fatalf("after timeout: %v, %q", at, problem)
	}
}

func TestPreheatAgainRestartsTheTimer(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.advance(20 * time.Minute)
	r.preheat(t)
	r.advance(20 * time.Minute)
	r.check()
	r.wantSets(t, [2]int{80, 150}, [2]int{80, 150})
	r.advance(10 * time.Minute)
	r.check()
	r.wantSets(t, [2]int{80, 150}, [2]int{80, 150}, [2]int{0, 0})
}

// A forgotten polling script mustn't keep the heaters on.
func TestStatusDoesNotRestartTheTimer(t *testing.T) {
	r := newRig()
	r.preheat(t)
	for range 30 {
		r.advance(time.Minute)
		w := httptest.NewRecorder()
		r.s.handleStatus(w, httptest.NewRequest(http.MethodGet, "/", nil))
		r.check()
	}
	r.wantSets(t, [2]int{80, 150}, [2]int{0, 0})
}

func TestHeatersSetElsewhereAlsoTimeOut(t *testing.T) {
	r := newRig()
	r.printer.setHeating(60, 0)
	r.check()
	at, _ := r.s.heatInfo()
	if at == nil || !at.Equal(r.clock.Add(30*time.Minute)) {
		t.Fatalf("timer %v", at)
	}
	r.advance(30 * time.Minute)
	r.check()
	r.wantSets(t, [2]int{0, 0})
}

func TestTimerClearsWhenHeatersAreTurnedOffElsewhere(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.setHeating(0, 0)
	r.check()
	if at, _ := r.s.heatInfo(); at != nil {
		t.Fatalf("timer %v", at)
	}
	r.advance(time.Hour)
	r.check()
	r.wantSets(t, [2]int{80, 150})
}

func TestPrintKeepsHeatersOnThenTimerResumes(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.printing = true
	r.advance(30 * time.Minute)
	r.check()
	r.advance(45 * time.Minute)
	r.check()
	r.wantSets(t, [2]int{80, 150})

	// The print ended and left the heaters on, so they get a full new period.
	r.printer.printing = false
	r.advance(time.Minute)
	r.check()
	r.wantSets(t, [2]int{80, 150})
	at, _ := r.s.heatInfo()
	r.clock = *at
	r.check()
	r.wantSets(t, [2]int{80, 150}, [2]int{0, 0})
}

func TestUnknownPrintStateStillTurnsHeatersOff(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.printingErr = errNoReply
	r.advance(30 * time.Minute)
	r.check()
	r.wantSets(t, [2]int{80, 150}, [2]int{0, 0})
}

func TestFailedOffIsRetriedThenPlugIsCut(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.setErr = errNoReply
	r.advance(30 * time.Minute)
	r.check()
	if _, problem := r.s.heatInfo(); !strings.Contains(problem, "couldn't turn the heaters off") {
		t.Fatalf("problem %q", problem)
	}
	if r.plug.switches != 0 {
		t.Fatal("plug cut with no grace")
	}
	r.advance(59 * time.Second)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug cut too early")
	}
	r.advance(time.Second)
	r.check()
	if r.plug.switches != 1 || r.plug.on {
		t.Fatalf("plug not cut: %+v", r.plug)
	}
	if at, problem := r.s.heatInfo(); at != nil || !strings.Contains(problem, "plug was cut") {
		t.Fatalf("after cut: %v, %q", at, problem)
	}
}

func TestFailedOffRecoversWithoutCuttingThePlug(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.setErr = errNoReply
	r.advance(30 * time.Minute)
	r.check()
	r.printer.setErr = nil
	r.advance(5 * time.Second)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug was cut")
	}
	if at, problem := r.s.heatInfo(); at != nil || problem != "" {
		t.Fatalf("after recovery: %v, %q", at, problem)
	}
}

func TestLostLinkCutsPlugOnlyAfterLimitAndGrace(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.disconnect()
	r.advance(29 * time.Minute)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug cut before the limit")
	}
	r.advance(time.Minute + 59*time.Second)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug cut before the grace ended")
	}
	if _, problem := r.s.heatInfo(); !strings.Contains(problem, "can't reach") {
		t.Fatalf("problem %q", problem)
	}
	r.advance(time.Second)
	r.check()
	if r.plug.switches != 1 || r.plug.on {
		t.Fatalf("plug not cut: %+v", r.plug)
	}
	r.advance(time.Hour)
	r.check()
	if r.plug.switches != 1 {
		t.Fatalf("plug switched again: %+v", r.plug)
	}
}

func TestLostLinkDuringPrintLeavesPowerOn(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.printing = true
	r.advance(time.Minute)
	r.check()
	r.printer.disconnect()
	r.advance(2 * time.Hour)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug cut during a print")
	}
	if _, problem := r.s.heatInfo(); !strings.Contains(problem, "during an SD print") {
		t.Fatalf("problem %q", problem)
	}
}

func TestLostLinkWithPlugOffClearsTheTimer(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.disconnect()
	r.plug.on = false
	r.check()
	if at, problem := r.s.heatInfo(); at != nil || problem != "" {
		t.Fatalf("got %v, %q", at, problem)
	}
}

func TestPlugCutFailureIsReported(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.disconnect()
	r.plug.setErr = errors.New("bridge down")
	r.advance(32 * time.Minute)
	r.check()
	if _, problem := r.s.heatInfo(); !strings.Contains(problem, "cutting the plug failed") {
		t.Fatalf("problem %q", problem)
	}
}

func TestNoTimerMeansNoPlugCut(t *testing.T) {
	r := newRig()
	r.printer.disconnect()
	r.advance(24 * time.Hour)
	r.check()
	if r.plug.switches != 0 {
		t.Fatal("plug was cut")
	}
}

func TestCool(t *testing.T) {
	r := newRig()
	r.preheat(t)
	if w := call(r.s.handleCool, ""); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	r.wantSets(t, [2]int{80, 150}, [2]int{0, 0})
	if at, _ := r.s.heatInfo(); at != nil {
		t.Fatalf("timer %v", at)
	}
}

func TestCoolWorksDuringAPrint(t *testing.T) {
	r := newRig()
	r.printer.printing = true
	if w := call(r.s.handleCool, ""); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	r.wantSets(t, [2]int{0, 0})
}

func TestCoolFailureKeepsTheTimer(t *testing.T) {
	r := newRig()
	r.preheat(t)
	r.printer.setErr = errNoReply
	if w := call(r.s.handleCool, ""); w.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if at, _ := r.s.heatInfo(); at == nil {
		t.Fatal("timer was cleared though the heaters may be on")
	}
}

func TestCoolWhileDisconnected(t *testing.T) {
	r := newRig()
	r.printer.disconnect()
	r.plug.on = false
	if w := call(r.s.handleCool, ""); w.Code != http.StatusOK {
		t.Fatalf("plug off: %d %s", w.Code, w.Body)
	}
	r.plug.on = true
	if w := call(r.s.handleCool, ""); w.Code != http.StatusConflict {
		t.Fatalf("plug on: %d %s", w.Code, w.Body)
	}
	r.plug.on = false
	r.plug.stateErr = errors.New("bridge down")
	if w := call(r.s.handleCool, ""); w.Code != http.StatusConflict {
		t.Fatalf("plug unknown: %d %s", w.Code, w.Body)
	}
}

func TestPowerOffRefusedAfterPreheat(t *testing.T) {
	r := newRig()
	r.preheat(t)
	if w := call(r.s.handlePower, `{"on":false}`); w.Code != http.StatusConflict {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if r.plug.switches != 0 {
		t.Fatal("plug was switched")
	}
}
