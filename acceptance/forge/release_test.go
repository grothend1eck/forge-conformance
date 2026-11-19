//go:build acceptance

package forge

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

func testReleases(t *testing.T, s *server) {
	runRequired(t, "pagination", func(t *testing.T) { testReleasePagination(t, s) })
	runRequired(t, "selection-and-conditions", func(t *testing.T) {
		r := s.newRepo(t)
		// Publish the highest semver first, so default/latest and a semver
		// constraint need not return the same tag. A draft is newer still.
		for _, tag := range []string{"v2.0.0", "v1.9.0", "v1.10.0", "v1.8.0", "v9.0.0"} {
			draft := tag == "v9.0.0"
			if _, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
				TagName: tag, Target: r.base, Title: tag, IsDraft: draft,
			}); err != nil {
				s.apiFailure(t, "seed release "+tag, resp, err)
			}
		}
		const tagOnly = "v3.0.0"
		r.git(t, "tag", tagOnly, r.base)
		r.git(t, "push", "origin", "refs/tags/"+tagOnly)
		before := r.refs(t)
		require.NotEmpty(t, before["refs/tags/"+tagOnly], "fixture needs a Git tag without a release")
		releases := readResourceReleases(t, s, r.name)
		var latest string
		for _, release := range releases {
			require.NotEqual(t, tagOnly, release.TagName)
			if latest == "" && !release.IsDraft {
				latest = release.TagName
			}
		}
		if len(releases) != 5 || latest == "" {
			t.Fatalf("expected five seeded releases including a published release; got %d, latest published = %q", len(releases), latest)
		}
		testResourceSelectors(t, s, r.name, "release", []resourceSelection{
			{name: "default", want: latest},
			{name: "latest", kind: "latest", want: latest},
			{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
			{name: "regex", kind: "regex", pattern: `^v2\.`, want: "v2.0.0"},
			{name: "age-match", kind: "semver", pattern: "~1", age: map[string]string{"maximum": "24h"}, want: "v1.10.0"},
			{name: "age-hold", kind: "semver", pattern: "~1", age: map[string]string{"minimum": "30d"}, hold: true},
			{name: "no-match-with-age", kind: "semver", pattern: "~7", age: map[string]string{"minimum": "30d"}, wantError: `no Gitea Release tag found matching pattern "~7"`},
			{name: "draft-excluded", kind: "latest", pattern: "v9.0.0", wantError: `no version found matching pattern "v9.0.0"`},
		})
		testResourceConditions(t, s, r.name, "release", "tag", "v2.0.0")
		runRequired(t, "condition/published-release-not-draft-or-plain-tag", func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.runner.Dir, resourceOutput, resourceUntouched)
			fresh := resourceNode(s, r.name, "release", map[string]any{
				"tag": "v2.0.0", "age": map[string]string{"minimum": "30d"},
				"versionfilter": map[string]string{"kind": "semver", "pattern": "~7"},
			})
			fresh["disablesourceinput"] = true
			absentAsRelease := func(tag string) map[string]any {
				node := resourceNode(s, r.name, "release", map[string]any{"tag": tag})
				node["disablesourceinput"], node["failwhen"] = true, true
				return node
			}
			c.ok(t, "apply", manifestYAML(t, map[string]any{
				"name":       "Only published releases satisfy conditions",
				"conditions": map[string]any{"published": fresh, "noDraft": absentAsRelease("v9.0.0"), "noPlainTag": absentAsRelease(tagOnly)},
				"targets":    map[string]any{"check": resourceFileNode(resourceOutput, "passed")},
			}))
			assertResourceFile(t, c.runner.Dir, resourceOutput, "passed")
		})
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("release selectors and conditions changed remote refs: got %v, want %v", got, before)
		}
	})
	runRequired(t, "target/explicit-create-preview-and-no-op", func(t *testing.T) {
		r := s.newRepo(t)
		r.stage(t, r.base, "stable.txt", "release commit\n")
		c := newCLI(t, s)
		before := r.awaitBranches(t, r.base)
		const tag, title, description = "v4.0.0-rc.1", "Test candidate", "Release notes\n\n- Ship the fixture"
		writeResourceFile(t, c.runner.Dir, resourceInput, "v0.0.1")
		spec := map[string]any{
			"tag": tag, "title": title, "description": description, "commitish": r.base, "prerelease": true,
		}
		target := resourceNode(s, r.name, "release", spec)
		target["sourceid"] = "input" // Explicit tag takes precedence over this value.
		manifest := map[string]any{
			"name":    "Ensure the explicit candidate exists",
			"sources": map[string]any{"input": resourceFileNode(resourceInput, "")},
			"targets": map[string]any{"ensure": target},
		}
		out := c.ok(t, "diff", manifestYAML(t, manifest))
		requireOutput(t, out, "doesn't exist and should be created")
		if got := readResourceReleases(t, s, r.name); len(got) != 0 {
			t.Fatalf("diff created %d API releases", len(got))
		}
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("diff changed remote refs: got %v, want %v", got, before)
		}

		c.ok(t, "apply", manifestYAML(t, manifest))
		release := onlyResourceRelease(t, s, r.name)
		assertResourceRelease(t, release, tag, title, description, r.base, false, true)
		tagRef := "refs/tags/" + tag
		r.git(t, "fetch", "origin", tagRef)
		if got, want := strings.TrimSpace(r.git(t, "rev-parse", "FETCH_HEAD^{commit}")), before["refs/heads/"+r.base]; got != want {
			t.Fatalf("created tag points to %q, want explicit commitish %q", got, want)
		}
		after := r.refs(t)
		expected := maps.Clone(before)
		expected[tagRef] = after[tagRef]
		if after[tagRef] == "" || !maps.Equal(after, expected) {
			t.Fatalf("expected creation of only %s, before=%v after=%v", tagRef, before, after)
		}

		// The target only ensures existence. Even when the requested metadata
		// changes, it does not edit or recreate an existing release/tag.
		spec["title"], spec["description"], spec["commitish"], spec["prerelease"] = "Changed title", "Changed body", "main", false
		target["spec"] = resourceNode(s, r.name, "release", spec)["spec"]
		out = c.ok(t, "apply", manifestYAML(t, manifest))
		requireOutput(t, out, "already exist")
		got := onlyResourceRelease(t, s, r.name)
		if got.ID != release.ID || !got.PublishedAt.Equal(release.PublishedAt) {
			t.Fatalf("existing release was replaced or republished: id=%d time=%s; want id=%d time=%s", got.ID, got.PublishedAt, release.ID, release.PublishedAt)
		}
		assertResourceRelease(t, got, tag, title, description, r.base, false, true)
		if refs := r.refs(t); !maps.Equal(refs, after) {
			t.Fatalf("reapply changed remote refs: got %v, want %v", refs, after)
		}
	})
	runRequired(t, "target/source-fed-defaults", func(t *testing.T) {
		r := s.newRepo(t)
		r.stage(t, r.base, "stable.txt", "alternate commit\n")
		before := r.refs(t)
		main, base := before["refs/heads/main"], before["refs/heads/"+r.base]
		if main == "" || base == "" || r.base == "main" || main == base {
			t.Fatalf("fixture needs distinct main and default branch commits, got %v", before)
		}
		const tag = "v5.0.0"
		c := newCLI(t, s)
		writeResourceFile(t, c.runner.Dir, resourceInput, tag)
		target := resourceNode(s, r.name, "release", nil)
		target["sourceid"] = "version"
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Create a release from a source-fed tag",
			"sources": map[string]any{"version": resourceFileNode(resourceInput, "")},
			"targets": map[string]any{"ensure": target},
		}))
		assertResourceRelease(t, onlyResourceRelease(t, s, r.name), tag, tag, "", "main", false, false)
		r.git(t, "fetch", "origin", "refs/tags/"+tag)
		if got := strings.TrimSpace(r.git(t, "rev-parse", "FETCH_HEAD^{commit}")); got != main {
			t.Fatalf("default commitish points to %q, want main at %q", got, main)
		}
	})
	runRequired(t, "target/publish-existing-git-tag", func(t *testing.T) {
		r := s.newRepo(t)
		r.stage(t, r.base, "stable.txt", "different commit than the tag\n")
		const tag = "v5.1.0"
		// Seed through the API and delete only the release, keeping the tag.
		// Pushing a tag here would race asynchronous post-receive bookkeeping.
		seed, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
			TagName: tag, Target: "main", Title: "Temporary fixture release",
		})
		if err != nil {
			s.apiFailure(t, "seed release tag", resp, err)
		}
		if resp, err := s.client.Releases.DeleteRelease(t.Context(), s.owner, r.name, seed.ID); err != nil {
			s.apiFailure(t, "delete fixture release while keeping its tag", resp, err)
		}
		before := r.refs(t)
		tagSHA := before["refs/tags/"+tag]
		require.NotEmpty(t, tagSHA)
		require.Equal(t, before["refs/heads/main"], tagSHA)
		require.NotEqual(t, before["refs/heads/"+r.base], tagSHA)
		require.Empty(t, readResourceReleases(t, s, r.name), "fixture needs a tag without a release")

		c := newCLI(t, s)
		target := resourceNode(s, r.name, "release", map[string]any{"tag": tag, "commitish": r.base})
		target["disablesourceinput"] = true
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Publish a release without moving its existing Git tag",
			"targets": map[string]any{"release": target},
		}))
		release := onlyResourceRelease(t, s, r.name)
		require.Equal(t, tag, release.TagName)
		require.False(t, release.IsDraft)
		require.Equal(t, tag, release.Title)
		require.Equal(t, before, r.refs(t), "publishing a release must not move its existing Git tag")
	})
	runRequired(t, "target/draft", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const tag = "preview/v6.0.0"
		target := resourceNode(s, r.name, "release", map[string]any{"tag": tag, "commitish": r.base, "draft": true})
		target["disablesourceinput"] = true
		before := r.refs(t)
		c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Create a draft without publishing its Git tag",
			"targets": map[string]any{"draft": target},
		}))
		draft := onlyResourceRelease(t, s, r.name)
		assertResourceRelease(t, draft, tag, tag, "", r.base, true, false)
		if got := r.refs(t); !maps.Equal(got, before) {
			t.Fatalf("creating a draft changed remote refs: got %v, want %v", got, before)
		}

		// Ensuring the same tag as a published release must not publish the draft.
		target["spec"] = resourceNode(s, r.name, "release", map[string]any{"tag": tag, "commitish": r.base})["spec"]
		requireOutput(t, c.ok(t, "apply", manifestYAML(t, map[string]any{
			"name":    "Leave the existing draft unpublished",
			"targets": map[string]any{"ensure": target},
		})), fmt.Sprintf("Release Tag %q already exist", tag))
		require.Equal(t, draft, onlyResourceRelease(t, s, r.name), "apply must preserve the same draft")
		require.Equal(t, before, r.refs(t), "ensuring an existing draft must not create its tag")
	})
}

func readResourceReleases(t *testing.T, s *server, repo string) []*gitea.Release {
	t.Helper()
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, repo, gitea.ListReleasesOptions{})
	if err != nil {
		s.apiFailure(t, "read releases", resp, err)
	}
	return releases
}

func onlyResourceRelease(t *testing.T, s *server, repo string) *gitea.Release {
	t.Helper()
	releases := readResourceReleases(t, s, repo)
	if len(releases) != 1 {
		t.Fatalf("API returned %d releases, want one", len(releases))
	}
	return releases[0]
}

func assertResourceRelease(t *testing.T, got *gitea.Release, tag, title, description, commitish string, draft, prerelease bool) {
	t.Helper()
	note := description + "\nMade with ❤️️ by updatecli"
	if got.TagName != tag || got.Title != title || got.Note != note || got.Target != commitish || got.IsDraft != draft || got.IsPrerelease != prerelease {
		t.Fatalf("API release tag=%q title=%q body=%q commitish=%q draft=%t prerelease=%t; want %q, %q, %q, %q, %t, %t",
			got.TagName, got.Title, got.Note, got.Target, got.IsDraft, got.IsPrerelease, tag, title, note, commitish, draft, prerelease)
	}
}
