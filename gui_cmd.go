package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"tramline/internal/gui"
)

// openBrowser opens an address in the browser, $BROWSER if it's set. Some browsers stay
// in the foreground of what starts them, so a launcher still going after webOpenWait is
// taken to have worked, and it carries on after this returns.
func openBrowser(address string) error {
	launcher := os.Getenv("BROWSER")
	if launcher == "" {
		launcher = "xdg-open"
		if runtime.GOOS == "darwin" {
			launcher = "open"
		}
	}
	opener := exec.Command(launcher, address)
	if err := opener.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- opener.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(webOpenWait):
	}
	return nil
}

// stdoutIsTerminal says whether a person is reading the output. The GUI's sign-in address
// is only shown to one, as it lets a browser in.
func stdoutIsTerminal() bool {
	info, err := os.Stdout.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func runGUI(args []string) error {
	fs := flag.NewFlagSet("gui", flag.ContinueOnError)
	noOpen := fs.Bool("no-open", false, "don't open a browser, show the sign-in address instead, in a terminal only")
	port := fs.Int("port", 0, "listen on this port on 127.0.0.1, default is any free one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *port < 0 || *port > 65535 {
		return errors.New("usage: tramline gui [--no-open] [--port <n>]")
	}
	if *noOpen && !stdoutIsTerminal() {
		return errors.New("--no-open shows the sign-in address, which is only shown in a terminal")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	pageURL, err := c.Login().WebURL()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	defer ln.Close()
	tmp, err := gui.TempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	g, err := gui.New(gui.Options{
		Addr:    ln.Addr().String(),
		API:     c.Transport(),
		APIURL:  c.Login().URL,
		PageURL: pageURL,
		Files:   c,
		Version: version,
		TmpDir:  tmp,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(os.Stderr, "gui: ", 0),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	fmt.Printf("Tramline GUI on http://%s/, for %s. Ctrl-C stops it\n", ln.Addr(), c.Login().URL)
	switch {
	case *noOpen:
		fmt.Println("Sign in once by opening:", g.OpenURL())
	default:
		if err := openBrowser(g.OpenURL()); err != nil {
			if !stdoutIsTerminal() {
				return fmt.Errorf("couldn't open a browser: %w", err)
			}
			fmt.Println("Couldn't open a browser. Sign in once by opening:", g.OpenURL())
		}
	}

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	fmt.Println("\nStopping the GUI. Anything already sent to the Pi carries on there")
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
