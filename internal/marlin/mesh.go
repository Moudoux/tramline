package marlin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// meshPointPrefix starts the line UBL's G29 P1 prints as it begins each point of the mesh,
// as in "Probing mesh point 7/25.". It's printed before the head moves there, and no
// height follows. See notes/probe-view.md.
const meshPointPrefix = "Probing mesh point "

// ParseMeshPoint reads "Probing mesh point N/M." into N and M.
func ParseMeshPoint(line string) (point, of int, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(line), meshPointPrefix)
	if !found {
		return 0, 0, false
	}
	n, m, found := strings.Cut(strings.TrimSuffix(rest, "."), "/")
	if !found {
		return 0, 0, false
	}
	point, err1 := strconv.Atoi(n)
	of, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || of <= 0 || point < 1 || point > of {
		return 0, 0, false
	}
	return point, of, true
}

// MeshCommand prints the mesh UBL holds now, as numbers separated by tabs. In this
// firmware's source (M420.cpp, MRiscoC tags 20231202 and 20240122, either side of the
// printer's build), M420 with no S, L or C changes nothing: V prints the map through
// display_map, T1 makes it CSV, and leveling is set back to the state it was already in,
// which does nothing. G29 T1 prints the same map, but G29 is the command that probes, so
// it isn't used. See notes/firmware.md.
const MeshCommand = "M420 V1 T1"

// MeshInfo asks for the mesh the firmware holds now. Nothing moves. After power-on that's
// the mesh loaded from its storage slot at boot, and after a print, until the printer is
// turned off, the one that print's G29 P1 made.
func (p *Printer) MeshInfo(ctx context.Context) ([]string, error) {
	link, err := p.currentLink()
	if err != nil {
		return nil, err
	}
	lines, err := link.Exchange(ctx, MeshCommand, infoIdle)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", MeshCommand, err)
	}
	return lines, nil
}

// MeshReport is the reply to MeshCommand, read.
type MeshReport struct {
	// Z holds the heights in mm, Z[j][i] for the point at column i from the left and
	// row j from the front, nil where the firmware has none (NAN).
	Z [][]*float64 `json:"z"`
	// Valid is the firmware's "Mesh is valid", which needs every point.
	Valid bool `json:"valid"`
	// Slot is the storage slot the firmware has chosen, -1 for none, or nil if unsaid.
	Slot *int `json:"slot,omitempty"`
	// Active is whether leveling is on, nil if unsaid.
	Active *bool `json:"active,omitempty"`
}

// Range gives the lowest and highest heights, or 0 and 0 if there are none.
func (r MeshReport) Range() (lo, hi float64) {
	first := true
	for _, row := range r.Z {
		for _, z := range row {
			if z == nil {
				continue
			}
			if first || *z < lo {
				lo = *z
			}
			if first || *z > hi {
				hi = *z
			}
			first = false
		}
	}
	return lo, hi
}

const meshCSVHeading = "Bed Topography Report for CSV:"

// ParseMeshReport reads the reply to MeshCommand. The firmware prints the rows from the
// back of the bed to the front, so they're turned round here.
func ParseMeshReport(lines []string) (MeshReport, error) {
	var r MeshReport
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == meshCSVHeading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return r, errors.New("no mesh in the reply")
	}
	var rows [][]*float64
	i := start
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			if len(rows) == 0 {
				continue
			}
			break
		}
		fields := strings.Fields(line)
		row := make([]*float64, len(fields))
		ok := true
		for k, f := range fields {
			if strings.EqualFold(f, "nan") {
				continue
			}
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				ok = false
				break
			}
			row[k] = &v
		}
		if !ok {
			break
		}
		if len(rows) > 0 && len(row) != len(rows[0]) {
			return r, fmt.Errorf("mesh rows of %d and %d points", len(rows[0]), len(row))
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return r, errors.New("the mesh has no rows")
	}
	for j := len(rows) - 1; j >= 0; j-- {
		r.Z = append(r.Z, rows[j])
	}
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "echo:"))
		switch {
		case line == "Mesh is valid":
			r.Valid = true
		case strings.HasPrefix(line, "Storage slot:"):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Storage slot:"))); err == nil {
				r.Slot = &n
			}
		case line == "Bed Leveling ON" || line == "Bed Leveling OFF":
			on := line == "Bed Leveling ON"
			r.Active = &on
		}
	}
	return r, nil
}
