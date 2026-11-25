package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "sb_0123456789abcdef"

// switchboard is a fake Switchboard that records what it's sent and answers with codes,
// one per request, then 202.
type switchboard struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	got   []map[string]any
	auth  []string
	codes []int
}

func newSwitchboard(t *testing.T, codes ...int) *switchboard {
	sb := &switchboard{t: t, codes: codes}
	sb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/notify" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil {
			t.Errorf("body: %v", err)
		}
		sb.mu.Lock()
		sb.got = append(sb.got, body)
		sb.auth = append(sb.auth, r.Header.Get("Authorization"))
		code := http.StatusAccepted
		if len(sb.codes) > 0 {
			code, sb.codes = sb.codes[0], sb.codes[1:]
		}
		sb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if code == http.StatusAccepted {
			io.WriteString(w, `{"id":"n1"}`)
		} else {
			io.WriteString(w, `{"error":"nope"}`)
		}
	}))
	t.Cleanup(sb.srv.Close)
	return sb
}

func (sb *switchboard) sent() []map[string]any {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return append([]map[string]any(nil), sb.got...)
}

func ptr[T any](v T) *T { return &v }

func configured(t *testing.T, sb *switchboard) *Notifier {
	t.Helper()
	n := New(t.TempDir())
	n.waits = []time.Duration{time.Millisecond, time.Millisecond}
	if _, err := n.Apply(Update{URL: ptr(sb.srv.URL + "/"), Token: ptr(testToken), Enabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSettingsAreSavedAndTheTokenKeptApart(t *testing.T) {
	dir := t.TempDir()
	n := New(dir)
	if v := n.View(); v.Enabled || v.TokenSet || len(v.Events) != len(Events) {
		t.Fatalf("defaults: %+v", v)
	}
	v, err := n.Apply(Update{URL: ptr("https://notify.example.lan/"), Token: ptr(" " + testToken + "\n"), Enabled: ptr(true), Events: &[]string{PrintFinished, Problem, PrintFinished}})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Ready || v.URL != "https://notify.example.lan" || len(v.Events) != 2 {
		t.Errorf("view: %+v", v)
	}
	data, _ := json.Marshal(v)
	if strings.Contains(string(data), testToken) {
		t.Error("the view mustn't hold the token")
	}
	settings, _ := os.ReadFile(filepath.Join(dir, settingsFile))
	if strings.Contains(string(settings), testToken) {
		t.Error("notify.json mustn't hold the token")
	}
	info, err := os.Stat(filepath.Join(dir, tokenFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", info, err)
	}
	again := New(dir)
	if v := again.View(); !v.Ready || v.URL != "https://notify.example.lan" || len(v.Events) != 2 || again.token != testToken {
		t.Errorf("reloaded: %+v", v)
	}

	// Removing the token turns them off, and the file goes.
	if v, err = again.Apply(Update{Token: ptr("")}); err != nil || v.Enabled || v.TokenSet {
		t.Errorf("after removing: %+v %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(dir, tokenFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file left: %v", err)
	}
}

func TestBadUpdatesAreRefused(t *testing.T) {
	n := New(t.TempDir())
	for _, u := range []Update{
		{URL: ptr("ftp://x")},
		{URL: ptr("https://user:pw@x")},
		{URL: ptr("https://x/?a=1")},
		{URL: ptr("notify.example.lan")},
		{Link: ptr("javascript:alert(1)")},
		{Token: ptr("has space")},
		{Token: ptr(strings.Repeat("a", MaxTokenBytes+1))},
		{Events: &[]string{"print_finishd"}},
		{Enabled: ptr(true)},
	} {
		if _, err := n.Apply(u); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", u, err)
		}
	}
	if _, err := os.Stat(filepath.Join(n.dir, settingsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("nothing should have been saved")
	}
}

func TestABrokenFileIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"enabled":true,"typo":1}`), 0o640)
	n := New(dir)
	if v := n.View(); v.Problem == "" || v.Enabled {
		t.Errorf("view: %+v", v)
	}
}

func TestSendGoesOutWithTheToken(t *testing.T) {
	sb := newSwitchboard(t)
	n := configured(t, sb)
	n.Apply(Update{Link: ptr("http://raspberrypi:8726/")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)
	n.Send(Message{Event: PrintFinished, Title: "Print finished", Body: "tile.gcode", Tags: []string{"tramline"}, Data: map[string]any{"job_id": 3}})
	waitFor(t, func() bool { return len(sb.sent()) == 1 })
	got := sb.sent()[0]
	if got["title"] != "Print finished" || got["body"] != "tile.gcode" || got["priority"] != 3.0 || got["url"] != "http://raspberrypi:8726/" {
		t.Errorf("sent: %v", got)
	}
	if data := got["data"].(map[string]any); data["job_id"] != 3.0 || data["event"] != PrintFinished {
		t.Errorf("data: %v", data)
	}
	if sb.auth[0] != "Bearer "+testToken {
		t.Error("wrong Authorization header")
	}
	waitFor(t, func() bool { v := n.View(); return v.Last != nil && v.Last.OK && v.Last.ID == "n1" })
}

func TestOnlyTheChosenEventsSend(t *testing.T) {
	sb := newSwitchboard(t)
	n := configured(t, sb)
	n.Apply(Update{Events: &[]string{Problem}})
	if n.Wants(PrintFinished) || !n.Wants(Problem) {
		t.Error("events")
	}
	n.Apply(Update{Enabled: ptr(false)})
	if n.Wants(Problem) {
		t.Error("switched off, nothing sends")
	}
	var none *Notifier
	none.Send(Message{Event: Problem}) // a server without one mustn't crash
}

func TestRetriesOnlyWhatMightPass(t *testing.T) {
	sb := newSwitchboard(t, 503, 502)
	n := configured(t, sb)
	if _, err := n.deliver(context.Background(), Message{Event: Problem, Title: "x"}, true); err != nil {
		t.Fatalf("after two 5xx it should get through: %v", err)
	}
	if len(sb.sent()) != 3 {
		t.Errorf("%d attempts", len(sb.sent()))
	}

	sb = newSwitchboard(t, 401, 401)
	n = configured(t, sb)
	if _, err := n.deliver(context.Background(), Message{Event: Problem, Title: "x"}, true); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("401: %v", err)
	}
	if len(sb.sent()) != 1 {
		t.Errorf("a 401 mustn't be retried: %d attempts", len(sb.sent()))
	}
	if v := n.View(); v.Last == nil || v.Last.OK || strings.Contains(v.Last.Error, testToken) {
		t.Errorf("last: %+v", v.Last)
	}
}

func TestTestSendsWhileOff(t *testing.T) {
	sb := newSwitchboard(t)
	n := configured(t, sb)
	n.Apply(Update{Enabled: ptr(false)})
	id, err := n.Test(context.Background())
	if err != nil || id != "n1" || len(sb.sent()) != 1 {
		t.Errorf("%q %v", id, err)
	}
	if _, err := New(t.TempDir()).Test(context.Background()); !errors.Is(err, ErrNotSet) {
		t.Errorf("unset: %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	sb := newSwitchboard(t)
	redirect := httptest.NewServer(http.RedirectHandler(sb.srv.URL+"/api/v1/notify", http.StatusTemporaryRedirect))
	defer redirect.Close()
	n := New(t.TempDir())
	n.Apply(Update{URL: ptr(redirect.URL), Token: ptr(testToken)})
	if _, err := n.Test(context.Background()); err == nil {
		t.Error("a redirect should fail")
	}
	if len(sb.sent()) != 0 {
		t.Error("the token followed a redirect")
	}
}

func TestLongTextIsCut(t *testing.T) {
	long := strings.Repeat("é", maxTitle)
	got := cut(long, maxTitle)
	if len(got) > maxTitle || !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, "éé") {
		t.Errorf("%d bytes", len(got))
	}
	if cut("short", 10) != "short" {
		t.Error("short text changed")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
