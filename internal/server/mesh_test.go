package server

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/sim"
)

// The mesh lines of a real stream, with the seconds after 18:46:00 when each arrived,
// from test-logs/2026-11-04-stream/t8-long/server.log. Only these reach PrinterLine: the
// link keeps "Mesh invalidated. Probing mesh." and the position report to itself.
var realMeshLines = []struct {
	at   float64
	line string
}{
	{8.875872, "Probing mesh point 1/25."}, {10.530980, "Probing mesh point 2/25."},
	{12.841371, "Probing mesh point 3/25."}, {15.147876, "Probing mesh point 4/25."},
	{17.451667, "Probing mesh point 5/25."}, {19.761578, "Probing mesh point 6/25."},
	{22.064801, "Probing mesh point 7/25."}, {24.611640, "Probing mesh point 8/25."},
	{27.152440, "Probing mesh point 9/25."}, {29.694666, "Probing mesh point 10/25."},
	{32.000696, "Probing mesh point 11/25."}, {34.635063, "Probing mesh point 12/25."},
	{37.263536, "Probing mesh point 13/25."}, {39.565747, "Probing mesh point 14/25."},
	{41.874866, "Probing mesh point 15/25."}, {44.430823, "Probing mesh point 16/25."},
	{46.730326, "Probing mesh point 17/25."}, {49.577485, "Probing mesh point 18/25."},
	{51.874145, "Probing mesh point 19/25."}, {54.175152, "Probing mesh point 20/25."},
	{56.955577, "Probing mesh point 21/25."}, {59.254098, "Probing mesh point 22/25."},
	{61.551975, "Probing mesh point 23/25."}, {64.547998, "Probing mesh point 24/25."},
	{67.547665, "Probing mesh point 25/25."},
}

func statusMesh(t *testing.T, r *uploadRig) *MeshProgress {
	t.Helper()
	w := httptestGet(r.s.handleStatus)
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var st StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Print == nil {
		t.Fatal("no print in status")
	}
	return st.Print.Mesh
}

func TestMeshPointsFromARealStream(t *testing.T) {
	r, name := streamRig(t, sim.Options{MoveTime: 20 * time.Millisecond}, 5000)
	r.startPrint(t, name)
	r.waitPrint(t, PrintRunning)
	var mu sync.Mutex
	base := time.Now()
	clock := base
	set := func(secs float64) {
		mu.Lock()
		defer mu.Unlock()
		clock = base.Add(time.Duration(secs * float64(time.Second)))
	}
	r.s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	if m := statusMesh(t, r); m != nil {
		t.Fatalf("no mesh yet: %+v", m)
	}
	for i, l := range realMeshLines {
		set(l.at)
		r.s.PrinterLine(l.line)
		set(l.at + 1.5)
		m := statusMesh(t, r)
		if m == nil || m.Point != i+1 || m.Of != 25 {
			t.Fatalf("after %q: %+v", l.line, m)
		}
		if !m.StartedAt.Equal(base.Add(time.Duration(realMeshLines[0].at * float64(time.Second)))) {
			t.Errorf("point %d: the mesh started at %v", m.Point, m.StartedAt.Sub(base))
		}
		if math.Abs(m.PointSeconds-1.5) > 1e-6 {
			t.Errorf("point %d started %.3f s ago, not 1.5", m.Point, m.PointSeconds)
		}
	}
	job := r.s.printJob()
	if job.State != PrintRunning || r.s.print.halted != "" {
		t.Errorf("mesh lines aren't trouble: %+v, halted %q", job, r.s.print.halted)
	}
	// A second G29 starts again.
	set(100)
	r.s.PrinterLine("Probing mesh point 1/25.")
	if m := statusMesh(t, r); m.Point != 1 || !m.StartedAt.Equal(base.Add(100*time.Second)) {
		t.Errorf("a new mesh: %+v", m)
	}
}

// From the file through the simulated firmware and the link to the job.
func TestG29InAStreamReportsItsMesh(t *testing.T) {
	r := newUploadRig(t, sim.Options{MeshScale: 0.002})
	const name = "mesh.gcode"
	r.put(t, name, ";TIME:100\nG28\nG29 P1 ; Probe and build the mesh\nG29 A\n"+strings.TrimPrefix(gcodeOf(20), ";TIME:100\n"))
	r.startPrint(t, name)
	job := r.waitPrint(t, PrintFinished)
	if m := job.Mesh; m == nil || m.Point != 25 || m.Of != 25 || m.PointAt.Before(m.StartedAt) {
		t.Errorf("mesh: %+v", job.Mesh)
	}
	if r.sawCommand("G29") != 2 || len(r.fileLines()) != 20 {
		t.Errorf("%d G29s, %d moves", r.sawCommand("G29"), len(r.fileLines()))
	}

	// Then the auto-off reads the print's mesh, before the plug is cut.
	deadline := time.Now().Add(5 * time.Second)
	for r.s.printJob().MeshRead == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	read := r.s.printJob().MeshRead
	if read == nil || read.Mesh == nil || read.Error != "" || !read.Mesh.Valid {
		t.Fatalf("mesh read: %+v", read)
	}
	// The simulator's probed mesh dips in the middle, unlike the one it has at power-on.
	if z := read.Mesh.Z[2][2]; z == nil || math.Abs(*z-(-0.02)) > 0.001 {
		t.Errorf("middle: %v", z)
	}
	if r.sawCommand("M420") != 1 || r.sawCommand("M420 V1 T1") != 1 {
		t.Errorf("sent M420 %d times", r.sawCommand("M420"))
	}
	data, err := os.ReadFile(filepath.Join(r.s.DataDir, meshHistoryFile))
	if err != nil || !strings.Contains(string(data), `"name":"mesh.gcode"`) || !strings.Contains(string(data), `"valid":true`) {
		t.Errorf("history: %s, %v", data, err)
	}
}

// Only a print whose own G29 finished leaves its mesh in the firmware. Without one, a
// read would give the mesh loaded at boot, so there's none.
func TestNoMeshReadWithoutTheFilesOwnMesh(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	const name = "plain.gcode"
	r.put(t, name, gcodeOf(20))
	r.startPrint(t, name)
	r.waitPrint(t, PrintFinished)
	time.Sleep(100 * time.Millisecond)
	if job := r.s.printJob(); job.MeshRead != nil || r.sawCommand("M420") != 0 {
		t.Errorf("read %+v, %d M420s", job.MeshRead, r.sawCommand("M420"))
	}
}

// Before the file starts, a mesh line can only be a stray, and mustn't fail the print as
// a halt would.
func TestMeshLineBeforeTheFileIsIgnored(t *testing.T) {
	r, name := printRig(t, sim.Options{HeatRate: 1})
	if w := call(r.s.handlePrint, `{"name":"`+name+`","from":"sd","bed_clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.s.PrinterLine("Probing mesh point 1/25.")
	time.Sleep(50 * time.Millisecond)
	if job := r.s.printJob(); job.State != PrintHeating || job.Mesh != nil || r.s.print.halted != "" {
		t.Errorf("job: %+v, halted %q", job, r.s.print.halted)
	}
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
}
