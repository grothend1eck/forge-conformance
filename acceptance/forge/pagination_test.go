//go:build acceptance

package forge

import (
	"fmt"
	"strings"
	"testing"

	"gitea.dev/sdk"
	"github.com/stretchr/testify/require"
)

const resourcePageSize = 30

func testBranchPagination(t *testing.T, s *server) {
	t.Helper()
	r := s.newRepo(t)
	push := []string{"push", "origin"}
	// The fixture already has two branches.
	for i := 1; i < resourcePageSize; i++ {
		push = append(push, fmt.Sprintf("main:refs/heads/fixture-%02d", i))
	}
	r.git(t, push...)
	before := r.refs(t)
	readPage := func(page int) []*gitea.Branch {
		t.Helper()
		branches, resp, err := s.client.Repositories.ListRepoBranches(t.Context(), s.owner, r.name, gitea.ListRepoBranchesOptions{
			ListOptions: gitea.ListOptions{Page: page, PageSize: resourcePageSize},
		})
		if err != nil {
			s.apiFailure(t, fmt.Sprintf("read branches page %d", page), resp, err)
		}
		return branches
	}
	first, second := readPage(1), readPage(2)
	require.Len(t, first, resourcePageSize)
	require.Len(t, second, 1)
	later := second[0].Name
	require.NotEmpty(t, before["refs/heads/"+later], "second page must contain a real Git branch")
	for _, branch := range first {
		require.NotEqual(t, later, branch.Name, "matched branch must be absent from page 1")
	}
	checkResourceSelection(t, s, r.name, "branch", resourceSelection{
		kind: "latest", pattern: later, want: later,
	})
	require.Equal(t, before, r.refs(t), "paginated source and condition must not change remote refs")
}

func testReleasePagination(t *testing.T, s *server) {
	t.Helper()
	r := s.newRepo(t)
	seeded := make(map[string]int64, resourcePageSize+1)
	for i := 1; i <= resourcePageSize+1; i++ {
		tag := fmt.Sprintf("v1.0.%d", i)
		release, resp, err := s.client.Releases.CreateRelease(t.Context(), s.owner, r.name, gitea.CreateReleaseOption{
			TagName: tag, Target: r.base, Title: tag,
		})
		if err != nil {
			s.apiFailure(t, "seed paginated release "+tag, resp, err)
		}
		require.Positive(t, release.ID)
		seeded[tag] = release.ID
	}

	first, second := readReleasePage(t, s, r.name, 1), readReleasePage(t, s, r.name, 2)
	require.Len(t, first, resourcePageSize)
	require.Len(t, second, 1)
	latest := first[0]
	later := second[0]
	require.False(t, latest.IsDraft)
	require.False(t, later.IsDraft)
	require.Contains(t, seeded, latest.TagName)
	require.Equal(t, seeded[latest.TagName], latest.ID)
	require.Contains(t, seeded, later.TagName)
	require.Equal(t, seeded[later.TagName], later.ID)
	latestPublished, resp, err := s.client.Releases.GetLatestRelease(t.Context(), s.owner, r.name)
	if err != nil {
		s.apiFailure(t, "read latest stable release", resp, err)
	}
	require.Equal(t, latestPublished.ID, latest.ID, "fixture's latest stable release must be first on page one")
	for _, release := range first {
		require.NotEqual(t, later.TagName, release.TagName, "matched tag must be absent from page 1")
		require.NotEqual(t, later.ID, release.ID, "matched release ID must be absent from page 1")
	}
	before := r.refs(t)
	require.NotEmpty(t, before["refs/tags/"+latest.TagName])
	require.NotEmpty(t, before["refs/tags/"+later.TagName])
	checkUnchanged := func(t *testing.T) {
		t.Helper()
		require.Equal(t, before, r.refs(t), "release lookup must not change remote refs")
		require.Equal(t, first, readReleasePage(t, s, r.name, 1), "page one releases must be unchanged")
		require.Equal(t, second, readReleasePage(t, s, r.name, 2), "page two release must be unchanged")
	}

	runRequired(t, "source/latest-is-head-of-first-page", func(t *testing.T) {
		// The releases API is newest first. Appending pages after reversing each
		// page separately makes "latest" wrong once there is a second page.
		checkResourceSelection(t, s, r.name, "release", resourceSelection{want: latest.TagName})
		checkUnchanged(t)
	})
	runRequired(t, "source-and-condition/exact-on-later-page", func(t *testing.T) {
		checkResourceSelection(t, s, r.name, "release", resourceSelection{
			kind: "latest", pattern: later.TagName, want: later.TagName,
		})
		checkUnchanged(t)
	})
	runRequired(t, "target/existing-on-later-page", func(t *testing.T) {
		c := newCLI(t, s)
		target := resourceNode(s, r.name, "release", map[string]any{
			"tag": later.TagName, "commitish": r.base, "title": "Do not replace the published release",
		})
		target["disablesourceinput"] = true
		manifest := manifestYAML(t, map[string]any{
			"name":    "Ensure a release already on page 2 exists",
			"targets": map[string]any{"ensure": target},
		})
		existing := fmt.Sprintf("Release Tag %q already exist", later.TagName)
		preview := c.ok(t, "diff", manifest)
		if strings.Contains(preview, "doesn't exist and should be created") {
			t.Fatalf("diff must not plan creation of existing release %q", later.TagName)
		}
		requireOutput(t, preview, existing)
		checkUnchanged(t)
		requireOutput(t, c.ok(t, "apply", manifest), existing)
		checkUnchanged(t)
	})
}

func readReleasePage(t *testing.T, s *server, repo string, page int) []*gitea.Release {
	t.Helper()
	releases, resp, err := s.client.Releases.ListReleases(t.Context(), s.owner, repo, gitea.ListReleasesOptions{
		ListOptions: gitea.ListOptions{Page: page, PageSize: resourcePageSize},
	})
	if err != nil {
		s.apiFailure(t, fmt.Sprintf("read releases page %d", page), resp, err)
	}
	return releases
}
