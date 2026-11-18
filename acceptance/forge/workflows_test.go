//go:build acceptance

package forge

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
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
		composite  = ".gitea/actions/fixture/action.yml"
	)

	action := s.newRepo(t)
	commits := make(map[string]string)
	stage := func(ref, label string) string {
		action.stage(t, ref, "action.yml", fmt.Sprintf(`name: Fixture action %s
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
		action.git(t, "tag", tag)
	}
	action.git(t, "push", "origin", "refs/tags/"+oldTag, "refs/tags/"+newTag)
	commits[branch] = stage(branch, branch)
	decoyCommit := stage(decoy, decoy)

	action.git(t, "fetch", "origin", "refs/tags/*:refs/remotes/origin/fixture-tags/*")
	for _, tag := range []string{oldRelease, newRelease, oldTag, newTag} {
		got := strings.TrimSpace(action.git(t, "rev-parse", "refs/remotes/origin/fixture-tags/"+tag+"^{commit}"))
		if got != commits[tag] {
			t.Fatalf("remote tag %s peels to %q, want %q", tag, got, commits[tag])
		}
	}
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, action.name, gitea.ListReleasesOptions{})
	if err != nil {
		s.apiFailure(t, "list fixture releases", resp, err)
	}
	published := make(map[string]bool)
	for _, release := range releases {
		published[release.TagName] = !release.IsDraft && !release.IsPrerelease
	}
	if len(releases) != 2 || !published[oldRelease] || !published[newRelease] {
		t.Fatalf("expected only the two stable fixture releases, got %v", published)
	}
	refs := action.refs(t)
	if refs["refs/heads/"+branch] != commits[branch] || refs["refs/heads/"+decoy] != decoyCommit || commits[branch] == decoyCommit {
		t.Fatal("fixture should expose the requested and later-sorting branches at distinct commits")
	}

	endpoint, err := url.Parse(s.url)
	if err != nil || endpoint.Hostname() == "" {
		t.Fatalf("invalid forge fixture URL: %v", err)
	}
	actionURL, err := url.JoinPath(s.url, s.owner, action.name)
	if err != nil {
		t.Fatalf("build fixture action URL: %v", err)
	}

	for _, tc := range []struct {
		crawler, mode string
		digest        bool
	}{
		{"gitea/action", "version", false},
		{"gitea/action", "digest", true},
		{"github/action", "version", false},
		{"github/action", "digest", true},
	} {
		t.Run(tc.crawler+"/"+tc.mode, func(t *testing.T) {
			c := newCLI(t, s)
			manifest := manifestYAML(t, map[string]any{
				"name": "Discover local forge actions",
				"autodiscovery": map[string]any{
					"crawlers": map[string]any{
						tc.crawler: map[string]any{
							"digest": tc.digest,
							// The crawler matches credentials by hostname, not host:port.
							"credentials": map[string]any{
								endpoint.Hostname(): map[string]any{
									"kind": s.provider,
								},
							},
						},
					},
				},
			})

			tagRef := oldTag
			if tc.digest {
				tagRef = commits[oldTag] + " # " + oldTag
			}
			files := []struct {
				path, oldRef, wantRef string
			}{
				{".gitea/workflows/release.yml", oldRelease, newRelease},
				{".forgejo/workflows/tag.yaml", tagRef, newTag},
				// The branch is neither a release nor a tag. Keep its name
				// when unpinned, or resolve that same branch's tip when pinned.
				{".forgejo/workflows/branch.yml", branch, branch},
				{composite, tagRef, newTag},
				// Same stale dependency, but not directly in the workflows directory.
				{".gitea/workflows/nested/ignored.yml", oldRelease, ""},
			}
			initial := make(map[string]string)
			for _, f := range files {
				path := filepath.Join(c.dir, f.path)
				template := `name: Local action workflow
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: %s@%s
      - run: echo untouched
`
				if f.path == composite {
					template = `name: Local composite action
description: Local forge fixture, not run
runs:
  using: composite
  steps:
    - run: echo before
      shell: sh
    - uses: %s@%s
    - uses: ./local-action # keep this
`
				}
				initial[f.path] = fmt.Sprintf(template, actionURL, f.oldRef)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(initial[f.path]), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			diff := c.ok(t, "diff", manifest, "--disable-changelog")
			for _, f := range files {
				if got := readWorkflowFixture(t, c.dir, f.path); got != initial[f.path] {
					t.Fatalf("diff changed %s:\n%s", f.path, got)
				}
			}
			c.ok(t, "apply", manifest, "--disable-changelog", "--commit=false", "--push=false")
			for _, f := range files {
				if f.wantRef == "" {
					if strings.Contains(diff, fmt.Sprintf("in file %q", f.path)) {
						t.Errorf("diff proposed a change to neighboring workflow %s:\n%s", f.path, diff)
					}
					if got := readWorkflowFixture(t, c.dir, f.path); got != initial[f.path] {
						t.Errorf("neighboring workflow %s changed:\n%s", f.path, got)
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
					t.Errorf("diff did not propose %s:\n%s", wantDiff, diff)
				}
				if f.path == composite {
					assertCompositeUses(t, c.dir, f.path, wantUses, comment)
				} else {
					assertWorkflowUses(t, c.dir, f.path, wantUses, comment)
				}
			}
		})
	}
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
		t.Fatalf("parse workflow %s: %v", path, err)
	}
	steps := workflow.Jobs["build"].Steps
	if len(steps) != 2 || steps[1].Run != "echo untouched" || steps[1].Uses.Kind != 0 {
		t.Fatalf("workflow %s: unexpected steps: %+v", path, steps)
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
		t.Fatalf("parse composite action %s: %v", path, err)
	}
	steps := action.Runs.Steps
	if action.Runs.Using != "composite" || len(steps) != 3 {
		t.Fatalf("composite action %s: unexpected runs: %+v", path, action.Runs)
	}
	if steps[0].Run != "echo before" || steps[0].Shell != "sh" || steps[0].Uses.Kind != 0 ||
		steps[1].Run != "" || steps[1].Shell != "" || steps[2].Run != "" || steps[2].Shell != "" {
		t.Errorf("composite action %s: unexpected steps: %+v", path, steps)
	}
	assertUsesNode(t, path+":runs.steps[1]", steps[1].Uses, wantUses, wantComment)
	assertUsesNode(t, path+":runs.steps[2]", steps[2].Uses, "./local-action", "# keep this")
}

func assertUsesNode(t *testing.T, location string, got yaml.Node, wantUses, wantComment string) {
	t.Helper()
	if got.Kind != yaml.ScalarNode || got.Value != wantUses || got.LineComment != wantComment {
		t.Errorf("%s: uses = %q (comment %q), want %q (comment %q)", location, got.Value, got.LineComment, wantUses, wantComment)
	}
}
