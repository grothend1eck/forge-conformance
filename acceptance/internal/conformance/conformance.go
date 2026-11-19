// Package conformance checks native Go test events against a capability matrix.
package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

type Matrix struct {
	Family   string
	Test     string // root test; defaults to TestCapabilities
	Profiles []string
	Rows     []Row
}

type Row struct {
	ID, Label string
	Cells     []Cell // same order as Matrix.Profiles
}

type Cell struct {
	Status Status
	Reason string
}

type Status string

const (
	Required      Status = "required"
	Gap           Status = "gap"
	KnownIssue    Status = "known issue"
	NotApplicable Status = "N/A"
	NotEnrolled   Status = ""
)

// Report writes one grid from single-package Go test streams marked by Observe.
func (m Matrix) Report(w io.Writer, streams map[string]io.Reader) error {
	if err := m.validate(); err != nil {
		return err
	}
	var errs []error
	for profile := range streams {
		if !slices.Contains(m.Profiles, profile) {
			errs = append(errs, fmt.Errorf("unexpected profile %q", profile))
		}
	}
	passed := make([]bool, len(m.Profiles))
	for i, profile := range m.Profiles {
		err := m.check(streams[profile], i)
		passed[i] = err == nil
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", profile, err))
		}
	}
	return errors.Join(append(errs, m.render(w, passed))...)
}

func (m Matrix) validate() error {
	if m.Family == "" || len(m.Profiles) == 0 || len(m.Rows) == 0 {
		return fmt.Errorf("empty conformance matrix")
	}
	for i, p := range m.Profiles {
		if p == "" || slices.Contains(m.Profiles[:i], p) {
			return fmt.Errorf("empty or duplicate profile %q", p)
		}
	}
	seen := make(map[string]bool)
	for _, row := range m.Rows {
		if row.ID == "" || seen[row.ID] || len(row.Cells) != len(m.Profiles) {
			return fmt.Errorf("expected a unique capability and one cell per profile: %q", row.ID)
		}
		seen[row.ID] = true
		for i, cell := range row.Cells {
			switch cell.Status {
			case Required, NotEnrolled:
			case Gap, KnownIssue, NotApplicable:
				if cell.Reason != "" {
					continue
				}
				return fmt.Errorf("%s/%s: %s needs a reason", m.Profiles[i], row.ID, cell.Status)
			default:
				return fmt.Errorf("%s/%s: unknown status %q", m.Profiles[i], row.ID, cell.Status)
			}
		}
	}
	return nil
}

func (m Matrix) check(r io.Reader, i int) error {
	if r == nil {
		return fmt.Errorf("missing test stream")
	}
	root := m.Test
	if root == "" {
		root = "TestCapabilities"
	}
	profile := root + "/" + m.Profiles[i]
	const expected, running, passed = 1, 2, 3
	states := map[string]int{"": expected, root: expected, profile: expected}
	for _, row := range m.Rows {
		if row.Cells[i].Status == Required {
			states[profile+"/"+row.ID] = expected
		}
	}
	pkg := ""
	unfiltered := false
	dec := json.NewDecoder(r)
	for {
		var e struct{ Action, Package, Test, Key, Value string }
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("malformed test stream: %w", err)
		}
		if pkg == "" && e.Action == "start" && e.Test == "" && e.Package != "" {
			pkg = e.Package
			continue
		}
		if pkg == "" || e.Package != pkg || states[""] == passed {
			return fmt.Errorf("missing start, mixed packages, or events after package pass")
		}
		group := e.Test
		if tail, ok := strings.CutPrefix(e.Test, profile+"/"); ok {
			id, _, _ := strings.Cut(tail, "/")
			group = profile + "/" + id
		}
		if states[group] == 0 {
			return fmt.Errorf("unexpected test %q", e.Test)
		}
		switch e.Action {
		case "skip", "fail":
			return fmt.Errorf("%q: %s", e.Test, e.Action)
		case "run":
			if state := states[e.Test]; state == running || state == passed || e.Test == "" {
				return fmt.Errorf("duplicate or invalid run for %q", e.Test)
			}
			states[e.Test] = running
		case "pass":
			if states[e.Test] != running && e.Test != "" {
				return fmt.Errorf("pass without a unique run for %q", e.Test)
			}
			states[e.Test] = passed
		case "attr":
			if e.Test == root && e.Key == markerKey && e.Value == markerValue {
				if unfiltered || states[root] != running {
					return fmt.Errorf("duplicate or misplaced unfiltered marker for %q", root)
				}
				unfiltered = true
			}
		case "output", "pause", "cont":
		default:
			return fmt.Errorf("unexpected test action %q", e.Action)
		}
	}
	if states[""] != passed {
		return fmt.Errorf("missing terminal package pass")
	}
	for test, state := range states {
		if state != passed {
			return fmt.Errorf("missing pass for %q", test)
		}
	}
	if !unfiltered {
		return fmt.Errorf("missing unfiltered marker for %q", root)
	}
	return nil
}
