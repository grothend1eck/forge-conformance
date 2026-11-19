package conformance

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCatalogueCannotCountOneCapabilityTwice(t *testing.T) {
	edit := Row{ID: "edit", Cells: []Cell{{Status: Required}}}
	m := Matrix{Family: "writing", Profiles: []string{"text"}, Rows: []Row{edit, edit}}
	if err := m.Overview(io.Discard); err == nil {
		t.Fatal("one passing test would be counted as two capabilities")
	}
	m.Rows = []Row{{ID: "comment", Cells: []Cell{{Status: NotApplicable}}}}
	if err := m.Overview(io.Discard); err == nil {
		t.Fatal("excluding a capability from the denominator needs a reason")
	}
}

func TestReport(t *testing.T) {
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	binary := filepath.Join(t.TempDir(), "fixture.test")
	if out, err := exec.CommandContext(t.Context(), goBin, "test", "-c", "-o", binary, "./testdata").CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	fixture := func(mode string, flags ...string) string {
		t.Helper()
		args := append([]string{"tool", "test2json", "-p", "writing", binary, "-test.v=test2json"}, flags...)
		cmd := exec.CommandContext(t.Context(), goBin, args...)
		cmd.Env = append(os.Environ(), "CONFORMANCE_FIXTURE="+mode)
		out, err := cmd.Output()
		if len(out) == 0 || (err != nil) != strings.HasSuffix(mode, ":fail") {
			t.Fatalf("fixture: %v\n%s", err, out)
		}
		return string(out)
	}
	m := Matrix{Family: "writing", Profiles: []string{"text", "object"}, Rows: []Row{
		{"edit", "Edit", []Cell{{Status: Required}, {Status: Required}}},
		{"comment", "Preserve comments", []Cell{{KnownIssue, "comments lost"}, {NotApplicable, "no comments"}}},
		{"preview", "Preview", []Cell{{}, {Gap, "no preview"}}},
	}}
	object, text := fixture("object"), fixture("text")
	filtered := fixture("text", "-test.skip=^TestCapabilities$/^text$/^edit$/^pick$/^path$")
	if strings.Contains(filtered, "pick/path") || !strings.Contains(filtered, `--- PASS: TestCapabilities/text/edit`) {
		t.Fatalf("filter must omit only the leaf, leaving the group passing:\n%s", filtered)
	}
	for name, stream := range map[string]string{
		"pass": text, "missing": "", "wrong profile": object,
		"nested skip": fixture("text:skip"), "nested fail": fixture("text:fail"), "filtered child": filtered,
		"unknown group": fixture("text:extra"), "missing group": fixture("text:partial"),
		"truncated": text[:strings.LastIndex(strings.TrimSuffix(text, "\n"), "\n")],
		"corrupt":   text[:len(text)-4], "duplicate": text + text,
		"mixed packages": strings.Replace(text, `"Package":"writing"`, `"Package":"other"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			streams := map[string]io.Reader{"object": strings.NewReader(object)}
			if stream != "" {
				streams["text"] = strings.NewReader(stream)
			}
			var b strings.Builder
			err := m.Report(&b, streams)
			if (err == nil) != (name == "pass") {
				t.Fatalf("bad result: %v\n%s", err, &b)
			}
			if name == "filtered child" && !strings.Contains(err.Error(), "missing unfiltered marker") {
				t.Fatalf("filter rejected for the wrong reason: %v", err)
			}
			want := "| **Verified / applicable** | 0/3 | 1/2 |"
			if name == "pass" {
				want = "| **Verified / applicable** | 1/3 | 1/2 |"
			}
			if !strings.Contains(b.String(), want) || strings.Contains(b.String(), "private test output") {
				t.Fatal(b.String())
			}
		})
	}
}
