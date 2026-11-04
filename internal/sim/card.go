package sim

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Card is a simulated SD card, plus the state a board keeps until it loses power.
// It follows queue.cpp and cardreader.cpp in the firmware source at tag 20231202.
type Card struct {
	mu sync.Mutex
	// Missing makes the board act as if no card is mounted.
	Missing bool
	files   map[string][]byte
	saving  string
	lastN   int
	// queued and maxQueued are bytes received and not yet taken up, the most there
	// has been, as the board's receive buffer would hold.
	queued, maxQueued int
	// faults holds the line faults that have already happened.
	faults map[string]bool
	// board is the connection now in use. A print outlasts a dropped link.
	board *printer
	print *cardPrint
}

// cardPrint is a file open for printing, which M24 starts.
type cardPrint struct {
	name      string
	size      int64
	running   bool
	startedAt time.Time
	lasts     time.Duration
	stop      chan struct{}
}

func (c *Card) attach(p *printer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.board = p
}

// lineRead counts a line's number as read, which the firmware does when a line goes
// into its queue, not when it runs.
func (c *Card) lineRead(raw string) {
	if m := numberRE.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			c.mu.Lock()
			c.lastN = max(c.lastN, n)
			c.mu.Unlock()
		}
	}
}

func (c *Card) openPrint(p *printer, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = strings.ToUpper(strings.TrimSpace(name))
	data, ok := c.files[name]
	if c.Missing || !ok {
		p.send("open failed, File: %s.", name)
		p.ok()
		return
	}
	c.print = &cardPrint{name: name, size: int64(len(data)), stop: make(chan struct{})}
	p.send("echo:Now fresh file: %s", name)
	p.send("File opened: %s Size: %d", name, len(data))
	p.send("File selected")
	p.ok()
}

func (c *Card) startPrint(p *printer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.print
	if cp == nil || cp.running {
		return
	}
	cp.running, cp.startedAt, cp.lasts = true, time.Now(), p.opts.PrintTime
	if cp.lasts == 0 {
		cp.lasts = 200 * time.Millisecond
	}
	go func() {
		select {
		case <-time.After(cp.lasts):
		case <-cp.stop:
			return
		}
		c.mu.Lock()
		finished := c.print == cp
		if finished {
			c.print = nil
		}
		board := c.board
		c.mu.Unlock()
		if finished && board != nil {
			board.send("Done printing file")
		}
	}()
}

// printPos is what M27 reports. An open file counts as printing before M24 too.
func (c *Card) printPos() (pos, size int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.print
	if cp == nil {
		return 0, 0, false
	}
	if cp.running {
		pos = min(cp.size, cp.size*int64(time.Since(cp.startedAt))/int64(cp.lasts))
	}
	return pos, cp.size, true
}

func (c *Card) printName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.print == nil {
		return "(no file)"
	}
	return c.print.name
}

// abortPrint reports whether there was a print to abort.
func (c *Card) abortPrint() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.print == nil {
		return false
	}
	close(c.print.stop)
	c.print = nil
	return true
}

// StopPrint stops a print as the LCD would, for tests.
func (c *Card) StopPrint() { c.abortPrint() }

// Printing reports whether a print from the card is running, as a test sees it.
func (c *Card) Printing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.print != nil
}

// StartPrint starts a print as the LCD would, for tests.
func (c *Card) StartPrint(name string, lasts time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data := c.files[strings.ToUpper(name)]
	cp := &cardPrint{name: strings.ToUpper(name), size: int64(len(data)), running: true, startedAt: time.Now(), lasts: lasts, stop: make(chan struct{})}
	c.print = cp
	go func() {
		select {
		case <-time.After(lasts):
		case <-cp.stop:
			return
		}
		c.mu.Lock()
		finished := c.print == cp
		if finished {
			c.print = nil
		}
		board := c.board
		c.mu.Unlock()
		if finished && board != nil {
			board.send("Done printing file")
		}
	}()
}

// NewCard returns an empty card.
func NewCard() *Card { return &Card{files: map[string][]byte{}, faults: map[string]bool{}} }

// PowerLost ends a save and forgets the line number, as a board losing power does.
// What was written so far stays on the card.
func (c *Card) PowerLost() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saving, c.lastN = "", 0
	if c.print != nil {
		close(c.print.stop)
		c.print = nil
	}
}

func (c *Card) arrived(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queued += n
	c.maxQueued = max(c.maxQueued, c.queued)
}

// MaxQueued is the most bytes that have been waiting to be read at once.
func (c *Card) MaxQueued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxQueued
}

// Saving returns the file being written, if any.
func (c *Card) Saving() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saving
}

// File returns a file's contents, and whether it exists.
func (c *Card) File(name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.files[strings.ToUpper(name)]
	return string(data), ok
}

// Put adds a file, as if it had been copied to the card some other way.
func (c *Card) Put(name, contents string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[strings.ToUpper(name)] = []byte(contents)
}

// Names lists the files on the card.
func (c *Card) Names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.files))
}

func (c *Card) list(p *printer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Missing {
		p.send("echo:No media")
	} else {
		p.send("Begin file list")
		for _, name := range slices.Sorted(maps.Keys(c.files)) {
			p.send("%s %d", name, len(c.files[name]))
		}
		p.send("End file list")
	}
	p.ok()
}

// open says nothing at all if no card is mounted, only the "ok", like the firmware.
// It empties a file that's already there.
func (c *Card) open(p *printer, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.Missing {
		name = strings.ToUpper(strings.TrimSpace(name))
		c.files[name] = nil
		c.saving = name
		p.send("echo:Now fresh file: %s", name)
		p.send("Writing to file: %s", name)
	}
	p.ok()
}

func (c *Card) remove(p *printer, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = strings.ToUpper(strings.TrimSpace(name))
	c.saving = ""
	if _, ok := c.files[name]; ok {
		delete(c.files, name)
		p.send("File deleted:%s", name)
	} else {
		p.send("Deletion failed, File: %s.", name)
	}
	p.ok()
}

// m29RE matches "M29" anywhere in a line, but not "M290", like the firmware's is_M29.
var m29RE = regexp.MustCompile(`M29(\D|$)`)

// fault reports whether this one-time fault applies to line n now.
func (p *printer) fault(kind string, at, n int) bool {
	c := p.card
	c.mu.Lock()
	defer c.mu.Unlock()
	if at == 0 || at != n || c.faults[kind] {
		return false
	}
	c.faults[kind] = true
	return true
}

// lineError also throws away what's waiting in the receive buffer, as the firmware does.
func (p *printer) lineError(msg string, lastN int) {
	p.send("Error:%s, Last Line: %d", msg, lastN)
	for flushing := true; flushing; {
		select {
		case raw, ok := <-p.queue:
			if !ok {
				flushing = false
				break
			}
			p.card.arrived(-(len(raw) + 1))
		default:
			flushing = false
		}
	}
	p.send("Resend: %d", lastN+1)
	p.send("ok")
}

// line handles one received line before it runs, as get_serial_commands and
// advance do: comments go, numbers and checksums are checked, and while a file is
// being saved everything but M29 goes into the file instead of running.
func (p *printer) line(line string) {
	if i := strings.IndexByte(line, ';'); i >= 0 {
		line = strings.TrimSpace(line[:i])
		if line == "" {
			return
		}
	}
	if !strings.HasPrefix(line, "N") {
		p.unnumbered(line)
		return
	}
	isM110 := strings.Contains(line, "M110")
	numberAt := line[1:]
	if isM110 {
		if i := strings.IndexByte(line[4:], 'N'); i >= 0 {
			numberAt = line[4+i+1:]
		}
	}
	end := strings.IndexFunc(numberAt, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(numberAt)
	}
	n, _ := strconv.Atoi(numberAt[:end])
	if p.fault("drop", p.opts.DropLine, n) {
		return
	}
	time.Sleep(p.opts.LineDelay)

	c := p.card
	c.mu.Lock()
	last := c.lastN
	c.mu.Unlock()
	if n != last+1 && !isM110 {
		// "A request-for-resend line was already in transit": dropped without a word.
		if n == last || n == last-1 {
			return
		}
		p.lineError("Line Number is not Last Line Number+1", last)
		return
	}
	star := strings.LastIndexByte(line, '*')
	if star < 0 {
		p.lineError("No Checksum with line number", last)
		return
	}
	var sum byte
	for i := range star {
		sum ^= line[i]
	}
	got, err := strconv.Atoi(line[star+1:])
	if err != nil || got != int(sum) || p.fault("corrupt", p.opts.CorruptLine, n) {
		p.lineError("checksum mismatch", last)
		return
	}
	c.mu.Lock()
	c.lastN = n
	if isM110 {
		c.lastN = 0
	}
	c.mu.Unlock()

	if p.opts.SilentFromLine != 0 && n >= p.opts.SilentFromLine {
		p.silent = true
		return
	}
	if p.fault("droplink", p.opts.DropLinkLine, n) {
		p.conn.Close()
		return
	}
	_, rest, _ := strings.Cut(line, " ")
	// The firmware ends a saved line at the first "*", though it checked the last.
	body, _, _ := strings.Cut(rest, "*")
	if c.Saving() == "" {
		p.curN = n
		p.command(body)
		p.curN = -1
		return
	}
	if m29RE.MatchString(body) {
		p.finishSave(fmt.Sprintf("ok N%d P15 B15", n))
		return
	}
	if p.opts.Seen != nil {
		p.opts.Seen(body)
	}
	c.mu.Lock()
	c.files[c.saving] = append(c.files[c.saving], body+"\r\n"...)
	c.mu.Unlock()
	if p.fault("write", p.opts.WriteErrorLine, n) {
		p.send("Error:error writing to file")
	}
	if p.fault("loseok", p.opts.LoseOKLine, n) {
		return
	}
	if p.opts.PlainOK {
		p.ok()
	} else {
		p.send("ok N%d P15 B15", n)
	}
}

func (p *printer) unnumbered(line string) {
	c := p.card
	if c.Saving() == "" {
		p.command(line)
		return
	}
	if m29RE.MatchString(line) {
		p.finishSave("ok P15 B15")
		return
	}
	c.mu.Lock()
	last := c.lastN
	c.mu.Unlock()
	p.lineError("No Checksum with line number", last)
}

func (p *printer) finishSave(ok string) {
	if p.opts.Seen != nil {
		p.opts.Seen("M29")
	}
	c := p.card
	c.mu.Lock()
	c.saving = ""
	c.mu.Unlock()
	p.send("Done saving file.")
	p.send("%s", ok)
}
