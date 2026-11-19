package conformance

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInvalidEnrollment(t *testing.T) {
	setup := func(*testing.T) int { return 1 }
	c := Capability[int]{"check", func(*testing.T, int) {}}
	profile := NewProfile("demo", setup, []Capability[int]{c})
	for name, profiles := range map[string][]Profile{
		"duplicate profiles": {profile, profile},
		"empty profile":      {NewProfile("demo", setup, nil)},
		"duplicate case":     {NewProfile("demo", setup, []Capability[int]{c, c})},
		"no assertion":       {NewProfile("demo", setup, []Capability[int]{{ID: "check"}})},
		"invalid name":       {NewProfile("demo", setup, []Capability[int]{{"", c.Run}})},
		"unreasoned N/A":     {NewProfile("demo", setup, []Capability[int]{c}, Unavailable{"gap", NotApplicable, ""})},
		"static pass":        {NewProfile("demo", setup, []Capability[int]{c}, Unavailable{"gap", pass, "not evidence"})},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := index(profiles); err == nil {
				t.Fatal("accepted invalid enrollment")
			}
		})
	}
}

const fixtureMode = "UPDATECLI_CONFORMANCE_TEST_MODE"

// Re-exec to observe actual testing.T outcomes. No Docker or credentials.
func TestEvidence(t *testing.T) {
	if os.Getenv(fixtureMode) != "" {
		return
	}
	for _, tc := range []struct {
		mode, flag       string
		bootstrap, check Status
		fails, complete  bool
	}{
		{mode: "parallel", bootstrap: pass, check: pass, complete: true},
		{mode: "cleanup_failure", bootstrap: pass, check: fail, fails: true, complete: true},
		{mode: "required_skip", bootstrap: pass, check: fail, fails: true, complete: true},
		{mode: "raw_skip", bootstrap: pass, check: pass, complete: true},
		{mode: "skip", bootstrap: pass, check: notExercised, fails: true},
		{mode: "bootstrap_failure", bootstrap: fail, check: notExercised, fails: true},
		{mode: "filtered", flag: "-test.run=^TestEvidenceFixture$/demo/check/case", fails: true},
		{mode: "skip_flag", flag: "-test.skip=case", fails: true},
		{mode: "repeat", flag: "-test.count=2", fails: true},
		{mode: "excluded", flag: "-test.run=^$"},
		{mode: "panic", fails: true},
		{mode: "local", flag: "-test.run=^TestEvidenceFixture$/demo/check/case"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evidence.json")
			args := []string{"-test.v"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
			cmd.Env = append(os.Environ(), fixtureMode+"="+tc.mode, "UPDATECLI_CONFORMANCE_TEST_REPORT="+path)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fails || tc.fails && cmd.ProcessState == nil {
				t.Fatalf("got %v, want failure=%t:\n%s", err, tc.fails, output)
			}
			// CI's test2json gate must see this child skip: testing.T exposes
			// the parent's pass, not its raw child's skip, to the runner.
			if tc.mode == "raw_skip" && !strings.Contains(string(output), "--- SKIP: TestEvidenceFixture/demo/check/raw") {
				t.Fatalf("missing native skip event:\n%s", output)
			}
			raw, err := os.ReadFile(path)
			if tc.bootstrap == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected evidence file: %v", err)
				}
				return
			}
			var got report
			if err != nil || json.Unmarshal(raw, &got) != nil || strings.Contains(string(raw), "not-for-report") {
				t.Fatalf("invalid evidence (%v): %s", err, raw)
			}
			if got.Version != 1 || got.Profile != "demo" || !slices.Equal(got.Profiles, []string{"demo", "other"}) ||
				got.Bootstrap != tc.bootstrap || got.Complete != tc.complete {
				t.Fatalf("wrong report header: %s", raw)
			}
			check := cell{ID: "check", Status: tc.check}
			if tc.mode == "skip" {
				check.Reason = "capability skipped"
			}
			want := []cell{check, {"dialect", NotApplicable, "different dialect"}, {"extra", notEnrolled, ""}, {"gap", KnownIssue, "known bug"}}
			if !slices.Equal(got.Capabilities, want) {
				t.Fatalf("wrong capabilities: %s", raw)
			}
		})
	}
}

func TestEvidenceFixture(t *testing.T) {
	mode := os.Getenv(fixtureMode)
	if mode == "" {
		return
	}
	setup := func(t *testing.T) int {
		if mode == "bootstrap_failure" || mode == "filtered" || mode == "skip_flag" || mode == "repeat" {
			t.Fatal("not-for-report")
		}
		return 42
	}
	check := func(t *testing.T, fixture int) {
		if fixture != 42 {
			t.Fatal("wrong fixture")
		}
		switch mode {
		case "parallel":
			t.Parallel()
		case "cleanup_failure":
			t.Cleanup(func() { t.Error("not-for-report") })
		case "required_skip":
			Required(t, "case", func(t *testing.T) { t.Skip("not-for-report") })
		case "raw_skip":
			t.Run("raw", func(t *testing.T) { t.Skip("not-for-report") })
		case "skip":
			t.Skip("not-for-report")
		case "panic":
			panic("not-for-report")
		case "local":
			ran := false
			Required(t, "excluded", func(t *testing.T) { t.Fatal("wanted filtered run") })
			Required(t, "case", func(*testing.T) { ran = true })
			if !ran {
				t.Fatal("selected case did not run")
			}
		}
	}
	demo := NewProfile("demo", setup, []Capability[int]{{"check", check}},
		Unavailable{"gap", KnownIssue, "known bug"}, Unavailable{"dialect", NotApplicable, "different dialect"})
	other := NewProfile("other", func(*testing.T) string { return "" }, []Capability[string]{{"extra", func(*testing.T, string) {}}})
	path := os.Getenv("UPDATECLI_CONFORMANCE_TEST_REPORT")
	if mode == "local" {
		path = ""
	}
	Run(t, "demo", path, other, demo)
}
