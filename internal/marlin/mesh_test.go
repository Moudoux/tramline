package marlin

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseMeshPoint(t *testing.T) {
	for _, c := range []struct {
		line      string
		point, of int
		ok        bool
	}{
		// As the real printer prints them, see notes/probe-view.md.
		{"Probing mesh point 1/25.", 1, 25, true},
		{"Probing mesh point 17/25.", 17, 25, true},
		{"Probing mesh point 25/25.", 25, 25, true},
		{" Probing mesh point 3/25.\r", 3, 25, true},
		{"Mesh invalidated. Probing mesh.", 0, 0, false},
		{"Probing mesh point 26/25.", 0, 0, false},
		{"Probing mesh point 0/25.", 0, 0, false},
		{"Probing mesh point x/25.", 0, 0, false},
		{"Probing mesh point 3", 0, 0, false},
		{"echo:Probing mesh point 3/25.", 0, 0, false},
	} {
		point, of, ok := ParseMeshPoint(c.line)
		if point != c.point || of != c.of || ok != c.ok {
			t.Errorf("%q: got %d/%d %v", c.line, point, of, ok)
		}
	}
}

// meshReply is what M420 V1 T1 should print, worked out from the firmware's source
// (display_map in ubl.cpp, M420.cpp). It hasn't been seen on the real printer yet.
var meshReply = []string{
	"",
	"Bed Topography Report for CSV:",
	"",
	"0.120\t0.080\t0.050\t0.030\t0.010",
	"0.090\t0.060\t0.040\t0.020\t0.000",
	"0.060\t0.040\tNAN\t0.010\t-0.010",
	"0.030\t0.020\t0.010\t0.000\t-0.020",
	"0.000\t-0.010\t-0.020\t-0.030\t-0.045",
	"Mesh is invalid",
	"Storage slot: 0",
	"echo:Invalid mesh.",
	"echo:Bed Leveling OFF",
	"echo:Fade Height 10.00",
	"ok P15 B15",
}

func TestParseMeshReport(t *testing.T) {
	r, err := ParseMeshReport(meshReply)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Z) != 5 || len(r.Z[0]) != 5 {
		t.Fatalf("got %d rows", len(r.Z))
	}
	// Row 0 is the front, printed last.
	if z := r.Z[0][4]; z == nil || *z != -0.045 {
		t.Errorf("front-right: %v", z)
	}
	if z := r.Z[4][0]; z == nil || *z != 0.12 {
		t.Errorf("back-left: %v", z)
	}
	if r.Z[2][2] != nil {
		t.Errorf("NAN should be nil, got %v", *r.Z[2][2])
	}
	if r.Valid || r.Slot == nil || *r.Slot != 0 || r.Active == nil || *r.Active {
		t.Errorf("got valid %v slot %v active %v", r.Valid, r.Slot, r.Active)
	}

	valid := append([]string(nil), meshReply...)
	valid[5] = "0.060\t0.040\t0.030\t0.010\t-0.010"
	valid[9] = "Mesh is valid"
	valid[11] = "echo:Bed Leveling ON"
	r, err = ParseMeshReport(valid)
	if err != nil || !r.Valid || r.Active == nil || !*r.Active {
		t.Errorf("valid mesh: %+v, %v", r, err)
	}

	for _, bad := range [][]string{
		{"ok"},
		{"Bed Topography Report for CSV:", "", "Mesh is valid"},
		{"Bed Topography Report for CSV:", "0.1\t0.2", "0.1\t0.2\t0.3"},
	} {
		if _, err := ParseMeshReport(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// During a print from the card, G29 runs while the server's M27 waits behind it, so the
// mesh lines would arrive in the middle of that wait. They still have to reach the server.
func TestMeshLinesDuringAnM27WaitReachTheServer(t *testing.T) {
	host, device := net.Pipe()
	told := make(chan string, 4)
	link := newLink(host, nil, func(line string) { told <- line }, time.Second)
	defer link.Close()
	go func() {
		r := bufio.NewReader(device)
		cmd, _ := r.ReadString('\n')
		if !strings.Contains(cmd, "M27") {
			t.Errorf("sent %q", cmd)
		}
		device.Write([]byte("Probing mesh point 3/25.\nProbing mesh point 4/25.\nSD printing byte 10/100\nok\n"))
	}()
	lines, err := link.Exchange(context.Background(), "M27", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "|"), "SD printing byte 10/100") {
		t.Errorf("reply %q", lines)
	}
	for _, want := range []string{"Probing mesh point 3/25.", "Probing mesh point 4/25."} {
		select {
		case got := <-told:
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q wasn't passed on", want)
		}
	}
}
