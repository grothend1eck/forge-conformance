//go:build acceptance

package forge

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v4"
)

func testWorkflowDiscovery(t *testing.T, s *server) {
	const (
		oldRelease = "v1.0.0"
		newRelease = "v1.1.0"
		oldTag     = "v2.0.0"
		newTag     = "v2.1.0"
		branch     = "action-track"
		decoy      = "zz-action-decoy"
		subdir     = "actions/example"
		composite  = ".gitea/actions/fixture/action.yml"
	)

	action := s.newRepo(t)
	commits := make(map[string]string)
	stage := func(ref, label string) string {
		action.stage(t, ref, subdir+"/action.yml", fmt.Sprintf(`name: Fixture action %s
description: Local forge fixture, not run
runs:
  using: composite
  steps:
    - run: echo fixture
      shell: sh
`, label))
		return strings.TrimSpace(action.git(t, "rev-parse", "HEAD"))
	}
	// Let the release API create each release tag at its own commit. Pushing
	// those tags first can race with the forge's post-receive bookkeeping.
	for _, tag := range []string{oldRelease, newRelease} {
		commits[tag] = stage(action.base, tag)
		_, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, action.name, gitea.CreateReleaseOption{
			TagName: tag,
			Title:   tag,
			Target:  commits[tag],
		})
		if err != nil {
			s.apiFailure(t, "publish fixture release "+tag, resp, err)
		}
	}
	// The tag-only versions outrank the releases in semver order, but should
	// not be selected when a workflow tracks a release.
	for _, tag := range []string{oldTag, newTag} {
		commits[tag] = stage(action.base, tag)
		action.git(t, "tag", "-a", tag, "-m", "Fixture annotated tag "+tag)
	}
	action.git(t, "push", "origin", "refs/tags/"+oldTag, "refs/tags/"+newTag)
	// A branch at the same name but a different commit proves that the tag
	// wins when neither ref is a release.
	shadow := stage(oldTag, "tag shadow")
	commits[branch] = stage(branch, branch)
	commits["main"] = stage("main", "main")
	decoyCommit := stage(decoy, decoy)

	action.git(t, "fetch", "origin", "refs/tags/*:refs/remotes/origin/fixture-tags/*")
	for _, tag := range []string{oldRelease, newRelease, oldTag, newTag} {
		ref := "refs/remotes/origin/fixture-tags/" + tag
		commit := strings.TrimSpace(action.git(t, "rev-parse", ref+"^{commit}"))
		require.Equal(t, commits[tag], commit, "remote tag %s must peel to the fixture commit", tag)
		if tag == oldTag || tag == newTag {
			object := strings.TrimSpace(action.git(t, "rev-parse", ref))
			require.NotEqual(t, object, commit, "fixture needs an annotated tag to distinguish tag object from commit")
		}
	}
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, action.name, gitea.ListReleasesOptions{})
	if err != nil {
		s.apiFailure(t, "list fixture releases", resp, err)
	}
	published := make(map[string]*gitea.Release)
	for _, release := range releases {
		require.False(t, release.IsDraft)
		require.False(t, release.IsPrerelease)
		published[release.TagName] = release
	}
	require.Len(t, releases, 2)
	require.Contains(t, published, oldRelease)
	require.Contains(t, published, newRelease)
	refs := action.refs(t)
	require.Equal(t, shadow, refs["refs/heads/"+oldTag], "fixture needs a branch with the tag's name")
	require.NotEqual(t, shadow, commits[oldTag], "tag and branch must point to different commits")
	require.Equal(t, commits["main"], refs["refs/heads/main"])
	if refs["refs/heads/"+branch] != commits[branch] || refs["refs/heads/"+decoy] != decoyCommit || commits[branch] == decoyCommit {
		t.Fatal("fixture should expose the requested and later-sorting branches at distinct commits")
	}

	endpoint, err := url.Parse(s.url)
	if err != nil || endpoint.Hostname() == "" {
		t.Fatal("invalid forge fixture URL")
	}
	actionURL, err := url.JoinPath(s.url, s.owner, action.name, subdir)
	if err != nil {
		t.Fatal("build fixture action URL")
	}
	manifestFor := func(t *testing.T, crawler string, options map[string]any) string {
		t.Helper()
		// The crawler matches credentials by hostname, not host:port.
		options["credentials"] = map[string]any{endpoint.Hostname(): map[string]any{"kind": s.provider}}
		return manifestYAML(t, map[string]any{
			"name": "Discover local forge actions",
			"autodiscovery": map[string]any{
				"crawlers": map[string]any{crawler: options},
			},
		})
	}

	for _, tc := range []struct {
		crawler string
		digest  bool
	}{
		{"gitea/action", false},
		{"gitea/action", true},
		{"github/action", false},
		{"github/action", true},
	} {
		mode := "version"
		options := map[string]any{"digest": false}
		if tc.digest {
			mode = "digest"
			// Pinning is the default for both aliases.
			options = map[string]any{}
		}
		runRequired(t, tc.crawler+"/"+mode, func(t *testing.T) {
			c := newCLI(t, s)
			manifest := manifestFor(t, tc.crawler, options)

			tagRef := oldTag
			mainRef := ""
			if tc.digest {
				tagRef = commits[oldTag] + " # " + oldTag
				mainRef = "main"
			}
			files := []struct {
				path, oldRef, wantRef string
			}{
				{".gitea/workflows/release.yml", oldRelease, newRelease},
				{".forgejo/workflows/tag.yaml", tagRef, newTag},
				// The branch is neither a release nor a tag. Keep its name
				// when unpinned, or resolve that same branch's tip when pinned.
				{".forgejo/workflows/branch.yml", branch, branch},
				// Unpinned floating main is deliberately not discovered.
				{".gitea/workflows/main.yml", "main", mainRef},
				{composite, tagRef, newTag},
				// Same stale dependency, but not directly in the workflows directory.
				{".gitea/workflows/nested/ignored.yml", oldRelease, ""},
			}
			initial := make(map[string]string)
			for _, f := range files {
				template := workflowTemplate
				if f.path == composite {
					template = compositeTemplate
				}
				initial[f.path] = fmt.Sprintf(template, actionURL, f.oldRef)
				writeWorkflowFixture(t, c.runner.Dir, f.path, initial[f.path])
			}

			diff := c.ok(t, "diff", manifest, "--disable-changelog")
			for _, f := range files {
				if got := readWorkflowFixture(t, c.runner.Dir, f.path); got != initial[f.path] {
					t.Fatalf("diff changed %s", f.path)
				}
			}
			c.ok(t, "apply", manifest, "--disable-changelog", "--commit=false", "--push=false")
			for _, f := range files {
				if f.wantRef == "" {
					if strings.Contains(diff, fmt.Sprintf("file %q", f.path)) {
						t.Errorf("diff produced a target for ignored workflow %s", f.path)
					}
					if got := readWorkflowFixture(t, c.runner.Dir, f.path); got != initial[f.path] {
						t.Errorf("ignored workflow %s changed", f.path)
					}
					continue
				}
				ref, comment := f.wantRef, ""
				if tc.digest {
					ref, comment = commits[f.wantRef], "# "+f.wantRef
				}
				wantUses := actionURL + "@" + ref
				wantDiff := fmt.Sprintf("to %q, in file %q", wantUses, f.path)
				if !tc.digest && f.oldRef == f.wantRef {
					// Require the generated target's no-op, not just an
					// unchanged file that could have been skipped by discovery.
					wantDiff = fmt.Sprintf("already set to %q, from file %q", wantUses, f.path)
				}
				if !strings.Contains(diff, wantDiff) {
					t.Errorf("diff did not propose %s", wantDiff)
				}
				if f.path == composite {
					assertCompositeUses(t, c.runner.Dir, f.path, wantUses, comment)
				} else {
					assertWorkflowUses(t, c.runner.Dir, f.path, wantUses, comment)
				}
			}
		})
	}
	runRequired(t, "release-cooldown", func(t *testing.T) {
		// Prove the otherwise-updatable release exists and is still in
		// cooldown, independently of Updatecli's generated pipeline.
		require.WithinDuration(t, time.Now(), published[newRelease].PublishedAt, time.Hour)
		c := newCLI(t, s)
		const path = ".forgejo/workflows/cooldown.yml"
		initial := fmt.Sprintf(workflowTemplate, actionURL, oldRelease)
		writeWorkflowFixture(t, c.runner.Dir, path, initial)
		manifest := manifestFor(t, "gitea/action", map[string]any{
			"age": map[string]string{"minimum": "30d"}, "digest": false,
		})
		out := c.ok(t, "apply", manifest, "--disable-changelog", "--commit=false", "--push=false")
		requireOutput(t, out, "matches the age filter yet")
		if got := readWorkflowFixture(t, c.runner.Dir, path); got != initial {
			t.Fatal("held release changed the workflow")
		}
	})
	require.Equal(t, refs, action.refs(t), "dependency discovery must not modify the remote action refs")
}

const workflowTemplate = `name: Local action workflow
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: %s@%s
      - run: echo untouched
`

const compositeTemplate = `name: Local composite action
description: Local forge fixture, not run
runs:
  using: composite
  steps:
    - run: echo before
      shell: sh
    - uses: %s@%s
    - uses: ./local-action # keep this
`

func writeWorkflowFixture(t *testing.T, dir, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755); err != nil {
		t.Fatal(err)
	}
	writeResourceFile(t, dir, path, content)
}

func readWorkflowFixture(t *testing.T, dir, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertWorkflowUses(t *testing.T, dir, path, wantUses, wantComment string) {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses yaml.Node
				Run  string
			}
		}
	}
	if err := yaml.Unmarshal([]byte(readWorkflowFixture(t, dir, path)), &workflow); err != nil {
		t.Fatalf("could not parse workflow %s", path)
	}
	steps := workflow.Jobs["build"].Steps
	if len(steps) != 2 || steps[1].Run != "echo untouched" || steps[1].Uses.Kind != 0 {
		t.Fatalf("workflow %s: unexpected steps", path)
	}
	assertUsesNode(t, path, steps[0].Uses, wantUses, wantComment)
}

func assertCompositeUses(t *testing.T, dir, path, wantUses, wantComment string) {
	t.Helper()
	var action struct {
		Runs struct {
			Using string
			Steps []struct {
				Uses  yaml.Node
				Run   string
				Shell string
			}
		}
	}
	if err := yaml.Unmarshal([]byte(readWorkflowFixture(t, dir, path)), &action); err != nil {
		t.Fatalf("could not parse composite action %s", path)
	}
	steps := action.Runs.Steps
	if action.Runs.Using != "composite" || len(steps) != 3 {
		t.Fatalf("composite action %s: unexpected runs", path)
	}
	if steps[0].Run != "echo before" || steps[0].Shell != "sh" || steps[0].Uses.Kind != 0 ||
		steps[1].Run != "" || steps[1].Shell != "" || steps[2].Run != "" || steps[2].Shell != "" {
		t.Errorf("composite action %s: unexpected steps", path)
	}
	assertUsesNode(t, path+":runs.steps[1]", steps[1].Uses, wantUses, wantComment)
	assertUsesNode(t, path+":runs.steps[2]", steps[2].Uses, "./local-action", "# keep this")
}

func assertUsesNode(t *testing.T, location string, got yaml.Node, wantUses, wantComment string) {
	t.Helper()
	// Do not print generated YAML from a pipeline with credentials.
	if got.Kind != yaml.ScalarNode || got.Value != wantUses || got.LineComment != wantComment {
		t.Errorf("%s: expected uses %q with comment %q", location, wantUses, wantComment)
	}
}
