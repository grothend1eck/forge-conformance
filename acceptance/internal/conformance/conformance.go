// Package conformance runs typed acceptance profiles as ordinary Go subtests.
package conformance

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type Capability[F any] struct {
	ID  string
	Run func(*testing.T, F)
}

// Unavailable records a known gap or inapplicability, not a runtime outcome.
type Unavailable cell

type Profile struct {
	id    string
	cells []cell
	run   func(*testing.T, *report)
	err   error
}

// NewProfile binds a serial fixture setup to the capabilities that use it.
// Setup may register cleanup on the profile's *testing.T, but must not call
// t.Parallel. Capabilities may run in parallel if the fixture supports it.
func NewProfile[F any](id string, setup func(*testing.T) F, capabilities []Capability[F], unavailable ...Unavailable) Profile {
	p := Profile{id: id}
	if setup == nil || len(capabilities) == 0 {
		p.err = fmt.Errorf("profile %q needs a fixture setup and at least one enrolled capability", id)
	}
	capabilities = slices.Clone(capabilities)
	for _, c := range capabilities {
		if c.Run == nil {
			p.err = fmt.Errorf("profile %q: capability %q has no test", id, c.ID)
		}
		p.cells = append(p.cells, cell{ID: c.ID, Status: notExercised})
	}
	for _, u := range unavailable {
		if u.Status != NotApplicable && u.Status != Unimplemented && u.Status != KnownIssue {
			p.err = fmt.Errorf("profile %q: invalid unavailable status for %q", id, u.ID)
		}
		if strings.TrimSpace(u.Reason) == "" {
			p.err = fmt.Errorf("profile %q: unavailable capability %q needs a reason", id, u.ID)
		}
		p.cells = append(p.cells, cell(u))
	}
	p.run = func(t *testing.T, r *report) {
		t.Helper()
		r.Bootstrap = fail
		fixture := setup(t)
		if t.Failed() {
			return
		}
		r.Bootstrap = pass
		for i, c := range capabilities {
			// Each subtest owns its cell; no shared writes between siblings.
			required(t, c.ID, &r.Capabilities[i], func(t *testing.T) { c.Run(t, fixture) })
		}
	}
	return p
}

// Required runs a subtest that must not skip. Ordinary Go filtering still works
// when not collecting evidence.
func Required(t *testing.T, name string, run func(*testing.T)) {
	t.Helper()
	required(t, name, nil, run)
}

func required(t *testing.T, name string, c *cell, run func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Cleanup(func() {
			if t.Skipped() {
				if c != nil {
					c.Reason = "capability skipped"
				}
				t.Error("enrolled acceptance test was skipped")
			} else if c != nil {
				c.Status = pass
				if t.Failed() {
					c.Status = fail
				}
			}
		})
		run(t)
	})
}

var validID = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type catalog struct {
	profiles, capabilities map[string]bool
}

func index(profiles []Profile) (catalog, error) {
	if len(profiles) == 0 {
		return catalog{}, fmt.Errorf("no profiles registered")
	}
	all := catalog{profiles: make(map[string]bool), capabilities: make(map[string]bool)}
	for _, p := range profiles {
		if p.err != nil {
			return catalog{}, p.err
		}
		if !validID.MatchString(p.id) || all.profiles[p.id] {
			return catalog{}, fmt.Errorf("invalid or duplicate profile ID %q", p.id)
		}
		all.profiles[p.id] = true
		cells := make(map[string]bool)
		for _, c := range p.cells {
			if !validID.MatchString(c.ID) || cells[c.ID] {
				return catalog{}, fmt.Errorf("profile %q: invalid or duplicate capability ID %q", p.id, c.ID)
			}
			cells[c.ID], all.capabilities[c.ID] = true, true
		}
	}
	return all, nil
}

// Run selects a profile (empty selects all). Collecting evidence additionally
// requires a single profile, no test filters, and a new absolute report path.
// The caller must also check Go's terminal test events: testing.T does not
// expose raw nested skips to a parent test.
func Run(t *testing.T, selected, path string, profiles ...Profile) {
	t.Helper()
	all, err := index(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		if err := checkReportRun(selected, path); err != nil {
			t.Fatal(err)
		}
	}
	if selected != "" && !all.profiles[selected] {
		t.Fatalf("no profile matches %q", selected)
	}
	for _, p := range profiles {
		if selected != "" && selected != p.id {
			continue
		}
		r := report{Version: 1, Profile: p.id, Bootstrap: notExercised, Capabilities: slices.Clone(p.cells)}
		Required(t, p.id, func(t *testing.T) { p.run(t, &r) })
		// A serial profile waits for its children, even when individual
		// capabilities are parallel. A crash does not reach the writer.
		if path != "" {
			r.finish(all)
			if err := r.write(path); err != nil {
				t.Errorf("write acceptance report: %v", err)
			}
			if !r.Complete {
				t.Error("profile evidence is incomplete")
			}
		}
	}
}
