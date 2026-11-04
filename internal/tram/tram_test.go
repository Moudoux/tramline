package tram

import "testing"

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		z      [4]float64
		pass   bool
		corner string
		action string
	}{
		{"flat", [4]float64{1, 1, 1, 1}, true, "", ""},
		{"just inside", [4]float64{0.100, 0.149, 0.100, 0.100}, true, "", ""},
		{"exactly the limit fails", [4]float64{0.250, 0.300, 0.250, 0.250}, false, "front-right", "lower"},
		{"high corner is lowered", [4]float64{0, 0, 0.3, 0}, false, "back-right", "lower"},
		{"low corner is raised", [4]float64{0.3, 0.3, 0.3, 0}, false, "back-left", "raise"},
		{"offset doesn't matter", [4]float64{-5.3, -5.3, -5, -5.3}, false, "back-right", "lower"},
		{"tie uses wizard order", [4]float64{0.2, 0, 0, 0.2}, false, "front-left", "lower"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.z)
			if got.Pass != tt.pass || got.Corner != tt.corner || got.Action != tt.action {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestParseProbe(t *testing.T) {
	got, err := ParseProbe([]string{
		"echo:busy: processing",
		"Bed X:25.00 Y:30.50 Z:-0.125",
		"X:25.00 Y:30.50 Z:10.00 E:0.00 Count X:0 Y:0 Z:0",
		"ok P15 B15",
	})
	if err != nil || got != (Reading{X: 25, Y: 30.5, Z: -0.125}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, lines := range [][]string{
		{"echo:Unknown command", "ok"},
		{"Error:Probing Failed", "X:25.00 Y:25.00 Z:10.00", "ok"},
		{"Z Probe Past Bed", "ok"},
		{"Bed X:25.00 Y:25.00 Z:nan", "ok"},
		{"Bed X:25.00 Y:25.00 Z:1.2.3", "ok"},
	} {
		if _, err := ParseProbe(lines); err == nil {
			t.Errorf("%q: expected an error", lines)
		}
	}
}

func TestPointsAreInTheWizardsOrder(t *testing.T) {
	want := [4]Point{{FrontLeft, 30, 30}, {FrontRight, 200, 30}, {BackRight, 200, 200}, {BackLeft, 30, 200}}
	if got := Points(30); got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestSpread(t *testing.T) {
	got := Spread([][4]float64{{0.10, 0.2, 0.3, 0.4}, {0.12, 0.2, 0.25, 0.4}, {0.11, 0.2, 0.3, 0.4}})
	if want := [4]float64{0.02, 0, 0.05, 0}; got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := Spread(nil); got != [4]float64{} {
		t.Fatalf("no rounds: %v", got)
	}
}
