package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
	"tramline/internal/sim"
)

// uploadRig runs the real protocol code against a simulated board.
type uploadRig struct {
	s       *Server
	plug    *fakePlug
	card    *sim.Card
	printer *marlin.Printer

	mu   sync.Mutex
	seen []string
}

func (r *uploadRig) saw(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, line)
}

func (r *uploadRig) sawCommand(word string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, line := range r.seen {
		if strings.HasPrefix(line, word) {
			n++
		}
	}
	return n
}

func newUploadRig(t *testing.T, opts sim.Options) *uploadRig {
	t.Helper()
	r := &uploadRig{plug: &fakePlug{on: true}, card: sim.NewCard()}
	opts.Card, opts.Seen = r.card, r.saw
	if opts.HeatRate == 0 {
		opts.HeatRate = 4000
	}
	r.printer = marlin.NewPrinter(func() (io.ReadWriteCloser, error) { return sim.New(opts), nil })
	r.printer.SDLineIdle = 40 * time.Millisecond
	r.printer.StreamNudge = 40 * time.Millisecond
	r.printer.OnLine = func(line string) {
		if r.s != nil {
			r.s.PrinterLine(line)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.s = &Server{
		Plug: r.plug, Heaters: r.printer, Card: r.printer, Prober: r.printer, Prints: r.printer,
		Printer: r.printer.Status, DataDir: t.TempDir(), ctx: ctx, HeatHold: time.Millisecond,
	}
	r.s.print.every = 10 * time.Millisecond
	r.s.print.grace = 50 * time.Millisecond
	tuneSchedule(r.s)
	go r.printer.Run(ctx)
	// Any print's end now saves as the printer turns off, which mustn't land while the
	// folder is being removed.
	t.Cleanup(func() { waitAutoOff(r.s) })
	r.waitFor(t, "connected", func() bool {
		st := r.printer.Status()
		return st.Connected && st.Temps != nil
	})
	return r
}

func (r *uploadRig) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func gcodeOf(n int) string {
	var b strings.Builder
	b.WriteString(";TIME:100\n")
	for i := range n {
		fmt.Fprintf(&b, "G1 X%d Y%d ;move\n", i, i)
	}
	return b.String()
}

func hashOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func named(h http.HandlerFunc, method, name, body string, header map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("name", name)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	h(w, req)
	return w
}

func (r *uploadRig) put(t *testing.T, name, content string) FileRecord {
	t.Helper()
	w := named(r.s.handleFilePut, http.MethodPut, name, content, map[string]string{HashHeader: hashOf(content)})
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	var rec FileRecord
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (r *uploadRig) startCopy(t *testing.T, name string) CopyResponse {
	t.Helper()
	w := named(r.s.handleCopy, http.MethodPost, name, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("copy: %d %s", w.Code, w.Body)
	}
	var out CopyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *uploadRig) waitCopy(t *testing.T) *CopyJob {
	t.Helper()
	r.waitFor(t, "the copy to finish", func() bool { return !r.s.copyRunning() })
	// The operation lock is freed just after the job is marked finished.
	r.s.heat.op.Lock()
	defer r.s.heat.op.Unlock()
	return r.s.copyJob()
}

func (r *uploadRig) record(t *testing.T, name string) FileRecord {
	t.Helper()
	r.s.files.mu.Lock()
	defer r.s.files.mu.Unlock()
	rec, err := r.s.loadRecord(name)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestPutSavesOnThePiWithoutTouchingThePrinter(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	content := gcodeOf(10)
	rec := r.put(t, "part.gcode", content)
	if rec.SDName == "" || rec.OnCard || rec.SDState != "" || rec.Info.Lines != 10 {
		t.Fatalf("got %+v", rec)
	}
	got, err := os.ReadFile(filepath.Join(r.s.DataDir, uploadsDirName, "part.gcode"))
	if err != nil || string(got) != content {
		t.Fatalf("got %v, %q", err, got)
	}
	if n := r.sawCommand("M28"); n != 0 || len(r.card.Names()) != 0 {
		t.Fatalf("the card was touched: %d, %q", n, r.card.Names())
	}
}

func TestPutRefusals(t *testing.T) {
	good := gcodeOf(3)
	for _, tt := range []struct {
		name, file, content, hash string
		code                      int
		want                      string
	}{
		{"bad name", "../evil.gcode", good, hashOf(good), 400, "file name"},
		{"dotted name", ".hidden.gcode", good, hashOf(good), 400, "file name"},
		{"wrong type", "notes.txt", good, hashOf(good), 400, "file name"},
		{"no hash", "a.gcode", good, "", 400, HashHeader},
		{"wrong hash", "a.gcode", good, hashOf("other"), 400, "damaged"},
		{"unsafe line", "a.gcode", "M29\n", hashOf("M29\n"), 400, "M29"},
		{"empty", "a.gcode", ";x\n", hashOf(";x\n"), 400, "no commands"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newUploadRig(t, sim.Options{})
			w := named(r.s.handleFilePut, http.MethodPut, tt.file, tt.content, map[string]string{HashHeader: tt.hash})
			if w.Code != tt.code || !strings.Contains(w.Body.String(), tt.want) {
				t.Fatalf("got %d %s", w.Code, w.Body)
			}
			entries, _ := os.ReadDir(filepath.Join(r.s.DataDir, uploadsDirName))
			if len(entries) != 0 {
				t.Fatalf("left behind: %v", entries)
			}
		})
	}
}

func TestCopyVerifiesAndSkipsWhenAlreadyThere(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	content := gcodeOf(500)
	rec := r.put(t, "part.gcode", content)
	out := r.startCopy(t, "part.gcode")
	if out.Job == nil {
		t.Fatal("expected a job")
	}
	job := r.waitCopy(t)
	if job.State != CopyDone || job.Lines != 500 || job.Bytes != rec.Info.SDSize || job.Error != "" {
		t.Fatalf("got %+v", job)
	}
	got, ok := r.card.File(rec.SDName)
	if !ok || int64(len(got)) != rec.Info.SDSize || strings.Contains(got, ";") {
		t.Fatalf("got %v, %d bytes", ok, len(got))
	}
	if saved := r.record(t, "part.gcode"); saved.SDState != CopyDone || !saved.OnCard {
		t.Fatalf("got %+v", saved)
	}
	if again := r.startCopy(t, "part.gcode"); again.Job != nil {
		t.Fatal("a verified file shouldn't be copied again")
	}
	if n := r.sawCommand("M29"); n != 1 {
		t.Fatalf("saved %d times", n)
	}
}

// A failed copy retries from the file on the Pi, which isn't sent again.
func TestFailedCopyRetriesWithoutANewUpload(t *testing.T) {
	r := newUploadRig(t, sim.Options{WriteErrorLine: 50})
	rec := r.put(t, "part.gcode", gcodeOf(200))
	r.startCopy(t, "part.gcode")
	job := r.waitCopy(t)
	if job.State != CopyFailed || !strings.Contains(job.Error, "write error") {
		t.Fatalf("got %+v", job)
	}
	if _, ok := r.card.File(rec.SDName); ok {
		t.Fatal("the partial file should be deleted")
	}
	if saved := r.record(t, "part.gcode"); saved.SDState != CopyFailed || saved.OnCard {
		t.Fatalf("got %+v", saved)
	}
	r.startCopy(t, "part.gcode")
	if job := r.waitCopy(t); job.State != CopyDone {
		t.Fatalf("got %+v", job)
	}
	if got, _ := r.card.File(rec.SDName); int64(len(got)) != rec.Info.SDSize {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestLineFaultsDuringACopyStillVerify(t *testing.T) {
	r := newUploadRig(t, sim.Options{CorruptLine: 30, LoseOKLine: 60, DropLine: 90})
	rec := r.put(t, "part.gcode", gcodeOf(200))
	r.startCopy(t, "part.gcode")
	job := r.waitCopy(t)
	if job.State != CopyDone || job.Resends == 0 || job.Window != marlin.DefaultSDWindow {
		t.Fatalf("got %+v", job)
	}
	if got, _ := r.card.File(rec.SDName); int64(len(got)) != rec.Info.SDSize {
		t.Fatalf("got %d bytes", len(got))
	}
}

// wrongSize makes the listing disagree, as a lost write would.
type wrongSize struct{ Card }

func (c wrongSize) ListSD(ctx context.Context) ([]marlin.SDFile, error) {
	files, err := c.Card.ListSD(ctx)
	for i := range files {
		files[i].Size--
	}
	return files, err
}

func TestCopyFailsIfTheSizeIsWrong(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	rec := r.put(t, "part.gcode", gcodeOf(20))
	r.s.Card = wrongSize{r.printer}
	r.startCopy(t, "part.gcode")
	job := r.waitCopy(t)
	if job.State != CopyFailed || !strings.Contains(job.Error, "should be") {
		t.Fatalf("got %+v", job)
	}
	if _, ok := r.card.File(rec.SDName); ok {
		t.Fatal("the bad file should be deleted")
	}
}

func TestCopyRefusals(t *testing.T) {
	t.Run("unknown file", func(t *testing.T) {
		r := newUploadRig(t, sim.Options{})
		if w := named(r.s.handleCopy, http.MethodPost, "none.gcode", "", nil); w.Code != http.StatusNotFound {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
	t.Run("plug off", func(t *testing.T) {
		r := newUploadRig(t, sim.Options{})
		r.put(t, "part.gcode", gcodeOf(5))
		r.plug.on = false
		w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "plug is off") {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
	t.Run("print running", func(t *testing.T) {
		r := newUploadRig(t, sim.Options{Printing: func() bool { return true }})
		r.put(t, "part.gcode", gcodeOf(5))
		w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "print is running") {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
		if n := r.sawCommand("M28"); n != 0 {
			t.Fatal("M28 would have aborted the print")
		}
	})
	t.Run("not from here", func(t *testing.T) {
		r := newUploadRig(t, sim.Options{})
		rec := r.put(t, "part.gcode", gcodeOf(5))
		r.card.Put(rec.SDName, "precious")
		w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "didn't come from here") {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
		if got, _ := r.card.File(rec.SDName); got != "precious" {
			t.Fatalf("overwritten: %q", got)
		}
	})
	t.Run("no card", func(t *testing.T) {
		r := newUploadRig(t, sim.Options{})
		r.put(t, "part.gcode", gcodeOf(5))
		r.card.Missing = true
		w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil)
		if w.Code == http.StatusOK || !strings.Contains(w.Body.String(), "no SD card") {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	})
}

func slowCopy(t *testing.T) *uploadRig {
	t.Helper()
	r := newUploadRig(t, sim.Options{LineDelay: 2 * time.Millisecond})
	r.put(t, "part.gcode", gcodeOf(3000))
	r.startCopy(t, "part.gcode")
	r.waitFor(t, "lines to be sent", func() bool {
		job := r.s.copyJob()
		return job.Lines > 5
	})
	return r
}

func TestEverythingElseIsRefusedDuringACopy(t *testing.T) {
	r := slowCopy(t)
	for _, tt := range []struct {
		name string
		h    http.HandlerFunc
		body string
	}{
		{"preheat", r.s.handlePreheat, `{"bed":50,"nozzle":50}`},
		{"tram", r.s.handleTram, bedClear},
		{"power off", r.s.handlePower, `{"on":false}`},
		{"power on", r.s.handlePower, `{"on":true}`},
	} {
		if w := call(tt.h, tt.body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "copy") {
			t.Errorf("%s: got %d %s", tt.name, w.Code, w.Body)
		}
	}
	if w := httptestGet(r.s.handleTramInfo); w.Code != http.StatusConflict {
		t.Errorf("tram info: got %d %s", w.Code, w.Body)
	}
	if w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil); w.Code != http.StatusConflict {
		t.Errorf("second copy: got %d %s", w.Code, w.Body)
	}
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusConflict {
		t.Errorf("delete: got %d %s", w.Code, w.Body)
	}
	content := gcodeOf(2)
	if w := named(r.s.handleFilePut, http.MethodPut, "part.gcode", content, map[string]string{HashHeader: hashOf(content)}); w.Code != http.StatusConflict {
		t.Errorf("replacing it: got %d %s", w.Code, w.Body)
	}
	// None of this touched the transfer.
	if !r.s.copyRunning() {
		t.Error("the copy stopped")
	}
	// Status needs no lock, and shows the copy.
	if w := httptestGet(r.s.handleStatus); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"running"`) {
		t.Errorf("status: got %d %s", w.Code, w.Body)
	}
	if w := httptestGet(r.s.handleFiles); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "a copy is going") {
		t.Errorf("files: got %d %s", w.Code, w.Body)
	}
}

func TestCopyIsRefusedDuringATramRun(t *testing.T) {
	r := newUploadRig(t, sim.Options{MoveDelay: 300 * time.Millisecond})
	r.put(t, "part.gcode", gcodeOf(5))
	if w := call(r.s.handleTram, bedClear); w.Code != http.StatusOK {
		t.Fatalf("tram: %d %s", w.Code, w.Body)
	}
	w := named(r.s.handleCopy, http.MethodPost, "part.gcode", "", nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "tram") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestCancelStopsTheCopyAndDeletesThePartialFile(t *testing.T) {
	r := slowCopy(t)
	rec := r.record(t, "part.gcode")
	if w := call(r.s.handleCopyCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	job := r.waitCopy(t)
	if job.State != CopyFailed || !strings.Contains(job.Error, "cancelled on request") {
		t.Fatalf("got %+v", job)
	}
	if _, ok := r.card.File(rec.SDName); ok || r.card.Saving() != "" {
		t.Fatalf("still there: %v, saving %q", ok, r.card.Saving())
	}
	if w := call(r.s.handleCopyCancel, ""); w.Code != http.StatusConflict {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

// cool can't wait for a long copy, so it stops it and the heaters do go off.
func TestCoolStopsACopyAndTurnsTheHeatersOff(t *testing.T) {
	r := newUploadRig(t, sim.Options{LineDelay: 2 * time.Millisecond})
	if w := call(r.s.handlePreheat, `{"bed":50,"nozzle":60}`); w.Code != http.StatusOK {
		t.Fatalf("preheat: %d %s", w.Code, w.Body)
	}
	r.put(t, "part.gcode", gcodeOf(3000))
	r.startCopy(t, "part.gcode")
	r.waitFor(t, "lines to be sent", func() bool { return r.s.copyJob().Lines > 5 })

	start := time.Now()
	if w := call(r.s.handleCool, ""); w.Code != http.StatusOK {
		t.Fatalf("cool: %d %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("cool took %s", took)
	}
	if job := r.s.copyJob(); job.State != CopyFailed || !strings.Contains(job.Error, "cool was requested") {
		t.Fatalf("got %+v", job)
	}
	lines, err := r.printer.SDPrinting(context.Background())
	if err != nil || lines {
		t.Fatalf("got %v, %v", lines, err)
	}
	r.waitFor(t, "the targets to report off", func() bool {
		temps := r.printer.Status().Temps
		return temps.BedTarget == 0 && temps.NozzleTarget == 0
	})
}

func TestIdleTimeoutStopsACopyAndTurnsTheHeatersOff(t *testing.T) {
	r := newUploadRig(t, sim.Options{LineDelay: 2 * time.Millisecond})
	if w := call(r.s.handlePreheat, `{"bed":50,"nozzle":60}`); w.Code != http.StatusOK {
		t.Fatalf("preheat: %d %s", w.Code, w.Body)
	}
	r.waitFor(t, "targets to be reported", func() bool { return r.printer.Status().Temps.BedTarget == 50 })
	r.put(t, "part.gcode", gcodeOf(3000))
	r.startCopy(t, "part.gcode")
	r.waitFor(t, "lines to be sent", func() bool { return r.s.copyJob().Lines > 5 })

	// Not expired yet: the check leaves the copy alone, and doesn't wait for it.
	before := r.s.offAt()
	start := time.Now()
	r.s.checkHeaters(context.Background())
	if time.Since(start) > 200*time.Millisecond || !r.s.copyRunning() {
		t.Fatal("the idle check got in the copy's way")
	}
	if !r.s.offAt().Equal(before) {
		t.Fatal("a copy mustn't extend the timer")
	}

	r.s.setOffAt(time.Now().Add(-time.Second))
	r.s.checkHeaters(context.Background())
	if job := r.s.copyJob(); job.State != CopyFailed || !strings.Contains(job.Error, "idle timeout") {
		t.Fatalf("got %+v", job)
	}
	r.waitFor(t, "the targets to report off", func() bool {
		temps := r.printer.Status().Temps
		return temps.BedTarget == 0 && temps.NozzleTarget == 0
	})
	if !r.s.offAt().IsZero() {
		t.Fatal("the timer should be cleared")
	}
}

func TestLinkDropFailsTheCopyAndARetryWorks(t *testing.T) {
	r := newUploadRig(t, sim.Options{DropLinkLine: 50})
	rec := r.put(t, "part.gcode", gcodeOf(200))
	r.startCopy(t, "part.gcode")
	job := r.waitCopy(t)
	if job.State != CopyFailed || !strings.Contains(job.Error, "disconnected") {
		t.Fatalf("got %+v", job)
	}
	saved := r.record(t, "part.gcode")
	if !saved.OnCard || !strings.Contains(saved.SDError, "partial file") {
		t.Fatalf("the partial file should be flagged: %+v", saved)
	}
	// The board kept its power, so it's still saving until the link comes back.
	r.waitFor(t, "reconnecting", func() bool {
		st := r.printer.Status()
		return st.Connected && st.Temps != nil
	})
	if r.card.Saving() != "" {
		t.Fatal("reconnecting should end the save")
	}
	r.startCopy(t, "part.gcode")
	if job := r.waitCopy(t); job.State != CopyDone {
		t.Fatalf("got %+v", job)
	}
	if got, _ := r.card.File(rec.SDName); int64(len(got)) != rec.Info.SDSize {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestRestartMarksARunningCopyAsFailed(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	rec := r.put(t, "part.gcode", gcodeOf(5))
	rec.SDState, rec.OnCard = CopyRunning, true
	if err := r.s.saveRecord(rec); err != nil {
		t.Fatal(err)
	}
	fresh := &Server{DataDir: r.s.DataDir}
	got, err := fresh.loadRecord("part.gcode")
	if err != nil {
		t.Fatal(err)
	}
	if got.SDState != CopyFailed || !strings.Contains(got.SDError, "server stopped") || !got.OnCard {
		t.Fatalf("got %+v", got)
	}
}

func TestNewVersionKeepsItsPlaceOnTheCard(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	first := r.put(t, "part.gcode", gcodeOf(50))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	second := r.put(t, "part.gcode", gcodeOf(5))
	if second.SDName != first.SDName || !second.OnCard || second.SDState != "" {
		t.Fatalf("got %+v", second)
	}
	r.startCopy(t, "part.gcode")
	if job := r.waitCopy(t); job.State != CopyDone {
		t.Fatalf("got %+v", job)
	}
	if got := r.card.Names(); !slices.Equal(got, []string{first.SDName}) {
		t.Fatalf("got %q", got)
	}
}

func TestDelete(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.card.Put("OTHER.GCO", "G28\r\n")
	rec := r.put(t, "part.gcode", gcodeOf(5))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if _, ok := r.card.File(rec.SDName); ok {
		t.Fatal("still on the card")
	}
	if _, ok := r.card.File("OTHER.GCO"); !ok {
		t.Fatal("deleted someone else's file")
	}
	entries, _ := os.ReadDir(filepath.Join(r.s.DataDir, uploadsDirName))
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestDeleteKeepsTheRecordIfTheCardCantBeReached(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.put(t, "part.gcode", gcodeOf(5))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	r.plug.on = false
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusConflict {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if !r.record(t, "part.gcode").OnCard {
		t.Fatal("the record should stay")
	}
}

func TestDeleteWorksWithThePrinterOffIfItNeverWentToTheCard(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.put(t, "part.gcode", gcodeOf(5))
	r.plug.on = false
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestFilesListsBothPlaces(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.card.Put("OTHER.GCO", "G28\r\n")
	r.put(t, "part.gcode", gcodeOf(5))
	w := httptestGet(r.s.handleFiles)
	var out FilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || len(out.Card) != 1 || out.Card[0].Name != "OTHER.GCO" {
		t.Fatalf("got %+v", out)
	}
}

func TestFilesDuringAPrintLeavesOutTheCard(t *testing.T) {
	r := newUploadRig(t, sim.Options{Printing: func() bool { return true }})
	w := httptestGet(r.s.handleFiles)
	if !strings.Contains(w.Body.String(), "print is running") || r.sawCommand("M20") != 0 {
		t.Fatalf("got %s, %d", w.Body, r.sawCommand("M20"))
	}
}

func TestShortNamesDontClash(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	a := r.put(t, "stargate_gate.gcode", gcodeOf(2))
	var clash string
	for i := range 100000 {
		name := fmt.Sprintf("stargate%d.gcode", i)
		if gcode.ShortName(name, 0) == a.SDName {
			clash = name
			break
		}
	}
	if clash == "" {
		t.Fatal("no clashing name found")
	}
	if b := r.put(t, clash, gcodeOf(2)); b.SDName == a.SDName {
		t.Fatalf("both are %s", b.SDName)
	}
}

// Seen on the real printer: status still said a deleted file was on the card.
func TestStatusForgetsACopyOfAFileThatChanged(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.put(t, "part.gcode", gcodeOf(5))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	r.put(t, "part.gcode", gcodeOf(6))
	if job := r.s.copyJob(); job != nil {
		t.Fatalf("replacing it: got %+v", job)
	}
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	if w := named(r.s.handleFileDelete, http.MethodDelete, "part.gcode", "", nil); w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if job := r.s.copyJob(); job != nil {
		t.Fatalf("deleting it: got %+v", job)
	}
}

func TestCopyWindow(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.put(t, "part.gcode", gcodeOf(100))
	for _, bad := range []string{`{"window":-1}`, `{"window":99999}`, `{"window":"big"}`} {
		if w := named(r.s.handleCopy, http.MethodPost, "part.gcode", bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", bad, w.Code, w.Body)
		}
	}
	w := named(r.s.handleCopy, http.MethodPost, "part.gcode", `{"window":1}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if job := r.waitCopy(t); job.State != CopyDone || job.Window != 1 {
		t.Fatalf("got %+v", job)
	}
}
