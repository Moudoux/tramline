package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"tramline/internal/notify"
)

const notifyUse = `usage: tramline notify [--json], tramline notify set [--url <url>] [--link <url>] [--events <list>|all|none], tramline notify token [--clear], tramline notify on|off, or tramline notify test`

// runNotify manages the notifications the Pi sends to Switchboard. The token is read from
// stdin, never the command line, where ps and shell history would keep it.
func runNotify(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return runNotifyShow(args)
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "set":
		return runNotifySet(args)
	case "token":
		return runNotifyToken(args)
	case "on", "off":
		if len(args) != 0 {
			return errors.New(notifyUse)
		}
		on := sub == "on"
		return changeNotify(notify.Update{Enabled: &on})
	case "test":
		if len(args) != 0 {
			return errors.New(notifyUse)
		}
		c, err := loggedIn()
		if err != nil {
			return err
		}
		id, err := c.TestNotify()
		if err != nil {
			return err
		}
		fmt.Printf("Sent a test notification, Switchboard's id %s. Switchboard's own page shows where it went\n", id)
		return nil
	}
	return errors.New(notifyUse)
}

func runNotifyShow(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(notifyUse)
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	v, err := c.Notify()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	printNotify(os.Stdout, v)
	return nil
}

func runNotifySet(args []string) error {
	fs := flag.NewFlagSet("notify set", flag.ContinueOnError)
	address := fs.String("url", "", "the Switchboard server, such as https://notify.example.lan, or \"\" to remove it")
	link := fs.String("link", "", "a link to put in each notification, such as the read-only page, or \"\" for none")
	events := fs.String("events", "", "comma-separated events to send, all, or none")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || fs.NFlag() == 0 {
		return errors.New(notifyUse)
	}
	var u notify.Update
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "url":
			u.URL = address
		case "link":
			u.Link = link
		case "events":
			list := eventList(*events)
			u.Events = &list
		}
	})
	return changeNotify(u)
}

// eventList reads --events. The server checks the names.
func eventList(text string) []string {
	switch strings.TrimSpace(text) {
	case "all":
		var all []string
		for _, e := range notify.Events {
			all = append(all, e.Name)
		}
		return all
	case "none", "":
		return []string{}
	}
	var out []string
	for _, name := range strings.Split(text, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func runNotifyToken(args []string) error {
	fs := flag.NewFlagSet("notify token", flag.ContinueOnError)
	clearToken := fs.Bool("clear", false, "remove the saved token, which turns notifications off")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(notifyUse)
	}
	token := ""
	if !*clearToken {
		var err error
		if token, err = readToken(); err != nil {
			return err
		}
		if err := notify.CheckToken(token); err != nil {
			return errors.New(strings.TrimPrefix(err.Error(), notify.ErrInvalid.Error()+": "))
		}
	}
	return changeNotify(notify.Update{Token: &token})
}

// readToken reads one line from stdin. On a terminal it asks for it, and typing isn't
// shown, so the token never appears on the screen.
func readToken() (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Switchboard token, which won't be shown: ")
		restore, err := hideTyping()
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return "", fmt.Errorf("couldn't hide the typing, so not asking for the token here: %w. Send it on stdin instead, such as tramline notify token < file", err)
		}
		defer restore()
	}
	line, err := bufio.NewReader(io.LimitReader(os.Stdin, notify.MaxTokenBytes+2)).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return "", errors.New("no token was given")
		}
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("no token was given")
	}
	return line, nil
}

// hideTyping turns off the terminal's echo with stty, and gives back what turns it on
// again, which also runs if the command is interrupted.
func hideTyping() (func(), error) {
	stty := func(arg string) error {
		cmd := exec.Command("stty", arg)
		cmd.Stdin = os.Stdin
		return cmd.Run()
	}
	if err := stty("-echo"); err != nil {
		return nil, err
	}
	var once sync.Once
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	restore := func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			stty("echo")
			fmt.Fprintln(os.Stderr)
		})
	}
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			restore()
			os.Exit(130)
		case <-done:
		}
	}()
	return restore, nil
}

func changeNotify(u notify.Update) error {
	c, err := loggedIn()
	if err != nil {
		return err
	}
	v, err := c.SetNotify(u)
	if err != nil {
		return err
	}
	fmt.Println("Saved on the Pi")
	printNotify(os.Stdout, v)
	return nil
}

func printNotify(w io.Writer, v notify.View) {
	state := "off"
	switch {
	case v.Ready:
		state = "on"
	case v.Enabled:
		state = "on, but they need an address and a token to send"
	case v.URL == "" || !v.TokenSet:
		state = "off. To start: tramline notify set --url <url>, tramline notify token, tramline notify test, then tramline notify on"
	default:
		state = "off, tramline notify on starts them"
	}
	fmt.Fprintf(w, "Notifications: %s\n", state)
	if v.Problem != "" {
		fmt.Fprintf(w, "Problem:       %s\n", v.Problem)
	}
	fmt.Fprintf(w, "Switchboard:   %s\n", orWord(v.URL, "not set"))
	token := "not saved"
	if v.TokenSet {
		token = "saved on the Pi"
	}
	fmt.Fprintf(w, "Token:         %s\n", token)
	fmt.Fprintf(w, "Link:          %s\n", orWord(v.Link, "none"))
	fmt.Fprintln(w, "Events:")
	for _, e := range v.Available {
		mark := " "
		if slices.Contains(v.Events, e.Name) {
			mark = "x"
		}
		fmt.Fprintf(w, "  [%s] %-16s %s. %s\n", mark, e.Name, e.Label, e.Description)
	}
	if l := v.Last; l != nil {
		outcome := "sent"
		if !l.OK {
			outcome = "NOT sent: " + l.Error
		}
		fmt.Fprintf(w, "Last:          %s, %q, %s\n", scheduleTime(l.At), l.Title, outcome)
		if l.Dropped > 0 {
			fmt.Fprintf(w, "Dropped:       %d, as too many were waiting to send\n", l.Dropped)
		}
	}
}

func orWord(s, word string) string {
	if s == "" {
		return word
	}
	return s
}
