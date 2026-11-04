package client

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/server"
)

// These run against the real listener, as the port carries both servers.

func plainURL(f fixture) string { return strings.Replace(f.url, "https://", "http://", 1) }

func TestWebPageAndAPIShareAPort(t *testing.T) {
	f := start(t)
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(plainURL(f) + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Tramline") {
		t.Fatalf("%s %s", resp.Status, body)
	}
	// The API on the same port still works, and still needs its certificate.
	if _, err := f.client(t).Status(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.stranger(t).Status(); err == nil {
		t.Fatal("a stranger got status")
	}
}

// Listening on an interface is the same server: the page needs nothing, the API needs
// its certificate, and plain HTTP can't reach the API.
func TestInterfaceListenerServesTheSameWay(t *testing.T) {
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()

	plug := &fakePlug{on: true}
	lookup := func(string) ([]net.IP, error) { return []net.IP{net.IPv4(127, 0, 0, 2)}, nil }
	f := serveOn(t, &server.Server{
		Version: "test",
		Plug:    plug,
		Printer: func() marlin.Status { return marlin.Status{Connected: true} },
	}, server.ListenInterfaces([]string{"fake0"}, port, lookup, 10*time.Millisecond))
	also := fixture{fmt.Sprintf("https://127.0.0.2:%d", port), f.fingerprint, f.serverDir, f.clientFP, f.cert}
	hc := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := hc.Get(plainURL(also) + "/api/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatal(resp.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := also.client(t).Status(); err != nil {
		t.Fatal(err)
	}
	if _, err := also.stranger(t).Status(); err == nil {
		t.Fatal("a stranger got status")
	}
	resp, err := hc.Post(plainURL(also)+"/v1/power", "application/json", strings.NewReader(`{"on":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 || plug.switches != 0 {
		t.Fatalf("%s, %d switches", resp.Status, plug.switches)
	}
}

// Nothing that changes anything answers without a certificate, and the plug proves it.
func TestPlainHTTPCantUseTheAPI(t *testing.T) {
	plug := &fakePlug{on: true}
	f := startWith(t, plug, marlin.Status{Connected: true})
	hc := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/v1/estop", "/v1/power", "/v1/cool", "/v1/cancel", "/v1/print"} {
		resp, err := hc.Post(plainURL(f)+path, "application/json", strings.NewReader(`{"on":false}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("%s: %s", path, resp.Status)
		}
	}
	if plug.switches != 0 {
		t.Fatalf("the plug was switched %d times", plug.switches)
	}
}

// TLS without a client certificate gets nothing, not even the page.
func TestWebPageIsntServedOverTLSWithoutACertificate(t *testing.T) {
	f := start(t)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
	}}
	resp, err := hc.Get(f.url + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("served: %s", resp.Status)
	}
}

// A client that connects and says nothing can't hold up anyone else.
func TestSilentConnectionHoldsUpNobody(t *testing.T) {
	f := start(t)
	addr := strings.TrimPrefix(f.url, "https://")
	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if _, err := f.client(t).Status(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("GET /api/status HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		t.Fatalf("%q, %v", line, err)
	}
}

func TestWebURL(t *testing.T) {
	got, err := Login{URL: "https://192.168.1.121:8726"}.WebURL()
	if err != nil || got != "http://192.168.1.121:8726/" {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := (Login{URL: "ftp://x"}).WebURL(); err == nil {
		t.Fatal("expected an error")
	}
}
