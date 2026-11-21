package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tramline/internal/hue"
)

// eventReader reads "status" events from a stream.
type eventReader struct {
	t      *testing.T
	events chan string
}

func openEvents(t *testing.T, h http.Handler, path string) *eventReader {
	t.Helper()
	ts := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	// The stream has to go before the server can close.
	t.Cleanup(ts.Close)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	er := &eventReader{t: t, events: make(chan string, 64)}
	go func() {
		defer resp.Body.Close()
		defer close(er.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		name, data := "", ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if name == "status" {
					er.events <- data
				}
				name, data = "", ""
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return er
}

// next waits for the next event, and decodes it into v.
func (er *eventReader) next(within time.Duration, v any) {
	er.t.Helper()
	select {
	case data, ok := <-er.events:
		if !ok {
			er.t.Fatal("the stream ended")
		}
		if err := json.Unmarshal([]byte(data), v); err != nil {
			er.t.Fatalf("%v: %s", err, data)
		}
	case <-time.After(within):
		er.t.Fatalf("no event within %v", within)
	}
}

func TestWebEventsPushAChangeStraightAway(t *testing.T) {
	s, h := webRig(t)
	started := time.Now().Add(-time.Minute)
	s.print.job = &PrintJob{ID: 7, Name: "part.gcode", Mode: ModeStream, State: PrintRunning, Pos: 50, Size: 200, StartedAt: started, MovedAt: started}
	er := openEvents(t, h, "/api/events")
	var st WebStatus
	er.next(time.Second, &st)
	if st.Print == nil || st.Print.Pos != 50 || st.Temps == nil || st.Temps.Nozzle != 230 {
		t.Fatalf("first event: %+v", st)
	}

	s.print.mu.Lock()
	s.print.job.Pos = 120
	s.print.mu.Unlock()
	changed := time.Now()
	for st.Print.Pos != 120 {
		er.next(time.Second, &st)
	}
	// Well inside the heartbeat, so it came because it changed.
	if took := time.Since(changed); took > eventHeartbeat-eventTick {
		t.Errorf("the change took %v to arrive", took)
	}
}

func TestEventsWithNoNewsComeAboutOnceASecond(t *testing.T) {
	s, h := webRig(t)
	started := time.Now().Add(-time.Minute)
	// Its "seconds ago" change on every read, which isn't news.
	s.print.job = &PrintJob{ID: 7, Name: "part.gcode", Mode: ModeSD, State: PrintRunning, Pos: 50, Size: 200, StartedAt: started, MovedAt: started}
	er := openEvents(t, h, "/api/events")
	var first, second WebStatus
	er.next(time.Second, &first)
	at := time.Now()
	er.next(2*eventHeartbeat, &second)
	if gap := time.Since(at); gap < eventHeartbeat-2*eventTick {
		t.Errorf("an unchanged status came again after %v", gap)
	}
	if second.Print.RunSeconds <= first.Print.RunSeconds {
		t.Errorf("run time didn't move on: %v then %v", first.Print.RunSeconds, second.Print.RunSeconds)
	}
}

// noPlug fails the test if anything asks the Hue Bridge.
type noPlug struct{ t *testing.T }

func (p noPlug) Pair(context.Context) error { p.t.Error("paired"); return nil }
func (p noPlug) Lights(context.Context) ([]hue.Light, error) {
	p.t.Error("listed lights")
	return nil, nil
}
func (p noPlug) State(context.Context) (hue.Light, error) {
	p.t.Error("the stream asked the Hue Bridge for the plug")
	return hue.Light{}, nil
}
func (p noPlug) Set(context.Context, bool) error { p.t.Error("switched"); return nil }

func TestAPIEventsFollowATramCheckWithoutThePlug(t *testing.T) {
	s, _ := webRig(t)
	s.Plug = noPlug{t}
	s.tram.job = &TramJob{ID: 2, State: TramRunning, Phase: "probe", Target: &TramTarget{}, stepStarted: time.Now()}
	er := openEvents(t, http.HandlerFunc(s.handleEvents), "/v1/events")
	var st StatusResponse
	er.next(time.Second, &st)
	if st.Plug != nil || st.Tram == nil || st.Tram.Phase != "probe" || st.Version != "test" {
		t.Fatalf("first event: %+v", st)
	}
	s.updateTram(func(j *TramJob) { j.Phase, j.Step = "lift", "lifting" })
	for st.Tram.Phase != "lift" {
		er.next(time.Second, &st)
	}
	if st.Tram.Step != "lifting" {
		t.Errorf("step %q", st.Tram.Step)
	}
}

func TestWebEventsAreOnThePageOnly(t *testing.T) {
	_, h := webRig(t)
	if w := webGet(h, http.MethodGet, "/v1/events"); w.Code != http.StatusNotFound {
		t.Errorf("the page served the API's stream: %d", w.Code)
	}
}
