package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"tramline/internal/sim"
)

func (r *uploadRig) waitPlugOff(t *testing.T) {
	t.Helper()
	r.waitFor(t, "the plug to go off", func() bool {
		st := r.s.plugStatus(context.Background())
		return st != nil && !st.On
	})
}

func TestEveryPrintTurnsThePrinterOffWhenItEnds(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 100 * time.Millisecond, Corners: inTram})
	tuneSchedule(r.s)
	r.startPrint(t, name)
	r.waitPrint(t, PrintFinished)
	r.waitPlugOff(t)
}

func TestACancelledPrintTurnsThePrinterOff(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Corners: inTram})
	tuneSchedule(r.s)
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
	r.waitPlugOff(t)
}

func TestAPrintFromTheLCDLeavesThePrinterOn(t *testing.T) {
	r := newUploadRig(t, sim.Options{PrintTime: 100 * time.Millisecond})
	r.card.Put("LCD.GCO", "G1 X1\nG1 X2\n")
	tuneSchedule(r.s)
	r.card.StartPrint("LCD.GCO", 100*time.Millisecond)
	if !r.s.adoptPrint(context.Background()) {
		t.Fatal("it should be followed")
	}
	r.waitFor(t, "the print to end", func() bool { return !r.s.printJob().active() })
	time.Sleep(30 * time.Millisecond)
	if st := r.s.plugStatus(context.Background()); st == nil || !st.On {
		t.Error("the plug went off for a print started from the LCD")
	}
}

func TestPrintTurnsThePrinterOn(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Corners: inTram})
	tuneSchedule(r.s)
	r.plug.on = false
	r.startPrint(t, name)
	if !r.plug.on || r.sawCommand("M24") != 1 {
		t.Errorf("plug on %t, M24 %d", r.plug.on, r.sawCommand("M24"))
	}
	call(r.s.handleCancel, "")
	r.waitPrint(t, PrintCancelled)
}

func TestARefusedPrintDoesntTouchThePlug(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: time.Minute})
	r.plug.on = false
	for _, body := range []string{
		`{"name":"` + name + `","bed_clear":true}`,
		`{"name":"` + name + `","from":"usb","bed_clear":true}`,
		`{"name":"none.gcode","from":"pi","bed_clear":true}`,
	} {
		if w := call(r.s.handlePrint, body); w.Code == http.StatusOK {
			t.Errorf("%s: should be refused", body)
		}
	}
	if r.plug.on {
		t.Error("it switched the plug on")
	}
}

func TestFromIsWhatTheUserSaid(t *testing.T) {
	// A verified copy on the card doesn't stop it streaming, when that's what's asked for.
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Corners: inTram})
	tuneSchedule(r.s)
	if w := call(r.s.handlePrint, `{"name":"`+name+`","from":"pi","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("print: %d %s", w.Code, w.Body)
	}
	if job := r.s.printJob(); job.Mode != ModeStream || job.SDName != "" {
		t.Errorf("job: %+v", job)
	}
	r.waitPrint(t, PrintFinished)
	if r.sawCommand("M23") != 0 {
		t.Error("it started the card's copy")
	}

	// Without a copy, sd is refused rather than falling back.
	r.put(t, "nocopy.gcode", gcodeOf(3))
	w := call(r.s.handlePrint, `{"name":"nocopy.gcode","from":"sd","bed_clear":true}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "tramline copy nocopy.gcode") {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
}
