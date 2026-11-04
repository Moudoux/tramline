// Package gcode prepares a sliced file to be sent over the serial line.
package gcode

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// MaxLine is the longest command that fits. The firmware holds 95 characters of a
// line, and "N<7 digits> " and "*<3 digits>" take 13 of them.
const MaxLine = 80

// The hottest a file may set the heaters. An SD print runs whatever the file says,
// so this is where the server's limits have to be applied.
const (
	MaxNozzle = 260
	MaxBed    = 100
)

// ErrEmpty means a file has nothing in it to send.
var ErrEmpty = errors.New("the file has no commands in it")

// reserved finds commands that would take over a transfer. The firmware looks for
// M29 anywhere in a line, and M110 changes the line count.
var reserved = regexp.MustCompile(`M(28|29|110)(\D|$)`)

var (
	heatCommand = regexp.MustCompile(`^M(104|109|140|190)(\s|$)`)
	heatValue   = regexp.MustCompile(`[SR](\d+)`)
)

// checkHeat refuses a heater command above the limits.
func checkHeat(line string) error {
	m := heatCommand.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	heater, limit := "nozzle", MaxNozzle
	if m[1] == "140" || m[1] == "190" {
		heater, limit = "bed", MaxBed
	}
	for _, v := range heatValue.FindAllStringSubmatch(line, -1) {
		if c, err := strconv.Atoi(v[1]); err != nil || c > limit {
			return fmt.Errorf("it sets the %s to %s C, the limit is %d", heater, v[1], limit)
		}
	}
	return nil
}

// clean removes the comment and surrounding space. The firmware would drop a
// comment anyway, along with the checksum after it, so it can't be sent as it is.
func clean(raw string) string {
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

// check refuses what can't be sent safely, rather than quietly changing the file.
func check(line string) error {
	if len(line) > MaxLine {
		return fmt.Errorf("it's %d characters long, the limit is %d", len(line), MaxLine)
	}
	for i := range len(line) {
		switch c := line[i]; {
		case c == '*':
			return errors.New(`it contains "*", where the firmware would cut it short`)
		case c == '\\':
			return errors.New(`it contains "\", which the firmware treats as an escape`)
		case c != '\t' && (c < 0x20 || c > 0x7e):
			return fmt.Errorf("it contains the byte 0x%02x", c)
		}
	}
	if m := reserved.FindStringSubmatch(line); m != nil {
		return fmt.Errorf("it contains M%s, which would interfere with the transfer", m[1])
	}
	return checkHeat(line)
}

// Reader gives a file's commands one at a time, ready to send.
type Reader struct {
	scanner *bufio.Scanner
	line    int
	// total and elapsed are the slicer's times from its comments, in seconds.
	total, elapsed float64
}

// NewReader reads a sliced file.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	return &Reader{scanner: scanner}
}

// Next returns the next command, or io.EOF after the last one.
func (r *Reader) Next() (string, error) {
	for r.scanner.Scan() {
		r.line++
		r.note(r.scanner.Text())
		line := clean(r.scanner.Text())
		if line == "" {
			continue
		}
		if err := check(line); err != nil {
			return "", fmt.Errorf("line %d can't be sent: %w", r.line, err)
		}
		return line, nil
	}
	if err := r.scanner.Err(); err != nil {
		return "", fmt.Errorf("after line %d: %w", r.line, err)
	}
	return "", io.EOF
}

// note picks up the slicer's ;TIME: and ;TIME_ELAPSED: comments.
func (r *Reader) note(raw string) {
	if v, ok := strings.CutPrefix(raw, ";TIME_ELAPSED:"); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			r.elapsed = f
		}
	} else if v, ok := strings.CutPrefix(raw, ";TIME:"); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			r.total = f
		}
	}
}

// remainingMinutes is the slicer's time left at this point in the file, rounded up, or
// -1 if the file doesn't say.
func (r *Reader) remainingMinutes() int {
	if r.total <= 0 {
		return -1
	}
	return int(math.Ceil(max(r.total-r.elapsed, 0) / 60))
}

// Progress gives a file's commands with an M73 ahead of a line whenever the percentage
// or the minutes left change, for the LCD during a stream. The bar goes by bytes, the
// same measure as a job's position.
type Progress struct {
	reader *Reader
	size   int64
	bytes  int64
	// next is a line waiting behind its M73.
	next                string
	percent, minutesWas int
}

// NewProgress reads a file whose size on the card is size.
func NewProgress(r *Reader, size int64) *Progress {
	return &Progress{reader: r, size: size, percent: -1, minutesWas: -1}
}

// Bytes is how much of the file has been read, counted as it would be on the card, so
// the M73 lines don't count.
func (p *Progress) Bytes() int64 { return p.bytes }

// Next returns the next line to send, or io.EOF after the last one.
func (p *Progress) Next() (string, error) {
	if p.next != "" {
		line := p.next
		p.next = ""
		return line, nil
	}
	line, err := p.reader.Next()
	if err != nil {
		return "", err
	}
	p.bytes += int64(len(line)) + 2
	var percent int
	if p.size > 0 {
		percent = int(min(p.bytes*100/p.size, 100))
	}
	minutes := p.reader.remainingMinutes()
	if percent == p.percent && minutes == p.minutesWas {
		return line, nil
	}
	p.percent, p.minutesWas = percent, minutes
	p.next = line
	if minutes < 0 {
		return fmt.Sprintf("M73 P%d", percent), nil
	}
	return fmt.Sprintf("M73 P%d R%d", percent, minutes), nil
}

// Info describes a file once it's been prepared.
type Info struct {
	Lines int `json:"lines"`
	// SDSize is the size the file will be on the card: the firmware ends each
	// line it saves with CR LF.
	SDSize int64 `json:"sd_size"`
}

// Measure checks a whole file and works out its size on the card.
func Measure(r io.Reader) (Info, error) {
	var info Info
	reader := NewReader(r)
	for {
		line, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if info.Lines == 0 {
				return info, ErrEmpty
			}
			return info, nil
		}
		if err != nil {
			return info, err
		}
		info.Lines++
		info.SDSize += int64(len(line)) + 2
	}
}

// ShortName makes a DOS 8.3 name for the card from a file's name, such as
// PIDECK3F.GCO. The same name always gives the same result, and a higher attempt
// gives a different one if two names clash.
func ShortName(name string, attempt int) string {
	var base strings.Builder
	for _, r := range strings.ToUpper(name) {
		if base.Len() == 6 {
			break
		}
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			base.WriteRune(r)
		}
	}
	if base.Len() == 0 {
		base.WriteString("TL")
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%02X.GCO", base.String(), byte(int(sum[0])+attempt))
}
