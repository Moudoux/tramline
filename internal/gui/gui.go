// Package gui is tramline gui: a web app served on this machine only, which does what
// the client's commands do from a browser.
//
// The browser talks only to this server, on 127.0.0.1. This server talks to the Pi with
// the client's certificate, so the key never reaches the browser, and the Pi's own rules
// still decide what's allowed. Browsers are kept out unless they were opened by
// tramline gui itself, the way Jupyter does it:
//
//   - The address it opens carries a random one-time token, swapped on first use for a
//     session cookie, so the address seen briefly in ps can't be used again.
//   - Every request needs the cookie.
//   - The Host header must name this server exactly, which stops DNS rebinding.
//   - Anything that isn't a GET must come from this server's own origin, so another
//     site's page can't send commands.
//   - Only the API routes the client's commands use are forwarded, by method and path.
package gui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tramline/internal/client"
	"tramline/internal/gcode"
	"tramline/internal/server"
	"tramline/internal/tram"
	"tramline/internal/webui"
)

// APIRoutes are the Pi's API routes the GUI may use: those of the client's commands, apart
// from uploading, which goes through this server so it can hash the file first. Nothing
// else is forwarded, whatever the browser asks for.
var APIRoutes = []string{
	"GET /v1/status",
	"POST /v1/power",
	"POST /v1/preheat",
	"POST /v1/cool",
	"POST /v1/tram",
	"GET /v1/tram/info",
	"GET /v1/mesh",
	"GET /v1/files",
	"GET /v1/files/{name}",
	"DELETE /v1/files/{name}",
	"POST /v1/files/{name}/copy",
	"POST /v1/copy/cancel",
	"POST /v1/print",
	"POST /v1/print/continue",
	"POST /v1/cancel",
	"POST /v1/estop",
	"POST /v1/schedule",
	"GET /v1/schedule",
	"POST /v1/schedule/{id}/cancel",
	"POST /v1/hue/pair",
	"GET /v1/hue/plugs",
}

const (
	// maxJSONBody is plenty for any of the API's requests.
	maxJSONBody = 1 << 20
	// pageTimeout is for the Pi's read-only page, which answers from memory.
	pageTimeout = 30 * time.Second
	csp         = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// Files is what the GUI needs from the client for uploads.
type Files interface {
	File(name string) (server.FileRecord, error)
	PutFile(name string, content io.Reader, size int64, sha256 string) (server.FileRecord, error)
}

// Server is one run of tramline gui.
type Server struct {
	// Addr is host:port of this server, which the Host header and the origin must match.
	addr    string
	cookie  string
	token   string
	session string
	version string
	files   Files
	pageURL *url.URL
	page    *http.Client
	mux     *http.ServeMux
	// tmpDir holds uploads while they're hashed and sent.
	tmpDir string

	mu        sync.Mutex
	tokenUsed bool
	upload    *Upload
}

// Upload is the latest upload, which the page follows.
type Upload struct {
	Name string `json:"name"`
	// Phase is receiving, from the browser, sending, to the Pi, done or failed.
	Phase string `json:"phase"`
	Bytes int64  `json:"bytes"`
	Total int64  `json:"total"`
	// Already means the Pi had the same file, so it wasn't sent again.
	Already bool               `json:"already,omitempty"`
	File    *server.FileRecord `json:"file,omitempty"`
	Error   string             `json:"error,omitempty"`
}

// Options are what New needs.
type Options struct {
	// Addr is the address this server listens on, 127.0.0.1:port.
	Addr string
	// API sends requests to the Pi's API with the client's certificate, and APIURL is
	// its https:// address.
	API    http.RoundTripper
	APIURL string
	// PageURL is the Pi's read-only page, http://host:port/.
	PageURL string
	Files   Files
	Version string
	// TmpDir is a private folder for uploads, which the caller removes.
	TmpDir string
}

// New makes a server with a fresh token and session.
func New(o Options) (*Server, error) {
	if !strings.HasPrefix(o.Addr, "127.0.0.1:") {
		return nil, fmt.Errorf("the GUI only listens on 127.0.0.1, not %s", o.Addr)
	}
	api, err := url.Parse(o.APIURL)
	if err != nil {
		return nil, err
	}
	page, err := url.Parse(o.PageURL)
	if err != nil {
		return nil, err
	}
	s := &Server{
		addr:    o.Addr,
		version: o.Version,
		files:   o.Files,
		pageURL: page,
		page:    &http.Client{Timeout: pageTimeout},
		tmpDir:  o.TmpDir,
		// Cookies don't keep to one port, so each run has its own name.
		cookie:  "tramline_gui_" + strings.TrimPrefix(o.Addr, "127.0.0.1:"),
		token:   random(),
		session: random(),
	}
	s.mux = http.NewServeMux()
	proxy := &httputil.ReverseProxy{
		Transport: o.API,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(api)
			r.Out.Host = api.Host
			// Nothing from the browser that the Pi has no use for.
			for _, h := range []string{"Cookie", "Origin", "Referer", "User-Agent"} {
				r.Out.Header.Del(h)
			}
			for name := range r.Out.Header {
				if strings.HasPrefix(name, "Sec-") {
					r.Out.Header.Del(name)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, "can't reach the Pi: "+client.Explain(err).Error())
		},
	}
	forward := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		proxy.ServeHTTP(w, r)
	})
	for _, route := range APIRoutes {
		s.mux.Handle(route, forward)
	}
	s.mux.Handle("GET /", http.FileServerFS(webui.GUI()))
	s.mux.Handle("GET /shared/", http.StripPrefix("/shared", http.FileServerFS(webui.Shared())))
	s.mux.HandleFunc("GET /pi/status", s.handlePageStatus)
	s.mux.HandleFunc("GET /pi/model/{id}", s.handlePageModel)
	s.mux.HandleFunc("GET /gui/info", s.handleInfo)
	s.mux.HandleFunc("POST /gui/upload", s.handleUpload)
	s.mux.HandleFunc("GET /gui/upload", s.handleUploadStatus)
	return s, nil
}

// Token is the one-time token for the address the browser is opened at.
func (s *Server) Token() string { return s.token }

// OpenURL is the address that lets a browser in, once.
func (s *Server) OpenURL() string { return "http://" + s.addr + "/?t=" + s.token }

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")

	if r.Host != s.addr {
		writeError(w, http.StatusMisdirectedRequest, "this server only answers to "+s.addr)
		return
	}
	// Browsers say where a request comes from. Only this page, or the browser itself
	// opening the address, may get anything.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		writeError(w, http.StatusForbidden, "requests from other sites aren't allowed")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "http://"+s.addr {
		writeError(w, http.StatusForbidden, "requests from other sites aren't allowed")
		return
	}
	if r.URL.Path == "/" && r.URL.Query().Has("t") {
		s.login(w, r)
		return
	}
	c, err := r.Cookie(s.cookie)
	if err != nil || !same(c.Value, s.session) {
		writeError(w, http.StatusUnauthorized, "not signed in: open the GUI with tramline gui, which signs this browser in")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// login swaps the one-time token for the session cookie.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ok := !s.tokenUsed && same(r.URL.Query().Get("t"), s.token)
	if ok {
		s.tokenUsed = true
	}
	s.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(w, "This sign-in link has been used already. Run tramline gui again to open a new one.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookie,
		Value:    s.session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// handleInfo tells the page this side's version and which Pi it talks to.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	perMB := server.CopyEstimate(gcode.Info{SDSize: 1 << 20}).Seconds()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":                   s.version,
		"server":                    s.pageURL.Host,
		"max_upload":                server.MaxUpload,
		"tram_tolerance":            tram.Tolerance,
		"copy_bytes_per_second":     float64(1<<20) / perMB,
		"schedule_min_lead_minutes": server.ScheduleMinLead.Minutes(),
	})
}

// The read-only page's status and model come from the Pi's memory, so they're cheap to
// ask for once a second, unlike /v1/status, which asks the Hue Bridge each time.
func (s *Server) handlePageStatus(w http.ResponseWriter, r *http.Request) {
	s.fromPage(w, r, "api/status")
}

func (s *Server) handlePageModel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		writeError(w, http.StatusNotFound, "no such job")
		return
	}
	s.fromPage(w, r, "api/model/"+strconv.Itoa(id))
}

func (s *Server) fromPage(w http.ResponseWriter, r *http.Request, path string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.pageURL.ResolveReference(&url.URL{Path: path}).String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := s.page.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "can't reach the Pi: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var out *Upload
	if s.upload != nil {
		u := *s.upload
		out = &u
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

// setUpload changes the upload the page follows.
func (s *Server) setUpload(change func(u *Upload)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s.upload)
}

// handleUpload does what tramline upload does: it hashes the file, and sends it unless
// the Pi already has the same one. The file goes to a private temporary folder first, as
// the hash goes before the file. Once it's on the Pi, the browser can go.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if err := server.ValidFileName(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	size := r.ContentLength
	if size <= 0 {
		writeError(w, http.StatusBadRequest, "the file is empty, or its size wasn't given")
		return
	}
	if size > server.MaxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the file is bigger than the %d MB the server takes", server.MaxUpload>>20))
		return
	}
	s.mu.Lock()
	if u := s.upload; u != nil && (u.Phase == "receiving" || u.Phase == "sending") {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "another upload is running: "+u.Name)
		return
	}
	s.upload = &Upload{Name: name, Phase: "receiving", Total: size}
	s.mu.Unlock()

	rec, already, err := s.receiveAndSend(r, name, size)
	s.setUpload(func(u *Upload) {
		if err != nil {
			u.Phase, u.Error = "failed", err.Error()
			return
		}
		u.Phase, u.Already, u.File, u.Bytes = "done", already, &rec, size
	})
	if err != nil {
		log.Printf("upload of %s failed: %v", name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"file": rec, "already": already})
}

func (s *Server) receiveAndSend(r *http.Request, name string, size int64) (server.FileRecord, bool, error) {
	var rec server.FileRecord
	f, err := os.CreateTemp(s.tmpDir, "upload-*")
	if err != nil {
		return rec, false, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	got, err := io.Copy(io.MultiWriter(f, hash, s.counter()), io.LimitReader(r.Body, size+1))
	if err != nil {
		return rec, false, fmt.Errorf("receiving the file: %w", err)
	}
	if got != size {
		return rec, false, fmt.Errorf("received %d bytes, expected %d", got, size)
	}
	sum := hex.EncodeToString(hash.Sum(nil))

	old, err := s.files.File(name)
	switch {
	case err == nil && old.SHA256 == sum:
		return old, true, nil
	case err != nil && !errors.Is(err, client.ErrNotFound):
		return rec, false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return rec, false, err
	}
	s.setUpload(func(u *Upload) { u.Phase, u.Bytes = "sending", 0 })
	rec, err = s.files.PutFile(name, io.TeeReader(f, s.counter()), size, sum)
	return rec, false, err
}

// counter adds what passes through it to the upload's progress.
func (s *Server) counter() io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		s.setUpload(func(u *Upload) { u.Bytes += int64(len(p)) })
		return len(p), nil
	})
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TempDir makes the private folder for uploads.
func TempDir() (string, error) {
	dir, err := os.MkdirTemp("", "tramline-gui-*")
	if err != nil {
		return "", err
	}
	return filepath.Clean(dir), os.Chmod(dir, 0o700)
}
