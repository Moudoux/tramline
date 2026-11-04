package hue

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	bridgeID = "001788FFFE123456"
	plugID   = "3f2b8a10-6c1d-4e7a-9b52-0d4c8e1f7a36"
	testKey  = "test-application-key-not-real"
)

func testCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// bridge is a fake Hue Bridge using the same request and reply shapes.
type bridge struct {
	mu           sync.Mutex
	linkPressed  bool
	on           bool
	ignoreSwitch bool
	delay        time.Duration
	switchAt     time.Time
	target       bool
	srv          *httptest.Server
}

func newBridge(t *testing.T, cn string) *bridge {
	t.Helper()
	b := &bridge{}
	b.srv = httptest.NewUnstartedServer(http.HandlerFunc(b.serve))
	b.srv.TLS = &tls.Config{Certificates: []tls.Certificate{testCert(t, cn)}}
	b.srv.StartTLS()
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bridge) address() string { return strings.TrimPrefix(b.srv.URL, "https://") }

func (b *bridge) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.URL.Path == "/api" {
		if !b.linkPressed {
			fmt.Fprint(w, `[{"error":{"type":101,"address":"","description":"link button not pressed"}}]`)
			return
		}
		fmt.Fprintf(w, `[{"success":{"username":%q,"clientkey":"abc"}}]`, testKey)
		return
	}
	if r.Header.Get("hue-application-key") != testKey {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"errors":[{"description":"unauthorized user"}],"data":[]}`)
		return
	}
	if !b.switchAt.IsZero() && time.Now().After(b.switchAt) {
		b.on, b.switchAt = b.target, time.Time{}
	}
	light := func() string {
		return fmt.Sprintf(`{"id":%q,"metadata":{"name":"Light plug","archetype":"plug"},"on":{"on":%t}}`, plugID, b.on)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/clip/v2/resource/light":
		fmt.Fprintf(w, `{"errors":[],"data":[%s,{"id":"other","metadata":{"name":"Lamp","archetype":"sultan_bulb"},"on":{"on":false}}]}`, light())
	case r.Method == http.MethodGet && r.URL.Path == "/clip/v2/resource/light/"+plugID:
		fmt.Fprintf(w, `{"errors":[],"data":[%s]}`, light())
	case r.Method == http.MethodPut && r.URL.Path == "/clip/v2/resource/light/"+plugID:
		var in struct {
			On struct {
				On bool `json:"on"`
			} `json:"on"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if !b.ignoreSwitch {
			b.target = in.On.On
			b.switchAt = time.Now().Add(b.delay)
		}
		fmt.Fprintf(w, `{"errors":[],"data":[{"rid":%q,"rtype":"light"}]}`, plugID)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[{"description":"not found"}],"data":[]}`)
	}
}

func clientFor(t *testing.T, b *bridge) *Client {
	t.Helper()
	return &Client{Address: b.address(), BridgeID: bridgeID, PlugID: plugID, DataDir: t.TempDir()}
}

func paired(t *testing.T, b *bridge) *Client {
	t.Helper()
	b.mu.Lock()
	b.linkPressed = true
	b.mu.Unlock()
	c := clientFor(t, b)
	if err := c.Pair(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPairNeedsLinkButton(t *testing.T) {
	b := newBridge(t, strings.ToLower(bridgeID))
	c := clientFor(t, b)
	if err := c.Pair(context.Background()); !errors.Is(err, ErrLinkButton) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.DataDir, keyFile)); err == nil {
		t.Fatal("a key file was written without a key")
	}
}

func TestPairSavesKeyPrivately(t *testing.T) {
	c := paired(t, newBridge(t, strings.ToLower(bridgeID)))
	info, err := os.Stat(filepath.Join(c.DataDir, keyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", info.Mode().Perm())
	}
}

func TestPairRefusesToReplace(t *testing.T) {
	c := paired(t, newBridge(t, bridgeID))
	if err := c.Pair(context.Background()); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("got %v", err)
	}
}

// The key must not be requested from a device claiming to be something else.
func TestPairRejectsWrongBridge(t *testing.T) {
	b := newBridge(t, "some-other-device")
	b.linkPressed = true
	c := clientFor(t, b)
	err := c.Pair(context.Background())
	if err == nil || !strings.Contains(err.Error(), "identifies as") {
		t.Fatalf("got %v", err)
	}
}

func TestCallsNeedPairing(t *testing.T) {
	c := clientFor(t, newBridge(t, bridgeID))
	if _, err := c.Lights(context.Background()); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("got %v", err)
	}
}

// The key must never be sent to a device whose certificate has changed.
func TestChangedCertificateIsRejected(t *testing.T) {
	first := newBridge(t, bridgeID)
	c := paired(t, first)
	second := newBridge(t, bridgeID)
	second.linkPressed = true
	c.Address = second.address()
	_, err := c.Lights(context.Background())
	if err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("got %v", err)
	}
}

func TestLightsListsNameAndUUID(t *testing.T) {
	c := paired(t, newBridge(t, bridgeID))
	lights, err := c.Lights(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lights) != 2 || lights[0].ID != plugID || lights[0].Name != "Light plug" || lights[0].Archetype != "plug" {
		t.Fatalf("got %+v", lights)
	}
}

func TestSetWaitsForTheStateToChange(t *testing.T) {
	b := newBridge(t, bridgeID)
	c := paired(t, b)
	b.mu.Lock()
	b.delay = 600 * time.Millisecond
	b.mu.Unlock()
	if err := c.Set(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	state, err := c.State(context.Background())
	if err != nil || !state.On {
		t.Fatalf("got %+v, %v", state, err)
	}
	if err := c.Set(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

func TestSetFailsIfThePlugNeverChanges(t *testing.T) {
	b := newBridge(t, bridgeID)
	c := paired(t, b)
	b.mu.Lock()
	b.ignoreSwitch = true
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if err := c.Set(ctx, true); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnknownPlugIsAnError(t *testing.T) {
	c := paired(t, newBridge(t, bridgeID))
	c.PlugID = "00000000-0000-0000-0000-000000000000"
	if _, err := c.State(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if err := c.Set(context.Background(), true); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNothingWorksWithoutConfig(t *testing.T) {
	c := &Client{DataDir: t.TempDir()}
	if c.Configured() {
		t.Fatal("reported as configured")
	}
	if err := c.Pair(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
	c = &Client{Address: "x", BridgeID: "y", DataDir: t.TempDir()}
	if err := c.Set(context.Background(), true); !errors.Is(err, ErrNoPlug) {
		t.Fatalf("got %v", err)
	}
}

// Errors reach logs and clients, so they must never carry the key.
func TestKeyNeverAppearsInErrors(t *testing.T) {
	b := newBridge(t, bridgeID)
	c := paired(t, b)
	b.srv.Close()
	_, err := c.Lights(context.Background())
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("got %v", err)
	}
}
