package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"tramline/internal/gcode"
	"tramline/internal/marlin"
)

const (
	// MaxUpload is the largest file accepted. The largest sliced so far is 28 MB.
	MaxUpload = 256 << 20
	// uploadSpare is how much disk must stay free after an upload, as the Pi's
	// boot disk also holds its containers.
	uploadSpare = 1 << 30

	uploadsDirName = "uploads"

	// HashHeader carries the SHA-256 of an upload, so damage on the way is caught
	// and a file the server already has needn't be sent again.
	HashHeader = "X-Tramline-SHA256"

	CopyRunning = "running"
	CopyDone    = "verified"
	CopyFailed  = "failed"

	// ModeSD is a job printing from the printer's card, and ModeStream one sent line by
	// line from the Pi as it prints.
	ModeSD     = "sd"
	ModeStream = "stream"

	// copyBytesPerSecond is the size on the card copied each second, as measured on
	// the real printer on 2026-11-03 with a sliced file and the default window.
	copyBytesPerSecond = 14000

	copyBusy = "a copy to the SD card is going, tramline copy --cancel stops it"
)

var (
	fileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hashRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Card is the part of the printer that copying files needs.
type Card interface {
	ListSD(ctx context.Context) ([]marlin.SDFile, error)
	WriteSD(ctx context.Context, name string, window int, next func() (string, error), progress func(marlin.SDProgress)) error
	DeleteSD(ctx context.Context, name string) error
}

// FileRecord is what the server knows about one uploaded file. It's kept next to
// the file, so it survives a restart.
type FileRecord struct {
	Name       string     `json:"name"`
	SHA256     string     `json:"sha256"`
	Size       int64      `json:"size"`
	Info       gcode.Info `json:"info"`
	UploadedAt time.Time  `json:"uploaded_at"`
	// SDName is the file's name on the card.
	SDName string `json:"sd_name"`
	// OnCard means a file from here may be on the card as SDName, complete or not.
	// Only such a file may be overwritten or deleted.
	OnCard bool `json:"on_card"`
	// SDState is empty, or the state of the last copy.
	SDState string `json:"sd_state,omitempty"`
	SDError string `json:"sd_error,omitempty"`
	// CardSHA256 is the SHA-256 of the bytes read from the Pi for the last copy whose
	// size checked out on the card. The card can't be read back, so this is what was
	// sent, not what was read there. It stays after a new upload, as that copy is still
	// on the card, and empties when a copy starts.
	CardSHA256 string `json:"card_sha256,omitempty"`
	CardSize   int64  `json:"card_size,omitempty"`
	// Disk is a fresh check of the file on the Pi, in listings only.
	Disk *DiskCheck `json:"disk,omitempty"`
}

// CopyJob is one copy to the card, and is what status reports.
type CopyJob struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	SDName     string `json:"sd_name"`
	State      string `json:"state"`
	Lines      int    `json:"lines"`
	TotalLines int    `json:"total_lines"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
	// SHA256 is the upload's, which the bytes sent must match.
	SHA256     string     `json:"sha256"`
	Window     int        `json:"window"`
	Resends    int        `json:"resends"`
	Timeouts   int        `json:"timeouts"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// CopyRequest is the optional body of POST /v1/files/{name}/copy.
type CopyRequest struct {
	// Window is how many bytes may be unanswered, for measuring. 0 is the default,
	// and 1 sends one line at a time.
	Window int `json:"window,omitempty"`
}

// CopyResponse is returned by POST /v1/files/{name}/copy. With no Job, the file
// was already on the card and nothing needed doing.
type CopyResponse struct {
	File FileRecord `json:"file"`
	Job  *CopyJob   `json:"job,omitempty"`
}

// FilesResponse is returned by GET /v1/files.
type FilesResponse struct {
	Files []FileRecord `json:"files"`
	// Card is what's on the card, including files from elsewhere. It's nil if that
	// can't be read right now, and CardNote says why.
	Card     []CardFile `json:"card"`
	CardNote string     `json:"card_note,omitempty"`
}

// CardFile is a file on the card. For one copied from here, Upload names the file on
// the Pi, and SHA256 is the bytes sent in its last checked copy, if the size on the
// card still matches.
type CardFile struct {
	marlin.SDFile
	Upload string `json:"upload,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Note says why a file from here has no SHA256.
	Note string `json:"note,omitempty"`
}

// cardFiles matches the card's listing with the records.
func cardFiles(listed []marlin.SDFile, records []FileRecord) []CardFile {
	if listed == nil {
		return nil
	}
	out := make([]CardFile, 0, len(listed))
	for _, f := range listed {
		item := CardFile{SDFile: f}
		i := slices.IndexFunc(records, func(r FileRecord) bool { return r.OnCard && strings.EqualFold(r.SDName, f.Name) })
		if i >= 0 {
			rec := records[i]
			item.Upload = rec.Name
			switch {
			case rec.SDState == CopyRunning:
				item.Note = "being copied now"
			case rec.SDState == CopyFailed:
				item.Note = "its last copy failed, so it may be partial"
			case rec.CardSHA256 == "":
				item.Note = "copied before checksums were kept, tramline copy " + rec.Name + " copies it again"
			case f.Size != rec.CardSize:
				item.Note = fmt.Sprintf("it's %d bytes on the card, but its copy was %d", f.Size, rec.CardSize)
			default:
				item.SHA256 = rec.CardSHA256
			}
		}
		out = append(out, item)
	}
	return out
}

// CopyEstimate is roughly how long a file takes to copy to the card.
func CopyEstimate(info gcode.Info) time.Duration {
	return time.Duration(info.SDSize) * time.Second / copyBytesPerSecond
}

type fileState struct {
	// mu guards the job and the records on disk. Take it after the operation lock.
	mu     sync.Mutex
	job    *CopyJob
	ids    int
	cancel context.CancelCauseFunc
}

func (s *Server) copyJob() *CopyJob {
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	if s.files.job == nil {
		return nil
	}
	job := *s.files.job
	return &job
}

func (s *Server) copyRunning() bool {
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	return s.files.job != nil && s.files.job.State == CopyRunning
}

// stopCopy asks a running copy to stop. It ends the save and frees the operation
// lock quickly, so a caller can then simply wait for that lock.
func (s *Server) stopCopy(why string) bool {
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	if s.files.job == nil || s.files.job.State != CopyRunning {
		return false
	}
	s.files.cancel(errors.New(why))
	return true
}

// forgetCopy stops status reporting a finished copy of a file that has since been
// replaced or deleted. The caller holds files.mu.
func (s *Server) forgetCopy(name string) {
	if s.files.job != nil && s.files.job.Name == name && s.files.job.State != CopyRunning {
		s.files.job = nil
	}
}

func (s *Server) uploadsDir() (string, error) {
	if s.DataDir == "" {
		return "", errors.New("the server has no data directory")
	}
	dir := filepath.Join(s.DataDir, uploadsDirName)
	return dir, os.MkdirAll(dir, 0o750)
}

// ValidFileName says whether the server would take a file by this name, so a client can
// say so before sending it.
func ValidFileName(name string) error { return validFileName(name) }

func validFileName(name string) error {
	lower := strings.ToLower(name)
	if !fileNameRE.MatchString(name) || !(strings.HasSuffix(lower, ".gcode") || strings.HasSuffix(lower, ".gco")) {
		return errors.New("a file name must be up to 64 letters, digits, dots, dashes or underscores, and end in .gcode or .gco")
	}
	return nil
}

// writeAtomic means a crash leaves the old contents or the new, never half.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Server) saveRecord(rec FileRecord) error {
	rec.Disk = nil
	dir, err := s.uploadsDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, rec.Name+".json"), append(data, '\n'))
}

// loadRecord returns os.ErrNotExist for an unknown file. The caller holds files.mu.
func (s *Server) loadRecord(name string) (FileRecord, error) {
	var rec FileRecord
	dir, err := s.uploadsDir()
	if err != nil {
		return rec, err
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("the record for %s is damaged: %w", name, err)
	}
	// A record left saying "running" with no such job is from before a restart.
	if rec.SDState == CopyRunning && (s.files.job == nil || s.files.job.Name != name || s.files.job.State != CopyRunning) {
		rec.SDState = CopyFailed
		rec.SDError = "the server stopped during the copy"
	}
	return rec, nil
}

// loadRecords lists every record, by name. The caller holds files.mu.
func (s *Server) loadRecords() ([]FileRecord, error) {
	dir, err := s.uploadsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	records := []FileRecord{}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || strings.HasPrefix(name, ".") {
			continue
		}
		rec, err := s.loadRecord(name)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

// pickSDName keeps a file's name on the card if it has one, and otherwise avoids
// the names of other uploads. The caller holds files.mu.
func (s *Server) pickSDName(name string) (string, error) {
	records, err := s.loadRecords()
	if err != nil {
		return "", err
	}
	for attempt := range 256 {
		candidate := gcode.ShortName(name, attempt)
		if !slices.ContainsFunc(records, func(r FileRecord) bool { return r.Name != name && r.SDName == candidate }) {
			return candidate, nil
		}
	}
	return "", errors.New("no free name on the card for this file")
}

// handleFilePut saves a file on the Pi, and goes no further. It works with the
// printer off, and nothing here depends on the client staying around afterwards.
func (s *Server) handleFilePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validFileName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	want := r.Header.Get(HashHeader)
	if !hashRE.MatchString(want) {
		writeError(w, http.StatusBadRequest, HashHeader+" must be the file's SHA-256 in lowercase hex")
		return
	}
	if r.ContentLength > MaxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the limit is %d MB", MaxUpload>>20))
		return
	}
	dir, err := s.uploadsDir()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err := checkDiskSpace(dir, r.ContentLength); err != nil {
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), http.MaxBytesReader(w, r.Body, MaxUpload))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the upload didn't finish: "+err.Error())
		return
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		writeError(w, http.StatusBadRequest, "the file was damaged on the way: its SHA-256 doesn't match")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	info, err := gcode.Measure(tmp)
	if err != nil {
		writeError(w, http.StatusBadRequest, "refusing this file: "+err.Error())
		return
	}
	if err := tmp.Sync(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// A job waiting for its tram check reads the file when that passes.
	if job := s.printJob(); job.active() && job.Mode == ModeStream && job.Name == name {
		writeError(w, http.StatusConflict, "this file is being streamed to the printer right now")
		return
	}
	if p := s.scheduleUsing(name); p != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("scheduled print %d needs this file as it is, tramline schedule cancel %d frees it", p.ID, p.ID))
		return
	}
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	if s.files.job != nil && s.files.job.Name == name && s.files.job.State == CopyRunning {
		writeError(w, http.StatusConflict, "this file is being copied to the card right now")
		return
	}
	rec := FileRecord{Name: name, SHA256: want, Size: size, Info: info, UploadedAt: s.now()}
	// A new version takes the old one's place on the card, so one isn't left behind.
	switch old, err := s.loadRecord(name); {
	case err == nil:
		rec.SDName, rec.OnCard = old.SDName, old.OnCard
		if old.SDState == CopyDone {
			rec.CardSHA256, rec.CardSize = old.CardSHA256, old.CardSize
		}
	case errors.Is(err, os.ErrNotExist):
		rec.SDName, err = s.pickSDName(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The record goes first: if the rename then fails, the hash won't match the
	// file and the next upload replaces both.
	if err := s.saveRecord(rec); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.forgetCopy(name)
	log.Printf("upload: saved %s, %d bytes, %d lines, %d bytes on the card as %s", name, size, info.Lines, info.SDSize, rec.SDName)
	writeJSON(w, http.StatusOK, rec)
}

func checkDiskSpace(dir string, size int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * int64(st.Bsize) // Bsize's type differs between systems
	if free-max(size, 0) < uploadSpare {
		return fmt.Errorf("not enough disk space on the server: %d MB free", free>>20)
	}
	return nil
}

func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validFileName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.files.mu.Lock()
	rec, err := s.loadRecord(name)
	s.files.mu.Unlock()
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, name+" isn't on the server")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		// upload asks for this to decide whether to send the file again, so it's read
		// afresh, unless a print or a copy is running.
		s.hashes.run.Lock()
		rec.Disk = s.diskCheck(rec, !s.printRunning() && !s.copyRunning())
		s.hashes.run.Unlock()
		writeJSON(w, http.StatusOK, rec)
	}
}

// handleFiles never waits: if the printer is busy, the card's listing is left out.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	var out FilesResponse
	listed, note := s.readCard(r.Context())
	s.files.mu.Lock()
	records, err := s.loadRecords()
	s.files.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.checkRecords(records)
	out.Files, out.Card, out.CardNote = records, cardFiles(listed, records), note
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) readCard(ctx context.Context) ([]marlin.SDFile, string) {
	switch {
	case s.Card == nil:
		return nil, "there's no printer"
	case s.copyRunning():
		return nil, "a copy is going"
	case s.tramRunning():
		return nil, "a tram run is going"
	case s.printRunning():
		return nil, "a print is running"
	case !s.Printer().Connected:
		return nil, "the printer isn't connected"
	}
	if !s.heat.op.TryLock() {
		return nil, "the printer is busy"
	}
	defer s.heat.op.Unlock()
	if s.copyRunning() || s.tramRunning() {
		return nil, "the printer is busy"
	}
	// Reading the listing could disturb a print, so it isn't done during one.
	printing, err := s.Heaters.SDPrinting(ctx)
	switch {
	case err != nil:
		return nil, "can't tell whether a print is running: " + err.Error()
	case printing:
		return nil, "an SD print is running"
	}
	files, err := s.Card.ListSD(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return files, ""
}

func findOnCard(files []marlin.SDFile, name string) (marlin.SDFile, bool) {
	i := slices.IndexFunc(files, func(f marlin.SDFile) bool { return strings.EqualFold(f.Name, name) })
	if i < 0 {
		return marlin.SDFile{}, false
	}
	return files[i], true
}

// handleCopy starts copying a file already on the Pi to the card, and returns
// straight away. It needs nothing more from the client, and running it again after
// a failure is how a copy is retried.
func (s *Server) handleCopy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validFileName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in CopyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, `body must be empty or {"window":<bytes>}`)
		return
	}
	if in.Window < 0 || in.Window > marlin.MaxSDWindow {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("window must be 0 to %d bytes", marlin.MaxSDWindow))
		return
	}
	window := in.Window
	if window == 0 {
		window = marlin.DefaultSDWindow
	}
	if s.Card == nil {
		writeError(w, http.StatusServiceUnavailable, "there's no printer to copy to")
		return
	}
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to copy: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, "refusing to copy: another copy is going")
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to copy: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, "refusing to copy: another copy is going")
		return
	}
	s.files.mu.Lock()
	rec, err := s.loadRecord(name)
	s.files.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, name+" isn't on the server")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// M28 would abort a print that's running, so anything unknown refuses.
	if code, why := s.printerBlocker(r.Context()); why != "" {
		writeError(w, code, "refusing to copy: "+why)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	listed, err := s.Card.ListSD(ctx)
	if err != nil {
		writePrinterError(w, fmt.Errorf("refusing to copy: can't read the card: %w", err))
		return
	}
	found, exists := findOnCard(listed, rec.SDName)
	switch {
	case exists && !rec.OnCard:
		writeError(w, http.StatusConflict, fmt.Sprintf("refusing to copy: %s is already on the card and didn't come from here, and M28 would overwrite it", rec.SDName))
		return
	case exists && rec.SDState == CopyDone && found.Size == rec.Info.SDSize && rec.CardSHA256 == rec.SHA256:
		writeJSON(w, http.StatusOK, CopyResponse{File: rec})
		return
	}

	copyCtx, cancel := context.WithCancelCause(s.baseCtx())
	s.files.mu.Lock()
	s.files.ids++
	job := &CopyJob{
		ID: s.files.ids, Name: name, SDName: rec.SDName, State: CopyRunning,
		TotalLines: rec.Info.Lines, TotalBytes: rec.Info.SDSize, SHA256: rec.SHA256, Window: window, StartedAt: s.now(),
	}
	s.files.job, s.files.cancel = job, cancel
	out := *job
	// Saved before M28, which can create the file even if its reply goes missing.
	rec.OnCard, rec.SDState, rec.SDError, rec.CardSHA256, rec.CardSize = true, CopyRunning, "", "", 0
	err = s.saveRecord(rec)
	if err != nil {
		job.State, job.Error = CopyFailed, err.Error()
		s.files.mu.Unlock()
		cancel(nil)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.files.mu.Unlock()
	log.Printf("copy %d: %s to the card as %s, %d lines, %d bytes, window %d", job.ID, name, rec.SDName, rec.Info.Lines, rec.Info.SDSize, window)
	go s.runCopy(copyCtx, cancel, job, rec)
	writeJSON(w, http.StatusOK, CopyResponse{File: rec, Job: &out})
}

// runCopy holds the operation lock throughout. Other commands are refused
// meanwhile, and the ones that can't wait, cool and the idle timer, stop the copy.
func (s *Server) runCopy(ctx context.Context, cancel context.CancelCauseFunc, job *CopyJob, rec FileRecord) {
	defer cancel(nil)
	s.heat.op.Lock()
	defer s.heat.op.Unlock()

	sent, err := s.writeAndVerify(ctx, job, rec)
	// The copy's own reason for stopping is more use than "context canceled".
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("stopped: %w", context.Cause(ctx))
	}
	rec.SDState, rec.SDError = CopyDone, ""
	if err == nil {
		rec.CardSHA256, rec.CardSize = sent, rec.Info.SDSize
	} else {
		rec.SDState, rec.SDError = CopyFailed, err.Error()
		// A partial file mustn't be left where the LCD could print it. If the link
		// is gone this fails, and the next copy overwrites the file.
		deleteErr := s.Card.DeleteSD(context.WithoutCancel(ctx), rec.SDName)
		if deleteErr == nil {
			rec.OnCard = false
		} else {
			rec.SDError += ". A partial file may be left on the card as " + rec.SDName + ", don't print it"
			log.Printf("copy %d: couldn't delete the partial file: %v", job.ID, deleteErr)
		}
	}
	now := s.now()
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	if saveErr := s.saveRecord(rec); saveErr != nil {
		log.Printf("copy %d: couldn't save the record: %v", job.ID, saveErr)
	}
	job.FinishedAt = &now
	job.State = rec.SDState
	job.Error = rec.SDError
	elapsed := now.Sub(job.StartedAt).Round(10 * time.Millisecond)
	log.Printf("copy %d: %s after %s, %d of %d lines, %d resends, %d timeouts. %s",
		job.ID, job.State, elapsed, job.Lines, job.TotalLines, job.Resends, job.Timeouts, job.Error)
}

// writeAndVerify returns the SHA-256 of what it read from the Pi, which must be the
// upload's, as the card can't be read back to check its contents.
func (s *Server) writeAndVerify(ctx context.Context, job *CopyJob, rec FileRecord) (string, error) {
	dir, err := s.uploadsDir()
	if err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Join(dir, rec.Name))
	if err != nil {
		return "", err
	}
	defer f.Close()
	read := newHashingReader(f)
	reader := gcode.NewReader(read)
	err = s.Card.WriteSD(ctx, rec.SDName, job.Window, reader.Next, func(p marlin.SDProgress) {
		s.files.mu.Lock()
		defer s.files.mu.Unlock()
		job.Lines, job.Bytes, job.Resends, job.Timeouts = p.Lines, p.Bytes, p.Resends, p.Timeouts
	})
	if err != nil {
		return "", err
	}
	// The reader stops at the end, so this only reads anything if it didn't.
	if _, err := io.Copy(io.Discard, read); err != nil {
		return "", err
	}
	sent := read.sum()
	if sent != rec.SHA256 {
		return "", errors.New(damagedText(rec, sent) + ", then copy it")
	}
	listed, err := s.Card.ListSD(context.WithoutCancel(ctx))
	if err != nil {
		return "", fmt.Errorf("the copy finished, but the card can't be read to check it: %w", err)
	}
	found, exists := findOnCard(listed, rec.SDName)
	switch {
	case !exists:
		return "", fmt.Errorf("the copy finished, but %s isn't on the card", rec.SDName)
	case found.Size != rec.Info.SDSize:
		return "", fmt.Errorf("%s is %d bytes on the card, it should be %d", rec.SDName, found.Size, rec.Info.SDSize)
	}
	return sent, nil
}

func (s *Server) handleCopyCancel(w http.ResponseWriter, r *http.Request) {
	if !s.stopCopy("cancelled on request") {
		writeError(w, http.StatusConflict, "no copy is going")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopping": true})
}

// handleFileDelete removes a file from the Pi and, if a copy from here is there,
// from the card too. It only ever deletes files that came from here.
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validFileName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.printRunning() {
		writeError(w, http.StatusConflict, printBusy)
		return
	}
	if p := s.scheduleUsing(name); p != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("refusing to delete: scheduled print %d needs this file, tramline schedule cancel %d frees it", p.ID, p.ID))
		return
	}
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to delete: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	s.heat.op.Lock()
	defer s.heat.op.Unlock()
	if s.tramRunning() {
		writeError(w, http.StatusConflict, "refusing to delete: a tram run is going")
		return
	}
	if s.copyRunning() {
		writeError(w, http.StatusConflict, copyBusy)
		return
	}
	s.files.mu.Lock()
	rec, err := s.loadRecord(name)
	s.files.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, name+" isn't on the server")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rec.OnCard {
		if err := s.deleteFromCard(r.Context(), rec.SDName); err != nil {
			writeError(w, http.StatusConflict, fmt.Sprintf("%s may be on the card, and can't be deleted: %v", rec.SDName, err))
			return
		}
	}
	s.files.mu.Lock()
	defer s.files.mu.Unlock()
	dir, err := s.uploadsDir()
	if err == nil {
		err = errors.Join(os.Remove(filepath.Join(dir, name)), os.Remove(filepath.Join(dir, name+".json")))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.forgetCopy(name)
	log.Printf("deleted %s", name)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) deleteFromCard(ctx context.Context, sdName string) error {
	if s.Card == nil {
		return errors.New("there's no printer")
	}
	if _, why := s.printerBlocker(ctx); why != "" {
		return errors.New(why)
	}
	ctx = context.WithoutCancel(ctx)
	listed, err := s.Card.ListSD(ctx)
	if err != nil {
		return err
	}
	if _, exists := findOnCard(listed, sdName); !exists {
		return nil
	}
	return s.Card.DeleteSD(ctx, sdName)
}
