package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
	"tramline/internal/sim"
)

// webRig has a server with no printer behind it at all: Prints, Heaters, Card, Prober and
// Plug are nil, so anything on the page trying to use them would panic.
func webRig(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := &Server{
		Version: "test",
		Printer: func() marlin.Status {
			return marlin.Status{Connected: true, Firmware: "Marlin", Temps: &marlin.Temps{Nozzle: 230, NozzleTarget: 235}}
		},
		DataDir: t.TempDir(),
	}
	return s, s.WebHandler()
}

func webGet(h http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func addUpload(t *testing.T, s *Server, name, content string) {
	t.Helper()
	dir, err := s.uploadsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestWebServesThePage(t *testing.T) {
	_, h := webRig(t)
	for path, want := range map[string]string{
		"/":                 "text/html",
		"/page.js":          "javascript",
		"/style.css":        "text/css",
		"/shared/view.js":   "javascript",
		"/shared/status.js": "javascript",
		"/shared/theme.css": "text/css",
	} {
		w := webGet(h, http.MethodGet, path)
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), want) {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s has no content security policy", path)
		}
	}
}

// The GUI's files are only served by tramline gui, on the machine it runs on.
func TestWebDoesNotServeTheGUI(t *testing.T) {
	_, h := webRig(t)
	for _, path := range []string{"/gui/index.html", "/gui/gui.js", "/shared/../gui/gui.js", "/gui.js"} {
		if w := webGet(h, http.MethodGet, path); w.Code == http.StatusOK {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
}

// The page's routes can't reach the API, whatever the method.
func TestWebHasNoAPI(t *testing.T) {
	_, h := webRig(t)
	for _, path := range []string{"/v1/status", "/v1/estop", "/v1/print", "/v1/files", "/v1/settings", "/v1/hue/plugs", "/../v1/status"} {
		if w := webGet(h, http.MethodGet, path); w.Code == http.StatusOK {
			t.Errorf("GET %s was served: %s", path, w.Body)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/", "/api/status", "/api/model/1", "/v1/estop"} {
			if w := webGet(h, method, path); w.Code < 400 {
				t.Errorf("%s %s: %d", method, path, w.Code)
			}
		}
	}
}

func TestWebStatusWithNoJob(t *testing.T) {
	_, h := webRig(t)
	w := webGet(h, http.MethodGet, "/api/status")
	var st WebStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Connected || st.Temps == nil || st.Temps.Nozzle != 230 || st.Print != nil {
		t.Fatalf("got %s", w.Body)
	}
}

func TestWebStatusFollowsTheJob(t *testing.T) {
	s, h := webRig(t)
	s.Now = func() time.Time { return time.Date(2026, 11, 4, 12, 0, 30, 0, time.UTC) }
	started := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	s.print.job = &PrintJob{
		ID: 7, Name: "part.gcode", Mode: ModeStream, State: PrintRunning, Pos: 50, Size: 200,
		StartedAt: started, MovedAt: started.Add(20 * time.Second),
	}
	w := webGet(h, http.MethodGet, "/api/status")
	var st WebStatus
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	p := st.Print
	if p == nil || p.ID != 7 || p.Name != "part.gcode" || p.Pos != 50 || p.Size != 200 || p.Mode != ModeStream || p.State != PrintRunning {
		t.Fatalf("got %s", w.Body)
	}
	if p.MovedAgo != 10 || p.RunSeconds != 30 {
		t.Fatalf("moved %v ago, running %v s", p.MovedAgo, p.RunSeconds)
	}
}

func TestWebModelGivesTheJobsMoves(t *testing.T) {
	s, h := webRig(t)
	file := gcodeOf(5)
	addUpload(t, s, "part.gcode", file)
	s.print.job = &PrintJob{ID: 3, Name: "part.gcode", State: PrintRunning}

	w := webGet(h, http.MethodGet, "/api/model/3")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	parsed, err := gcode.ParsePath(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.String() != string(parsed.Encode()) {
		t.Fatalf("got %d bytes, want the %d of the encoded path", w.Body.Len(), len(parsed.Encode()))
	}

	// Only the current job's number gets anything.
	for _, path := range []string{"/api/model/2", "/api/model/x", "/api/model/"} {
		if w := webGet(h, http.MethodGet, path); w.Code == http.StatusOK {
			t.Errorf("%s was served", path)
		}
	}
}

func TestWebModelReadsTheFileOnce(t *testing.T) {
	s, h := webRig(t)
	addUpload(t, s, "part.gcode", gcodeOf(5))
	s.print.job = &PrintJob{ID: 1, Name: "part.gcode", State: PrintRunning}
	first := webGet(h, http.MethodGet, "/api/model/1").Body.String()
	s.web.model[0] = 'X'
	if got := webGet(h, http.MethodGet, "/api/model/1").Body.String(); got[0] != 'X' {
		t.Fatalf("read again: %q", got[:4])
	}

	// A changed file is read again.
	dir, _ := s.uploadsDir()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "part.gcode"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := webGet(h, http.MethodGet, "/api/model/1").Body.String(); got != first {
		t.Fatalf("wasn't read again")
	}
}

// A real stream on the simulator, seen through the page's own routes.
func TestWebFollowsAStream(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	const name = "part.gcode"
	file := gcodeOf(200)
	r.put(t, name, file)
	h := r.s.WebHandler()
	r.startPrint(t, name)

	r.waitFor(t, "the stream to finish", func() bool {
		var st WebStatus
		if err := json.Unmarshal(webGet(h, http.MethodGet, "/api/status").Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st.Print != nil && st.Print.State == PrintFinished
	})
	job := r.s.printJob()
	w := webGet(h, http.MethodGet, "/api/model/"+strconv.Itoa(job.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// The file's last move ends where a finished job's position does.
	parsed, err := gcode.ParsePath(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if last := parsed.Moves[len(parsed.Moves)-1]; int64(last.End) != job.Pos {
		t.Fatalf("the last move ends at %d, the job at %d", last.End, job.Pos)
	}
}

// A print from the LCD, or a job name from a damaged save, can't read anything else.
func TestWebModelRefusesWhatIsntAnUpload(t *testing.T) {
	s, h := webRig(t)
	secret := filepath.Join(s.DataDir, "hue.gcode")
	if err := os.WriteFile(secret, []byte("G1 X1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"../hue.gcode", "TEST.GCO", "", "..", "/etc/passwd"} {
		s.print.job = &PrintJob{ID: i + 1, Name: name, State: PrintRunning, Elsewhere: true}
		if w := webGet(h, http.MethodGet, "/api/model/"+strconv.Itoa(i+1)); w.Code != http.StatusNotFound {
			t.Errorf("%q: %d %s", name, w.Code, w.Body)
		}
	}
}
