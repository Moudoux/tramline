package gui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"tramline/internal/client"
	"tramline/internal/server"
)

const addr = "127.0.0.1:41234"

// fakePi records what reaches the Pi's API and its page.
type fakePi struct {
	mu   sync.Mutex
	seen []string
	hdr  http.Header
}

func (f *fakePi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.hdr = r.Header.Clone()
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", "pi=1")
	io.WriteString(w, `{"ok":true}`)
}

func (f *fakePi) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

type fakeFiles struct {
	mu     sync.Mutex
	stored map[string]server.FileRecord
	puts   int
	body   string
}

func (f *fakeFiles) File(name string) (server.FileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.stored[name]
	if !ok {
		return rec, client.ErrNotFound
	}
	return rec, nil
}

func (f *fakeFiles) PutFile(name string, content io.Reader, size int64, sum string) (server.FileRecord, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return server.FileRecord{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.body = string(data)
	rec := server.FileRecord{Name: name, SHA256: sum, Size: size}
	f.stored[name] = rec
	return rec, nil
}

type rig struct {
	s     *Server
	pi    *fakePi
	files *fakeFiles
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pi := &fakePi{}
	ts := httptest.NewServer(pi)
	t.Cleanup(ts.Close)
	files := &fakeFiles{stored: map[string]server.FileRecord{}}
	s, err := New(Options{
		Addr:    addr,
		API:     http.DefaultTransport,
		APIURL:  ts.URL,
		PageURL: ts.URL + "/",
		Files:   files,
		Version: "test",
		TmpDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{s: s, pi: pi, files: files}
}

// request makes a request as this page would, signed in, unless changed.
func (r *rig) request(method, path string, body io.Reader, change func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+addr+path, body)
	req.Host = addr
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", "http://"+addr)
	}
	req.AddCookie(&http.Cookie{Name: r.s.cookie, Value: r.s.session})
	if change != nil {
		change(req)
	}
	w := httptest.NewRecorder()
	r.s.ServeHTTP(w, req)
	return w
}

func TestOnlyListensOnLoopback(t *testing.T) {
	for _, a := range []string{"0.0.0.0:1", "192.168.1.2:1", "[::1]:1", "localhost:1"} {
		if _, err := New(Options{Addr: a}); err == nil {
			t.Errorf("%s was accepted", a)
		}
	}
}

func TestTokenSignsInOnce(t *testing.T) {
	r := newRig(t)
	open := func(token string) *httptest.ResponseRecorder {
		return r.request(http.MethodGet, "/?t="+url.QueryEscape(token), nil, func(req *http.Request) {
			req.Header.Del("Cookie")
			req.Header.Set("Sec-Fetch-Site", "none")
		})
	}
	if w := open("wrong"); w.Code != http.StatusForbidden {
		t.Fatalf("wrong token: %d", w.Code)
	}
	w := open(r.s.Token())
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("first use: %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != r.s.session || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie: %+v", cookies)
	}
	if w := open(r.s.Token()); w.Code != http.StatusForbidden {
		t.Fatalf("second use: %d", w.Code)
	}
}

func TestNeedsTheSession(t *testing.T) {
	r := newRig(t)
	for name, change := range map[string]func(*http.Request){
		"no cookie": func(req *http.Request) { req.Header.Del("Cookie") },
		"wrong cookie": func(req *http.Request) {
			req.Header.Del("Cookie")
			req.AddCookie(&http.Cookie{Name: r.s.cookie, Value: strings.Repeat("0", 64)})
		},
		"token as cookie": func(req *http.Request) {
			req.Header.Del("Cookie")
			req.AddCookie(&http.Cookie{Name: r.s.cookie, Value: r.s.Token()})
		},
	} {
		for _, path := range []string{"/", "/gui.js", "/v1/status", "/pi/status", "/gui/info"} {
			if w := r.request(http.MethodGet, path, nil, change); w.Code != http.StatusUnauthorized {
				t.Errorf("%s, %s: %d", name, path, w.Code)
			}
		}
		if w := r.request(http.MethodPost, "/v1/estop", nil, change); w.Code != http.StatusUnauthorized {
			t.Errorf("%s, estop: %d", name, w.Code)
		}
	}
	if got := r.pi.got(); len(got) != 0 {
		t.Errorf("reached the Pi: %v", got)
	}
}

func TestWrongHostIsRefused(t *testing.T) {
	r := newRig(t)
	for _, host := range []string{"evil.example:41234", "localhost:41234", "127.0.0.1", "127.0.0.1:1"} {
		w := r.request(http.MethodGet, "/v1/status", nil, func(req *http.Request) { req.Host = host })
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s: %d", host, w.Code)
		}
	}
	if got := r.pi.got(); len(got) != 0 {
		t.Errorf("reached the Pi: %v", got)
	}
}

func TestOtherSitesCantSendCommands(t *testing.T) {
	r := newRig(t)
	for name, change := range map[string]func(*http.Request){
		"no origin":         func(req *http.Request) { req.Header.Del("Origin") },
		"other origin":      func(req *http.Request) { req.Header.Set("Origin", "http://evil.example") },
		"other port":        func(req *http.Request) { req.Header.Set("Origin", "http://127.0.0.1:1") },
		"cross-site fetch":  func(req *http.Request) { req.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site, not us": func(req *http.Request) { req.Header.Set("Sec-Fetch-Site", "same-site") },
	} {
		if w := r.request(http.MethodPost, "/v1/estop", nil, change); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	// A cross-site GET can't read anything either.
	w := r.request(http.MethodGet, "/v1/status", nil, func(req *http.Request) { req.Header.Set("Sec-Fetch-Site", "cross-site") })
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site GET: %d", w.Code)
	}
	if got := r.pi.got(); len(got) != 0 {
		t.Errorf("reached the Pi: %v", got)
	}
}

func TestForwardsOnlyTheListedRoutes(t *testing.T) {
	r := newRig(t)
	for _, route := range APIRoutes {
		method, path, _ := strings.Cut(route, " ")
		path = strings.NewReplacer("{name}", "a.gcode", "{id}", "3").Replace(path)
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{}`)
		}
		w := r.request(method, path, body, nil)
		if w.Code != http.StatusOK {
			t.Errorf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s %s passed on the Pi's cookie", method, path)
		}
	}
	if got := r.pi.got(); len(got) != len(APIRoutes) {
		t.Errorf("reached the Pi %d times, want %d", len(got), len(APIRoutes))
	}
	r.pi.mu.Lock()
	hdr := r.pi.hdr
	r.pi.mu.Unlock()
	for _, h := range []string{"Cookie", "Origin", "Sec-Fetch-Site"} {
		if hdr.Get(h) != "" {
			t.Errorf("%s was forwarded", h)
		}
	}

	before := len(r.pi.got())
	for _, route := range []string{
		"PUT /v1/files/a.gcode", "PUT /v1/update", "GET /v1/settings", "POST /v1/status",
		"GET /v1/files/a.gcode/../../x", "DELETE /v1/schedule/3", "POST /v1/events", "POST /v1/hue/plugs",
	} {
		method, path, _ := strings.Cut(route, " ")
		if w := r.request(method, path, strings.NewReader("x"), nil); w.Code == http.StatusOK {
			t.Errorf("%s was forwarded", route)
		}
	}
	if got := r.pi.got(); len(got) != before {
		t.Errorf("unlisted routes reached the Pi: %v", got[before:])
	}
}

func TestServesThePageAndPiStatus(t *testing.T) {
	r := newRig(t)
	for path, want := range map[string]string{
		"/":                 "text/html",
		"/gui.js":           "javascript",
		"/gui.css":          "text/css",
		"/shared/view.js":   "javascript",
		"/shared/theme.css": "text/css",
	} {
		w := r.request(http.MethodGet, path, nil, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), want) {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s has no content security policy", path)
		}
	}
	r.request(http.MethodGet, "/pi/status", nil, nil)
	r.request(http.MethodGet, "/pi/model/7", nil, nil)
	r.request(http.MethodGet, "/pi/events", nil, nil)
	if w := r.request(http.MethodGet, "/pi/model/x", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("bad model id: %d", w.Code)
	}
	got := strings.Join(r.pi.got(), ",")
	if got != "GET /api/status,GET /api/model/7,GET /api/events" {
		t.Errorf("page requests: %s", got)
	}
}

func upload(r *rig, name, body string) *httptest.ResponseRecorder {
	return r.request(http.MethodPost, "/gui/upload?name="+url.QueryEscape(name), strings.NewReader(body), nil)
}

func TestUploadSendsOnceAndSkipsTheSameFile(t *testing.T) {
	r := newRig(t)
	const body = "G28\nG1 X10\n"
	w := upload(r, "part.gcode", body)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	sum := sha256.Sum256([]byte(body))
	if rec := r.files.stored["part.gcode"]; rec.SHA256 != hex.EncodeToString(sum[:]) || r.files.body != body {
		t.Fatalf("stored %+v %q", rec, r.files.body)
	}
	var out struct {
		Already bool `json:"already"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Already {
		t.Error("first upload said already")
	}

	w = upload(r, "part.gcode", body)
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || !out.Already || r.files.puts != 1 {
		t.Errorf("same file again: %d already=%t puts=%d", w.Code, out.Already, r.files.puts)
	}
	if w := upload(r, "part.gcode", body+"M84\n"); w.Code != http.StatusOK || r.files.puts != 2 {
		t.Errorf("changed file: %d puts=%d", w.Code, r.files.puts)
	}

	st := r.request(http.MethodGet, "/gui/upload", nil, nil)
	var u Upload
	json.Unmarshal(st.Body.Bytes(), &u)
	if u.Phase != "done" || u.Name != "part.gcode" || u.Bytes != u.Total {
		t.Errorf("progress: %+v", u)
	}
}

func TestUploadRefusesBadNames(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"", "a b.gcode", "../x.gcode", "x.stl", ".hidden.gcode", strings.Repeat("a", 70) + ".gcode"} {
		if w := upload(r, name, "G28\n"); w.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", name, w.Code)
		}
	}
	if w := upload(r, "empty.gcode", ""); w.Code != http.StatusBadRequest {
		t.Errorf("empty: %d", w.Code)
	}
	if r.files.puts != 0 {
		t.Errorf("sent %d", r.files.puts)
	}
}
