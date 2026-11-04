package gcode

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// defaultFeed is the speed assumed until a file sets one, in mm/min.
const defaultFeed = 1500

// Move is one change of the head's position.
type Move struct {
	// X, Y and Z are where the move ends, in mm.
	X, Y, Z float32
	// Seconds is how far into the file the move ends, by the file's own speeds.
	Seconds float32
	// End is the file position just after this move's line, counted as on the card, the
	// same measure as a print job's position.
	End uint32
	// Extrude means it lays down plastic, rather than travelling.
	Extrude bool
}

// Path is what a file does with the head, for drawing it.
type Path struct {
	Moves   []Move
	Seconds float64
}

// pathState follows the modes a file can change as it goes.
type pathState struct {
	x, y, z, e, feed  float64
	relative, relExtr bool
}

// words splits a command into its letters and values. The firmware doesn't need spaces
// between them, so neither does this.
func words(line string) map[byte]float64 {
	out := make(map[byte]float64, 5)
	for i := 0; i < len(line); {
		c := line[i] &^ 0x20
		i++
		if c < 'A' || c > 'Z' {
			continue
		}
		start := i
		for i < len(line) && strings.IndexByte("+-.0123456789", line[i]) >= 0 {
			i++
		}
		v, err := strconv.ParseFloat(line[start:i], 64)
		if err != nil {
			v = 0
		}
		out[c] = v
	}
	return out
}

// command is a line's first word, such as G1, with leading zeros gone.
func command(line string) string {
	end := 1
	for end < len(line) && line[end] >= '0' && line[end] <= '9' {
		end++
	}
	number := strings.TrimLeft(line[1:end], "0")
	if number == "" {
		number = "0"
	}
	return strings.ToUpper(line[:1]) + number
}

// ParsePath reads a sliced file's moves. It's an approximation of what the printer will
// do, as it knows nothing of acceleration or the mesh. Times are scaled to the slicer's
// total if the file gives one, since the slicer does allow for acceleration.
func ParsePath(r io.Reader) (*Path, error) {
	reader := NewReader(r)
	st := pathState{feed: defaultFeed}
	path := &Path{}
	var seconds float64
	var pos int64
	for {
		line, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		pos += int64(len(line)) + 2
		switch command(line) {
		case "G0", "G1", "G2", "G3":
			// Arcs are drawn as straight lines. Cura doesn't produce them.
			if m, ok := st.move(words(line), &seconds); ok {
				m.Seconds, m.End = float32(seconds), uint32(pos)
				path.Moves = append(path.Moves, m)
			}
		case "G4":
			w := words(line)
			seconds += w['S'] + w['P']/1000
		case "G28":
			if m, ok := st.home(words(line)); ok {
				m.Seconds, m.End = float32(seconds), uint32(pos)
				path.Moves = append(path.Moves, m)
			}
		case "G90":
			st.relative, st.relExtr = false, false
		case "G91":
			st.relative, st.relExtr = true, true
		case "M82":
			st.relExtr = false
		case "M83":
			st.relExtr = true
		case "G92":
			st.setPosition(words(line))
		}
	}
	path.Seconds = seconds
	if reader.total > 0 && seconds > 0 {
		scale := reader.total / seconds
		for i := range path.Moves {
			path.Moves[i].Seconds = float32(float64(path.Moves[i].Seconds) * scale)
		}
		path.Seconds = reader.total
	}
	return path, nil
}

// move applies a G0 or G1. A move that only retracts takes time but isn't a move of the
// head, so its time goes on the next one.
func (st *pathState) move(w map[byte]float64, seconds *float64) (Move, bool) {
	if f, ok := w['F']; ok && f > 0 {
		st.feed = f
	}
	target := func(axis byte, at float64, relative bool) float64 {
		v, ok := w[axis]
		switch {
		case !ok:
			return at
		case relative:
			return at + v
		}
		return v
	}
	x, y, z := target('X', st.x, st.relative), target('Y', st.y, st.relative), target('Z', st.z, st.relative)
	e := target('E', st.e, st.relExtr)
	dx, dy, dz, de := x-st.x, y-st.y, z-st.z, e-st.e
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if dist > 0 {
		*seconds += dist / st.feed * 60
	} else {
		*seconds += math.Abs(de) / st.feed * 60
	}
	st.x, st.y, st.z, st.e = x, y, z, e
	if dist == 0 {
		return Move{}, false
	}
	return Move{X: float32(x), Y: float32(y), Z: float32(z), Extrude: de > 0 && dx*dx+dy*dy > 0}, true
}

// home applies a G28, which homes the named axes, or all of them if it names none.
func (st *pathState) home(w map[byte]float64) (Move, bool) {
	_, hx := w['X']
	_, hy := w['Y']
	_, hz := w['Z']
	all := !hx && !hy && !hz
	before := *st
	if all || hx {
		st.x = 0
	}
	if all || hy {
		st.y = 0
	}
	if all || hz {
		st.z = 0
	}
	if before.x == st.x && before.y == st.y && before.z == st.z {
		return Move{}, false
	}
	return Move{X: float32(st.x), Y: float32(st.y), Z: float32(st.z)}, true
}

func (st *pathState) setPosition(w map[byte]float64) {
	for axis, at := range map[byte]*float64{'X': &st.x, 'Y': &st.y, 'Z': &st.z, 'E': &st.e} {
		if v, ok := w[axis]; ok {
			*at = v
		}
	}
}

// The binary form read by the web page: pathMagic, the number of moves and the total
// seconds, then for each move X, Y, Z and Seconds as float32, End and a flags word as
// uint32, all little-endian, so the page can read it straight into typed arrays.
const (
	pathMagic = "TLP1"
	// PathRecordSize is the size of one move in the binary form.
	PathRecordSize = 24
	// PathHeaderSize is the size of what comes before the moves.
	PathHeaderSize = 12

	pathExtrude = 1
)

// Encode gives the binary form.
func (p *Path) Encode() []byte {
	out := make([]byte, PathHeaderSize, PathHeaderSize+len(p.Moves)*PathRecordSize)
	copy(out, pathMagic)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(p.Moves)))
	binary.LittleEndian.PutUint32(out[8:], math.Float32bits(float32(p.Seconds)))
	for _, m := range p.Moves {
		var flags uint32
		if m.Extrude {
			flags = pathExtrude
		}
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(m.X))
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(m.Y))
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(m.Z))
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(m.Seconds))
		out = binary.LittleEndian.AppendUint32(out, m.End)
		out = binary.LittleEndian.AppendUint32(out, flags)
	}
	return out
}
