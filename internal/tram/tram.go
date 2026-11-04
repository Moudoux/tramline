// Package tram applies the firmware tramming wizard's pass/fail rule to probe
// readings, because the wizard itself only reports to the printer's LCD.
package tram

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// Tolerance is the wizard's limit in mm, read from trammingWizard() in the
// firmware's libproui at tags 20231202 and 20240122.
const Tolerance = 0.05

// Corner names follow the wizard's probing order.
type Corner int

const (
	FrontLeft Corner = iota
	FrontRight
	BackRight
	BackLeft
)

func (c Corner) String() string {
	return [...]string{"front-left", "front-right", "back-right", "back-left"}[c]
}

// Result is the outcome for one set of readings, indexed by Corner.
type Result struct {
	Relative [4]float64 `json:"relative_mm"`
	Range    float64    `json:"range_mm"`
	Pass     bool       `json:"pass"`
	// Corner and Action say what to change, and are empty on a pass.
	Corner string `json:"corner,omitempty"`
	Action string `json:"action,omitempty"`
}

// Evaluate subtracts the average from each reading and passes if the highest
// and lowest differ by less than Tolerance. On a fail it names the corner
// furthest from the average: "lower" if above it, "raise" if below.
func Evaluate(z [4]float64) Result {
	avg := (z[0] + z[1] + z[2] + z[3]) / 4
	var r Result
	lo, hi := math.Inf(1), math.Inf(-1)
	for i, v := range z {
		d := v - avg
		r.Relative[i] = d
		lo, hi = math.Min(lo, d), math.Max(hi, d)
	}
	// Rounded because 0.300-0.250 is just under 0.05 in binary floating point,
	// and a reading of exactly the limit must fail.
	r.Range = math.Round((hi-lo)*1e6) / 1e6
	r.Pass = r.Range < Tolerance
	if r.Pass {
		return r
	}
	// The wizard scans the corners in this order, so ties go the same way.
	worst, furthest := FrontLeft, 0.0
	for _, c := range []Corner{FrontLeft, BackLeft, FrontRight, BackRight} {
		if d := math.Abs(r.Relative[c]); furthest < d {
			worst, furthest = c, d
		}
	}
	r.Corner = worst.String()
	if r.Relative[worst] >= 0 {
		r.Action = "lower"
	} else {
		r.Action = "raise"
	}
	return r
}

// Turn says which way to turn a corner's knob, seen from above. It was measured on
// back-left, and the other corners are assumed to match.
func Turn(action string) string {
	if action == "lower" {
		return "counter-clockwise"
	}
	return "clockwise"
}

var bedRE = regexp.MustCompile(`^Bed X:\s*(-?[\d.]+)\s+Y:\s*(-?[\d.]+)\s+Z:\s*(-?[\d.]+)`)

// Reading is one probed point, as the printer reported it.
type Reading struct {
	Corner string  `json:"corner"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Z      float64 `json:"z"`
}

// ParseProbe finds the reading in a G30 reply. G30 ends in a normal "ok" even
// when probing failed, so a reply with no reading is the only sign of that.
func ParseProbe(lines []string) (Reading, error) {
	for _, line := range lines {
		m := bedRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var v [3]float64
		for i := range v {
			f, err := strconv.ParseFloat(m[i+1], 64)
			if err != nil {
				return Reading{}, fmt.Errorf("bad probe reading %q: %w", line, err)
			}
			v[i] = f
		}
		return Reading{X: v[0], Y: v[1], Z: v[2]}, nil
	}
	return Reading{}, fmt.Errorf("no probe reading in reply: %q", lines)
}

const (
	bedSize = 230
	// DefaultInset gives the wizard's points, seen on this printer's LCD: 25 and 205.
	// They match its mesh limits in M503's C29 line. If the mesh limits are changed
	// in the LCD menu, the wizard may follow them and this wouldn't.
	DefaultInset = 25
	// MinInset is the firmware's PROBING_MARGIN, and MaxInset is well short of the
	// bed's middle, where the corners would stop meaning anything.
	MinInset = 10
	MaxInset = 60
)

// Point is a place to probe.
type Point struct {
	Corner Corner
	X, Y   int
}

// Points gives the four corners in the wizard's probing order.
func Points(inset int) [4]Point {
	lo, hi := inset, bedSize-inset
	return [4]Point{
		{FrontLeft, lo, lo},
		{FrontRight, hi, lo},
		{BackRight, hi, hi},
		{BackLeft, lo, hi},
	}
}

// Spread is how far each corner's reading moved across rounds of probing, which
// is the probe's repeatability. Each round is indexed by Corner.
func Spread(rounds [][4]float64) [4]float64 {
	var out [4]float64
	for c := range out {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, z := range rounds {
			lo, hi = math.Min(lo, z[c]), math.Max(hi, z[c])
		}
		if len(rounds) > 0 {
			out[c] = math.Round((hi-lo)*1e6) / 1e6
		}
	}
	return out
}
