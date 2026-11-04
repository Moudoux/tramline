package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"tramline/internal/marlin"
	"tramline/internal/secure"
)

// fakeInterfaces are interfaces whose addresses a test can change. Linux can listen on
// anything in 127.0.0.0/8, so these are real sockets.
type fakeInterfaces struct {
	mu  sync.Mutex
	ips map[string][]net.IP
	err map[string]error
}

func (f *fakeInterfaces) set(name string, err error, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ips == nil {
		f.ips, f.err = map[string][]net.IP{}, map[string]error{}
	}
	f.err[name], f.ips[name] = err, nil
	for _, ip := range ips {
		f.ips[name] = append(f.ips[name], net.ParseIP(ip))
	}
}

func (f *fakeInterfaces) lookup(name string) ([]net.IP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ips[name], f.err[name]
}

// freePort finds a port that's free now. It's on 127.0.0.1, and these tests listen
// elsewhere in 127.0.0.0/8, so holding it isn't needed for them to get it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// followAndServe runs a whole server on the interfaces. stop ends it.
func followAndServe(t *testing.T, f *fakeInterfaces, names ...string) (port int, stop func()) {
	t.Helper()
	dir := t.TempDir()
	if _, err := secure.Init(dir, net.IPv4(127, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := secure.ServerTLS(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Version: "test",
		Printer: func() marlin.Status { return marlin.Status{Connected: true} },
		DataDir: t.TempDir(),
	}
	port = freePort(t)
	ln := ListenInterfaces(names, port, f.lookup, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln, tlsCfg) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Serve didn't stop")
			}
		})
	}
	t.Cleanup(stop)
	return port, stop
}

func answers(ip string, port int) bool {
	hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := hc.Get("http://" + (&net.TCPAddr{IP: net.ParseIP(ip), Port: port}).String() + "/api/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("never happened: " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// This is Tailscale coming up after Tramline: the LAN works throughout, and the other
// interface starts working by itself once it exists.
func TestFollowsAnInterfaceThatComesUpLater(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil, "127.0.0.2")
	f.set("ts", errors.New("route ip+net: no such network interface"))
	port, _ := followAndServe(t, f, "lan", "ts")

	waitUntil(t, "lan answers", func() bool { return answers("127.0.0.2", port) })
	time.Sleep(50 * time.Millisecond) // a few failed lookups
	if answers("127.0.0.3", port) {
		t.Fatal("answered on an address no interface has")
	}
	if !answers("127.0.0.2", port) {
		t.Fatal("lan stopped answering")
	}
	f.set("ts", nil, "127.0.0.3")
	waitUntil(t, "ts answers", func() bool { return answers("127.0.0.3", port) })
	if !answers("127.0.0.2", port) {
		t.Fatal("lan stopped answering")
	}
}

// With nothing to listen on yet, the server still runs and waits.
func TestStartsWithNoAddressesYet(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil)
	port, _ := followAndServe(t, f, "lan")
	time.Sleep(50 * time.Millisecond)
	f.set("lan", nil, "127.0.0.2")
	waitUntil(t, "lan answers", func() bool { return answers("127.0.0.2", port) })
}

// If DHCP or Tailscale gives the Pi a new address, the server moves without a restart,
// and the other interface doesn't notice.
func TestFollowsAnInterfacesNewAddress(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil, "127.0.0.2")
	f.set("ts", nil, "127.0.0.4")
	port, _ := followAndServe(t, f, "lan", "ts")

	waitUntil(t, "first address answers", func() bool { return answers("127.0.0.2", port) })
	f.set("lan", nil, "127.0.0.3")
	waitUntil(t, "new address answers", func() bool { return answers("127.0.0.3", port) })
	waitUntil(t, "old address stops", func() bool { return !answers("127.0.0.2", port) })
	if !answers("127.0.0.4", port) {
		t.Fatal("the other interface stopped answering")
	}
}

// Losing the interface for a moment leaves what's open alone.
func TestKeepsListeningWhenALookupFails(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil, "127.0.0.2")
	port, _ := followAndServe(t, f, "lan")

	waitUntil(t, "lan answers", func() bool { return answers("127.0.0.2", port) })
	f.set("lan", errors.New("no such network interface"))
	time.Sleep(50 * time.Millisecond)
	if !answers("127.0.0.2", port) {
		t.Fatal("stopped answering after a failed lookup")
	}
}

// An address something else holds is tried again until it's free.
func TestRetriesAnAddressInUse(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil, "127.0.0.2")
	f.set("ts", nil)
	port, _ := followAndServe(t, f, "lan", "ts")
	waitUntil(t, "lan answers", func() bool { return answers("127.0.0.2", port) })

	addr := (&net.TCPAddr{IP: net.ParseIP("127.0.0.3"), Port: port}).String()
	holder, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	f.set("ts", nil, "127.0.0.3")
	time.Sleep(50 * time.Millisecond)
	holder.Close()
	waitUntil(t, "ts answers", func() bool { return answers("127.0.0.3", port) })
}

// Stopping the server has to free every address.
func TestStoppingFreesTheAddresses(t *testing.T) {
	f := &fakeInterfaces{}
	f.set("lan", nil, "127.0.0.2")
	f.set("ts", nil, "127.0.0.3")
	port, stop := followAndServe(t, f, "lan", "ts")
	waitUntil(t, "both answer", func() bool { return answers("127.0.0.2", port) && answers("127.0.0.3", port) })
	stop()
	for _, ip := range []string{"127.0.0.2", "127.0.0.3"} {
		again, err := net.Listen("tcp", (&net.TCPAddr{IP: net.ParseIP(ip), Port: port}).String())
		if err != nil {
			t.Fatalf("%s wasn't freed: %v", ip, err)
		}
		again.Close()
	}
}

func TestUsableIPs(t *testing.T) {
	var addrs []net.Addr
	for _, cidr := range []string{"100.64.0.7/32", "fd7a:115c:a1e0::7/128", "fe80::7/64", "127.0.0.1/8"} {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, &net.IPNet{IP: ip, Mask: ipnet.Mask})
	}
	got := usableIPs(addrs)
	if len(got) != 2 || got[0].String() != "100.64.0.7" || got[1].String() != "fd7a:115c:a1e0::7" {
		t.Fatalf("got %v", got)
	}
}

func TestInterfaceIPsOfAMissingInterface(t *testing.T) {
	if _, err := InterfaceIPs("no-such-interface0"); err == nil {
		t.Fatal("expected an error")
	}
}
