package sim

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// The mesh as this printer's UBL probes it, for G29 P1: 5 by 5 points from 25 to 205 mm
// (C29), with the probe 41.5 mm left of the nozzle and 7 mm in front (M851). The same rule
// is copied in internal/webui/shared/view.js for drawing the head, so keep them in step.
const (
	meshLo   = 25.0
	meshHi   = 205.0
	MeshSize = 5
)

var probeOffset = [2]float64{-41.5, -7}

// MeshStart is where the nozzle is before G29 on the real printer, after G28: the probe
// over the middle of the bed.
var MeshStart = [2]float64{156.5, 122}

// Timing fitted to the 25 points of a real G29 P1, test-logs/2026-11-04-stream/t8-long:
// about 1.66 s probing each point, plus the travel there, which takes as long as a move of
// the longer axis's distance at up to 180 mm/s with 460 mm/s² acceleration. That gives
// every gap in that log to within about 0.05 s, from 2.30 s for 45 mm to 3.00 s for 180.
const (
	meshProbeSeconds = 1.66
	meshTravelSpeed  = 180.0
	meshTravelAccel  = 460.0
	// After the last point's probe, until G29 reports the position and answers.
	meshEndSeconds = 0.8
)

// MeshPoint is one point of the mesh, in the order it's probed: where the probe touches,
// where the nozzle is then, and how long it takes from its "Probing mesh point" line to
// the next.
type MeshPoint struct {
	X, Y             float64
	NozzleX, NozzleY float64
	Seconds          float64
}

// MeshOrder copies UBL's rule (find_closest_mesh_point_of_type in Marlin's ubl_G29.cpp):
// the next point is the unprobed one with the least distance from where the probe was
// before G29, plus a tenth of its distance from the nozzle now. Ties go to the first in
// X then Y. The real printer's log fits it: the last point is back-left, and each gap
// matches the travel this order gives.
func MeshOrder(start [2]float64) []MeshPoint {
	step := (meshHi - meshLo) / (MeshSize - 1)
	ref := [2]float64{start[0] + probeOffset[0], start[1] + probeOffset[1]}
	nozzle := start
	var done [MeshSize][MeshSize]bool
	var out []MeshPoint
	for range MeshSize * MeshSize {
		bi, bj, best := -1, -1, math.Inf(1)
		for i := range MeshSize {
			for j := range MeshSize {
				if done[i][j] {
					continue
				}
				x, y := meshLo+float64(i)*step, meshLo+float64(j)*step
				d := math.Hypot(ref[0]-x, ref[1]-y) + 0.1*math.Hypot(nozzle[0]-x, nozzle[1]-y)
				if d < best {
					bi, bj, best = i, j, d
				}
			}
		}
		done[bi][bj] = true
		x, y := meshLo+float64(bi)*step, meshLo+float64(bj)*step
		next := [2]float64{x - probeOffset[0], y - probeOffset[1]}
		travel := max(math.Abs(next[0]-nozzle[0]), math.Abs(next[1]-nozzle[1]))
		out = append(out, MeshPoint{X: x, Y: y, NozzleX: next[0], NozzleY: next[1], Seconds: meshTravelSeconds(travel) + meshProbeSeconds})
		nozzle = next
	}
	return out
}

// meshTravelSeconds is a move of d mm that speeds up and slows down at meshTravelAccel,
// reaching meshTravelSpeed if it's long enough.
func meshTravelSeconds(d float64) float64 {
	if d <= 0 {
		return 0
	}
	if d < meshTravelSpeed*meshTravelSpeed/meshTravelAccel {
		return 2 * math.Sqrt(d/meshTravelAccel)
	}
	return d/meshTravelSpeed + meshTravelSpeed/meshTravelAccel
}

// probeMesh is G29 P1, as the real firmware reports it: a line as each point starts, no
// heights, then the position at the end.
func (p *printer) probeMesh() {
	scaled := func(secs float64) time.Duration {
		return time.Duration(secs * p.opts.MeshScale * float64(time.Second))
	}
	p.send("Mesh invalidated. Probing mesh.")
	points := MeshOrder(MeshStart)
	for i, pt := range points {
		p.send("Probing mesh point %d/%d.", i+1, len(points))
		p.wait(scaled(pt.Seconds), true)
	}
	p.wait(scaled(meshEndSeconds), true)
	p.mesh = probedMesh()
	last := points[len(points)-1]
	p.send("X:%.2f Y:%.2f Z:5.90 E:0.00 Count X:0 Y:0 Z:0", last.NozzleX, last.NozzleY)
	p.ok()
}

// meshGrid holds heights as [i][j], column i from the left and row j from the front, as
// UBL's z_values does. NaN is a point with no height.
type meshGrid [MeshSize][MeshSize]float64

// storedMesh is the simulator's stand-in for the mesh the real firmware loads from its
// storage slot at boot: a bed tilted a little, high at the back-left.
func storedMesh() meshGrid {
	var g meshGrid
	for i := range MeshSize {
		for j := range MeshSize {
			g[i][j] = 0.03*float64(j) - 0.025*float64(i) + 0.01
		}
	}
	return g
}

// probedMesh is what G29 P1 leaves: the stored tilt, with a small sag in the middle, so
// a view can tell the two apart.
func probedMesh() meshGrid {
	g := storedMesh()
	for i := range MeshSize {
		for j := range MeshSize {
			di, dj := float64(i)-2, float64(j)-2
			g[i][j] -= 0.04 * math.Exp(-(di*di+dj*dj)/2)
		}
	}
	return g
}

// reportMesh answers M420 as the firmware's source does for UBL. With V, it prints the
// map (CSV for T1), whether the mesh is valid and its slot. Nothing here changes the
// mesh: S, L and C aren't simulated, and only answer.
func (p *printer) reportMesh(args string) {
	fields := strings.Fields(args)
	has := func(letter byte) (string, bool) {
		for _, f := range fields {
			if len(f) > 0 && f[0] == letter {
				return f[1:], true
			}
		}
		return "", false
	}
	if _, v := has('V'); v {
		t, _ := has('T')
		valid := true
		var rows []string
		for j := MeshSize - 1; j >= 0; j-- {
			var vals []string
			for i := range MeshSize {
				z := p.mesh[i][j]
				if math.IsNaN(z) {
					valid = false
					vals = append(vals, "NAN")
				} else {
					vals = append(vals, fmt.Sprintf("%.3f", z))
				}
			}
			if t == "1" {
				rows = append(rows, strings.Join(vals, "\t"))
			} else {
				rows = append(rows, " "+strings.Join(vals, "  "))
			}
		}
		p.send("")
		if t == "1" {
			p.send("Bed Topography Report for CSV:")
		} else {
			p.send("Bed Topography Report:")
		}
		p.send("")
		for _, r := range rows {
			p.send("%s", r)
		}
		if valid {
			p.send("Mesh is valid")
		} else {
			p.send("Mesh is invalid")
		}
		p.send("Storage slot: 0")
		if !valid {
			p.send("echo:Invalid mesh.")
		}
	}
	if p.leveling {
		p.send("echo:Bed Leveling ON")
	} else {
		p.send("echo:Bed Leveling OFF")
	}
	p.send("echo:Fade Height 10.00")
	p.ok()
}
