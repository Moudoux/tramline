package client

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tramline/internal/hue"
	"tramline/internal/marlin"
	"tramline/internal/secure"
	"tramline/internal/server"
	"tramline/internal/sim"
)

type fixture struct {
	url, fingerprint string
	serverDir        string
	clientFP         string
	cert             tls.Certificate
}

func (f fixture) client(t *testing.T) *Client {
	t.Helper()
	return mustClient(t, Login{f.url, f.fingerprint}, f.cert)
}

// stranger has a valid certificate that was never allowed on the server.
func (f fixture) stranger(t *testing.T) *Client {
	t.Helper()
	cert, _ := newClientCert(t)
	return mustClient(t, Login{f.url, f.fingerprint}, cert)
}

func newClientCert(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	dir := t.TempDir()
	fp, err := secure.KeyGen(dir)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := secure.LoadClientCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	return cert, fp
}

// fakePlug counts switches, so tests can prove a refused one never happened.
type fakePlug struct {
	on       bool
	switches int
	pairErr  error
}

func (p *fakePlug) Pair(context.Context) error { return p.pairErr }
func (p *fakePlug) Lights(context.Context) ([]hue.Light, error) {
	return []hue.Light{{ID: "uuid-1", Name: "Light plug", Archetype: "plug", On: p.on}}, nil
}
func (p *fakePlug) State(context.Context) (hue.Light, error) {
	return hue.Light{ID: "uuid-1", Name: "Light plug", On: p.on}, nil
}
func (p *fakePlug) Set(_ context.Context, on bool) error {
	p.on = on
	p.switches++
	return nil
}

func start(t *testing.T) fixture {
	t.Helper()
	return startWith(t, nil, marlin.Status{Connected: true, Firmware: "Marlin", Temps: &marlin.Temps{Bed: 22.5}})
}

func startWith(t *testing.T, plug server.Plug, st marlin.Status) fixture {
	t.Helper()
	s := &server.Server{
		Version: "test",
		Printer: func() marlin.Status { return st },
	}
	if plug != nil {
		s.Plug = plug
	}
	return serve(t, s)
}

func serve(t *testing.T, s *server.Server) fixture {
	t.Helper()
	dir := t.TempDir()
	fingerprint, err := secure.Init(dir, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	cert, clientFP := newClientCert(t)
	if err := secure.Allow(dir, clientFP, "test"); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := secure.ServerTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Allowed = func(fp string) bool { return secure.Allowed(dir, fp) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, tlsCfg) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return fixture{"https://" + ln.Addr().String(), fingerprint, dir, clientFP, cert}
}

func mustClient(t *testing.T, l Login, cert tls.Certificate) *Client {
	t.Helper()
	c, err := New(l, cert)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStatusWithAllowedClient(t *testing.T) {
	f := start(t)
	got, err := f.client(t).Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "test" || !got.Printer.Connected || got.Printer.Temps.Bed != 22.5 {
		t.Fatalf("got %+v", got)
	}
}

func TestUnknownClientIsRejected(t *testing.T) {
	f := start(t)
	_, err := f.stranger(t).Status()
	if err == nil || !strings.Contains(err.Error(), "refused this client") {
		t.Fatalf("got %v", err)
	}
}

func TestClientWithNoCertificateIsRejected(t *testing.T) {
	f := start(t)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
	}}
	resp, err := hc.Get(f.url + "/v1/status")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("served without a client certificate: %s", resp.Status)
	}
}

func TestAllowingAClientAppliesWithoutRestart(t *testing.T) {
	f := start(t)
	cert, fp := newClientCert(t)
	c := mustClient(t, Login{f.url, f.fingerprint}, cert)
	if _, err := c.Status(); err == nil {
		t.Fatal("expected a refusal before being allowed")
	}
	if err := secure.Allow(f.serverDir, fp, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := mustClient(t, Login{f.url, f.fingerprint}, cert).Status(); err != nil {
		t.Fatal(err)
	}
}

// Revoking must also cut off a connection that's already open.
func TestRevokeCutsOffOpenConnection(t *testing.T) {
	f := start(t)
	c := f.client(t)
	if _, err := c.Status(); err != nil {
		t.Fatal(err)
	}
	if err := secure.Revoke(f.serverDir, f.clientFP); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(); err == nil {
		t.Fatal("still served on the open connection after revoking")
	}
	if _, err := f.client(t).Status(); err == nil {
		t.Fatal("new connection was served after revoking")
	}
}

func TestWrongServerCertificateIsRejected(t *testing.T) {
	f := start(t)
	wrong := strings.Repeat("0", 64)
	_, err := mustClient(t, Login{f.url, wrong}, f.cert).Status()
	if err == nil || !strings.Contains(err.Error(), "doesn't match the saved fingerprint") {
		t.Fatalf("got %v", err)
	}
}

func TestPlainHTTPGetsNoData(t *testing.T) {
	f := start(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(strings.Replace(f.url, "https://", "http://", 1) + "/v1/status")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "Marlin") {
		t.Fatalf("plain HTTP was served: %s %s", resp.Status, body)
	}
}

func TestClientRefusesPlainHTTPURL(t *testing.T) {
	cert, _ := newClientCert(t)
	if _, err := New(Login{"http://192.168.1.121:8726", "ab"}, cert); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	cert, _ := newClientCert(t)
	c := mustClient(t, Login{"https://127.0.0.1:1", "ab"}, cert)
	if err := c.http.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("got %v", err)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	want := Login{"https://h:1", "fp"}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "client.json")); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestLoadWithoutLoginExplains(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "tramline login") {
		t.Fatalf("got %v", err)
	}
}

func TestPowerOnAndOffWhenCold(t *testing.T) {
	plug := &fakePlug{}
	f := startWith(t, plug, marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 22, Bed: 22}})
	c := f.client(t)
	if err := c.Power(true); err != nil || !plug.on {
		t.Fatalf("on: %v, plug %+v", err, plug)
	}
	if err := c.Power(false); err != nil || plug.on {
		t.Fatalf("off: %v, plug %+v", err, plug)
	}
}

func TestPowerOffAllowedWhenPrinterDisconnected(t *testing.T) {
	plug := &fakePlug{on: true}
	f := startWith(t, plug, marlin.Status{})
	if err := f.client(t).Power(false); err != nil {
		t.Fatal(err)
	}
}

func TestPowerOffRefusals(t *testing.T) {
	tests := []struct {
		name string
		st   marlin.Status
		want string
	}{
		{"hot nozzle", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 50, Bed: 30}}, "nozzle is 50"},
		{"nozzle heating", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 22, NozzleTarget: 150}}, "heaters are on"},
		{"bed heating", marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 22, BedTarget: 80}}, "heaters are on"},
		{"unknown temperatures", marlin.Status{Connected: true}, "aren't known"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plug := &fakePlug{on: true}
			f := startWith(t, plug, tt.st)
			err := f.client(t).Power(false)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v", err)
			}
			if plug.switches != 0 || !plug.on {
				t.Fatalf("plug was switched: %+v", plug)
			}
		})
	}
}

// Turning on stays possible while hot, since that can only help cooling.
func TestPowerOnAllowedWhileHot(t *testing.T) {
	plug := &fakePlug{}
	f := startWith(t, plug, marlin.Status{Connected: true, Temps: &marlin.Temps{Nozzle: 200}})
	if err := f.client(t).Power(true); err != nil {
		t.Fatal(err)
	}
}

func TestPowerRejectsBadBody(t *testing.T) {
	plug := &fakePlug{}
	f := startWith(t, plug, marlin.Status{})
	c := f.client(t)
	if err := c.do(http.MethodPost, "/v1/power", map[string]string{"state": "on"}, nil); err == nil {
		t.Fatal("expected an error")
	}
	if plug.switches != 0 {
		t.Fatal("plug was switched")
	}
}

func TestPowerNeedsAllowedClient(t *testing.T) {
	plug := &fakePlug{}
	f := startWith(t, plug, marlin.Status{})
	if err := f.stranger(t).Power(true); err == nil {
		t.Fatal("expected an error")
	}
	if plug.switches != 0 {
		t.Fatal("plug was switched")
	}
}

func TestStatusIncludesPlug(t *testing.T) {
	f := startWith(t, &fakePlug{on: true}, marlin.Status{})
	got, err := f.client(t).Status()
	if err != nil {
		t.Fatal(err)
	}
	if got.Plug == nil || !got.Plug.On || got.Plug.Name != "Light plug" {
		t.Fatalf("got %+v", got.Plug)
	}
}

func TestHuePairAndPlugs(t *testing.T) {
	plug := &fakePlug{pairErr: hue.ErrLinkButton}
	f := startWith(t, plug, marlin.Status{})
	c := f.client(t)
	if err := c.HuePair(); err == nil || !strings.Contains(err.Error(), "link button") {
		t.Fatalf("got %v", err)
	}
	plug.pairErr = nil
	if err := c.HuePair(); err != nil {
		t.Fatal(err)
	}
	lights, err := c.HuePlugs()
	if err != nil || len(lights) != 1 || lights[0].ID != "uuid-1" {
		t.Fatalf("got %+v, %v", lights, err)
	}
	plug.pairErr = errors.New("boom")
	if err := c.HuePair(); err == nil {
		t.Fatal("expected an error")
	}
}

// simulated runs the whole server against a simulated printer behind a simulated
// plug, with only the idle timeout's clock left real.
func simulated(t *testing.T) (*Client, *sim.Plug) {
	t.Helper()
	plug := &sim.Plug{Options: sim.Options{Garbage: true}}
	printer := marlin.NewPrinter(plug.Open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go printer.Run(ctx)
	f := serve(t, &server.Server{Version: "test", Simulated: true, Printer: printer.Status, Heaters: printer, Plug: plug})
	return f.client(t), plug
}

func waitStatus(t *testing.T, c *Client, what string, cond func(server.StatusResponse) bool) server.StatusResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := c.Status()
		if err != nil {
			t.Fatal(err)
		}
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %+v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHeatingWorkflowOnSimulator(t *testing.T) {
	c, _ := simulated(t)

	_, err := c.Preheat(40, 60)
	if err == nil || !strings.Contains(err.Error(), "plug is off") {
		t.Fatalf("plug off: %v", err)
	}
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })

	if _, err := c.Preheat(101, 60); err == nil || !strings.Contains(err.Error(), "bed must be") {
		t.Fatalf("bed limit: %v", err)
	}
	if _, err := c.Preheat(40, 261); err == nil || !strings.Contains(err.Error(), "nozzle must be") {
		t.Fatalf("nozzle limit: %v", err)
	}

	out, err := c.Preheat(40, 100)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(out.HeatersOffAt); left < 29*time.Minute || left > 31*time.Minute {
		t.Fatalf("heaters off in %s", left)
	}
	if err := c.Power(false); err == nil || !strings.Contains(err.Error(), "heaters are on") {
		t.Fatalf("power off while heating: %v", err)
	}
	st := waitStatus(t, c, "heating", func(st server.StatusResponse) bool {
		return st.Printer.Temps.Nozzle >= 99 && st.Printer.Temps.Bed >= 39
	})
	if st.HeatersOffAt == nil {
		t.Fatal("status has no off time")
	}

	if _, err := c.Cool(); err != nil {
		t.Fatal(err)
	}
	// A nozzle still hot with the heaters off must also refuse.
	if err := c.Power(false); err == nil || !strings.Contains(err.Error(), "nozzle is") {
		t.Fatalf("power off while hot: %v", err)
	}
	st = waitStatus(t, c, "cooling", func(st server.StatusResponse) bool {
		return st.Printer.Temps.NozzleTarget == 0 && st.Printer.Temps.Nozzle < server.CoolEnough
	})
	if st.HeatersOffAt != nil {
		t.Fatalf("timer still set: %v", st.HeatersOffAt)
	}
	if err := c.Power(false); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "disconnect", func(st server.StatusResponse) bool { return !st.Printer.Connected })
	if _, err := c.Preheat(40, 60); err == nil {
		t.Fatal("heated with the plug off")
	}
	if _, err := c.Cool(); err != nil {
		t.Fatalf("cool with plug off: %v", err)
	}
}

func TestHeatCommandsNeedAllowedClient(t *testing.T) {
	plug := &sim.Plug{}
	printer := marlin.NewPrinter(plug.Open)
	f := serve(t, &server.Server{Version: "test", Printer: printer.Status, Heaters: printer, Plug: plug})
	stranger := f.stranger(t)
	if _, err := stranger.Preheat(40, 60); err == nil {
		t.Fatal("stranger could preheat")
	}
	if _, err := stranger.Cool(); err == nil {
		t.Fatal("stranger could cool")
	}
}

// The running server must apply the timeout by itself, with no client involved.
func TestRunningServerAppliesIdleTimeout(t *testing.T) {
	plug := &sim.Plug{}
	printer := marlin.NewPrinter(plug.Open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go printer.Run(ctx)
	var skipped atomic.Int64
	f := serve(t, &server.Server{
		Version: "test", Printer: printer.Status, Heaters: printer, Plug: plug,
		Now: func() time.Time { return time.Now().Add(time.Duration(skipped.Load())) },
	})
	c := f.client(t)
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	if _, err := c.Preheat(40, 60); err != nil {
		t.Fatal(err)
	}
	skipped.Store(int64(server.HeatIdleLimit))
	st := waitStatus(t, c, "heaters off", func(st server.StatusResponse) bool {
		return st.Printer.Temps.BedTarget == 0 && st.Printer.Temps.NozzleTarget == 0
	})
	if st.HeatersOffAt != nil {
		t.Fatalf("timer still set: %v", st.HeatersOffAt)
	}
}

func simulatedTram(t *testing.T, opts sim.Options) (*Client, *sim.Plug, string) {
	t.Helper()
	plug := &sim.Plug{Options: opts}
	printer := marlin.NewPrinter(plug.Open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go printer.Run(ctx)
	dir := t.TempDir()
	f := serve(t, &server.Server{
		Version: "test", Simulated: true, Printer: printer.Status, Heaters: printer, Prober: printer, Plug: plug, DataDir: dir,
	})
	c := f.client(t)
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	return c, plug, dir
}

func waitTram(t *testing.T, c *Client) server.TramJob {
	t.Helper()
	st := waitStatus(t, c, "tram to finish", func(st server.StatusResponse) bool {
		return st.Tram != nil && st.Tram.State != server.TramRunning
	})
	return *st.Tram
}

func TestTramWorkflowOnSimulator(t *testing.T) {
	c, _, dir := simulatedTram(t, sim.Options{
		Garbage: true, MoveDelay: 30 * time.Millisecond, Corners: sim.Corners{FL: 0.1, FR: 0.1, BR: 0.4, BL: 0.1},
	})
	if _, err := c.Tram(server.TramRequest{}); err == nil || !strings.Contains(err.Error(), "bed_clear") {
		t.Fatalf("no confirmation: %v", err)
	}
	if _, err := c.Tram(server.TramRequest{BedClear: true}); err != nil {
		t.Fatal(err)
	}
	job := waitTram(t, c)
	if job.State != server.TramDone || !job.Parked || job.Result == nil {
		t.Fatalf("got %+v", job)
	}
	if job.Result.Pass || job.Result.Corner != "back-right" || job.Result.Action != "lower" {
		t.Fatalf("got %+v", job.Result)
	}
	got := job.Rounds[0].Readings
	if got[0].Corner != "front-left" || got[2].X != 205 || got[2].Y != 205 || got[2].Z != 0.4 {
		t.Fatalf("got %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tram-history.jsonl"))
	if err != nil || !strings.Contains(string(data), "back-right") {
		t.Fatalf("history: %q, %v", data, err)
	}
	lines, err := c.TramInfo()
	if err != nil || len(lines) == 0 {
		t.Fatalf("info: %v, %v", lines, err)
	}
}

func TestTramLinkDropOnSimulator(t *testing.T) {
	c, _, _ := simulatedTram(t, sim.Options{DropProbe: 2, Corners: sim.Corners{FL: 0.1}})
	if _, err := c.Tram(server.TramRequest{BedClear: true}); err != nil {
		t.Fatal(err)
	}
	job := waitTram(t, c)
	if job.State != server.TramFailed || job.Result != nil || job.Parked || !strings.Contains(job.Error, "disconnected") {
		t.Fatalf("got %+v", job)
	}
	if len(job.Rounds[0].Readings) != 1 {
		t.Fatalf("partial readings: %+v", job.Rounds)
	}
	// It comes back by itself, and stays usable.
	waitStatus(t, c, "reconnect", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	if _, err := c.Preheat(40, 60); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cool(); err != nil {
		t.Fatal(err)
	}
}

func TestPowerLossDuringTramOnSimulator(t *testing.T) {
	c, plug, _ := simulatedTram(t, sim.Options{MoveDelay: 200 * time.Millisecond})
	if _, err := c.Tram(server.TramRequest{BedClear: true}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "probing", func(st server.StatusResponse) bool {
		return st.Tram != nil && strings.HasPrefix(st.Tram.Step, "probing")
	})
	// Like the plug switching itself off. The server's own power off is refused.
	if err := c.Power(false); err == nil || !strings.Contains(err.Error(), "tram run") {
		t.Fatalf("power off during a run: %v", err)
	}
	if err := plug.Set(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	job := waitTram(t, c)
	if job.State != server.TramFailed || job.Result != nil || job.Parked {
		t.Fatalf("got %+v", job)
	}
	if _, err := c.Tram(server.TramRequest{BedClear: true}); err == nil || !strings.Contains(err.Error(), "plug is off") {
		t.Fatalf("got %v", err)
	}
}

func TestTramNeedsAllowedClient(t *testing.T) {
	plug := &sim.Plug{}
	printer := marlin.NewPrinter(plug.Open)
	f := serve(t, &server.Server{Version: "test", Printer: printer.Status, Heaters: printer, Prober: printer, Plug: plug})
	stranger := f.stranger(t)
	if _, err := stranger.Tram(server.TramRequest{BedClear: true}); err == nil {
		t.Fatal("stranger could tram")
	}
	if _, err := stranger.TramInfo(); err == nil {
		t.Fatal("stranger could ask for info")
	}
}

func simulatedUpload(t *testing.T, opts sim.Options) (*Client, *sim.Plug, *sim.Card) {
	t.Helper()
	opts.Card = sim.NewCard()
	plug := &sim.Plug{Options: opts}
	printer := marlin.NewPrinter(plug.Open)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go printer.Run(ctx)
	f := serve(t, &server.Server{
		Printer: printer.Status, Plug: plug, Heaters: printer, Prober: printer, Card: printer, Prints: printer, DataDir: t.TempDir(),
		HeatHold: time.Millisecond,
	})
	return f.client(t), plug, opts.Card
}

func put(t *testing.T, c *Client, name, content string) server.FileRecord {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	rec, err := c.PutFile(name, strings.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestUploadWorkflowOnSimulator(t *testing.T) {
	c, _, card := simulatedUpload(t, sim.Options{Garbage: true})
	var b strings.Builder
	b.WriteString(";TIME:60\r\n")
	for i := range 300 {
		fmt.Fprintf(&b, "G1 X%d Y%d ; move\r\n", i, i)
	}

	// The printer is off: the file still goes to the Pi, and only the copy is refused.
	if _, err := c.File("part.gcode"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	rec := put(t, c, "part.gcode", b.String())
	if _, err := c.Copy("part.gcode", 0); err == nil || !strings.Contains(err.Error(), "plug is off") {
		t.Fatalf("got %v", err)
	}
	if got, err := c.File("part.gcode"); err != nil || got.SHA256 != rec.SHA256 {
		t.Fatalf("got %+v, %v", got, err)
	}

	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "connected", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	out, err := c.Copy("part.gcode", 0)
	if err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, c, "the copy", func(st server.StatusResponse) bool {
		return st.Copy != nil && st.Copy.State != server.CopyRunning
	})
	if st.Copy.ID != out.Job.ID || st.Copy.State != server.CopyDone || st.Copy.Lines != 300 {
		t.Fatalf("got %+v", st.Copy)
	}
	got, ok := card.File(rec.SDName)
	if !ok || int64(len(got)) != rec.Info.SDSize || strings.Contains(got, "TIME") {
		t.Fatalf("got %v, %d bytes, want %d", ok, len(got), rec.Info.SDSize)
	}

	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	if files.Mode != server.ModeAuto || len(files.Files) != 1 || files.Files[0].SDState != server.CopyDone ||
		len(files.Card) != 1 || files.Card[0].Size != rec.Info.SDSize {
		t.Fatalf("got %+v", files)
	}
	if again, err := c.Copy("part.gcode", 0); err != nil || again.Job != nil {
		t.Fatalf("got %+v, %v", again, err)
	}

	if err := c.SetMode(server.ModeStream, 0); err != nil {
		t.Fatal(err)
	}
	if settings, err := c.Settings(); err != nil || settings.Mode != server.ModeStream {
		t.Fatalf("got %+v, %v", settings, err)
	}

	if err := c.DeleteFile("part.gcode"); err != nil {
		t.Fatal(err)
	}
	if len(card.Names()) != 0 {
		t.Fatalf("got %q", card.Names())
	}
}

// Turning the printer off mid-copy fails the copy, and the retry needs only the
// file already on the Pi.
func TestPowerLossDuringACopyOnSimulator(t *testing.T) {
	c, plug, card := simulatedUpload(t, sim.Options{LineDelay: 2 * time.Millisecond})
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "connected", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	var b strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&b, "G1 X%d\n", i)
	}
	rec := put(t, c, "part.gcode", b.String())
	if _, err := c.Copy("part.gcode", 0); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "lines to be sent", func(st server.StatusResponse) bool { return st.Copy != nil && st.Copy.Lines > 5 })
	if err := plug.Set(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, c, "the copy to fail", func(st server.StatusResponse) bool { return st.Copy.State != server.CopyRunning })
	if st.Copy.State != server.CopyFailed || !strings.Contains(st.Copy.Error, "partial file") {
		t.Fatalf("got %+v", st.Copy)
	}
	if _, err := c.Copy("part.gcode", 0); err == nil {
		t.Fatal("the copy should be refused with the plug off")
	}
	plug.Options.LineDelay = 0
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "connected", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	if _, err := c.Copy("part.gcode", 0); err != nil {
		t.Fatal(err)
	}
	st = waitStatus(t, c, "the retry", func(st server.StatusResponse) bool { return st.Copy.State != server.CopyRunning })
	if st.Copy.State != server.CopyDone {
		t.Fatalf("got %+v", st.Copy)
	}
	if got, _ := card.File(rec.SDName); int64(len(got)) != rec.Info.SDSize {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestFilesNeedAnAllowedClient(t *testing.T) {
	f := serve(t, &server.Server{Printer: func() marlin.Status { return marlin.Status{} }, DataDir: t.TempDir()})
	stranger := f.stranger(t)
	if _, err := stranger.Files(); err == nil {
		t.Fatal("a stranger listed the files")
	}
	if _, err := stranger.PutFile("a.gcode", strings.NewReader("G28"), 3, strings.Repeat("0", 64)); err == nil {
		t.Fatal("a stranger uploaded a file")
	}
	if err := stranger.SetMode(server.ModeStream, 0); err == nil {
		t.Fatal("a stranger changed the mode")
	}
}

func TestPrintWorkflowOnSimulator(t *testing.T) {
	c, plug, card := simulatedUpload(t, sim.Options{Garbage: true, PrintTime: time.Hour})
	const name = "tile.gcode"
	put(t, c, name, "G1 X1\nG1 X2\n")
	if _, err := c.Print(name, true); err == nil || !strings.Contains(err.Error(), "the plug is off") {
		t.Fatalf("no power yet: %v", err)
	}
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	if _, err := c.Copy(name, 0); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "the copy", func(st server.StatusResponse) bool {
		return st.Copy != nil && st.Copy.State == server.CopyDone
	})
	if _, err := c.Print(name, false); err == nil || !strings.Contains(err.Error(), "bed") {
		t.Fatalf("no confirmation: %v", err)
	}
	job, err := c.Print(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != server.PrintHeating {
		t.Fatalf("job: %+v", job)
	}
	waitStatus(t, c, "the tram check to pass", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintRunning
	})
	if !card.Printing() {
		t.Fatal("the print should have started")
	}
	if err := c.Power(false); err == nil {
		t.Fatal("power off should be refused during a print")
	}
	if err := c.CancelPrint(); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, c, "the cancel", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintCancelled
	})
	if st.HeatersOffAt != nil {
		t.Errorf("timer still set: %v", st.HeatersOffAt)
	}

	if _, err := c.Print(name, true); err != nil {
		t.Fatal(err)
	}
	out, err := c.Estop()
	if err != nil || !out.M112 || !out.PlugOff {
		t.Fatalf("estop: %+v, %v", out, err)
	}
	if on, _ := plug.State(context.Background()); on.On {
		t.Error("the plug should be off")
	}
}

func TestStreamWorkflowOnSimulator(t *testing.T) {
	c, _, card := simulatedUpload(t, sim.Options{Garbage: true, MoveTime: 5 * time.Millisecond})
	const name = "large.gcode"
	var file strings.Builder
	for i := range 200 {
		fmt.Fprintf(&file, "G1 X%d Y%d\n", i, i)
	}
	rec := put(t, c, name, file.String())
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	job, err := c.Print(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if job.Mode != server.ModeStream || job.SDName != "" {
		t.Fatalf("job: %+v", job)
	}
	st := waitStatus(t, c, "the stream to finish", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintFinished
	})
	if st.Print.Pos != rec.Info.SDSize || st.Print.Resends != 0 {
		t.Errorf("job: %+v", st.Print)
	}
	if card.Printing() || len(card.Names()) != 0 {
		t.Errorf("the card was used: %q", card.Names())
	}

	if _, err := c.Print(name, true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "the second stream", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintRunning
	})
	if err := c.Power(false); err == nil {
		t.Fatal("power off should be refused during a stream")
	}
	if err := c.CancelPrint(); err != nil {
		t.Fatal(err)
	}
	st = waitStatus(t, c, "the cancel", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintCancelled
	})
	if st.Printer.Temps == nil || st.HeatersOffAt != nil {
		t.Errorf("status: %+v", st)
	}
}

func TestPrintTramLoopOnSimulator(t *testing.T) {
	var mu sync.Mutex
	bed := sim.Corners{BL: 0.1}
	c, _, card := simulatedUpload(t, sim.Options{PrintTime: time.Hour, Bed: func() sim.Corners {
		mu.Lock()
		defer mu.Unlock()
		return bed
	}})
	const name = "tile.gcode"
	put(t, c, name, "G1 X1\nG1 X2\n")
	if err := c.Power(true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "temperatures", func(st server.StatusResponse) bool { return st.Printer.Temps != nil })
	if _, err := c.Copy(name, 0); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "the copy", func(st server.StatusResponse) bool {
		return st.Copy != nil && st.Copy.State == server.CopyDone
	})
	if _, err := c.ContinuePrint(); err == nil {
		t.Fatal("nothing is waiting yet")
	}
	if _, err := c.Print(name, true); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, c, "the failed check", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintAdjusting
	})
	if ch := st.Print.Check; ch == nil || ch.Corner != "back-left" || ch.Action != "lower" || !st.Print.Parked {
		t.Fatalf("job: %+v", st.Print)
	}
	if st.HeatersOffAt == nil {
		t.Error("status should show when the heaters go off during the wait")
	}
	if card.Printing() {
		t.Fatal("the file started without a pass")
	}
	mu.Lock()
	bed = sim.Corners{}
	mu.Unlock()
	if _, err := c.ContinuePrint(); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "the print to start", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintRunning
	})
	if !card.Printing() {
		t.Error("the print should have started")
	}
	if err := c.CancelPrint(); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, c, "the cancel", func(st server.StatusResponse) bool {
		return st.Print != nil && st.Print.State == server.PrintCancelled
	})
}

func TestDirFollowsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/somewhere/else")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/somewhere/else/tramline" {
		t.Fatalf("dir = %q", dir)
	}
}

func TestDirDefaultsToDotConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/someone")
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/home/someone/.config/tramline" {
		t.Fatalf("dir = %q", dir)
	}
}
