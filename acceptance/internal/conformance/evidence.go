package conformance

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type Status string

const (
	pass          Status = "pass"
	fail          Status = "fail"
	notExercised  Status = "not_exercised"
	notEnrolled   Status = "not_enrolled"
	NotApplicable Status = "not_applicable"
	Unimplemented Status = "unimplemented"
	KnownIssue    Status = "known_issue"
)

type cell struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type report struct {
	Version      int      `json:"version"`
	Profile      string   `json:"profile"`
	Profiles     []string `json:"profiles"`
	Bootstrap    Status   `json:"bootstrap"`
	Complete     bool     `json:"complete"`
	Capabilities []cell   `json:"capabilities"`
}

func (r *report) finish(all catalog) {
	r.Profiles = slices.Sorted(maps.Keys(all.profiles))
	r.Complete = r.Bootstrap == pass
	missing := maps.Clone(all.capabilities)
	for _, c := range r.Capabilities {
		if c.Status == notExercised {
			r.Complete = false
		}
		delete(missing, c.ID)
	}
	for id := range missing {
		r.Capabilities = append(r.Capabilities, cell{ID: id, Status: notEnrolled})
	}
	slices.SortFunc(r.Capabilities, func(a, b cell) int { return cmp.Compare(a.ID, b.ID) })
}

func (r report) write(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(r)
	return errors.Join(err, f.Close())
}

func checkReportRun(selected, path string) error {
	if selected == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("reporting requires one selected profile and an absolute report path")
	}
	testFlag := func(name string) string { return flag.Lookup("test." + name).Value.String() }
	if testFlag("run") != "" || testFlag("skip") != "" || testFlag("short") == "true" {
		return fmt.Errorf("reporting requires an unfiltered test run (no -run, -skip, or -short)")
	}
	if testFlag("count") != "1" || strings.Contains(testFlag("cpu"), ",") {
		return fmt.Errorf("reporting requires a single test run")
	}
	return nil
}
