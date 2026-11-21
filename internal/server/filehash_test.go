package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tramline/internal/sim"
)

func (r *uploadRig) files(t *testing.T) FilesResponse {
	t.Helper()
	w := httptestGet(r.s.handleFiles)
	var out FilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// damage changes a file on the Pi's disk behind the server's back, keeping its size.
func (r *uploadRig) damage(t *testing.T, name string) {
	t.Helper()
	path := filepath.Join(r.s.DataDir, uploadsDirName, name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(data), "X1")
	if i < 0 {
		t.Fatal("nothing to change")
	}
	data[i+1] = '2'
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	// A cached check would hide it until it's 10 minutes old.
	r.s.hashes.mu.Lock()
	r.s.hashes.cache = nil
	r.s.hashes.mu.Unlock()
}

func TestFilesChecksTheDiskAndTheCard(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	r.card.Put("OTHER.GCO", "G28\r\n")
	rec := r.put(t, "part.gcode", gcodeOf(50))
	r.startCopy(t, "part.gcode")
	if job := r.waitCopy(t); job.State != CopyDone || job.SHA256 != rec.SHA256 {
		t.Fatalf("got %+v", job)
	}
	if saved := r.record(t, "part.gcode"); saved.CardSHA256 != rec.SHA256 || saved.CardSize != rec.Info.SDSize || saved.Disk != nil {
		t.Fatalf("got %+v", saved)
	}
	out := r.files(t)
	if d := out.Files[0].Disk; d == nil || !d.Match || d.SHA256 != rec.SHA256 || d.CheckedAt == nil {
		t.Fatalf("got %+v", out.Files[0].Disk)
	}
	for _, f := range out.Card {
		switch f.Name {
		case rec.SDName:
			if f.Upload != "part.gcode" || f.SHA256 != rec.SHA256 || f.Note != "" {
				t.Errorf("got %+v", f)
			}
		case "OTHER.GCO":
			if f.Upload != "" || f.SHA256 != "" {
				t.Errorf("got %+v", f)
			}
		}
	}

	r.damage(t, "part.gcode")
	out = r.files(t)
	if d := out.Files[0].Disk; d == nil || d.Match || d.SHA256 == rec.SHA256 {
		t.Fatalf("damage not seen: %+v", d)
	}
	// The card's copy was made before, so it's still the upload.
	if f := out.Card[0]; f.Name == rec.SDName && f.SHA256 != rec.SHA256 {
		t.Errorf("got %+v", f)
	}
}

func TestDiskIsntReadDuringACopy(t *testing.T) {
	r := slowCopy(t)
	out := r.files(t)
	if d := out.Files[0].Disk; d == nil || d.SHA256 != "" || !strings.Contains(d.Error, "not read") {
		t.Fatalf("got %+v", d)
	}
}

func TestACopyOfADamagedFileFails(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	rec := r.put(t, "part.gcode", gcodeOf(50))
	r.damage(t, "part.gcode")
	r.startCopy(t, "part.gcode")
	job := r.waitCopy(t)
	if job.State != CopyFailed || !strings.Contains(job.Error, "doesn't match its upload") {
		t.Fatalf("got %+v", job)
	}
	if _, ok := r.card.File(rec.SDName); ok {
		t.Fatal("the bad copy should be deleted")
	}
	if saved := r.record(t, "part.gcode"); saved.CardSHA256 != "" {
		t.Fatalf("got %+v", saved)
	}
}

func TestADamagedFileIsntStreamed(t *testing.T) {
	r, name := streamRig(t, sim.Options{}, 5)
	r.damage(t, name)
	w := call(r.s.handlePrint, `{"name":"`+name+`","from":"pi","bed_clear":true}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "doesn't match its upload") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if r.s.printJob() != nil {
		t.Fatal("no job should start")
	}
}

func TestPrintJobsCarryTheirSum(t *testing.T) {
	r, name := streamRig(t, sim.Options{}, 5)
	rec := r.record(t, name)
	r.startPrint(t, name)
	if job := r.waitPrint(t, PrintFinished); job.SHA256 != rec.SHA256 {
		t.Fatalf("got %q, want %q", job.SHA256, rec.SHA256)
	}
}

// A new upload over a copied file leaves the old copy on the card, and its sum says so.
func TestANewVersionShowsTheCardsOldSum(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	first := r.put(t, "part.gcode", gcodeOf(50))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	second := r.put(t, "part.gcode", gcodeOf(5))
	if second.CardSHA256 != first.SHA256 {
		t.Fatalf("got %+v", second)
	}
	out := r.files(t)
	if out.Card[0].SHA256 != first.SHA256 || out.Files[0].SHA256 != second.SHA256 {
		t.Fatalf("got %+v", out)
	}
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	if out := r.files(t); out.Card[0].SHA256 != second.SHA256 {
		t.Fatalf("got %+v", out.Card)
	}
}

// A copy made before sums were kept is copied again, so its sum is known.
func TestAnOldCopyWithoutASumIsCopiedAgain(t *testing.T) {
	r := newUploadRig(t, sim.Options{})
	rec := r.put(t, "part.gcode", gcodeOf(20))
	r.startCopy(t, "part.gcode")
	r.waitCopy(t)
	saved := r.record(t, "part.gcode")
	saved.CardSHA256, saved.CardSize = "", 0
	if err := r.s.saveRecord(saved); err != nil {
		t.Fatal(err)
	}
	if out := r.files(t); out.Card[0].SHA256 != "" || !strings.Contains(out.Card[0].Note, "before checksums") {
		t.Fatalf("got %+v", out.Card)
	}
	if again := r.startCopy(t, "part.gcode"); again.Job == nil {
		t.Fatal("it should be copied again")
	}
	r.waitCopy(t)
	if got := r.record(t, "part.gcode").CardSHA256; got != rec.SHA256 {
		t.Fatalf("got %q", got)
	}
}
