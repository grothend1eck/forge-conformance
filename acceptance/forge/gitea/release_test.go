//go:build acceptance

package gitea_test

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	gitea "gitea.dev/sdk"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func (s *server) testReleases(t *testing.T) {
	t.Run("pagination", func(t *testing.T) { testReleasePagination(t, s) })
	testMissingResourceRepository(t, s, "release", "tag", "search gitea release: Not Found", "looking for Gitea release: Not Found")
	t.Run("repository-without-releases", func(t *testing.T) {
		r := s.newRepo(t)
		before := r.Refs(t)
		require.Empty(t, readResourceReleases(t, s, r.name))
		checkResourceSelection(t, newCLI(t, s), s, r.name, "release", resourceSelection{wantError: "no Gitea Release found"})
		c := newCLI(t, s)
		writeResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
		condition := explicitReleaseNode(r, forgeMap{"tag": resourceMissing})
		out := c.PipelineOK(t, "apply", cli.Manifest(t, forgeMap{
			"name":       "No release in an existing repository",
			"conditions": forgeMap{"exists": condition},
			"targets":    forgeMap{"check": resourceFileNode(resourceOutput, "passed")},
		}))
		cli.RequireOutput(t, out, "no Gitea release found")
		assertResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
		require.Equal(t, before, r.Refs(t))
	})
	t.Run("selection-and-conditions", func(t *testing.T) {
		r := s.newRepo(t)
		// Publish the highest semver first, so default/latest and a semver
		// constraint need not return the same tag. A draft is newer still.
		for _, tag := range []string{"v2.0.0", "v1.9.0", "v1.10.0", "v1.8.0", "v9.0.0"} {
			seedResourceRelease(t, r, tag, tag, r.Base, tag == "v9.0.0")
		}
		const tagOnly = "v3.0.0"
		r.Git(t, "tag", tagOnly, r.Base)
		r.Git(t, "push", "origin", "refs/tags/"+tagOnly)
		before := r.Refs(t)
		require.NotEmpty(t, before["refs/tags/"+tagOnly], "fixture needs a Git tag without a release")
		releases := readResourceReleases(t, s, r.name)
		require.Len(t, releases, 5)
		var latest string
		for _, release := range releases {
			require.NotEqual(t, tagOnly, release.TagName)
			if latest == "" && !release.IsDraft {
				latest = release.TagName
			}
		}
		require.NotEmpty(t, latest, "fixture needs a published release")
		t.Run("source/default", func(t *testing.T) {
			checkResourceSelection(t, publicResourceCLI(t, r), s, r.name, "release", resourceSelection{want: latest})
		})
		testResourceSelectors(t, s, r.name, "release", []resourceSelection{
			{name: "latest", kind: "latest", want: latest},
			{name: "semver", kind: "semver", pattern: "~1", want: "v1.10.0"},
			{name: "regex", kind: "regex", pattern: `^v2\.`, want: "v2.0.0"},
			{name: "age-match", kind: "semver", pattern: "~1", age: map[string]string{"maximum": "24h"}, want: "v1.10.0"},
			{name: "age-hold", kind: "semver", pattern: "~1", age: map[string]string{"minimum": "30d"}, hold: true},
			{name: "no-match-with-age", kind: "semver", pattern: "~7", age: map[string]string{"minimum": "30d"}, wantError: `no Gitea Release tag found matching pattern "~7"`},
			{name: "draft-excluded", kind: "latest", pattern: "v9.0.0", wantError: `no version found matching pattern "v9.0.0"`},
		})
		testResourceConditions(t, s, r.name, "release", "tag", "v2.0.0")
		t.Run("condition/published-release-not-draft-or-plain-tag", func(t *testing.T) {
			c := newCLI(t, s)
			writeResourceFile(t, c.Dir, resourceOutput, resourceUntouched)
			// The sole source supplies v1.10.0 implicitly. The explicit spec
			// tag still wins, and its age and version filters don't apply here.
			fresh := resourceNode(s, r.name, "release", forgeMap{
				"tag": "v2.0.0", "age": map[string]string{"minimum": "30d"},
				"versionfilter": map[string]string{"kind": "semver", "pattern": "~7"},
			})
			absentAsRelease := func(tag string) forgeMap {
				node := explicitReleaseNode(r, forgeMap{"tag": tag})
				node["failwhen"] = true
				return node
			}
			out := c.PipelineOK(t, "apply", cli.Manifest(t, forgeMap{
				"name": "Only published releases satisfy conditions",
				"sources": forgeMap{"selection": resourceNode(s, r.name, "release", forgeMap{
					"versionfilter": map[string]string{"kind": "semver", "pattern": "~1"},
				})},
				"conditions": forgeMap{
					"published": fresh, "sourceFed": resourceNode(s, r.name, "release", nil, forgeMap{"sourceid": "selection"}),
					"noDraft": absentAsRelease("v9.0.0"), "noPlainTag": absentAsRelease(tagOnly),
				},
				"targets": forgeMap{"check": resourceFileNode(resourceOutput, "passed", forgeMap{"disablesourceinput": true})},
			}))
			cli.RequireOutput(t, out, `Gitea release "v2.0.0" found`, `Gitea release "v1.10.0" found`)
			assertResourceFile(t, c.Dir, resourceOutput, "passed")
		})
		require.Equal(t, before, r.Refs(t), "release reads must not change refs")
	})
	t.Run("target/explicit-create-preview-and-no-op", func(t *testing.T) {
		r := s.newRepo(t)
		r.Stage(t, r.Base, "stable.txt", "release commit\n")
		c := newCLI(t, s)
		before := r.awaitBranches(t, r.Base)
		const tag, title, description = "v4.0.0-rc.1", "Test candidate", "Release notes\n\n- Ship the fixture"
		writeResourceFile(t, c.Dir, resourceInput, "v0.0.1")
		spec := forgeMap{
			"tag": tag, "title": title, "description": description, "commitish": r.Base, "prerelease": true,
		}
		manifest := func() string {
			// Explicit tag takes precedence over the file source.
			return sourceFedReleaseYAML(t, r, "Ensure the explicit candidate exists", "input", spec)
		}
		cli.RequireOutput(t, c.PipelineOK(t, "diff", manifest()), "doesn't exist and should be created")
		require.Empty(t, readResourceReleases(t, s, r.name), "diff must not create API releases")
		require.Equal(t, before, r.Refs(t), "diff must not change refs")

		c.PipelineOK(t, "apply", manifest())
		release := onlyResourceRelease(t, s, r.name)
		assertResourceRelease(t, release, tag, title, description, r.Base, false, true)
		tagRef := "refs/tags/" + tag
		require.Equal(t, before["refs/heads/"+r.Base], fetchResourceTagCommit(t, r, tag), "explicit commitish")
		after := r.Refs(t)
		require.NotEmpty(t, after[tagRef])
		withoutTag := maps.Clone(after)
		delete(withoutTag, tagRef)
		require.Equal(t, before, withoutTag, "only %s should have been created", tagRef)

		// The target only ensures existence. Even when the requested metadata
		// changes, it does not edit or recreate an existing release/tag.
		spec["title"], spec["description"], spec["commitish"], spec["prerelease"] = "Changed title", "Changed body", "main", false
		cli.RequireOutput(t, c.PipelineOK(t, "apply", manifest()), "already exist")
		got := onlyResourceRelease(t, s, r.name)
		require.Equal(t, release.ID, got.ID, "existing release must not be replaced")
		require.True(t, got.PublishedAt.Equal(release.PublishedAt), "existing release must not be republished")
		assertResourceRelease(t, got, tag, title, description, r.Base, false, true)
		require.Equal(t, after, r.Refs(t), "reapply must not change refs")
	})
	t.Run("target/source-fed-defaults", func(t *testing.T) {
		r := s.newRepo(t)
		r.Stage(t, r.Base, "stable.txt", "alternate commit\n")
		before := r.Refs(t)
		main, base := before["refs/heads/main"], before["refs/heads/"+r.Base]
		if main == "" || base == "" || r.Base == "main" || main == base {
			t.Fatalf("fixture needs distinct main and default branch commits, got %v", before)
		}
		const tag = "v5.0.0"
		c := newCLI(t, s)
		writeResourceFile(t, c.Dir, resourceInput, tag)
		c.PipelineOK(t, "apply", sourceFedReleaseYAML(t, r, "Create a release from a source-fed tag", "version", nil))
		assertResourceRelease(t, onlyResourceRelease(t, s, r.name), tag, tag, "", "main", false, false)
		require.Equal(t, main, fetchResourceTagCommit(t, r, tag), "default commitish must resolve to main")
	})
	t.Run("target/publish-existing-git-tag", func(t *testing.T) {
		r := s.newRepo(t)
		r.Stage(t, r.Base, "stable.txt", "different commit than the tag\n")
		const tag = "v5.1.0"
		// Seed through the API and delete only the release, keeping the tag.
		// Pushing a tag here would race asynchronous post-receive bookkeeping.
		seed := seedResourceRelease(t, r, tag, "Temporary fixture release", "main", false)
		resp, err := s.client.Releases.DeleteRelease(t.Context(), s.owner, r.name, seed.ID)
		s.check(t, "delete fixture release while keeping its tag", resp, err)
		before := r.Refs(t)
		tagSHA := before["refs/tags/"+tag]
		require.NotEmpty(t, tagSHA)
		require.Equal(t, before["refs/heads/main"], tagSHA)
		require.NotEqual(t, before["refs/heads/"+r.Base], tagSHA)
		require.Empty(t, readResourceReleases(t, s, r.name), "fixture needs a tag without a release")

		c := newCLI(t, s)
		target := explicitReleaseNode(r, forgeMap{"tag": tag, "commitish": r.Base})
		c.PipelineOK(t, "apply", singleTargetYAML(t, "Publish a release without moving its existing Git tag", "release", target))
		release := onlyResourceRelease(t, s, r.name)
		require.Equal(t, tag, release.TagName)
		require.False(t, release.IsDraft)
		require.Equal(t, tag, release.Title)
		require.Equal(t, before, r.Refs(t), "publishing a release must not move its existing Git tag")
	})
	t.Run("target/draft", func(t *testing.T) {
		r := s.newRepo(t)
		c := newCLI(t, s)
		const tag = "preview/v6.0.0"
		spec := forgeMap{"tag": tag, "commitish": r.Base, "draft": true}
		before := r.Refs(t)
		c.PipelineOK(t, "apply", singleTargetYAML(t, "Create a draft without publishing its Git tag", "draft", explicitReleaseNode(r, spec)))
		draft := onlyResourceRelease(t, s, r.name)
		assertResourceRelease(t, draft, tag, tag, "", r.Base, true, false)
		require.Equal(t, before, r.Refs(t), "creating a draft must not change refs")

		// Ensuring the same tag as a published release must not publish the draft.
		delete(spec, "draft")
		manifest := singleTargetYAML(t, "Leave the existing draft unpublished", "ensure", explicitReleaseNode(r, spec))
		cli.RequireOutput(t, c.PipelineOK(t, "apply", manifest), fmt.Sprintf("Release Tag %q already exist", tag))
		require.Equal(t, draft, onlyResourceRelease(t, s, r.name), "apply must preserve the same draft")
		require.Equal(t, before, r.Refs(t), "ensuring an existing draft must not create its tag")
	})
}

func explicitReleaseNode(r *repository, spec forgeMap) forgeMap {
	return resourceNode(r.server, r.name, "release", spec, forgeMap{"disablesourceinput": true})
}

func sourceFedReleaseYAML(t *testing.T, r *repository, name, source string, spec forgeMap) string {
	t.Helper()
	return cli.Manifest(t, forgeMap{
		"name": name, "sources": forgeMap{source: resourceFileNode(resourceInput, "")},
		"targets": forgeMap{"ensure": resourceNode(r.server, r.name, "release", spec, forgeMap{"sourceid": source})},
	})
}

func fetchResourceTagCommit(t *testing.T, r *repository, tag string) string {
	t.Helper()
	r.Git(t, "fetch", "origin", "refs/tags/"+tag)
	return strings.TrimSpace(r.Git(t, "rev-parse", "FETCH_HEAD^{commit}"))
}

func seedResourceRelease(t *testing.T, r *repository, tag, title, target string, draft bool) *gitea.Release {
	t.Helper()
	s := r.server
	release, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
		TagName: tag, Target: target, Title: title, IsDraft: draft,
	})
	s.check(t, "seed release "+tag, resp, err)
	return release
}

func readResourceReleases(t *testing.T, s *server, repo string) []*gitea.Release {
	t.Helper()
	return listResourceReleases(t, s, repo, gitea.ListOptions{})
}

func listResourceReleases(t *testing.T, s *server, repo string, opts gitea.ListOptions) []*gitea.Release {
	t.Helper()
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, repo, gitea.ListReleasesOptions{ListOptions: opts})
	s.check(t, fmt.Sprintf("read releases (page %d)", opts.Page), resp, err)
	return releases
}

func onlyResourceRelease(t *testing.T, s *server, repo string) *gitea.Release {
	t.Helper()
	releases := readResourceReleases(t, s, repo)
	require.Len(t, releases, 1, "API release count")
	return releases[0]
}

func assertResourceRelease(t *testing.T, got *gitea.Release, tag, title, description, commitish string, draft, prerelease bool) {
	t.Helper()
	require.Equal(t, tag, got.TagName, "tag")
	require.Equal(t, title, got.Title, "title")
	require.Equal(t, description+"\nMade with ❤️️ by updatecli", got.Note, "release notes")
	require.Equal(t, commitish, got.Target, "commitish")
	require.Equal(t, draft, got.IsDraft, "draft")
	require.Equal(t, prerelease, got.IsPrerelease, "prerelease")
}

func testReleasePagination(t *testing.T, s *server) {
	t.Helper()
	r := s.newRepo(t)
	seeded := make(map[string]int64, resourcePageSize+1)
	for i := 1; i <= resourcePageSize+1; i++ {
		tag := fmt.Sprintf("v1.0.%d", i)
		release := seedResourceRelease(t, r, tag, tag, r.Base, false)
		require.Positive(t, release.ID)
		seeded[tag] = release.ID
	}
	readPage := func(t *testing.T, page int) []*gitea.Release {
		t.Helper()
		return listResourceReleases(t, s, r.name, gitea.ListOptions{Page: page, PageSize: resourcePageSize})
	}
	first, second := readPage(t, 1), readPage(t, 2)
	require.Len(t, first, resourcePageSize)
	require.Len(t, second, 1)
	latest, later := first[0], second[0]
	latestPublished, resp, err := s.client.Releases.GetLatestRelease(t.Context(), s.owner, r.name)
	s.check(t, "read latest stable release", resp, err)
	require.Equal(t, latestPublished.ID, latest.ID, "fixture's latest stable release must be first on page one")
	for _, release := range first {
		require.NotEqual(t, later.TagName, release.TagName, "matched tag must be absent from page 1")
		require.NotEqual(t, later.ID, release.ID, "matched release ID must be absent from page 1")
	}
	before := r.Refs(t)
	for _, release := range []*gitea.Release{latest, later} {
		require.False(t, release.IsDraft)
		require.Contains(t, seeded, release.TagName)
		require.Equal(t, seeded[release.TagName], release.ID)
		require.NotEmpty(t, before["refs/tags/"+release.TagName])
	}
	checkUnchanged := func(t *testing.T) {
		t.Helper()
		require.Equal(t, before, r.Refs(t), "release lookup must not change remote refs")
		require.Equal(t, first, readPage(t, 1), "page one releases must be unchanged")
		require.Equal(t, second, readPage(t, 2), "page two release must be unchanged")
	}

	t.Run("source/latest-is-head-of-first-page", func(t *testing.T) {
		// The releases API is newest first. Reversing each page separately
		// before appending makes "latest" wrong once there is a second page.
		checkResourceSelection(t, newCLI(t, s), s, r.name, "release", resourceSelection{want: latest.TagName})
		checkUnchanged(t)
	})
	t.Run("source-and-condition/exact-on-later-page", func(t *testing.T) {
		checkResourceSelection(t, newCLI(t, s), s, r.name, "release", resourceSelection{kind: "latest", pattern: later.TagName, want: later.TagName})
		checkUnchanged(t)
	})
	t.Run("target/existing-on-later-page", func(t *testing.T) {
		c := newCLI(t, s)
		target := explicitReleaseNode(r, forgeMap{
			"tag": later.TagName, "commitish": r.Base, "title": "Do not replace the published release",
		})
		manifest := singleTargetYAML(t, "Ensure a release already on page 2 exists", "ensure", target)
		existing := fmt.Sprintf("Release Tag %q already exist", later.TagName)
		preview := c.PipelineOK(t, "diff", manifest)
		require.False(t, strings.Contains(preview, "doesn't exist and should be created"), "diff planned creation of %q", later.TagName)
		cli.RequireOutput(t, preview, existing)
		checkUnchanged(t)
		cli.RequireOutput(t, c.PipelineOK(t, "apply", manifest), existing)
		checkUnchanged(t)
	})
}
