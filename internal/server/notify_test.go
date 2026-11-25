package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tramline/internal/notify"
	"tramline/internal/sim"
)

type fakeSwitchboard struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []map[string]any
}

func newFakeSwitchboard(t *testing.T) *fakeSwitchboard {
	sb := &fakeSwitchboard{}
	sb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		sb.mu.Lock()
		sb.got = append(sb.got, body)
		sb.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"n1"}`))
	}))
	t.Cleanup(sb.srv.Close)
	return sb
}

func (sb *fakeSwitchboard) titles() []string {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	var out []string
	for _, m := range sb.got {
		out = append(out, m["title"].(string))
	}
	return out
}

func (sb *fakeSwitchboard) find(title string) map[string]any {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	for _, m := range sb.got {
		if m["title"] == title {
			return m
		}
	}
	return nil
}

const serverTestToken = "sb_serverside0123456789"

// withNotify gives the rig's server a notifier sending to a fake Switchboard.
func withNotify(t *testing.T, s *Server) *fakeSwitchboard {
	t.Helper()
	sb := newFakeSwitchboard(t)
	s.Notify = notify.New(t.TempDir())
	body := `{"url":"` + sb.srv.URL + `","token":"` + serverTestToken + `","enabled":true}`
	if w := call(s.handleNotifyPut, body); w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Notify.Run(ctx)
	return sb
}

func TestNotifySettingsNeverShowTheToken(t *testing.T) {
	s := &Server{}
	if w := call(s.handleNotifyGet, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("without a notifier: %d", w.Code)
	}
	withNotify(t, s)
	for _, w := range []*httptest.ResponseRecorder{
		call(s.handleNotifyGet, ""),
		call(s.handleNotifyPut, `{"link":"http://raspberrypi:8726/"}`),
	} {
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), serverTestToken) || !strings.Contains(w.Body.String(), `"token_set":true`) {
			t.Errorf("%d %s", w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"url":"ftp://x"}`, `{"tokn":"x"}`, `{"events":["nope"]}`} {
		if w := call(s.handleNotifyPut, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := call(s.handleNotifyTest, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "n1") {
		t.Errorf("test: %d %s", w.Code, w.Body)
	}
}

func TestAFinishedPrintNotifies(t *testing.T) {
	r, name := printRig(t, sim.Options{PrintTime: 100 * time.Millisecond, Corners: inTram})
	sb := withNotify(t, r.s)
	r.startPrint(t, name)
	r.waitPrint(t, PrintFinished)
	r.waitFor(t, "the notification", func() bool { return sb.find("Print finished") != nil })
	m := sb.find("Print finished")
	if body := m["body"].(string); !strings.Contains(body, name) || !strings.Contains(body, "from the card") {
		t.Errorf("body: %q", body)
	}
	if data := m["data"].(map[string]any); data["file"] != name || data["from"] != FromSD || data["state"] != PrintFinished {
		t.Errorf("data: %v", data)
	}
	if got := sb.titles(); len(got) != 1 {
		t.Errorf("sent %q", got)
	}
}

func TestAFailedCheckAsksForTheKnobs(t *testing.T) {
	k := &knobs{bed: outOfTram}
	r, name := printRig(t, sim.Options{PrintTime: time.Minute, Bed: k.get})
	sb := withNotify(t, r.s)
	r.startPrint(t, name)
	r.waitPrint(t, PrintAdjusting)
	r.waitFor(t, "the notification", func() bool { return sb.find("Bed needs adjusting") != nil })
	m := sb.find("Bed needs adjusting")
	if body := m["body"].(string); !strings.Contains(body, "back-left") || !strings.Contains(body, "print --continue") || m["priority"] != 4.0 {
		t.Errorf("sent: %v", m)
	}
	if w := call(r.s.handleCancel, ""); w.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	r.waitPrint(t, PrintCancelled)
	r.waitFor(t, "the cancel's notification", func() bool { return sb.find("Print cancelled") != nil })
	if m := sb.find("Print cancelled"); m["priority"] != 2.0 {
		t.Errorf("a cancel on request is low priority: %v", m)
	}
}

func TestAMissedScheduleNotifies(t *testing.T) {
	s := &Server{DataDir: t.TempDir(), Now: time.Now}
	sb := withNotify(t, s)
	at := time.Now().Add(-time.Hour)
	s.sched.mu.Lock()
	p := &ScheduledPrint{ID: 7, Name: "tile.gcode", From: FromPi, At: at, State: SchedWaiting}
	s.sched.data.Prints = append(s.sched.data.Prints, p)
	s.endScheduleLocked(p, SchedMissed, "the server wasn't running at its time")
	q := &ScheduledPrint{ID: 8, Name: "tile.gcode", From: FromPi, At: at, State: SchedWaiting}
	s.sched.data.Prints = append(s.sched.data.Prints, q)
	s.endScheduleLocked(q, PrintCancelled, requestCancel)
	s.sched.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for sb.find("Scheduled print missed") == nil {
		if time.Now().After(deadline) {
			t.Fatalf("sent %q", sb.titles())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := sb.titles(); len(got) != 1 {
		t.Errorf("a cancel on request isn't news: sent %q", got)
	}
}
