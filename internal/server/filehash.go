package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// diskCheckAge is how long a check of a file on the Pi's disk is reused for a listing.
// Printing from the Pi always checks again first.
const diskCheckAge = 10 * time.Minute

// DiskCheck is what the file on the Pi's disk hashes to now, against its upload's
// SHA-256. It's only in listings, never saved.
type DiskCheck struct {
	SHA256 string `json:"sha256,omitempty"`
	// Match means SHA256 is the upload's, so the file is as it arrived.
	Match     bool       `json:"match"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// Error says why there's no answer, such as a read error, or that it isn't read
	// during a print or a copy.
	Error string `json:"error,omitempty"`
}

type hashEntry struct {
	size      int64
	modTime   time.Time
	sha       string
	checkedAt time.Time
}

type hashState struct {
	// run means only one listing reads files at a time, so several browsers don't
	// multiply the load. mu guards the cache.
	run   sync.Mutex
	mu    sync.Mutex
	cache map[string]hashEntry
}

// hashFile returns a file's SHA-256 in lowercase hex.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ShortSum is how a SHA-256 is shown in a list: its first 12 hex digits, the same as
// the start of sha256sum's output.
func ShortSum(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// diskCheck hashes a file on the Pi. A recent result for an unchanged file is reused,
// and fresh means always reading it. While a print or a copy is running only a
// reused result is given, so a listing never competes with them for the disk.
func (s *Server) diskCheck(rec FileRecord, fresh bool) *DiskCheck {
	dir, err := s.uploadsDir()
	if err != nil {
		return &DiskCheck{Error: err.Error()}
	}
	path := filepath.Join(dir, rec.Name)
	fi, err := os.Stat(path)
	if err != nil {
		return &DiskCheck{Error: err.Error()}
	}
	now := s.now()
	s.hashes.mu.Lock()
	entry, ok := s.hashes.cache[rec.Name]
	s.hashes.mu.Unlock()
	ok = ok && entry.size == fi.Size() && entry.modTime.Equal(fi.ModTime())
	busy := s.printRunning() || s.copyRunning()
	if ok && !fresh && (busy || now.Sub(entry.checkedAt) < diskCheckAge) {
		return entry.check(rec)
	}
	if busy && !fresh {
		return &DiskCheck{Error: "not read during a print or a copy"}
	}
	sha, err := hashFile(path)
	if err != nil {
		return &DiskCheck{Error: fmt.Sprintf("can't read it: %v", err)}
	}
	entry = hashEntry{size: fi.Size(), modTime: fi.ModTime(), sha: sha, checkedAt: now}
	s.hashes.mu.Lock()
	if s.hashes.cache == nil {
		s.hashes.cache = map[string]hashEntry{}
	}
	s.hashes.cache[rec.Name] = entry
	s.hashes.mu.Unlock()
	return entry.check(rec)
}

func (e hashEntry) check(rec FileRecord) *DiskCheck {
	at := e.checkedAt
	return &DiskCheck{SHA256: e.sha, Match: e.sha == rec.SHA256, CheckedAt: &at}
}

// checkRecords fills in Disk for each record. The caller doesn't hold files.mu, as
// reading large files takes a while and a copy's progress needs that lock.
func (s *Server) checkRecords(records []FileRecord) {
	s.hashes.run.Lock()
	defer s.hashes.run.Unlock()
	for i := range records {
		records[i].Disk = s.diskCheck(records[i], false)
	}
}

// damagedText says how a file on the Pi differs from its upload.
func damagedText(rec FileRecord, sha string) string {
	return fmt.Sprintf("%s on the Pi doesn't match its upload: its SHA-256 is now %s, it was %s. Upload it again",
		rec.Name, ShortSum(sha), ShortSum(rec.SHA256))
}

// hashingReader passes reads through and hashes what it passes.
type hashingReader struct {
	r io.Reader
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func newHashingReader(r io.Reader) *hashingReader { return &hashingReader{r: r, h: sha256.New()} }

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	return n, err
}

func (h *hashingReader) sum() string { return hex.EncodeToString(h.h.Sum(nil)) }
