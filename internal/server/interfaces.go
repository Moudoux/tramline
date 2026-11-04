package server

import (
	"errors"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// DefaultInterfaceCheck is how often an interface's addresses are looked at.
const DefaultInterfaceCheck = 30 * time.Second

// InterfaceIPs gives the addresses of a network interface that can be listened on.
func InterfaceIPs(name string) ([]net.IP, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	return usableIPs(addrs), nil
}

// usableIPs leaves out link-local addresses, which need a zone to be listened on and
// aren't what anyone connects to.
func usableIPs(addrs []net.Addr) []net.IP {
	var ips []net.IP
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.IsGlobalUnicast() {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips
}

// InterfaceListener is a net.Listener on whatever addresses some network interfaces
// have, so the config needn't name an address that DHCP or Tailscale could change. An
// interface may not exist yet, as Tailscale can come up after Tramline, so it keeps
// looking, and a problem with one leaves the others alone. It never fails to start: a
// problem is logged when it changes, so it can't fill the Pi's log.
type InterfaceListener struct {
	port   string
	lookup func(name string) ([]net.IP, error)
	check  time.Duration
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// ListenInterfaces starts following the interfaces. Pass InterfaceIPs and
// DefaultInterfaceCheck, unless it's a test.
func ListenInterfaces(names []string, port int, lookup func(string) ([]net.IP, error), check time.Duration) *InterfaceListener {
	l := &InterfaceListener{
		port:   strconv.Itoa(port),
		lookup: lookup,
		check:  check,
		conns:  make(chan net.Conn),
		done:   make(chan struct{}),
	}
	for _, name := range names {
		l.wg.Go(func() { l.follow(name) })
	}
	return l
}

func (l *InterfaceListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops listening on every address, and they're free once it returns.
func (l *InterfaceListener) Close() error {
	l.once.Do(func() { close(l.done) })
	l.wg.Wait()
	return nil
}

// Addr has only the port, as the addresses come and go.
func (l *InterfaceListener) Addr() net.Addr {
	port, _ := strconv.Atoi(l.port)
	return &net.TCPAddr{Port: port}
}

// oneAddress is one listening address of a followed interface.
type oneAddress struct {
	ln    net.Listener
	ended chan struct{}
}

func (l *InterfaceListener) follow(name string) {
	open := map[string]*oneAddress{}
	defer func() {
		for _, a := range open {
			a.ln.Close()
			<-a.ended
		}
	}()
	lastProblem := ""
	for {
		problem := l.update(name, open)
		if problem != lastProblem && problem != "" {
			log.Printf("%s: %s, checking again every %s", name, problem, l.check)
		}
		lastProblem = problem
		select {
		case <-l.done:
			return
		case <-time.After(l.check):
		}
	}
}

// update makes what's open match the interface, and says what got in the way.
func (l *InterfaceListener) update(name string, open map[string]*oneAddress) string {
	ips, err := l.lookup(name)
	if err != nil {
		// What's open stays open: if the same address comes back, it still works.
		return err.Error()
	}
	want := map[string]bool{}
	for _, ip := range ips {
		want[net.JoinHostPort(ip.String(), l.port)] = true
	}
	for addr, a := range open {
		select {
		case <-a.ended:
		default:
			if want[addr] {
				continue
			}
			log.Printf("%s: no longer listening on %s", name, addr)
			a.ln.Close()
			<-a.ended
		}
		delete(open, addr)
	}
	var problems []error
	for addr := range want {
		if open[addr] != nil {
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		log.Printf("%s: listening on %s", name, addr)
		a := &oneAddress{ln: ln, ended: make(chan struct{})}
		open[addr] = a
		go l.accept(a)
	}
	if err := errors.Join(problems...); err != nil {
		return err.Error()
	}
	return ""
}

func (l *InterfaceListener) accept(a *oneAddress) {
	defer close(a.ended)
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			return
		}
		select {
		case l.conns <- conn:
		case <-l.done:
			conn.Close()
		}
	}
}
