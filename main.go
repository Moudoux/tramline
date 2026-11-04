// Tramline is a small print server for a Marlin printer. The same binary is the
// server, which runs next to the printer, and the client, which runs anywhere.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tramline/internal/client"
	"tramline/internal/config"
	"tramline/internal/hue"
	"tramline/internal/marlin"
	"tramline/internal/secure"
	"tramline/internal/serial"
	"tramline/internal/server"
	"tramline/internal/sim"
	"tramline/internal/tram"
)

var version = "dev"

const usage = `Tramline %s

Server, on the Pi:
  tramline init --ip <address> [--data <dir>]    create the server's certificate, once
  tramline fingerprint [--data <dir>]            show the server's public fingerprint
  tramline allow <fingerprint> [--name <name>]   allow a client, no restart needed
  tramline revoke <fingerprint>                  remove a client
  tramline clients                               list allowed clients
  tramline serve [--config <file>] [--sim]       run the server

Client:
  tramline keygen                                create this client's key, once
  tramline login <url> <server-fingerprint>      save and check the connection
  tramline status [--json]                       show the printer's status
  tramline power on|off                          switch the printer's plug
  tramline preheat [bed] [nozzle]                start heating, default bed 80 C, nozzle 150 C
  tramline cool                                  turn the heaters off
  tramline tram --bed-clear [--inset <mm>] [--repeat <n>] [--json]
                                                 probe the corners, exit 3 if out of tram
  tramline tram info                             ask the firmware about probing, no movement
  tramline upload <file> [--sd|--pi-only] [--no-wait] [--window <bytes>]
                                                 save a file on the Pi, then copy it to
                                                 the SD card if that's the default mode
  tramline copy <name> [--no-wait] [--window <bytes>]
                                                 copy a file on the Pi to the SD card,
                                                 also how to retry a failed copy
  tramline copy --cancel                         stop the running copy
  tramline files [--json]                        list files on the Pi and the SD card
  tramline files rm <name>                       delete a file, and its copy on the card
  tramline mode [auto [--limit <minutes>]|sd|stream]
                                                 show or change what upload does by default:
                                                 auto copies to SD unless that would take
                                                 longer than the limit, default 5 minutes
  tramline print <name> --bed-clear              heat, check tram, then print a file: from the
                                                 card if it has a verified copy, otherwise
                                                 line by line from the Pi. Don't restart the
                                                 server during that, as it can't carry on
  tramline print --continue                      say the knobs are adjusted, so it checks again
  tramline cancel                                cancel the print: heaters off. From the card
                                                 the head goes home, from the Pi it lifts 5 mm
  tramline estop                                 emergency stop: M112, then cut the plug
  tramline hue pair                              pair the server with the Hue Bridge
  tramline hue plugs                             list names and UUIDs on the bridge

  tramline version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "init":
		err = runInit(args)
	case "fingerprint":
		err = runFingerprint(args)
	case "allow":
		err = runAllow(args)
	case "revoke":
		err = runRevoke(args)
	case "clients":
		err = runClients(args)
	case "serve":
		err = runServe(args)
	case "keygen":
		err = runKeyGen(args)
	case "login":
		err = runLogin(args)
	case "status":
		err = runStatus(args)
	case "power":
		err = runPower(args)
	case "preheat":
		err = runPreheat(args)
	case "cool":
		err = runCool(args)
	case "tram":
		err = runTram(args)
	case "upload":
		err = runUpload(args)
	case "copy":
		err = runCopy(args)
	case "files":
		err = runFiles(args)
	case "mode":
		err = runMode(args)
	case "print":
		err = runPrint(args)
	case "cancel":
		err = runCancel(args)
	case "estop":
		err = runEstop(args)
	case "hue":
		err = runHue(args)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	if errors.Is(err, errOutOfTram) {
		os.Exit(3)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	ipFlag := fs.String("ip", "", "address the server will listen on")
	data := fs.String("data", config.DefaultDataDir, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ip := net.ParseIP(*ipFlag)
	if ip == nil {
		return errors.New("--ip must be an IP address")
	}
	fingerprint, err := secure.Init(*data, ip)
	if err != nil {
		return err
	}
	fmt.Println("Server fingerprint:", fingerprint)
	return nil
}

func runFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ContinueOnError)
	data := fs.String("data", config.DefaultDataDir, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	fingerprint, err := secure.ServerFingerprint(*data)
	if err != nil {
		return err
	}
	fmt.Println(fingerprint)
	return nil
}

// oneFingerprint parses a command taking one fingerprint, with flags on either side.
func oneFingerprint(name string, args []string, extra func(*flag.FlagSet)) (data, fingerprint string, err error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dataFlag := fs.String("data", config.DefaultDataDir, "data directory")
	if extra != nil {
		extra(fs)
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		fingerprint, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fingerprint == "" && fs.NArg() == 1 {
		fingerprint = fs.Arg(0)
	}
	if fingerprint == "" {
		return "", "", fmt.Errorf("usage: tramline %s <fingerprint>", name)
	}
	return *dataFlag, fingerprint, nil
}

func runAllow(args []string) error {
	var name *string
	data, fingerprint, err := oneFingerprint("allow", args, func(fs *flag.FlagSet) {
		name = fs.String("name", "", "a label for this client")
	})
	if err != nil {
		return err
	}
	if err := secure.Allow(data, fingerprint, *name); err != nil {
		return err
	}
	fmt.Println("Allowed")
	return nil
}

func runRevoke(args []string) error {
	data, fingerprint, err := oneFingerprint("revoke", args, nil)
	if err != nil {
		return err
	}
	if err := secure.Revoke(data, fingerprint); err != nil {
		return err
	}
	fmt.Println("Revoked")
	return nil
}

func runClients(args []string) error {
	fs := flag.NewFlagSet("clients", flag.ContinueOnError)
	data := fs.String("data", config.DefaultDataDir, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	clients, err := secure.Clients(*data)
	if err != nil {
		return err
	}
	for _, c := range clients {
		fmt.Println(c.Fingerprint, c.Name)
	}
	return nil
}

func runKeyGen(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tramline keygen")
	}
	dir, err := client.Dir()
	if err != nil {
		return err
	}
	fingerprint, err := secure.KeyGen(dir)
	if err != nil {
		return err
	}
	fmt.Println("Client fingerprint:", fingerprint)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath, "config file")
	useSim := fs.Bool("sim", false, "use a simulated printer instead of the serial port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	tlsCfg, err := secure.ServerTLS(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("%w (has tramline init been run?)", err)
	}

	open := func() (io.ReadWriteCloser, error) { return serial.Open(cfg.Serial.Device, cfg.Serial.Baud) }
	simPlug := &sim.Plug{Options: sim.Options{
		Garbage:   true,
		MoveDelay: 500 * time.Millisecond,
		PrintTime: 2 * time.Minute,
		Corners:   sim.Corners{FL: 0.10, FR: 0.13, BR: 0.21, BL: 0.09},
	}}
	if *useSim {
		open = simPlug.Open
	}
	printer := marlin.NewPrinter(open)
	var s *server.Server
	printer.OnLine = func(line string) {
		if s != nil {
			s.PrinterLine(line)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Printf("tramline %s listening on https://%s", version, cfg.Listen)
	s = &server.Server{
		Version:   version,
		Simulated: *useSim,
		Printer:   printer.Status,
		Heaters:   printer,
		Prober:    printer,
		Card:      printer,
		Prints:    printer,
		TramSoak:  time.Duration(cfg.TramSoakSeconds) * time.Second,
		DataDir:   cfg.DataDir,
		Allowed:   func(fp string) bool { return secure.Allowed(cfg.DataDir, fp) },
	}
	// A simulated printer must never switch the real plug.
	if *useSim {
		s.Plug = simPlug
	} else if cfg.Hue.Address != "" {
		s.Plug = &hue.Client{
			Address:  cfg.Hue.Address,
			BridgeID: cfg.Hue.BridgeID,
			PlugID:   cfg.Hue.PlugLightID,
			DataDir:  cfg.DataDir,
		}
	}
	go printer.Run(ctx)
	return s.Serve(ctx, ln, tlsCfg)
}

func runLogin(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: tramline login <url> <server-fingerprint>")
	}
	dir, err := client.Dir()
	if err != nil {
		return err
	}
	cert, err := secure.LoadClientCert(dir)
	if err != nil {
		return fmt.Errorf("%w (has tramline keygen been run?)", err)
	}
	login := client.Login{URL: args[0], Fingerprint: strings.TrimSpace(args[1])}
	c, err := client.New(login, cert)
	if err != nil {
		return err
	}
	// Saved only once both sides have accepted each other's certificates.
	if _, err := c.Status(); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	if err := client.Save(dir, login); err != nil {
		return err
	}
	fmt.Println("Logged in")
	return nil
}

func loggedIn() (*client.Client, error) {
	dir, err := client.Dir()
	if err != nil {
		return nil, err
	}
	login, err := client.Load(dir)
	if err != nil {
		return nil, err
	}
	cert, err := secure.LoadClientCert(dir)
	if err != nil {
		return nil, err
	}
	return client.New(login, cert)
}

func runPower(args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: tramline power on|off")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	if err := c.Power(args[0] == "on"); err != nil {
		return err
	}
	fmt.Println("Plug is", args[0])
	return nil
}

const (
	defaultBed    = 80
	defaultNozzle = 150
)

// runPreheat only parses the request. The limits are enforced by the server.
func runPreheat(args []string) error {
	const use = "usage: tramline preheat [bed] [nozzle]"
	if len(args) > 2 {
		return errors.New(use)
	}
	temps := []int{defaultBed, defaultNozzle}
	for i, arg := range args {
		v, err := strconv.Atoi(arg)
		if err != nil {
			return errors.New(use)
		}
		temps[i] = v
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	out, err := c.Preheat(temps[0], temps[1])
	if err != nil {
		return err
	}
	fmt.Printf("Heating: bed %d C, nozzle %d C\n", out.Bed, out.Nozzle)
	fmt.Printf("Heaters turn off at %s unless preheat is run again\n", out.HeatersOffAt.Local().Format("15:04:05"))
	return nil
}

func runCool(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tramline cool")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	out, err := c.Cool()
	if err != nil {
		return err
	}
	if out.Cancelling {
		fmt.Println("A print was running, so it's being cancelled, which turns the heaters off. tramline status follows it")
		return nil
	}
	fmt.Println("Heaters are off")
	return nil
}

// errOutOfTram means the run worked, and its answer is that the bed needs adjusting.
var errOutOfTram = errors.New("out of tram")

// tramContactLimit is how long the client keeps trying to reach the server.
const tramContactLimit = time.Minute

func runTram(args []string) error {
	if len(args) == 1 && args[0] == "info" {
		c, err := loggedIn()
		if err != nil {
			return err
		}
		lines, err := c.TramInfo()
		if err != nil {
			return err
		}
		for _, line := range lines {
			fmt.Println(line)
		}
		return nil
	}
	fs := flag.NewFlagSet("tram", flag.ContinueOnError)
	bedClear := fs.Bool("bed-clear", false, "confirm nothing is on the bed")
	inset := fs.Int("inset", 0, "probe this far in from each edge, default is the server's")
	repeat := fs.Int("repeat", 1, "rounds of probing, to measure repeatability")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: tramline tram --bed-clear [--inset <mm>] [--repeat <n>] [--json]")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	req := server.TramRequest{BedClear: *bedClear, Repeat: repeat}
	if *inset != 0 {
		req.Inset = inset
	}
	job, err := c.Tram(req)
	if err != nil {
		return err
	}
	final, err := followTram(c, job.ID, *asJSON)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(final); err != nil {
			return err
		}
	} else {
		printTramResult(os.Stdout, final)
	}
	switch {
	case final.State == server.TramFailed:
		return errors.New(final.Error)
	case final.Result != nil && !final.Result.Pass:
		return errOutOfTram
	}
	return nil
}

// followTram shows progress until the run ends. Leaving doesn't stop the run, as
// the server finishes it either way.
func followTram(c *client.Client, id int, quiet bool) (*server.TramJob, error) {
	step, printed := "", 0
	contact := time.Now()
	for {
		st, err := c.Status()
		if err != nil {
			if time.Since(contact) > tramContactLimit {
				return nil, fmt.Errorf("lost contact with the server, the run may still be going, tramline status shows it: %w", err)
			}
			time.Sleep(time.Second)
			continue
		}
		contact = time.Now()
		job := st.Tram
		if job == nil || job.ID != id {
			return nil, errors.New("the run is gone, the server may have restarted")
		}
		if !quiet {
			var readings []tram.Reading
			for _, round := range job.Rounds {
				readings = append(readings, round.Readings...)
			}
			for _, r := range readings[printed:] {
				fmt.Printf("  %-12s X%-7.2f Y%-7.2f Z%+.3f\n", r.Corner, r.X, r.Y, r.Z)
			}
			printed = len(readings)
			if job.Step != "" && job.Step != step && !strings.HasPrefix(job.Step, "probing") {
				fmt.Println(job.Step)
			}
			step = job.Step
		}
		if job.State != server.TramRunning {
			return job, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func printTramResult(w io.Writer, job *server.TramJob) {
	if job.Repeat > 1 {
		for i, round := range job.Rounds {
			if round.Result != nil {
				fmt.Fprintf(w, "Round %d: range %.3f mm, %s\n", i+1, round.Result.Range, passWord(round.Result.Pass))
			}
		}
	}
	if job.Spread != nil {
		for c, spread := range job.Spread {
			fmt.Fprintf(w, "Repeatability, %-12s %.3f mm\n", tram.Corner(c), spread)
		}
	}
	if r := job.Result; r != nil {
		for c, d := range r.Relative {
			fmt.Fprintf(w, "%-12s %+.3f mm from average\n", tram.Corner(c), d)
		}
		fmt.Fprintf(w, "Range %.3f mm, limit %.2f mm: %s\n", r.Range, tram.Tolerance, passWord(r.Pass))
		if !r.Pass {
			fmt.Fprintf(w, "Adjust: %s the %s corner\n", r.Action, r.Corner)
		}
	}
	if job.Parked {
		fmt.Fprintln(w, "Head parked")
		if job.Temps.BedTarget > 0 || job.Temps.NozzleTarget > 0 || job.Temps.Bed >= tramWarmBed {
			fmt.Fprintln(w, "Careful: the bed and nozzle may be hot")
		}
	} else {
		fmt.Fprintln(w, "The head was NOT parked")
	}
}

// tramWarmBed is the bed temperature worth warning about before reaching under it.
const tramWarmBed = 40

func passWord(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}

func runHue(args []string) error {
	if len(args) != 1 || (args[0] != "pair" && args[0] != "plugs") {
		return errors.New("usage: tramline hue pair|plugs")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	if args[0] == "pair" {
		if err := c.HuePair(); err != nil {
			return err
		}
		fmt.Println("Paired with the Hue Bridge. The key stays on the server.")
		return nil
	}
	lights, err := c.HuePlugs()
	if err != nil {
		return err
	}
	for _, l := range lights {
		fmt.Printf("%s  %-12s %-5t %s\n", l.ID, l.Archetype, l.On, l.Name)
	}
	return nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	st, err := c.Status()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(os.Stdout, st)
	printTramStatus(os.Stdout, st.Tram)
	printCopyStatus(os.Stdout, st.Copy)
	printPrintStatus(os.Stdout, st.Print)
	return nil
}

func printStatus(w io.Writer, st server.StatusResponse) {
	fmt.Fprintf(w, "Server:   %s\n", st.Version)
	if p := st.Plug; p != nil {
		switch {
		case p.Error != "":
			fmt.Fprintf(w, "Plug:     unknown (%s)\n", p.Error)
		case p.On:
			fmt.Fprintf(w, "Plug:     on (%s)\n", p.Name)
		default:
			fmt.Fprintf(w, "Plug:     off (%s)\n", p.Name)
		}
	}
	if st.Simulated {
		fmt.Fprintln(w, "Mode:     SIMULATED PRINTER")
	}
	if st.HeatProblem != "" {
		fmt.Fprintf(w, "WARNING:  %s\n", st.HeatProblem)
	}
	if !st.Printer.Connected {
		fmt.Fprintln(w, "Printer:  not connected")
		return
	}
	fmt.Fprintf(w, "Printer:  %s\n", st.Printer.Firmware)
	if t := st.Printer.Temps; t != nil {
		fmt.Fprintf(w, "Nozzle:   %.1f / %.0f C\nBed:      %.1f / %.0f C\n", t.Nozzle, t.NozzleTarget, t.Bed, t.BedTarget)
	}
	if st.HeatersOffAt != nil {
		fmt.Fprintf(w, "Heaters:  off at %s unless preheat is run again\n", st.HeatersOffAt.Local().Format("15:04:05"))
	}
}

func printTramStatus(w io.Writer, job *server.TramJob) {
	switch {
	case job == nil:
	case job.State == server.TramRunning:
		fmt.Fprintf(w, "Tram:     running, %s\n", job.Step)
	case job.State == server.TramFailed:
		fmt.Fprintf(w, "Tram:     last run failed: %s\n", job.Error)
	case job.Result != nil:
		fmt.Fprintf(w, "Tram:     last run %s, range %.3f mm\n", passWord(job.Result.Pass), job.Result.Range)
	}
}

func printCopyStatus(w io.Writer, job *server.CopyJob) {
	switch {
	case job == nil:
	case job.State == server.CopyRunning:
		fmt.Fprintf(w, "Copy:     %s to SD, %d of %d lines\n", job.Name, job.Lines, job.TotalLines)
	case job.State == server.CopyFailed:
		fmt.Fprintf(w, "Copy:     %s failed: %s\n", job.Name, job.Error)
	default:
		fmt.Fprintf(w, "Copy:     %s is on the SD card as %s, size checked\n", job.Name, job.SDName)
	}
}

func printPrintStatus(w io.Writer, job *server.PrintJob) {
	if job == nil {
		return
	}
	var percent float64
	if job.Size > 0 {
		percent = 100 * float64(job.Pos) / float64(job.Size)
	}
	switch job.State {
	case server.PrintHeating:
		fmt.Fprintf(w, "Print:    %s, heating to bed %d C and nozzle %d C for its tram check\n", job.Name, server.TramBed, server.TramNozzle)
	case server.PrintSoaking:
		fmt.Fprintf(w, "Print:    %s, at temperature, soaking before its tram check", job.Name)
		if job.SoakUntil != nil {
			fmt.Fprintf(w, " at %s", job.SoakUntil.Local().Format("15:04:05"))
		}
		fmt.Fprintln(w)
	case server.PrintChecking:
		fmt.Fprintf(w, "Print:    %s, checking tram\n", job.Name)
	case server.PrintAdjusting:
		fmt.Fprintf(w, "Print:    %s is waiting for you\n", job.Name)
		if c := job.Check; c != nil && !c.Pass {
			fmt.Fprintf(w, "          Last check: %s %s, turn its knob %s seen from above. Range %.3f mm, it must be under %.2f mm\n",
				c.Action, c.Corner, tram.Turn(c.Action), c.Range, tram.Tolerance)
		}
		if job.Parked {
			fmt.Fprintln(w, "          The head is parked. The bed may be hot")
		} else {
			fmt.Fprintln(w, "          The head isn't parked, and may be low. The bed may be hot")
		}
		fmt.Fprintln(w, "          When you're done: tramline print --continue, or tramline cancel")
	case server.PrintStarting:
		fmt.Fprintf(w, "Print:    %s, tram passed, starting\n", job.Name)
	case server.PrintRunning:
		fmt.Fprintf(w, "Print:    %s, %.0f%%, %d of %d bytes", job.Name, percent, job.Pos, job.Size)
		if job.Mode == server.ModeStream {
			fmt.Fprintf(w, " sent, %d resends, %d nudges", job.Resends, job.Nudges)
		}
		fmt.Fprintln(w)
	case server.PrintCancelling:
		fmt.Fprintf(w, "Print:    %s is being cancelled\n", job.Name)
	case server.PrintFailed:
		fmt.Fprintf(w, "Print:    %s failed: %s\n", job.Name, job.Error)
	default:
		fmt.Fprintf(w, "Print:    %s %s\n", job.Name, job.State)
	}
	if job.Stopping {
		fmt.Fprintln(w, "          being cancelled")
	}
	if job.Note != "" {
		fmt.Fprintf(w, "          %s\n", job.Note)
	}
}

func runPrint(args []string) error {
	fs := flag.NewFlagSet("print", flag.ContinueOnError)
	bedClear := fs.Bool("bed-clear", false, "confirm nothing is on the bed")
	again := fs.Bool("continue", false, "the knobs are adjusted, check tram again")
	if len(args) == 1 && args[0] == "--continue" {
		c, err := loggedIn()
		if err != nil {
			return err
		}
		if _, err := c.ContinuePrint(); err != nil {
			return err
		}
		fmt.Println("Checking tram again, the head will move. tramline status follows it")
		return nil
	}
	path, err := flagsAfter(fs, args)
	if err != nil || *again {
		return errors.New("usage: tramline print <name> --bed-clear, or tramline print --continue")
	}
	if !*bedClear {
		return errors.New("refusing to print: check the bed is clear, then add --bed-clear")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	job, err := c.Print(filepath.Base(path), true)
	if err != nil {
		return err
	}
	if job.Mode == server.ModeStream {
		fmt.Printf("Heating for the tram check, and %s streams from the Pi once that passes. It carries on without this client, but not without the server: tramline status follows it\n", job.Name)
		return nil
	}
	fmt.Printf("Heating for the tram check, and %s starts from the SD card as %s once that passes. It carries on without this client: tramline status follows it\n", job.Name, job.SDName)
	return nil
}

func runCancel(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tramline cancel")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	if err := c.CancelPrint(); err != nil {
		return err
	}
	fmt.Println("Cancelling: heaters off, and the head goes home, or lifts 5 mm if it's streaming. tramline status confirms it")
	return nil
}

func runEstop(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: tramline estop")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	out, err := c.Estop()
	if err != nil {
		return fmt.Errorf("the server didn't answer, use the printer's own switch: %w", err)
	}
	fmt.Printf("M112 sent: %t\nPlug off:  %t\n", out.M112, out.PlugOff)
	for _, problem := range out.Problems {
		fmt.Println("Problem:  ", problem)
	}
	if !out.PlugOff {
		return errors.New("the plug isn't confirmed off, use the printer's own switch")
	}
	return nil
}

// flagsAfter parses a command taking one name, with flags on either side.
func flagsAfter(fs *flag.FlagSet, args []string) (string, error) {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if name == "" && fs.NArg() == 1 {
		return fs.Arg(0), nil
	}
	if fs.NArg() != 0 {
		return "", errors.New("too many arguments")
	}
	return name, nil
}

func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// runUpload saves the file on the Pi unless it's already there, so a retry doesn't
// send it again. After that, this machine can go away and nothing is lost.
func runUpload(args []string) error {
	const use = "usage: tramline upload <file> [--sd|--pi-only] [--no-wait] [--window <bytes>]"
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	toSD := fs.Bool("sd", false, "copy to the SD card, whatever the default mode")
	piOnly := fs.Bool("pi-only", false, "only save on the Pi, whatever the default mode")
	noWait := fs.Bool("no-wait", false, "don't wait for the copy, the server finishes it either way")
	window := fs.Int("window", 0, "bytes that may be unanswered, for measuring: default is the server's, 1 is one line at a time")
	path, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if path == "" || (*toSD && *piOnly) {
		return errors.New(use)
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	hash, size, err := fileHash(path)
	if err != nil {
		return err
	}
	rec, err := c.File(name)
	switch {
	case err == nil && rec.SHA256 == hash:
		fmt.Printf("%s is already on the Pi, not sending it again\n", name)
	case err == nil || errors.Is(err, client.ErrNotFound):
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		start := time.Now()
		rec, err = c.PutFile(name, f, size, hash)
		if err != nil {
			return err
		}
		fmt.Printf("Saved on the Pi: %s, %d bytes, in %s\n", name, size, time.Since(start).Round(10*time.Millisecond))
	default:
		return err
	}
	fmt.Printf("%d commands, %d bytes on the card without comments\n", rec.Info.Lines, rec.Info.SDSize)

	estimate := server.CopyEstimate(rec.Info).Round(time.Second)
	switch {
	case *toSD:
	case *piOnly:
		fmt.Println("Kept on the Pi, not copied to the SD card")
		return nil
	default:
		settings, err := c.Settings()
		if err != nil {
			return fmt.Errorf("it's saved on the Pi, but the default mode can't be read: %w", err)
		}
		if !settings.CopiesToSD(rec.Info) {
			if settings.Mode == server.ModeAuto {
				fmt.Printf("Copying would take about %s, over the %d minute limit\n", estimate, settings.AutoLimit)
			}
			fmt.Println("Kept on the Pi for streaming. To copy it anyway: tramline copy", name)
			return nil
		}
	}
	fmt.Printf("Copying should take about %s\n", estimate)
	if err := startCopy(c, name, *window, *noWait); err != nil {
		return fmt.Errorf("%w\nThe file is safe on the Pi, to retry run: tramline copy %s", err, name)
	}
	return nil
}

func runCopy(args []string) error {
	const use = "usage: tramline copy <name> [--no-wait] [--window <bytes>], or tramline copy --cancel"
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	noWait := fs.Bool("no-wait", false, "don't wait for the copy, the server finishes it either way")
	window := fs.Int("window", 0, "bytes that may be unanswered, for measuring: default is the server's, 1 is one line at a time")
	cancel := fs.Bool("cancel", false, "stop the running copy")
	name, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if *cancel == (name != "") {
		return errors.New(use)
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	if *cancel {
		if err := c.CancelCopy(); err != nil {
			return err
		}
		fmt.Println("Stopping the copy")
		return nil
	}
	return startCopy(c, name, *window, *noWait)
}

func startCopy(c *client.Client, name string, window int, noWait bool) error {
	out, err := c.Copy(name, window)
	if err != nil {
		return err
	}
	if out.Job == nil {
		fmt.Printf("Already on the SD card as %s, and the size matches\n", out.File.SDName)
		return nil
	}
	fmt.Printf("Copying to the SD card as %s\n", out.File.SDName)
	if noWait {
		fmt.Println("The server carries on with it, tramline status shows how it's going")
		return nil
	}
	return followCopy(c, out.Job.ID)
}

// followCopy shows progress until the copy ends. Leaving doesn't stop the copy.
func followCopy(c *client.Client, id int) error {
	const every = 10 * time.Second
	contact, shown := time.Now(), time.Now()
	for {
		st, err := c.Status()
		if err != nil {
			if time.Since(contact) > tramContactLimit {
				return fmt.Errorf("lost contact with the server, the copy may still be going, tramline status shows it: %w", err)
			}
			time.Sleep(time.Second)
			continue
		}
		contact = time.Now()
		job := st.Copy
		if job == nil || job.ID != id {
			return errors.New("the copy is gone, the server may have restarted")
		}
		switch job.State {
		case server.CopyRunning:
			if time.Since(shown) >= every {
				fmt.Printf("  %d of %d lines\n", job.Lines, job.TotalLines)
				shown = time.Now()
			}
			time.Sleep(500 * time.Millisecond)
			continue
		case server.CopyFailed:
			return errors.New(job.Error)
		}
		elapsed := job.FinishedAt.Sub(job.StartedAt).Seconds()
		fmt.Printf("Copied: %d lines, %d bytes in %.1f s, %.1f KB/s, size checked with M20\n",
			job.TotalLines, job.TotalBytes, elapsed, float64(job.TotalBytes)/1000/elapsed)
		fmt.Printf("%d resends, %d timeouts, window %d bytes\n", job.Resends, job.Timeouts, job.Window)
		return nil
	}
}

func runFiles(args []string) error {
	if len(args) == 2 && args[0] == "rm" {
		c, err := loggedIn()
		if err != nil {
			return err
		}
		if err := c.DeleteFile(args[1]); err != nil {
			return err
		}
		fmt.Println("Deleted", args[1])
		return nil
	}
	fs := flag.NewFlagSet("files", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: tramline files [--json], or tramline files rm <name>")
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	out, err := c.Files()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printFiles(os.Stdout, out)
	return nil
}

func printFiles(w io.Writer, out server.FilesResponse) {
	fmt.Fprintf(w, "Default mode: %s\n", out.Mode)
	fmt.Fprintln(w, "On the Pi:")
	if len(out.Files) == 0 {
		fmt.Fprintln(w, "  nothing")
	}
	ours := map[string]bool{}
	for _, rec := range out.Files {
		ours[strings.ToUpper(rec.SDName)] = true
		state := "not on the SD card"
		switch rec.SDState {
		case server.CopyDone:
			state = "on the SD card as " + rec.SDName
		case server.CopyRunning:
			state = "copying to the SD card as " + rec.SDName
		case server.CopyFailed:
			state = "COPY FAILED: " + rec.SDError
		}
		fmt.Fprintf(w, "  %-40s %10d bytes  %s\n", rec.Name, rec.Size, state)
	}
	if out.Card == nil {
		fmt.Fprintf(w, "On the SD card: can't say, %s\n", out.CardNote)
		return
	}
	fmt.Fprintln(w, "On the SD card:")
	if len(out.Card) == 0 {
		fmt.Fprintln(w, "  nothing")
	}
	for _, f := range out.Card {
		origin := "not from here"
		if ours[strings.ToUpper(f.Name)] {
			origin = "from here"
		}
		fmt.Fprintf(w, "  %-40s %10d bytes  %s\n", f.Name, f.Size, origin)
	}
}

func runMode(args []string) error {
	const use = "usage: tramline mode [auto [--limit <minutes>]|sd|stream]"
	fs := flag.NewFlagSet("mode", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "the longest copy auto will wait for, in minutes")
	mode, err := flagsAfter(fs, args)
	if err != nil {
		return err
	}
	if (mode != "" && mode != server.ModeAuto && mode != server.ModeSD && mode != server.ModeStream) ||
		(*limit != 0 && mode != server.ModeAuto) {
		return errors.New(use)
	}
	c, err := loggedIn()
	if err != nil {
		return err
	}
	if mode != "" {
		if err := c.SetMode(mode, *limit); err != nil {
			return err
		}
	}
	settings, err := c.Settings()
	if err != nil {
		return err
	}
	fmt.Println("Default mode:", settings.Mode)
	switch settings.Mode {
	case server.ModeAuto:
		fmt.Printf("upload copies a file to SD unless that would take over %d minutes, then keeps it on the Pi.\n", settings.AutoLimit)
		fmt.Println("print streams a file that has no verified copy on the card.")
	case server.ModeStream:
		fmt.Println("upload keeps files on the Pi, and print streams them.")
	}
	return nil
}
