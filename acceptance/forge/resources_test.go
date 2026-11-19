//go:build acceptance

package forge

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

const (
	resourceInput     = "input.txt"
	resourceOutput    = "selected.txt"
	resourceUntouched = "not selected"
	resourceMissing   = "v77.0.0"
)

func testBranches(t *testing.T, s *server) {
	r := s.newRepo(t)
	for _, branch := range []string{"v1.9.0", "v1.10.0", "v2.0.0", "release/candidate"} {
		r.git(t, "push", "origin", r.base+":refs/heads/"+branch)
	}
	before := r.refs(t)
	branches, resp, err := s.client.Repositories.ListRepoBranches(t.Context(), s.owner, r.name, gitea.ListRepoBranchesOptions{})
	if err != nil {
		s.apiFailure(t, "read fixture branches", resp, err)
	}
	if len(branches) < 4 {
		t.Fatalf("read fixture branches: got %d branches, want at least four", len(branches))
	}
	// With no filter the resource takes the final item in server list order,
	// not the repository's default branch or the most recent Git commit.
	last := branches[len(branches)-1].Name
	if before["refs/heads/"+last] == "" {
		t.Fatalf("API branch %q is absent from remote Git refs", last)
	}
	testResourceSelectors(t, s, r.name, "branch", []resourceSelection{
		{name: "default", want: last},
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^release/`, want: "release/candidate"},
		{name: "latest-exact", kind: "latest", pattern: r.base, want: r.base},
		{name: "no-match", kind: "regex", pattern: `^missing/`, wantError: `no version found matching pattern "^missing/"`},
	})
	testResourceConditions(t, s, r.name, "branch", "branch", "release/candidate")
	testUnsupportedRefTarget(t, s, r.name, "branch", "target not supported for the plugin Gitea branch")
	if got := r.refs(t); !maps.Equal(got, before) {
		t.Fatalf("branch sources/conditions and rejected target changed remote refs: got %v, want %v", got, before)
	}
	runRequired(t, "pagination", func(t *testing.T) { testBranchPagination(t, s) })
}

func testTags(t *testing.T, s *server) {
	r := s.newRepo(t)
	r.git(t, "tag", "v1.9.0", r.base)
	r.git(t, "push", "origin", "refs/tags/v1.9.0")
	// Use a single known ref for the default: the Git refs endpoint's list
	// order is not a portable recency contract.
	runRequired(t, "source/default", func(t *testing.T) {
		before := r.refs(t)
		checkResourceSelection(t, s, r.name, "tag", resourceSelection{want: "v1.9.0"})
		require.Equal(t, before, r.refs(t), "reading the default tag must not change remote refs")
	})
	for _, tag := range []string{"v1.10.0", "v2.0.0", "snapshot/nightly"} {
		r.git(t, "tag", tag, r.base)
	}
	r.git(t, "push", "origin", "--tags")
	before := r.refs(t)
	testResourceSelectors(t, s, r.name, "tag", []resourceSelection{
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^snapshot/`, want: "snapshot/nightly"},
		{name: "latest-exact", kind: "latest", pattern: "v1.9.0", want: "v1.9.0"},
		{name: "no-match", kind: "semver", pattern: "~7", wantError: `no Gitea tags found matching pattern "~7"`},
	})
	testResourceConditions(t, s, r.name, "tag", "tag", "snapshot/nightly")
	testUnsupportedRefTarget(t, s, r.name, "tag", "target not supported for the plugin Gitea Tags")
	if got := r.refs(t); !maps.Equal(got, before) {
		t.Fatalf("tag sources/conditions and rejected target changed remote refs: got %v, want %v", got, before)
	}
}

type resourceSelection struct {
	name      string
	kind      string
	pattern   string
	age       map[string]string
	want      string
	hold      bool
	wantError string
}

func testResourceSelectors(t *testing.T, s *server, repo, kind string, cases []resourceSelection) {
	t.Helper()
	for _, tc := range cases {
		runRequired(t, "source/"+tc.name, func(t *testing.T) {
			checkResourceSelection(t, s, repo, kind, tc)
		})
	}
}

func checkResourceSelection(t *testing.T, s *server, repo, kind string, tc resourceSelection) {
	t.Helper()
	c := newCLI(t, s)
	spec := map[string]any{}
	if tc.kind != "" {
		spec["versionfilter"] = map[string]string{"kind": tc.kind, "pattern": tc.pattern}
	}
	if tc.age != nil {
		spec["age"] = tc.age
	}
	manifest := map[string]any{
		"name":    "Select a Gitea " + kind,
		"sources": map[string]any{"selection": resourceNode(s, repo, kind, spec)},
	}
	if tc.wantError != "" {
		// Leave the source as a leaf so its error isn't masked by skipped dependents.
		res := c.run(t, "apply", manifestYAML(t, manifest))
		if res.ExitCode != 1 {
			t.Fatalf("source failure: apply exited %d, want 1", res.ExitCode)
		}
		requireOutput(t, res.Output, tc.wantError)
		return
	}

	writeResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
	condition := resourceNode(s, repo, kind, nil)
	condition["sourceid"] = "selection"
	target := resourceFileNode(resourceOutput, "")
	target["sourceid"] = "selection"
	manifest["conditions"] = map[string]any{"exists": condition}
	manifest["targets"] = map[string]any{"record": target}
	out := c.ok(t, "apply", manifestYAML(t, manifest))
	if tc.hold {
		requireOutput(t, out, "matches the age filter yet")
		assertResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
		return
	}
	assertResourceFile(t, c.runner.Dir, resourceOutput, tc.want)
}

func testResourceConditions(t *testing.T, s *server, repo, kind, field, present string) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		explicit   string
		fed        bool
		failWhen   bool
		wantPassed bool
	}{
		{name: "explicit-overrides-source", explicit: present, fed: true, wantPassed: true},
		{name: "source-missing", fed: true},
		{name: "failwhen-missing", explicit: resourceMissing, failWhen: true, wantPassed: true},
		{name: "failwhen-present", explicit: present, failWhen: true},
	} {
		runRequired(t, "condition/"+tc.name, func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
			spec := map[string]any{}
			if tc.explicit != "" {
				spec[field] = tc.explicit
			}
			condition := resourceNode(s, repo, kind, spec)
			condition["failwhen"] = tc.failWhen
			target := resourceFileNode(resourceOutput, "passed")
			target["disablesourceinput"] = true
			manifest := map[string]any{
				"name":       "Gate a local write with Gitea " + kind,
				"conditions": map[string]any{"exists": condition},
				"targets":    map[string]any{"check": target},
			}
			if tc.fed {
				writeResourceFile(t, c.runner.Dir, resourceInput, resourceMissing)
				manifest["sources"] = map[string]any{"input": resourceFileNode(resourceInput, "")}
				condition["sourceid"] = "input"
			} else {
				condition["disablesourceinput"] = true
			}
			// An unmet condition gates the target; it is not a CLI execution error.
			c.ok(t, "apply", manifestYAML(t, manifest))
			want := resourceUntouched
			if tc.wantPassed {
				want = "passed"
			}
			assertResourceFile(t, c.runner.Dir, resourceOutput, want)
		})
	}
}

func testUnsupportedRefTarget(t *testing.T, s *server, repo, kind, wantError string) {
	t.Helper()
	runRequired(t, "target/unsupported", func(t *testing.T) {
		c := newCLI(t, s)
		target := resourceNode(s, repo, kind, map[string]any{kind: resourceMissing})
		target["disablesourceinput"] = true
		manifest := manifestYAML(t, map[string]any{
			"name":    "Reject an unsupported Gitea target",
			"targets": map[string]any{"unsupported": target},
		})
		for _, mode := range []string{"diff", "apply"} {
			res := c.run(t, mode, manifest)
			if res.ExitCode != 1 {
				t.Fatalf("unsupported target: %s exited %d, want 1", mode, res.ExitCode)
			}
			requireOutput(t, res.Output, wantError)
		}
	})
}

func resourceNode(s *server, repo, kind string, extra map[string]any) map[string]any {
	spec := map[string]any{
		"url": s.url, "owner": s.owner, "repository": repo,
	}
	maps.Copy(spec, extra)
	return map[string]any{"kind": "gitea/" + kind, "spec": spec}
}

func resourceFileNode(file, content string) map[string]any {
	spec := map[string]any{"file": file}
	if content != "" {
		spec["content"] = content
	}
	return map[string]any{"kind": "file", "spec": spec}
}

func writeResourceFile(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertResourceFile(t *testing.T, dir, file, want string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("%s did not contain the expected value %q", file, want)
	}
}
