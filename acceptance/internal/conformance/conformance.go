// Package conformance checks native Go test events against a capability matrix.
package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

type Matrix struct {
	Family   string
	Test     string // root test; defaults to TestCapabilities
	Profiles []string
	Rows     []Row
}

type Row struct {
	ID, Label string
	Cells     map[string]Cell // keyed by profile; an omitted cell is unassessed
}

type Cell struct {
	Status Status
	Reason string // required for Gap and NotApplicable
}

// Status describes an assessed product capability, not a test result.
type Status string

const (
	Supported     Status = "supported"
	Gap           Status = "gap"
	NotApplicable Status = "not applicable"
)

// Implementations returns the supported profiles for a capability in matrix
// order. A known capability can have no supported profiles.
func (m Matrix) Implementations(capabilityID string) ([]string, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	for _, row := range m.Rows {
		if row.ID != capabilityID {
			continue
		}
		var profiles []string
		for _, profile := range m.Profiles {
			if row.Cells[profile].Status == Supported {
				profiles = append(profiles, profile)
			}
		}
		return profiles, nil
	}
	return nil, fmt.Errorf("unknown capability %q in %s", capabilityID, m.Family)
}

// Implementation pairs a supported profile with its test driver.
type Implementation[T any] struct {
	Name   string
	Driver T
}

// Bind returns drivers for the capability's supported profiles in matrix order.
// Every supported profile must have a registered driver; other entries are ignored.
// Drivers are taken as-is, including zero values.
func Bind[T any](m Matrix, capabilityID string, drivers map[string]T) ([]Implementation[T], error) {
	profiles, err := m.Implementations(capabilityID)
	if err != nil {
		return nil, err
	}
	bound := make([]Implementation[T], 0, len(profiles))
	for _, profile := range profiles {
		driver, ok := drivers[profile]
		if !ok {
			return nil, fmt.Errorf("%s/%s: no driver registered for supported profile %q", m.Family, capabilityID, profile)
		}
		bound = append(bound, Implementation[T]{Name: profile, Driver: driver})
	}
	return bound, nil
}

// Run uses the catalogue to select this profile's checks. Multiple sets let
// an implementation combine shared contracts with its own integration tests.
func (m Matrix) Run(t *testing.T, profile string, sets ...map[string]func(*testing.T)) {
	t.Helper()
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Profiles, profile) {
		t.Fatalf("%s: unknown profile %q", m.Family, profile)
	}
	for _, row := range m.Rows {
		if row.Cells[profile].Status != Supported {
			continue
		}
		var check func(*testing.T)
		for _, set := range sets {
			if f, ok := set[row.ID]; ok {
				if check != nil || f == nil {
					t.Fatalf("%s/%s: expected one non-nil check", profile, row.ID)
				}
				check = f
			}
		}
		if check == nil {
			t.Fatalf("%s/%s: supported capability has no check", profile, row.ID)
		}
		t.Run(row.ID, check)
	}
}

// Report writes one grid from single-package Go test streams marked by Observe.
// A profile with no supported capabilities need not supply a stream.
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
	results := make(map[string]profileResult, len(m.Profiles))
	for _, profile := range m.Profiles {
		outcomes, err := m.check(streams[profile], profile)
		results[profile] = profileResult{passed: err == nil, outcomes: outcomes}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", profile, err))
		}
	}
	return errors.Join(append(errs, m.render(w, results))...)
}

type profileResult struct {
	passed   bool
	outcomes map[string]string // explicit failed/skipped outcomes by capability
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
		if row.ID == "" || seen[row.ID] {
			return fmt.Errorf("empty or duplicate capability %q", row.ID)
		}
		seen[row.ID] = true
		for profile, cell := range row.Cells {
			if !slices.Contains(m.Profiles, profile) {
				return fmt.Errorf("%s: unknown profile %q", row.ID, profile)
			}
			switch cell.Status {
			case Supported:
			case Gap, NotApplicable:
				if strings.TrimSpace(cell.Reason) == "" {
					return fmt.Errorf("%s/%s: %s needs a reason", profile, row.ID, cell.Status)
				}
			default:
				return fmt.Errorf("%s/%s: unknown status %q", profile, row.ID, cell.Status)
			}
		}
	}
	return nil
}

func (m Matrix) check(r io.Reader, name string) (map[string]string, error) {
	root := m.Test
	if root == "" {
		root = "TestCapabilities"
	}
	profile := root + "/" + name
	const (
		expected = iota + 1
		running
		passed
		failed
		skipped
	)
	states := map[string]int{"": expected, root: expected, profile: expected}
	hasSupported := false
	for _, row := range m.Rows {
		if row.Cells[name].Status == Supported {
			states[profile+"/"+row.ID] = expected
			hasSupported = true
		}
	}
	if r == nil {
		if hasSupported {
			return nil, fmt.Errorf("missing test stream")
		}
		return nil, nil
	}
	pkg := ""
	unfiltered := false
	var testErr error
	sawTestFailure := false
	outcomes := make(map[string]string)
	dec := json.NewDecoder(r)
	for {
		var e struct{ Action, Package, Test, Key, Value string }
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("malformed test stream: %w", err)
		}
		if pkg == "" && e.Action == "start" && e.Test == "" && e.Package != "" {
			pkg = e.Package
			continue
		}
		if pkg == "" || e.Package != pkg || states[""] >= passed {
			return nil, fmt.Errorf("missing start, mixed packages, or events after package result")
		}
		group, id := e.Test, ""
		if tail, ok := strings.CutPrefix(e.Test, profile+"/"); ok {
			id, _, _ = strings.Cut(tail, "/")
			group = profile + "/" + id
		}
		if states[group] == 0 {
			return nil, fmt.Errorf("unexpected test %q", e.Test)
		}
		switch e.Action {
		case "run":
			if states[e.Test] >= running || e.Test == "" {
				return nil, fmt.Errorf("duplicate or invalid run for %q", e.Test)
			}
			states[e.Test] = running
		case "pass", "skip", "fail":
			if e.Test != "" && (states[e.Test] != running || states[group] != running) {
				return nil, fmt.Errorf("%s without a unique run for %q", e.Action, e.Test)
			}
			if e.Action != "fail" && ((e.Test == "" && sawTestFailure) ||
				(id != "" && e.Test == group && outcomes[id] == "failed")) {
				return nil, fmt.Errorf("%s contradicts test failure for %q", e.Action, e.Test)
			}
			switch e.Action {
			case "pass":
				states[e.Test] = passed
			case "skip":
				states[e.Test] = skipped
				if id != "" && outcomes[id] == "" {
					outcomes[id] = "skipped"
				}
			case "fail":
				states[e.Test] = failed
				sawTestFailure = sawTestFailure || e.Test != ""
				if id != "" {
					outcomes[id] = "failed"
				}
			}
			if e.Action != "pass" && testErr == nil {
				testErr = fmt.Errorf("%q: %s", e.Test, e.Action)
			}
		case "attr":
			if e.Test == root && e.Key == markerKey && e.Value == markerValue {
				if unfiltered || states[root] != running {
					return nil, fmt.Errorf("duplicate or misplaced unfiltered marker for %q", root)
				}
				unfiltered = true
			}
		case "output", "pause", "cont":
		default:
			return nil, fmt.Errorf("unexpected test action %q", e.Action)
		}
	}
	// Negative outcomes are only attributed once the stream is complete and
	// qualified. A failure outside a capability never labels a row.
	if states[""] < passed {
		return nil, fmt.Errorf("missing terminal package result")
	}
	if !unfiltered {
		return nil, fmt.Errorf("missing unfiltered marker for %q", root)
	}
	var missing error
	for test, state := range states {
		switch state {
		case running:
			return nil, fmt.Errorf("missing terminal outcome for %q", test)
		case expected:
			if missing == nil {
				missing = fmt.Errorf("missing pass for %q", test)
			}
		}
	}
	if testErr != nil {
		return outcomes, testErr
	}
	return nil, missing
}
