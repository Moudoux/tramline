package gcode

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

func TestReaderCleansLines(t *testing.T) {
	r := NewReader(strings.NewReader(";FLAVOR:Marlin\r\nG28 ; home\r\n\r\n   \n  G1 X1 Y2  \nM117 no newline"))
	var got []string
	for {
		line, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, line)
	}
	if want := "G28|G1 X1 Y2|M117 no newline"; strings.Join(got, "|") != want {
		t.Fatalf("got %q", got)
	}
}

// The size has to match what the firmware writes: the line, then CR LF.
func TestMeasure(t *testing.T) {
	info, err := Measure(strings.NewReader(";comment\nG28\nG1 X1 ;move\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (Info{Lines: 2, SDSize: 5 + 7}); info != want {
		t.Fatalf("got %+v, want %+v", info, want)
	}
}

func TestMeasureRefusals(t *testing.T) {
	for _, tt := range []struct{ name, in, want string }{
		{"star", "G28\nM117 a*b\n", "line 2"},
		{"backslash", "M117 a\\b\n", `"\"`},
		{"M29", "M29\n", "M29"},
		{"M29 inside", "M117 M29 x\n", "M29"},
		{"M28", "M28 X.GCO\n", "M28"},
		{"M110", "M110 N5\n", "M110"},
		{"control byte", "G1\x08\n", "0x08"},
		{"non-ASCII", "M117 caf\xc3\xa9\n", "0xc3"},
		{"too long", "M117 " + strings.Repeat("x", MaxLine) + "\n", "characters long"},
		{"only comments", ";nothing\n\n", "no commands"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Measure(strings.NewReader(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestMeasureAllowsWhatLooksLikeReserved(t *testing.T) {
	for _, line := range []string{"M290 Z0.1", "M280 P0 S10", "M1100", "G1 X1 ;M29 in a comment"} {
		if _, err := Measure(strings.NewReader(line + "\n")); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
}

func TestLongestLineAllowedFits(t *testing.T) {
	// "N9999999 " + line + "*255" has to stay within the firmware's 95 characters.
	if got := len("N9999999 ") + MaxLine + len("*255"); got > 95 {
		t.Fatalf("a full line is %d characters", got)
	}
}

func TestShortName(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Z0-9]{1,8}\.GCO$`)
	for _, name := range []string{"pi-deck-slide_PETG.gcode", "a.gcode", "---.gco", "123456789.gcode"} {
		got := ShortName(name, 0)
		if !valid.MatchString(got) {
			t.Errorf("%s gave %s", name, got)
		}
		if ShortName(name, 0) != got {
			t.Errorf("%s isn't stable", name)
		}
		if ShortName(name, 1) == got {
			t.Errorf("%s: a new attempt gave the same name", name)
		}
	}
	if got := ShortName("pi-deck-slide_PETG.gcode", 0); !strings.HasPrefix(got, "PIDECK") {
		t.Errorf("got %s", got)
	}
}

func TestHeatLimits(t *testing.T) {
	for _, line := range []string{"M104 S260", "M109 R260", "M140 S100", "M190 S0", "M104", "M106 S255", "G1 X300"} {
		if err := check(line); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	for _, line := range []string{"M104 S261", "M109 S300", "M109 R261", "M140 S101", "M190 S120", "M104 T0 S999"} {
		if err := check(line); err == nil {
			t.Errorf("%q should be refused", line)
		}
	}
}

func TestProgressAddsM73WhenThingsChange(t *testing.T) {
	file := ";TIME:600\nG1 X1\nG1 X2\n;TIME_ELAPSED:300\nG1 X3\n;TIME_ELAPSED:599.5\nG1 X4\n"
	info, err := Measure(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProgress(NewReader(strings.NewReader(file)), info.SDSize)
	var got []string
	for {
		line, err := p.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, line)
	}
	want := "M73 P25 R10|G1 X1|M73 P50 R10|G1 X2|M73 P75 R5|G1 X3|M73 P100 R1|G1 X4"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q", got)
	}
	if p.Bytes() != info.SDSize {
		t.Errorf("%d bytes, want %d", p.Bytes(), info.SDSize)
	}
}

func TestProgressWithoutTimesAndWithFewChanges(t *testing.T) {
	var file strings.Builder
	for range 1000 {
		file.WriteString("G1 X1\n")
	}
	info, err := Measure(strings.NewReader(file.String()))
	if err != nil {
		t.Fatal(err)
	}
	p := NewProgress(NewReader(strings.NewReader(file.String())), info.SDSize)
	count := 0
	for {
		line, err := p.Next()
		if err != nil {
			break
		}
		if strings.Contains(line, " R") {
			t.Fatalf("no time to report: %q", line)
		}
		if strings.HasPrefix(line, "M73") {
			count++
		}
	}
	if count != 101 {
		t.Errorf("%d M73 lines, want one for each percent from 0", count)
	}
}
