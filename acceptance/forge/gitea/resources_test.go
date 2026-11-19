//go:build acceptance

package gitea_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

const (
	resourceInput     = "input.txt"
	resourceOutput    = "selected.txt"
	resourceUntouched = "not selected"
	resourceMissing   = "v77.0.0"
	resourcePageSize  = 30
)

func (s *server) testBranches(t *testing.T) {
	r := s.newRepo(t)
	push := []string{"push", "origin"}
	for _, branch := range []string{"v1.9.0", "v1.10.0", "v2.0.0", "release/candidate"} {
		push = append(push, r.Base+":refs/heads/"+branch)
	}
	r.Git(t, push...)
	before := r.Refs(t)
	branches := readResourceBranches(t, s, r.name, gitea.ListOptions{})
	require.GreaterOrEqual(t, len(branches), 4)
	// With no filter the resource takes the final item in server list order,
	// not the repository's default branch or the most recent Git commit.
	last := branches[len(branches)-1].Name
	require.NotEmpty(t, before["refs/heads/"+last], "API branch %q must exist as a remote Git ref", last)
	t.Run("source/default", func(t *testing.T) {
		// The legacy branch source also included a branch field. It belongs
		// to conditions and does not override the default source selection.
		const explicit = "main"
		require.NotEmpty(t, before["refs/heads/"+explicit])
		require.NotEqual(t, last, explicit)
		checkResourceSelection(t, publicResourceCLI(t, r), s, r.name, "branch", resourceSelection{
			spec: forgeMap{"branch": explicit}, want: last,
		})
	})
	testResourceSelectors(t, s, r.name, "branch", []resourceSelection{
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^release/`, want: "release/candidate"},
		{name: "latest-exact", kind: "latest", pattern: r.Base, want: r.Base},
		{name: "no-match", kind: "regex", pattern: `^missing/`, wantError: `no version found matching pattern "^missing/"`},
	})
	testResourceConditions(t, s, r.name, "branch", "branch", "release/candidate")
	testMissingResourceRepository(t, s, "branch", "branch", "searching gitea branches: Not Found", "Not Found")
	testUnsupportedRefTarget(t, s, r.name, "branch", "target not supported for the plugin Gitea branch")
	require.Equal(t, before, r.Refs(t), "branch reads and rejected target must not change refs")
	t.Run("pagination", func(t *testing.T) { testBranchPagination(t, s) })
}

func (s *server) testTags(t *testing.T) {
	r := s.newRepo(t)
	r.Git(t, "tag", "v1.9.0", r.Base)
	r.Git(t, "push", "origin", "refs/tags/v1.9.0")
	// Use a single known ref for the default: the Git refs endpoint's list
	// order is not a portable recency contract.
	t.Run("source/single-tag", func(t *testing.T) {
		before := r.Refs(t)
		checkResourceSelection(t, newCLI(t, s), s, r.name, "tag", resourceSelection{want: "v1.9.0"})
		require.Equal(t, before, r.Refs(t), "reading the default tag must not change remote refs")
	})
	for _, tag := range []string{"v1.10.0", "v2.0.0", "snapshot/nightly"} {
		r.Git(t, "tag", tag, r.Base)
	}
	r.Git(t, "push", "origin", "--tags")
	before := r.Refs(t)
	t.Run("source/default", func(t *testing.T) {
		c := publicResourceCLI(t, r)
		tags, resp, err := s.sdkClient(t).Git.GetRepoRefs(t.Context(), s.owner, r.name, "tags")
		s.check(t, "list public tag refs", resp, err)
		require.Len(t, tags, 4)
		lastRef := tags[len(tags)-1].Ref
		last, ok := strings.CutPrefix(lastRef, "refs/tags/")
		require.True(t, ok)
		require.NotEmpty(t, before[lastRef], "selected API tag must exist as a remote Git ref")
		checkResourceSelection(t, c, s, r.name, "tag", resourceSelection{want: last})
	})
	testResourceSelectors(t, s, r.name, "tag", []resourceSelection{
		{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
		{name: "regex", kind: "regex", pattern: `^snapshot/`, want: "snapshot/nightly"},
		{name: "latest-exact", kind: "latest", pattern: "v1.9.0", want: "v1.9.0"},
		{name: "no-match", kind: "semver", pattern: "~7", wantError: `no Gitea tags found matching pattern "~7"`},
	})
	testResourceConditions(t, s, r.name, "tag", "tag", "snapshot/nightly")
	testMissingResourceRepository(t, s, "tag", "tag", "search gitea tags: Not Found", "looking for Gitea tag: Not Found")
	testUnsupportedRefTarget(t, s, r.name, "tag", "target not supported for the plugin Gitea Tags")
	require.Equal(t, before, r.Refs(t), "tag reads and rejected target must not change refs")
}

type resourceSelection struct {
	name, kind, pattern, want, wantError string
	spec                                 forgeMap
	age                                  map[string]string
	hold                                 bool
}

func testResourceSelectors(t *testing.T, s *server, repo, kind string, cases []resourceSelection) {
	t.Helper()
	for _, tc := range cases {
		t.Run("source/"+tc.name, func(t *testing.T) {
			checkResourceSelection(t, newCLI(t, s), s, repo, kind, tc)
		})
	}
}

func checkResourceSelection(t *testing.T, c *cli.Runner, s *server, repo, kind string, tc resourceSelection) {
	t.Helper()
	spec := forgeMap{}
	maps.Copy(spec, tc.spec)
	if tc.kind != "" {
		spec["versionfilter"] = map[string]string{"kind": tc.kind, "pattern": tc.pattern}
	}
	if tc.age != nil {
		spec["age"] = tc.age
	}
	manifest := forgeMap{
		"name":    "Select a Gitea " + kind,
		"sources": forgeMap{"selection": resourceNode(s, repo, kind, spec)},
	}
	if tc.wantError != "" {
		// Leave the source as a leaf so its error isn't masked by skipped dependents.
		res := c.Pipeline(t, "apply", cli.Manifest(t, manifest))
		require.Equal(t, 1, res.ExitCode, "failed source must fail apply")
		cli.RequireOutput(t, res.Output, tc.wantError)
		return
	}

	writeResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
	input := forgeMap{"sourceid": "selection"}
	manifest["conditions"] = forgeMap{"exists": resourceNode(s, repo, kind, nil, input)}
	manifest["targets"] = forgeMap{"record": resourceFileNode(resourceOutput, "", input)}
	out := c.PipelineOK(t, "apply", cli.Manifest(t, manifest))
	if tc.hold {
		cli.RequireOutput(t, out, "matches the age filter yet")
		assertResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
		return
	}
	assertResourceFile(t, c.Dir, resourceOutput, tc.want)
}

func testResourceConditions(t *testing.T, s *server, repo, kind, field, present string) {
	t.Helper()
	for _, tc := range []struct {
		name, explicit            string
		fed, failWhen, wantPassed bool
	}{
		{name: "explicit-without-source", explicit: present, wantPassed: true},
		{name: "explicit-overrides-source", explicit: present, fed: true, wantPassed: true},
		{name: "no-input"},
		{name: "source-missing", fed: true},
		{name: "failwhen-missing", explicit: resourceMissing, failWhen: true, wantPassed: true},
		{name: "failwhen-present", explicit: present, failWhen: true},
	} {
		t.Run("condition/"+tc.name, func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
			spec := forgeMap{}
			if tc.explicit != "" {
				spec[field] = tc.explicit
			}
			condition := resourceNode(s, repo, kind, spec, forgeMap{"failwhen": tc.failWhen})
			manifest := forgeMap{
				"name":       "Gate a local write with Gitea " + kind,
				"conditions": forgeMap{"exists": condition},
				"targets":    forgeMap{"check": resourceFileNode(resourceOutput, "passed", forgeMap{"disablesourceinput": true})},
			}
			input := present
			if tc.fed {
				input = resourceMissing
				condition["sourceid"] = "input"
			} else {
				condition["disablesourceinput"] = true
			}
			writeResourceFile(t, c.Dir, resourceInput, input)
			manifest["sources"] = forgeMap{"input": resourceFileNode(resourceInput, "")}
			// An unmet condition gates the target; it is not a CLI execution error.
			c.PipelineOK(t, "apply", cli.Manifest(t, manifest))
			want := resourceUntouched
			if tc.wantPassed {
				want = "passed"
			}
			assertResourceFile(t, c.Dir, resourceOutput, want)
		})
	}
}

func publicResourceCLI(t *testing.T, r *repository) *cli.Runner {
	t.Helper()
	s := r.server
	_, resp, err := s.client.Repositories.EditRepo(t.Context(), s.owner, r.name, gitea.EditRepoOption{Private: new(false)})
	s.check(t, "make fixture repository public", resp, err)
	t.Cleanup(func() {
		_, resp, err := s.client.Repositories.EditRepo(context.WithoutCancel(t.Context()), s.owner, r.name, gitea.EditRepoOption{Private: new(true)})
		s.check(t, "make fixture repository private again", resp, err)
	})
	return newCLIWithCredentials(t, "", "")
}

func testMissingResourceRepository(t *testing.T, s *server, kind, field, sourceError, conditionError string) {
	t.Helper()
	const repo = "nonexistent-fixture-repository"
	t.Run("missing-repository", func(t *testing.T) {
		_, resp, err := s.client.Repositories.GetRepo(t.Context(), s.owner, repo)
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			s.apiFailure(t, "read nonexistent fixture repository (want HTTP 404)", resp, err)
		}
		t.Run("source", func(t *testing.T) {
			checkResourceSelection(t, newCLI(t, s), s, repo, kind, resourceSelection{wantError: sourceError})
		})
		t.Run("condition", func(t *testing.T) {
			c := newCLI(t, s)
			condition := resourceNode(s, repo, kind, forgeMap{field: resourceMissing}, forgeMap{"disablesourceinput": true, "failwhen": true})
			res := c.Pipeline(t, "apply", cli.Manifest(t, forgeMap{
				"name":       "Do not invert a repository lookup error",
				"conditions": forgeMap{"exists": condition},
			}))
			require.Equal(t, 1, res.ExitCode, "an API error must fail apply even with failwhen")
			cli.RequireOutput(t, res.Output, conditionError)
		})
	})
}

func testUnsupportedRefTarget(t *testing.T, s *server, repo, kind, wantError string) {
	t.Helper()
	t.Run("target/unsupported", func(t *testing.T) {
		c := newCLI(t, s)
		target := resourceNode(s, repo, kind, forgeMap{kind: resourceMissing}, forgeMap{"disablesourceinput": true})
		manifest := singleTargetYAML(t, "Reject an unsupported Gitea target", "unsupported", target)
		for _, mode := range []string{"diff", "apply"} {
			res := c.Pipeline(t, mode, manifest)
			require.Equal(t, 1, res.ExitCode, "unsupported target must fail %s", mode)
			cli.RequireOutput(t, res.Output, wantError)
		}
	})
}

func testBranchPagination(t *testing.T, s *server) {
	t.Helper()
	r := s.newRepo(t)
	push := []string{"push", "origin"}
	// The fixture already has two branches.
	for i := 1; i < resourcePageSize; i++ {
		push = append(push, fmt.Sprintf("main:refs/heads/fixture-%02d", i))
	}
	r.Git(t, push...)
	before := r.Refs(t)
	first := readResourceBranches(t, s, r.name, gitea.ListOptions{Page: 1, PageSize: resourcePageSize})
	second := readResourceBranches(t, s, r.name, gitea.ListOptions{Page: 2, PageSize: resourcePageSize})
	require.Len(t, first, resourcePageSize)
	require.Len(t, second, 1)
	later := second[0].Name
	require.NotEmpty(t, before["refs/heads/"+later], "second page must contain a real Git branch")
	for _, branch := range first {
		require.NotEqual(t, later, branch.Name, "matched branch must be absent from page 1")
	}
	checkResourceSelection(t, newCLI(t, s), s, r.name, "branch", resourceSelection{kind: "latest", pattern: later, want: later})
	require.Equal(t, before, r.Refs(t), "paginated source and condition must not change remote refs")
}

func readResourceBranches(t *testing.T, s *server, repo string, opts gitea.ListOptions) []*gitea.Branch {
	t.Helper()
	branches, resp, err := s.client.Repositories.ListRepoBranches(t.Context(), s.owner, repo, gitea.ListRepoBranchesOptions{ListOptions: opts})
	s.check(t, fmt.Sprintf("read branches (page %d)", opts.Page), resp, err)
	return branches
}

type forgeMap = map[string]any

func forgeNode(kind string, spec forgeMap, fields ...forgeMap) forgeMap {
	node := forgeMap{"kind": kind, "spec": spec}
	for _, extra := range fields {
		maps.Copy(node, extra)
	}
	return node
}

func singleTargetYAML(t *testing.T, name, id string, target forgeMap) string {
	t.Helper()
	return cli.Manifest(t, forgeMap{"name": name, "targets": forgeMap{id: target}})
}

func resourceNode(s *server, repo, kind string, extra forgeMap, fields ...forgeMap) forgeMap {
	spec := forgeMap{"url": s.url, "owner": s.owner, "repository": repo}
	maps.Copy(spec, extra)
	return forgeNode("gitea/"+kind, spec, fields...)
}

func resourceFileNode(file, content string, fields ...forgeMap) forgeMap {
	return forgeNode("file", fileSpec(file, content), fields...)
}

func fileSpec(file, content string) forgeMap {
	spec := forgeMap{"file": file}
	if content != "" {
		spec["content"] = content
	}
	return spec
}

func forceFileNode(file, content string, fields forgeMap) forgeMap {
	spec := fileSpec(file, content)
	spec["forcecreate"] = true
	return forgeNode("file", spec, fields)
}

func writeResourceFile(t *testing.T, dir, file, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600))
}

func readResourceFile(t *testing.T, dir, file string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, file))
	require.NoError(t, err)
	return string(content)
}

func assertResourceFile(t *testing.T, dir, file, want string) {
	t.Helper()
	require.True(t, readResourceFile(t, dir, file) == want, "%s did not contain the expected value %q", file, want)
}
