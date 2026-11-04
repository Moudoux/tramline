// Package sim is a simulated Marlin printer for tests and for trying the server
// without hardware. Its replies are based on the real printer's output.
package sim

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Corners holds the bed height the probe reads at each corner.
type Corners struct{ FL, FR, BR, BL float64 }

// Options configures a simulated printer.
type Options struct {
	Corners Corners
	// Bed replaces Corners if set, so a test can turn the knobs.
	Bed func() Corners
	// Garbage is sent to the host on connect, like the real port does.
	Garbage bool
	// BootDelay is how long the firmware ignores input after connecting.
	BootDelay time.Duration
	// GarbageIn makes the board answer the garbage from opening the port with
	// its own "ok", before the first real command, like the real board.
	GarbageIn bool
	// Silent never answers anything, like a stuck board.
	Silent bool
	// IgnoreHeat answers heat commands without changing the targets.
	IgnoreHeat bool
	// NoHeatReply never answers heat commands.
	NoHeatReply bool
	// Printing reports whether an SD print is running. Nil means never.
	Printing func() bool
	// MoveDelay is how long G28 and G30 take, saying "busy" while they run.
	MoveDelay time.Duration
	// StallOn makes the command starting with this word take Stall to answer,
	// like a long move. Later commands wait their turn, as they do on the real board.
	StallOn string
	Stall   time.Duration
	// StallBusy keeps saying "busy" during a stall.
	StallBusy bool
	// FailProbe, DropProbe and HaltProbe count G30s from 1. On that one, probing
	// fails, the link drops, or the firmware halts.
	FailProbe, DropProbe, HaltProbe int
	// ProbeShiftX reports a different X from the one asked for.
	ProbeShiftX float64
	// Seen is told every command received.
	Seen func(string)
	// Card is the SD card and what the board remembers while it has power, so it
	// outlasts a dropped link. Nil gives each connection its own.
	Card *Card
	// These count line numbers, and each applies once. The line is lost on the way,
	// arrives corrupted, is taken but its "ok" is lost, or the link drops as it
	// arrives, or the card fails to write it.
	DropLine, CorruptLine, LoseOKLine, DropLinkLine, WriteErrorLine int
	// SilentFromLine stops the board answering anything from that line on.
	SilentFromLine int
	// LineDelay is how long the board takes over each numbered line.
	LineDelay time.Duration
	// PlainOK leaves the line number out of "ok".
	PlainOK bool
	// PrintTime is how long a print from the card takes. Zero means 200 ms.
	PrintTime time.Duration
	// PrintHold is how long the board answers nothing after M24, reporting only
	// temperatures, like the start of a print waiting for its heaters. M524 ends it.
	PrintHold time.Duration
	// HeatRate is how many degrees a second the temperatures move. Zero means 40.
	HeatRate float64
	// MoveTime is how long each G0 and G1 takes. The planner holds PlannerSlots of
	// them, and once it's full a move's "ok" waits for room, like the real board. Zero
	// means moves take no time.
	MoveTime time.Duration
	// HeatWait keeps M109 and M190 waiting at least this long, reporting only
	// temperatures, until M108 ends the wait.
	HeatWait time.Duration
	// M86 is the reply to M86. Empty means what the real board says.
	M86 string
	// KillAfter is how long M85 waits with no moves or heater waits before it kills the
	// printer, whatever its S value says. Zero never kills.
	KillAfter time.Duration
}

// PlannerSlots is how many moves the real board's planner holds, by its "ok P15".
const PlannerSlots = 15

// How fast simulated temperatures move, sped up so tests don't wait.
const degreesPerSecond = 40.0

const (
	ambientNozzle = 21.59
	ambientBed    = 22.12
)

var (
	coordRE = regexp.MustCompile(`([XY])(-?[\d.]+)`)
	heatRE  = regexp.MustCompile(`S(\d+)`)
)

type printer struct {
	opts   Options
	conn   net.Conn
	wmu    sync.Mutex
	probes int
	card   *Card
	queue  chan string
	silent bool
	// curN is the number of the line being run, or -1.
	curN int
	// halted means M112 arrived, so the board has stopped answering anything.
	halted atomic.Bool
	// holdEnds ends a PrintHold early.
	holdEnds chan struct{}
	// heatEnds ends a heater wait, and is what M108 does.
	heatEnds chan struct{}
	// stopMoves empties the planner, and is what M410 does.
	stopMoves chan struct{}

	planMu sync.Mutex
	// plan holds when each planned move will finish.
	plan []time.Time
	// killAt is when M85 fires, or zero.
	killAt time.Time

	mu     sync.Mutex
	stop   chan struct{}
	closed chan struct{}

	heatMu       sync.Mutex
	nozzle       float64
	nozzleTarget float64
	bed          float64
	bedTarget    float64
	movedAt      time.Time
}

// New starts a printer and returns the host's end of its connection. Closing
// that end acts like unplugging the printer.
func New(opts Options) io.ReadWriteCloser {
	host, device := net.Pipe()
	card := opts.Card
	if card == nil {
		card = NewCard()
	}
	p := &printer{
		opts: opts, conn: device, closed: make(chan struct{}), card: card,
		nozzle: ambientNozzle, bed: ambientBed, movedAt: time.Now(), curN: -1,
		holdEnds: make(chan struct{}, 1), heatEnds: make(chan struct{}, 1), stopMoves: make(chan struct{}, 1),
	}
	card.attach(p)
	go p.run()
	return host
}

// ok ends a command's reply, naming its line like the firmware's ADVANCED_OK.
func (p *printer) ok() {
	if p.curN >= 0 && !p.opts.PlainOK {
		p.send("ok N%d P15 B15", p.curN)
		return
	}
	p.send("ok P15 B15")
}

var (
	urgentRE = regexp.MustCompile(`^(?:N-?\d+ *)?(M108|M112|M410|M524)(?:\*\d+)?$`)
	numberRE = regexp.MustCompile(`^N(\d+) `)
)

// emergency acts on the commands the firmware's emergency parser catches as they
// arrive, ahead of the queue. It's off during a save, and a damaged line doesn't
// match. M524 clears the queue, which loses the replies to everything in it, and to
// M524 itself as the worst case. Their numbers still count as read.
func (p *printer) emergency(raw string) bool {
	m := urgentRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil || p.card.Saving() != "" {
		return false
	}
	// These act as they arrive, then still go through the queue as ordinary lines,
	// checked and answered in their turn.
	switch m[1] {
	case "M108":
		select {
		case p.heatEnds <- struct{}{}:
		default:
		}
		return false
	case "M410":
		p.planMu.Lock()
		p.plan = nil
		p.planMu.Unlock()
		select {
		case p.stopMoves <- struct{}{}:
		default:
		}
		return false
	}
	if p.opts.Seen != nil {
		p.opts.Seen(m[1])
	}
	p.card.lineRead(raw)
	if m[1] == "M112" {
		p.halted.Store(true)
		p.heatMu.Lock()
		p.nozzleTarget, p.bedTarget = 0, 0
		p.heatMu.Unlock()
		p.send("Error:Printer halted. kill() called!")
		return true
	}
	if !p.card.abortPrint() {
		return false
	}
	for drained := true; drained; {
		select {
		case old, ok := <-p.queue:
			if !ok {
				return true
			}
			p.card.arrived(-(len(old) + 1))
			p.card.lineRead(old)
		default:
			drained = false
		}
	}
	p.heatMu.Lock()
	p.nozzleTarget, p.bedTarget = 0, 0
	p.heatMu.Unlock()
	select {
	case p.holdEnds <- struct{}{}:
	default:
	}
	return true
}

func (p *printer) send(format string, args ...any) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	fmt.Fprintf(p.conn, format+"\n", args...)
}

func (p *printer) run() {
	defer close(p.closed)
	defer p.conn.Close()
	started := time.Now()
	if p.opts.Garbage {
		go p.send("\xff\xfe\xfdh\xc3")
	}
	answeredGarbage := false
	// Commands queue up while one runs, like the real board's serial buffer.
	queue := make(chan string, 256)
	p.queue = queue
	go func() {
		defer close(queue)
		scanner := bufio.NewScanner(p.conn)
		for scanner.Scan() {
			if p.emergency(scanner.Text()) {
				continue
			}
			p.card.arrived(len(scanner.Text()) + 1)
			queue <- scanner.Text()
		}
	}()
	for raw := range queue {
		p.card.arrived(-(len(raw) + 1))
		line := strings.TrimSpace(raw)
		if p.opts.Silent || p.silent || p.halted.Load() || line == "" {
			continue
		}
		// The real board keeps what it's sent during startup, and answers late.
		time.Sleep(time.Until(started.Add(p.opts.BootDelay)))
		if p.opts.GarbageIn && !answeredGarbage {
			answeredGarbage = true
			p.send("echo:Unknown command: \"h\"")
			p.ok()
		}
		p.line(line)
	}
	p.mu.Lock()
	if p.stop != nil {
		close(p.stop)
	}
	p.mu.Unlock()
}

// temps moves the temperatures on, and formats them like Marlin does.
func (p *printer) temps() string {
	p.heatMu.Lock()
	defer p.heatMu.Unlock()
	now := time.Now()
	rate := p.opts.HeatRate
	if rate == 0 {
		rate = degreesPerSecond
	}
	step := now.Sub(p.movedAt).Seconds() * rate
	p.movedAt = now
	p.nozzle = approach(p.nozzle, max(p.nozzleTarget, ambientNozzle), step)
	p.bed = approach(p.bed, max(p.bedTarget, ambientBed), step)
	return fmt.Sprintf("T:%.2f /%.2f B:%.2f /%.2f @:0 B@:0", p.nozzle, p.nozzleTarget, p.bed, p.bedTarget)
}

func approach(from, to, step float64) float64 {
	if from < to {
		return min(from+step, to)
	}
	return max(from-step, to)
}

func (p *printer) heat(line string, target *float64) {
	if p.opts.NoHeatReply {
		return
	}
	if m := heatRE.FindStringSubmatch(line); m != nil && !p.opts.IgnoreHeat {
		v, _ := strconv.ParseFloat(m[1], 64)
		p.temps()
		p.heatMu.Lock()
		*target = v
		p.heatMu.Unlock()
	}
	p.ok()
}

func (p *printer) command(line string) {
	word, rest, _ := strings.Cut(line, " ")
	if p.opts.Seen != nil {
		p.opts.Seen(line)
	}
	if p.opts.StallOn == word {
		p.wait(p.opts.Stall, p.opts.StallBusy)
	}
	switch word {
	case "M115":
		p.send("FIRMWARE_NAME:Marlin 2.1.3 MRiscoC Ender3V2-422-BLTUBL-MPC, based on bugfix-2.1.x (Dec 17 2023 23:45:58) SOURCE_CODE_URL:github.com/mriscoc/Ender3V2S1 PROTOCOL_VERSION:1.0 MACHINE_TYPE:Ender3V2-422-BLTUBL-MPC EXTRUDER_COUNT:1")
		p.send("Cap:Z_PROBE:1")
		p.send("Cap:SD_WRITE:1")
		p.ok()
	case "M105":
		p.send("ok %s", p.temps())
	case "M104":
		p.heat(line, &p.nozzleTarget)
	case "M140":
		p.heat(line, &p.bedTarget)
	case "M23":
		p.card.openPrint(p, rest)
	case "M24":
		p.ok()
		p.card.startPrint(p)
		if p.opts.PrintHold > 0 {
			select {
			case <-time.After(p.opts.PrintHold):
			case <-p.holdEnds:
			}
		}
	case "M108", "M410", "M117", "M107", "M84", "G91", "M73":
		p.ok()
	case "M109":
		p.heatWait(line, &p.nozzleTarget)
	case "M190":
		p.heatWait(line, &p.bedTarget)
	case "M85":
		p.setKill(line)
		p.ok()
	case "M86":
		if p.opts.M86 != "" {
			p.send("%s", p.opts.M86)
		} else {
			p.send("M86 B0 E0 S600 T170")
		}
		p.ok()
	case "G4":
		p.touchKill()
		if m := heatRE.FindStringSubmatch(line); m != nil {
			secs, _ := strconv.Atoi(m[1])
			time.Sleep(time.Duration(secs) * time.Second)
		} else if ms, err := strconv.Atoi(strings.TrimPrefix(rest, "P")); err == nil {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		p.ok()
	case "G0", "G1":
		p.planMove()
		p.ok()
	case "M27":
		if rest == "C" {
			p.send("Current file: %s", p.card.printName())
			p.ok()
			break
		}
		if pos, size, ok := p.card.printPos(); ok {
			p.send("SD printing byte %d/%d", pos, size)
			p.ok()
			break
		}
		if p.opts.Printing != nil && p.opts.Printing() {
			p.send("SD printing byte 1000/20000")
		} else {
			p.send("Not SD printing")
		}
		p.ok()
	case "M155":
		p.autoreport(line)
		p.ok()
	case "G28":
		p.wait(p.opts.MoveDelay, true)
		p.send("X:0.00 Y:0.00 Z:10.00 E:0.00 Count X:0 Y:0 Z:0")
		p.ok()
	case "G30":
		p.probe(line)
	case "G90":
		p.ok()
	case "M400":
		p.finishMoves()
		p.ok()
	case "M20":
		p.card.list(p)
	case "M28":
		p.card.open(p, rest)
	case "M29":
		p.ok()
	case "M30":
		p.card.remove(p, rest)
	case "M110":
		p.ok()
	case "M118":
		p.send("%s", rest)
		p.ok()
	case "M114":
		p.send("X:115.00 Y:115.00 Z:30.00 E:0.00 Count X:0 Y:0 Z:0")
		p.ok()
	case "M851":
		p.send("echo:; Z-Probe Offset:")
		p.send("echo:  M851 X-40.00 Y-10.00 Z-1.80 ; (mm)")
		p.ok()
	case "M503":
		p.send("echo:  G21 ; (mm)")
		p.ok()
	case "G29":
		p.send("MESH_MIN_X  25=25")
		p.ok()
	default:
		p.send("echo:Unknown command: %q", line)
		p.ok()
	}
}

// planMove adds a move to the planner, first waiting for room if it's full. Nothing
// else runs meanwhile, including reading the next line, like the real board.
func (p *printer) planMove() {
	p.touchKill()
	if p.opts.MoveTime <= 0 {
		return
	}
	for {
		now := time.Now()
		p.planMu.Lock()
		for len(p.plan) > 0 && !p.plan[0].After(now) {
			p.plan = p.plan[1:]
		}
		if len(p.plan) < PlannerSlots {
			start := now
			if len(p.plan) > 0 {
				start = p.plan[len(p.plan)-1]
			}
			p.plan = append(p.plan, start.Add(p.opts.MoveTime))
			p.planMu.Unlock()
			p.touchKill()
			return
		}
		first := p.plan[0]
		p.planMu.Unlock()
		select {
		case <-time.After(time.Until(first)):
		case <-p.stopMoves:
		case <-p.closed:
			return
		}
	}
}

// finishMoves is M400.
func (p *printer) finishMoves() {
	p.planMu.Lock()
	var last time.Time
	if len(p.plan) > 0 {
		last = p.plan[len(p.plan)-1]
	}
	p.planMu.Unlock()
	if last.IsZero() {
		return
	}
	select {
	case <-time.After(time.Until(last)):
	case <-p.stopMoves:
	case <-p.closed:
	}
	p.touchKill()
}

// heatWait is M109 and M190. The firmware sends nothing but temperatures meanwhile,
// and doesn't read the next line, so the receive buffer can fill up behind it.
func (p *printer) heatWait(line string, target *float64) {
	select {
	case <-p.heatEnds:
	default:
	}
	if m := heatRE.FindStringSubmatch(line); m != nil && !p.opts.IgnoreHeat {
		v, _ := strconv.ParseFloat(m[1], 64)
		p.temps()
		p.heatMu.Lock()
		*target = v
		p.heatMu.Unlock()
	}
	defer p.ok()
	p.touchKill()
	end := time.Now().Add(p.opts.HeatWait)
	for time.Now().Before(end) {
		p.touchKill()
		select {
		case <-time.After(min(time.Until(end), 5*time.Millisecond)):
		case <-p.heatEnds:
			return
		case <-p.closed:
			return
		}
	}
}

// setKill is M85. A real board checks in manage_inactivity.
func (p *printer) setKill(line string) {
	p.planMu.Lock()
	defer p.planMu.Unlock()
	armed := false
	if m := heatRE.FindStringSubmatch(line); m != nil {
		secs, _ := strconv.Atoi(m[1])
		armed = secs > 0 && p.opts.KillAfter > 0
	}
	if !armed {
		p.killAt = time.Time{}
		return
	}
	first := p.killAt.IsZero()
	p.killAt = time.Now().Add(p.opts.KillAfter)
	if first {
		go p.watchKill()
	}
}

func (p *printer) touchKill() {
	p.planMu.Lock()
	defer p.planMu.Unlock()
	if !p.killAt.IsZero() {
		p.killAt = time.Now().Add(p.opts.KillAfter)
	}
}

func (p *printer) watchKill() {
	for {
		select {
		case <-time.After(time.Millisecond):
		case <-p.closed:
			return
		}
		p.planMu.Lock()
		at := p.killAt
		moving := len(p.plan) > 0 && p.plan[len(p.plan)-1].After(time.Now())
		if moving && !at.IsZero() {
			p.killAt = time.Now().Add(p.opts.KillAfter)
			at = p.killAt
		}
		p.planMu.Unlock()
		if at.IsZero() {
			return
		}
		if time.Now().After(at) {
			p.halted.Store(true)
			p.heatMu.Lock()
			p.nozzleTarget, p.bedTarget = 0, 0
			p.heatMu.Unlock()
			p.send("Error:Printer halted. kill() called!")
			return
		}
	}
}

// wait passes the time like a blocking move, with or without keepalives.
func (p *printer) wait(d time.Duration, busy bool) {
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return
		}
		if !busy {
			time.Sleep(left)
			return
		}
		p.send("echo:busy: processing")
		time.Sleep(min(left, 20*time.Millisecond))
	}
}

func (p *printer) autoreport(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stop != nil {
		close(p.stop)
		p.stop = nil
	}
	secs, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(line, "M155")), "S"))
	if err != nil || secs <= 0 {
		return
	}
	stop := make(chan struct{})
	p.stop = stop
	go func() {
		// Sped up, so tests don't wait for real report intervals.
		tick := time.NewTicker(time.Duration(secs) * 20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if !p.halted.Load() {
					p.send(" %s", p.temps())
				}
			case <-stop:
				return
			}
		}
	}()
}

func (p *printer) probe(line string) {
	var x, y float64
	for _, m := range coordRE.FindAllStringSubmatch(line, -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		if m[1] == "X" {
			x = v
		} else {
			y = v
		}
	}
	bed := p.opts.Corners
	if p.opts.Bed != nil {
		bed = p.opts.Bed()
	}
	z := bed.FL
	switch {
	case x > 115 && y <= 115:
		z = bed.FR
	case x > 115 && y > 115:
		z = bed.BR
	case x <= 115 && y > 115:
		z = bed.BL
	}
	p.probes++
	switch p.probes {
	case p.opts.DropProbe:
		p.send("echo:busy: processing")
		p.conn.Close()
		return
	case p.opts.HaltProbe:
		p.send("Error:Printer halted. kill() called!")
		return
	case p.opts.FailProbe:
		p.send("Error:Probing Failed")
		p.send("X:%.2f Y:%.2f Z:10.00 E:0.00 Count X:0 Y:0 Z:0", x, y)
		p.ok()
		return
	}
	p.send("echo:busy: processing")
	p.wait(p.opts.MoveDelay, true)
	p.send("Bed X:%.2f Y:%.2f Z:%.3f", x+p.opts.ProbeShiftX, y, z)
	p.send("X:%.2f Y:%.2f Z:10.00 E:0.00 Count X:0 Y:0 Z:0", x, y)
	p.ok()
}
