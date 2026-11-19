package conformance

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestMatrixValidation(t *testing.T) {
	edit := Row{ID: "edit", Cells: map[string]Cell{"text": {Status: Supported}}}
	for _, test := range []struct {
		name string
		rows []Row
		want string
	}{
		{"duplicate capability", []Row{edit, edit}, "duplicate capability"},
		{"unknown profile", []Row{{ID: "edit", Cells: map[string]Cell{"typo": {Status: Supported}}}}, "unknown profile"},
		{"explicit empty cell", []Row{{ID: "edit", Cells: map[string]Cell{"text": {}}}}, "unknown status"},
		{"gap without reason", []Row{{ID: "edit", Cells: map[string]Cell{"text": {Status: Gap}}}}, "needs a reason"},
		{"not applicable without reason", []Row{{ID: "edit", Cells: map[string]Cell{"text": {NotApplicable, " "}}}}, "needs a reason"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := Matrix{Family: "writing", Profiles: []string{"text"}, Rows: test.rows}
			if err := m.Overview(io.Discard); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Overview error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestImplementations(t *testing.T) {
	m := Matrix{Family: "writing", Profiles: []string{"text", "object", "tree", "stream", "binary"}, Rows: []Row{
		{"edit", "Edit", map[string]Cell{
			"object": {Status: Supported},
			"text":   {Status: Supported},
			"tree":   {Gap, "not implemented"},
			"stream": {NotApplicable, "not seekable"},
		}},
		{"preview", "Preview", nil},
	}}
	got, err := m.Implementations("edit")
	if err != nil || !slices.Equal(got, []string{"text", "object"}) {
		t.Fatalf("Implementations(edit) = %v, %v", got, err)
	}
	if got, err := m.Implementations("preview"); err != nil || len(got) != 0 {
		t.Fatalf("Implementations(preview) = %v, %v; want no supported profiles", got, err)
	}
	if got, err := m.Implementations("typo"); err == nil || len(got) != 0 {
		t.Fatalf("Implementations(typo) = %v, %v; want unknown capability error", got, err)
	}
}

func TestRunUsesOnlySupportedChecks(t *testing.T) {
	m := Matrix{Family: "writing", Profiles: []string{"json"}, Rows: []Row{
		{"edit", "Edit a field", map[string]Cell{"json": {Status: Supported}}},
		{"rename", "Rename a field", map[string]Cell{"json": {Gap, "writer cannot rename fields"}}},
		{"comments", "Preserve comments", map[string]Cell{"json": {NotApplicable, "JSON has no comments"}}},
		{"indent", "Preserve indentation", nil},
	}}
	checks := map[string]func(*testing.T){}
	for _, row := range m.Rows {
		checks[row.ID] = func(t *testing.T) { t.Fatalf("should not run %s", row.ID) }
	}
	ran := false
	checks["edit"] = func(*testing.T) { ran = true }
	m.Run(t, "json", checks)
	if !ran {
		t.Fatal("supported edit did not run")
	}
}

func TestBind(t *testing.T) {
	m := Matrix{Family: "writing", Profiles: []string{"text", "object", "tree", "stream", "binary", "remote"}, Rows: []Row{
		{"edit", "Edit", map[string]Cell{
			"text":   {Status: Supported},
			"object": {Status: Supported},
			"tree":   {Gap, "not implemented"},
			"stream": {NotApplicable, "not seekable"},
		}},
		{"read", "Read", map[string]Cell{"tree": {Status: Supported}}},
		{"preview", "Preview", map[string]Cell{"text": {Status: Supported}, "remote": {Status: Supported}}},
	}}
	drivers := map[string]int{"object": 2, "text": 0, "tree": 3, "stream": 4, "binary": 5}
	for _, test := range []struct {
		id      string
		want    []Implementation[int]
		wantErr string
	}{
		{id: "edit", want: []Implementation[int]{{"text", 0}, {"object", 2}}},
		{id: "read", want: []Implementation[int]{{"tree", 3}}},
		{id: "preview", wantErr: `no driver registered for supported profile "remote"`},
		{id: "typo", wantErr: `unknown capability "typo"`},
	} {
		t.Run(test.id, func(t *testing.T) {
			got, err := Bind(m, test.id, drivers)
			if test.wantErr != "" {
				if got != nil || err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Bind(%q) = %v, %v; want nil result and %q", test.id, got, err, test.wantErr)
				}
			} else if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("Bind(%q) = %v, %v; want %v", test.id, got, err, test.want)
			}
		})
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
	m := Matrix{Family: "writing", Profiles: []string{"text", "object", "pending"}, Rows: []Row{
		{"edit", "Edit", map[string]Cell{"text": {Status: Supported}, "object": {Status: Supported}, "pending": {Gap, "no editor"}}},
		{"read", "Read", map[string]Cell{"text": {Status: Supported}, "object": {Status: Supported}}},
		{"comment", "Preserve comments", map[string]Cell{"text": {Gap, "comments lost"}, "object": {NotApplicable, "no comments"}, "pending": {NotApplicable, "no comments"}}},
		{"preview", "Preview", map[string]Cell{"object": {Gap, "no preview"}}},
	}}
	assertLines := func(t *testing.T, output string, lines ...string) {
		t.Helper()
		for _, line := range lines {
			if !strings.Contains(output, line) {
				t.Errorf("missing %q from:\n%s", line, output)
			}
		}
	}
	var overview strings.Builder
	if err := m.Overview(&overview); err != nil {
		t.Fatal(err)
	}
	assertLines(t, overview.String(),
		"| Edit | supported | supported | gap |",
		"| Read | supported | supported | unassessed |",
		"| Preserve comments | gap | not applicable | not applicable |",
		"| Preview | unassessed | gap | unassessed |",
		"| **Supported / assessed applicable** | 2/3 | 2/3 | 0/1 |",
		"| **Unassessed** | 1 | 0 | 2 |",
		"- Preserve comments / text (gap): comments lost",
		"- Preserve comments / object (not applicable): no comments",
	)
	object, text := fixture("object"), fixture("text")
	failure := fixture("text:fail")
	dropTerminal := func(stream string) string {
		return stream[:strings.LastIndex(strings.TrimSuffix(stream, "\n"), "\n")]
	}
	filtered := fixture("text", "-test.skip=^TestCapabilities$/^text$/^edit$/^pick$/^path$")
	if strings.Contains(filtered, "pick/path") || !strings.Contains(filtered, `--- PASS: TestCapabilities/text/edit`) {
		t.Fatalf("filter must omit only the leaf, leaving the group passing:\n%s", filtered)
	}
	for name, stream := range map[string]string{
		"pass": text, "missing stream": "", "wrong profile": object,
		"nested skip": fixture("text:skip"), "nested fail": failure, "filtered child": filtered,
		"root failure":     fixture("text:root:fail"),
		"unassessed group": fixture("text:extra-preview"), "gap group": fixture("text:extra-comment"),
		"unknown group": fixture("text:extra-typo"), "missing group": fixture("text:partial"),
		"truncated": dropTerminal(text), "truncated failure": dropTerminal(failure),
		"corrupt": text[:len(text)-4], "corrupt after failure": failure + "{", "duplicate": text + text,
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
			want := "0"
			edit, read := "unverified", "unverified"
			switch name {
			case "pass":
				want, edit, read = "2", "pass", "pass"
			case "nested fail":
				edit = "failed"
			case "nested skip":
				edit = "skipped"
			}
			assertLines(t, b.String(),
				"| Edit | "+edit+" | pass | gap |",
				"| Read | "+read+" | pass | unassessed |",
				"| **Verified / supported** | "+want+"/2 | 2/2 | 0/0 |",
				"| **Verified / assessed applicable** | "+want+"/3 | 2/3 | 0/1 |",
				"| **Unassessed** | 1 | 0 | 2 |",
			)
			if strings.Contains(b.String(), "private test output") {
				t.Fatal(b.String())
			}
		})
	}
	t.Run("not applicable group", func(t *testing.T) {
		streams := map[string]io.Reader{
			"text":   strings.NewReader(text),
			"object": strings.NewReader(fixture("object:extra-comment")),
		}
		if err := m.Report(io.Discard, streams); err == nil || !strings.Contains(err.Error(), `unexpected test "TestCapabilities/object/comment"`) {
			t.Fatalf("Report error = %v; want unexpected not-applicable test", err)
		}
	})
	t.Run("stream without supported groups", func(t *testing.T) {
		streams := map[string]io.Reader{
			"text":    strings.NewReader(text),
			"object":  strings.NewReader(object),
			"pending": strings.NewReader(fixture("pending")),
		}
		if err := m.Report(io.Discard, streams); err == nil || !strings.Contains(err.Error(), `unexpected test "TestCapabilities/pending/edit"`) {
			t.Fatalf("Report error = %v; want unexpected gap test", err)
		}
	})
}
