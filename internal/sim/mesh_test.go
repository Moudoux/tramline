package sim

import (
	"math"
	"testing"
)

// When each "Probing mesh point N/25." arrived in a real stream, and the position after
// the last, from test-logs/2026-11-04-stream/t8-long/server.log, 18:46:08 to 18:47:11.
var realMeshSeconds = []float64{
	8.875872, 10.530980, 12.841371, 15.147876, 17.451667, 19.761578, 22.064801, 24.611640,
	27.152440, 29.694666, 32.000696, 34.635063, 37.263536, 39.565747, 41.874866, 44.430823,
	46.730326, 49.577485, 51.874145, 54.175152, 56.955577, 59.254098, 61.551975, 64.547998,
	67.547665, 71.357036,
}

func TestMeshOrderMatchesTheRealPrinter(t *testing.T) {
	points := MeshOrder(MeshStart)
	if len(points) != 25 {
		t.Fatalf("%d points", len(points))
	}
	seen := map[[2]float64]bool{}
	for _, p := range points {
		seen[[2]float64{p.X, p.Y}] = true
	}
	if len(seen) != 25 {
		t.Errorf("only %d different points", len(seen))
	}
	if p := points[0]; p.X != 115 || p.Y != 115 {
		t.Errorf("the first should be the middle, under the probe after G28: %+v", p)
	}
	// The real G29 ended with the nozzle at X66.50 Y212.00.
	if p := points[24]; p.NozzleX != 66.5 || p.NozzleY != 212 || p.X != 25 || p.Y != 205 {
		t.Errorf("the last should be back-left: %+v", p)
	}
	// Each point's time is the gap to the next line. The first, with no travel, and the
	// last, which includes the end, are left out of the fit.
	for i := 1; i < 24; i++ {
		gap := realMeshSeconds[i+1] - realMeshSeconds[i]
		if d := math.Abs(points[i].Seconds - gap); d > 0.1 {
			t.Errorf("point %d at %+v: %.2f s against %.2f s on the printer", i+1, points[i], points[i].Seconds, gap)
		}
		if points[i].Seconds < 2.2 || points[i].Seconds > 3.1 {
			t.Errorf("point %d takes %.2f s", i+1, points[i].Seconds)
		}
	}
	total := meshEndSeconds
	for _, p := range points {
		total += p.Seconds
	}
	if real := realMeshSeconds[25] - realMeshSeconds[0]; math.Abs(total-real) > 1.5 {
		t.Errorf("%.1f s in all, against %.1f s", total, real)
	}
}
