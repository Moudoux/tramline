package gcode

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func parsePath(t *testing.T, file string) *Path {
	t.Helper()
	p, err := ParsePath(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPathFollowsMoves(t *testing.T) {
	p := parsePath(t, strings.Join([]string{
		"G28 ; home",
		"G90",
		"M82",
		"G1 Z5 F600",
		"G1 X10 Y0 F6000",
		"G1 X20 E1.5",
		"G1 E0.5",   // retract only, not a move
		"G1 X30",    // no plastic
		"G1X40E2.5", // no spaces, and more plastic than the retract
		"M104 S200",
	}, "\n"))
	want := []struct {
		x, z    float32
		extrude bool
	}{{0, 5, false}, {10, 5, false}, {20, 5, true}, {30, 5, false}, {40, 5, true}}
	if len(p.Moves) != len(want) {
		t.Fatalf("got %d moves: %+v", len(p.Moves), p.Moves)
	}
	for i, w := range want {
		if m := p.Moves[i]; m.X != w.x || m.Z != w.z || m.Extrude != w.extrude {
			t.Errorf("move %d: got %+v, want %+v", i, m, w)
		}
	}
}

func TestPathRelativeAndReset(t *testing.T) {
	p := parsePath(t, strings.Join([]string{
		"G1 X10 Y10 Z1 E1 F6000",
		"G91",
		"G1 X5 E1",
		"G90",
		"G92 E0",
		"G1 X20 E0.5",
		"M83",
		"G1 X25 E0.5",
		"G92 X0",
		"G1 X1",
	}, "\n"))
	xs := []float32{10, 15, 20, 25, 1}
	for i, x := range xs {
		if p.Moves[i].X != x || !p.Moves[i].Extrude && i < 4 {
			t.Errorf("move %d: got %+v, want X %v, extruding", i, p.Moves[i], x)
		}
	}
	if p.Moves[4].Extrude {
		t.Errorf("a plain move counted as plastic: %+v", p.Moves[4])
	}
}

// A job's position has to find the right move, so this is the same count as Measure's.
func TestPathPositionsMatchTheCard(t *testing.T) {
	file := "; header\nG1 X1 F60 ; comment\n\nM107\n  G1 X2  \n"
	p := parsePath(t, file)
	info, err := Measure(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if got := int64(p.Moves[1].End); got != info.SDSize {
		t.Fatalf("last move ends at %d, the card's size is %d", got, info.SDSize)
	}
	if got := p.Moves[0].End; got != uint32(len("G1 X1 F60")+2) {
		t.Fatalf("first move ends at %d", got)
	}
}

func TestPathTimes(t *testing.T) {
	// 60 mm at 60 mm/s, a 2 s dwell, and a retract of 1 mm at 60 mm/s that lands on the next move.
	p := parsePath(t, "G1 X60 F3600\nG4 S2\nG1 E-1\nG1 X120\n")
	if got := p.Moves[0].Seconds; got != 1 {
		t.Errorf("first move: %v", got)
	}
	if got := p.Moves[1].Seconds; math.Abs(float64(got)-4.0167) > 0.001 {
		t.Errorf("second move: %v", got)
	}
}

func TestPathScalesToTheSlicersTime(t *testing.T) {
	p := parsePath(t, ";TIME:20\nG1 X60 F3600\nG1 X120\n")
	if p.Seconds != 20 || p.Moves[0].Seconds != 10 || p.Moves[1].Seconds != 20 {
		t.Fatalf("got %v, %+v", p.Seconds, p.Moves)
	}
}

func TestPathHomesOnlyWhatItNames(t *testing.T) {
	p := parsePath(t, "G1 X10 Y10 Z10\nG28 X\nG28 Y0\nG28\n")
	last := p.Moves[len(p.Moves)-1]
	if last.X != 0 || last.Y != 0 || last.Z != 0 {
		t.Fatalf("got %+v", last)
	}
	if m := p.Moves[1]; m.X != 0 || m.Y != 10 || m.Z != 10 {
		t.Fatalf("G28 X: got %+v", m)
	}
}

func TestPathEncoding(t *testing.T) {
	p := parsePath(t, ";TIME:3\nG1 X1.5 Y2.5 Z0.2 E1 F600\n")
	got := p.Encode()
	if len(got) != PathHeaderSize+PathRecordSize || string(got[:4]) != "TLP1" {
		t.Fatalf("got % x", got)
	}
	if n := binary.LittleEndian.Uint32(got[4:]); n != 1 {
		t.Fatalf("count %d", n)
	}
	f := func(at int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(got[at:])) }
	if f(8) != 3 || f(12) != 1.5 || f(16) != 2.5 || f(20) != 0.2 || f(24) != 3 {
		t.Fatalf("got % x", got)
	}
	if end := binary.LittleEndian.Uint32(got[28:]); end != uint32(len("G1 X1.5 Y2.5 Z0.2 E1 F600")+2) {
		t.Fatalf("end %d", end)
	}
	if flags := binary.LittleEndian.Uint32(got[32:]); flags != 1 {
		t.Fatalf("flags %d", flags)
	}
}
