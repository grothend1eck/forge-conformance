//go:build acceptance

package gitea_test

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
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
		action.Stage(t, ref, subdir+"/action.yml", fmt.Sprintf(`name: Fixture action %s
description: Local forge fixture, not run
runs:
  using: composite
  steps:
    - run: echo fixture
      shell: sh
`, label))
		return strings.TrimSpace(action.Git(t, "rev-parse", "HEAD"))
	}
	// Let the release API create each release tag at its own commit. Pushing
	// those tags first can race with the forge's post-receive bookkeeping.
	for _, tag := range []string{oldRelease, newRelease} {
		commits[tag] = stage(action.Base, tag)
		seedResourceRelease(t, action, tag, tag, commits[tag], false)
	}
	// The tag-only versions outrank the releases in semver order, but should
	// not be selected when a workflow tracks a release.
	for _, tag := range []string{oldTag, newTag} {
		commits[tag] = stage(action.Base, tag)
		action.Git(t, "tag", "-a", tag, "-m", "Fixture annotated tag "+tag)
	}
	action.Git(t, "push", "origin", "refs/tags/"+oldTag, "refs/tags/"+newTag)
	// A branch at the same name but a different commit proves that the tag
	// wins when neither ref is a release.
	shadow := stage(oldTag, "tag shadow")
	commits[branch] = stage(branch, branch)
	commits["main"] = stage("main", "main")
	commits[decoy] = stage(decoy, decoy)

	action.Git(t, "fetch", "origin", "refs/tags/*:refs/remotes/origin/fixture-tags/*")
	for _, tag := range []string{oldRelease, newRelease, oldTag, newTag} {
		ref := "refs/remotes/origin/fixture-tags/" + tag
		commit := strings.TrimSpace(action.Git(t, "rev-parse", ref+"^{commit}"))
		require.Equal(t, commits[tag], commit, "remote tag %s must peel to the fixture commit", tag)
		if tag == oldTag || tag == newTag {
			object := strings.TrimSpace(action.Git(t, "rev-parse", ref))
			require.NotEqual(t, object, commit, "fixture needs an annotated tag to distinguish tag object from commit")
		}
	}
	releases := readResourceReleases(t, s, action.name)
	require.Len(t, releases, 2)
	published := make(map[string]time.Time)
	for _, release := range releases {
		require.False(t, release.IsDraft)
		require.False(t, release.IsPrerelease)
		published[release.TagName] = release.PublishedAt
	}
	require.Contains(t, published, oldRelease)
	require.Contains(t, published, newRelease)
	refs := action.Refs(t)
	require.Equal(t, shadow, refs["refs/heads/"+oldTag], "fixture needs a branch with the tag's name")
	require.NotEqual(t, shadow, commits[oldTag], "tag and branch must point to different commits")
	for _, name := range []string{"main", branch, decoy} {
		require.Equal(t, commits[name], refs["refs/heads/"+name], "fixture must expose branch %s", name)
	}
	require.NotEqual(t, commits[branch], commits[decoy], "the requested and later-sorting branches must differ")

	endpoint, err := url.Parse(s.url)
	require.NoError(t, err, "parse forge fixture URL")
	require.NotEmpty(t, endpoint.Hostname())
	actionURL, err := url.JoinPath(s.url, s.owner, action.name, subdir)
	require.NoError(t, err, "build fixture action URL")
	manifestFor := func(t *testing.T, crawler string, options forgeMap) string {
		t.Helper()
		// The crawler matches credentials by hostname, not host:port.
		options["credentials"] = forgeMap{endpoint.Hostname(): forgeMap{"kind": s.provider}}
		return cli.Manifest(t, forgeMap{
			"name":          "Discover local forge actions",
			"autodiscovery": forgeMap{"crawlers": forgeMap{crawler: options}},
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
		options := forgeMap{}
		if tc.digest {
			mode = "digest"
		} else {
			// Pinning is the default for both aliases.
			options["digest"] = false
		}
		t.Run(tc.crawler+"/"+mode, func(t *testing.T) {
			c := newCLI(t, s)
			manifest := manifestFor(t, tc.crawler, options)

			tagRef := oldTag
			mainRef := ""
			if tc.digest {
				tagRef = commits[oldTag] + " # " + oldTag
				mainRef = "main"
			}
			files := []struct {
				path, oldRef, wantRef, template string
			}{
				{".gitea/workflows/release.yml", oldRelease, newRelease, workflowTemplate},
				{".forgejo/workflows/tag.yaml", tagRef, newTag, workflowTemplate},
				// The branch is neither a release nor a tag. Keep its name
				// when unpinned, or resolve that same branch's tip when pinned.
				{".forgejo/workflows/branch.yml", branch, branch, workflowTemplate},
				// Unpinned floating main is deliberately not discovered.
				{".gitea/workflows/main.yml", "main", mainRef, workflowTemplate},
				{composite, tagRef, newTag, compositeTemplate},
				// Same stale dependency, but not directly in the workflows directory.
				{".gitea/workflows/nested/ignored.yml", oldRelease, "", workflowTemplate},
			}
			for _, f := range files {
				writeWorkflowFixture(t, c.Dir, f.path, fmt.Sprintf(f.template, actionURL, f.oldRef))
			}

			unchanged := func(path, template, ref string) bool {
				return readResourceFile(t, c.Dir, path) == fmt.Sprintf(template, actionURL, ref)
			}
			diff := c.PipelineOK(t, "diff", manifest, "--disable-changelog")
			for _, f := range files {
				require.True(t, unchanged(f.path, f.template, f.oldRef), "diff changed %s", f.path)
			}
			c.PipelineOK(t, "apply", manifest, "--disable-changelog", "--commit=false", "--push=false")
			for _, f := range files {
				if f.wantRef == "" {
					assert.False(t, strings.Contains(diff, fmt.Sprintf("file %q", f.path)), "diff targeted ignored workflow %s", f.path)
					assert.True(t, unchanged(f.path, f.template, f.oldRef), "ignored workflow %s changed", f.path)
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
				assert.True(t, strings.Contains(diff, wantDiff), "diff did not report %s", wantDiff)
				assertDiscoveredUses(t, c.Dir, f.path, wantUses, comment, f.path == composite)
			}
		})
	}
	t.Run("release-cooldown", func(t *testing.T) {
		// Prove the otherwise-updatable release exists and is still in
		// cooldown, independently of Updatecli's generated pipeline.
		require.WithinDuration(t, time.Now(), published[newRelease], time.Hour)
		c := newCLI(t, s)
		const path = ".forgejo/workflows/cooldown.yml"
		initial := fmt.Sprintf(workflowTemplate, actionURL, oldRelease)
		writeWorkflowFixture(t, c.Dir, path, initial)
		manifest := manifestFor(t, "gitea/action", forgeMap{
			"age": map[string]string{"minimum": "30d"}, "digest": false,
		})
		out := c.PipelineOK(t, "apply", manifest, "--disable-changelog", "--commit=false", "--push=false")
		cli.RequireOutput(t, out, "matches the age filter yet")
		require.True(t, readResourceFile(t, c.Dir, path) == initial, "held release changed the workflow")
	})
	require.Equal(t, refs, action.Refs(t), "dependency discovery must not modify the remote action refs")
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
	require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
	writeResourceFile(t, dir, path, content)
}

func assertDiscoveredUses(t *testing.T, dir, path, wantUses, wantComment string, composite bool) {
	t.Helper()
	type step struct {
		Uses       yaml.Node
		Run, Shell string
	}
	var doc struct {
		Jobs map[string]struct{ Steps []step }
		Runs struct {
			Using string
			Steps []step
		}
	}
	// Do not report parser errors or node contents from a pipeline with credentials.
	require.True(t, yaml.Unmarshal([]byte(readResourceFile(t, dir, path)), &doc) == nil, "could not parse %s", path)
	if !composite {
		steps := doc.Jobs["build"].Steps
		require.Equal(t, 2, len(steps), "step count in %s", path)
		require.True(t, steps[1].Run == "echo untouched" && steps[1].Uses.Kind == 0, "unexpected final step in %s", path)
		assertUsesNode(t, path, steps[0].Uses, wantUses, wantComment)
		return
	}
	steps := doc.Runs.Steps
	require.True(t, doc.Runs.Using == "composite", "unexpected runs.using in %s", path)
	require.True(t, len(steps) == 3, "expected three composite steps in %s", path)
	assert.True(t, steps[0].Run == "echo before" && steps[0].Shell == "sh" && steps[0].Uses.Kind == 0,
		"unexpected first composite step in %s", path)
	for _, step := range steps[1:] {
		assert.True(t, step.Run == "" && step.Shell == "", "unexpected run or shell on a uses step in %s", path)
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
